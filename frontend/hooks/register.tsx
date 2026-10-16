// routr for Claude Code: wakes the model when a worker it started finishes or
// needs someone, shows the workers as steps above the prompt (or as cards in
// a pane) with their live tool calls, and answers a worker's question from a
// picker. The Claude Code counterpart of pi/src/index.ts. What to wake for,
// and when, is the core's Coordinator (../core); so is the layout of steps and
// cards. This module turns Claude Code's events into the Coordinator's, runs
// the CLI, delivers what it returns and draws.
//
// Everything goes through the routr CLI (`list --json`, `result`, `show
// --context --json`, `answer`); the registry is never read. A mod may pass `$`
// only to functions declared at the top of this file, so every call on `$` is
// here. What the drawings read is in $.state (../types); the rest of the
// module's state is its own: a reload starts it over, as a Pi restart does,
// with the claims kept in $.store.

import { atom, read, update } from "claude-code";
import type { ElementConstructor, EngineInterface as Engine, Register, TextProps, TurnCompleteReason } from "claude-code";
import {
	ACTIVE,
	activityLine,
	answerArgs,
	answered,
	asks,
	cardLines,
	closed,
	Coordinator,
	holdFromEnv,
	oneRow,
	parseContext,
	parseLine,
	parseList,
	question,
	questionKey,
	statusText,
	stepLines,
	stillAsked,
	toCards,
	toSteps,
	waiting,
	type ClaimsEntry,
	type Notice,
	type Paint,
	type QuestionContext,
	type TurnEnd,
	type WakeItem,
	type WakeMode,
	type Worker,
} from "../core/index.ts";
import type { Asker, Picker, View, ViewMode } from "../types";

/** No file-watch API: the registry is polled. Pi polls at 1.5 s while busy and watches the file. */
const POLL_MS = 2000;
/** tool-call tails and ages; Pi's are 700 ms */
const TAIL_MS = 1000;
/** how much of a transcript's end a tail reads: its last tool calls, past a long tool result */
const TAIL_BYTES = 256 * 1024;
const ACTIVITY_KEEP = 20;
const LINGER_MS = 90_000;
/** most rows of steps in the band; Pi's widget holds 12 */
const BAND_ROWS = 12;
/** opens the picker; ends a waiting worker's note */
const ANSWER_HINT = "/routr answer";
/** $.store keys of the claims kept per session, and how many sessions keep theirs */
const CLAIMS_KEY = "claims:";
const KEEP_SESSIONS = 50;
const SECTION_ID = "routr:wake";
const SECTION =
	"The routr plugin is active: start routr workers without --wait, and don't run routr wait in the background for them;" +
	" end your turn instead. The plugin wakes you with their results (a message starting with [routr])," +
	" and puts a worker's question into your turn as soon as it asks.";

const SUBCOMMANDS = ["list", "show", "hide", "result", "answer", "approve", "cancel", "close", "wake", "view"];

/** pane ids */
const CARDS = "routr-cards";
const PICKER = "routr-answer";
/** after an option routr lets only the user pick (`lasting`) */
const LASTS = " (lasts past this call)";

const viewState = atom({ plugin: "routr", key: "view" } as const, { mode: "steps", steps: [], cards: [], waiting: [] } as View);
const pickerState = atom({ plugin: "routr", key: "picker" } as const, null as Picker | null);

let coord = new Coordinator();
let sessionId = "";
let cwd = "";
let wakeMode: WakeMode = "mine";
/** this repo has routr state, or the session ran routr: only then is `routr list` polled */
let engaged = false;
let polling = false;
let ticking = false;
let lastError = "";
/** what the view last held, to write it only when it changed */
let drawn = "";
/** background shells the model started, task id → command: their end arrives as a task notification */
const background = new Map<string, string>();
/** the last tool calls of each worker at work, read from its transcript's end when the file grew */
const tails = new Map<number, { path: string; size: number; lines: string[] }>();

// ── Painting: the core lays out lines through a Paint; these turn its runs into Text ──

const INKS = ["run", "wait", "alert", "ok", "fail", "muted", "dim", "bold"] as const;
type Ink = (typeof INKS)[number];
const OPEN = "";
const SHUT = "";

/** Marks each painted run in the line (private-use characters, which oneRow keeps), for runs() to read back. */
const mark: Paint = (ink, text) => (text === "" ? "" : `${OPEN}${INKS.indexOf(ink)}${text}${SHUT}`);

/** A marked line as its runs, each with the innermost ink it was painted with. */
function runs(line: string): { ink?: Ink; text: string }[] {
	const out: { ink?: Ink; text: string }[] = [];
	const inks: Ink[] = [];
	let text = "";
	const flush = (): void => {
		if (text) out.push({ ink: inks.at(-1), text });
		text = "";
	};
	for (let i = 0; i < line.length; i++) {
		const c = line[i]!;
		if (c === OPEN) {
			flush();
			inks.push(INKS[Number(line[++i])] ?? "dim");
		} else if (c === SHUT) {
			flush();
			inks.pop();
		} else text += c;
	}
	flush();
	return out;
}

/** Pi's colours for each ink, in the person's theme. */
function style(ink: Ink | undefined): TextProps {
	switch (ink) {
		case "run":
			return { color: "suggestion" };
		case "alert":
		case "fail":
			return { color: "error" };
		case "ok":
			return { color: "success" };
		case "bold":
			return { bold: true };
		case undefined:
			return {};
		default:
			return { dimColor: true };
	}
}

/** One line the core laid out, as one row cut at the box's width. */
function row(Text: ElementConstructor<TextProps>, line: string) {
	return <Text wrap="truncate">{runs(oneRow(line)).map((r) => <Text {...style(r.ink)}>{r.text}</Text>)}</Text>;
}

// ── The CLI ──

/** Runs routr; `missing` when the binary can't be started. */
async function routr($: Engine, args: readonly string[]): Promise<{ code: number; stdout: string; stderr: string; missing: boolean }> {
	const bin = (await $.env.get("ROUTR_BIN"))?.trim() || "routr";
	try {
		const r = await $.process.run([bin, ...args], { cwd, timeoutMs: 15_000 });
		return { code: r.exitCode, stdout: r.stdout, stderr: r.stderr, missing: false };
	} catch (err) {
		return { code: 1, stdout: "", stderr: `${bin}: ${err instanceof Error ? err.message : String(err)}`, missing: true };
	}
}

/** Does this repo have routr state? The CLI's rule (skills/routr/cli.md, "Where state lives"). */
async function hasState($: Engine): Promise<boolean> {
	const dir = await $.env.get("ROUTR_STATE_DIR");
	if (dir) return $.fs.exists(`${dir}/state`);
	try {
		const r = await $.process.run(["git", "rev-parse", "--path-format=absolute", "--git-common-dir"], { cwd, timeoutMs: 3000 });
		const common = r.stdout.trim();
		if (r.exitCode === 0 && common.endsWith("/.git")) return $.fs.exists(`${common.slice(0, -"/.git".length)}/.routr/state`);
	} catch {
		// not a git repo: the working directory's own
	}
	return $.fs.exists(`${cwd}/.routr/state`);
}

/** The claims kept for session `id`, oldest first. */
async function loadClaims($: Engine, id: string): Promise<Partial<ClaimsEntry>[]> {
	const v = await $.store.get(CLAIMS_KEY + id);
	return Array.isArray(v) ? (v as Partial<ClaimsEntry>[]) : [];
}

/** Keeps new claims with the session, and forgets the claims of all but the latest sessions. */
async function saveClaims($: Engine, keep: readonly ClaimsEntry[]): Promise<void> {
	if (keep.length === 0 || !sessionId) return;
	const key = CLAIMS_KEY + sessionId;
	await $.store.set(key, [...(await loadClaims($, sessionId)), ...keep]);
	const old = (await $.store.keys()).filter((k) => k.startsWith(CLAIMS_KEY) && k !== key);
	for (const k of old.slice(0, Math.max(0, old.length - KEEP_SESSIONS + 1))) await $.store.delete(k);
}

/**
 * The session this coordinator is for. After a /clear the process goes on
 * under a new session id with no session.start: start over, as Pi does on
 * /new, so this conversation wakes only for the workers it starts.
 */
async function sync($: Engine): Promise<void> {
	const id = await $.session.id();
	if (id === sessionId) return;
	coord.end();
	sessionId = id;
	background.clear();
	coord = new Coordinator({ sessionId: id, wakeMode, maxHoldMs: holdFromEnv(await $.env.get("ROUTR_WAKE_HOLD")), restored: await loadClaims($, id) });
}

// ── The view ──

function asker(w: Worker): Asker {
	return { id: w.id, label: `${w.id} ${w.title ?? w.role} · ${w.harness} ${asks(w)}` };
}

/** Lays the workers out as steps, cards and the status count, and writes what changed for the drawings. */
async function refreshView($: Engine): Promise<void> {
	const o = {
		now: await $.clock.now(),
		lingerMs: LINGER_MS,
		mine: (w: Worker) => coord.isMine(w),
		answerHint: ANSWER_HINT,
		activity: new Map([...tails].map(([id, t]) => [id, t.lines] as const)),
	};
	const cards = toCards([...coord.workers], o);
	const s = statusText(cards);
	$.ui.status(s); // Claude Code draws it after the plugin's name: "⚠ routr: ▶2 !1"
	const view = {
		steps: stepLines(toSteps(coord.workers, { ...o, group: (w) => coord.group(w) }), mark, BAND_ROWS),
		cards: cardLines(cards, mark),
		waiting: waiting(coord.workers).map(asker),
	};
	const next = JSON.stringify(view);
	if (next === drawn) return;
	drawn = next;
	await update($, viewState, (v) => ({ ...v, ...view }));
}

/** Reads the end of each working worker's transcript that grew since the last read; says if any did. */
async function readTails($: Engine): Promise<boolean> {
	const want = coord.workers.filter((w) => w.transcript && (ACTIVE.has(w.state) || w.state === "blocked"));
	for (const id of tails.keys()) if (!want.some((w) => w.id === id)) tails.delete(id);
	const grew = await Promise.all(
		want.map(async (w) => {
			const path = w.transcript!;
			try {
				const { size } = await $.fs.stat(path);
				const cur = tails.get(w.id);
				if (cur?.path === path && cur.size === size) return false;
				const r = await $.process.run(["tail", "-c", String(TAIL_BYTES), path], { timeoutMs: 3000 });
				if (r.exitCode !== 0) return false;
				const lines = r.stdout.split("\n");
				if (size > TAIL_BYTES) lines.shift(); // started mid-line
				const acts = lines.flatMap((l) => parseLine(w.harness, l).flatMap((e) => activityLine(e) ?? []));
				tails.set(w.id, { path, size, lines: acts.slice(-ACTIVITY_KEEP) });
				return true;
			} catch {
				return false; // not written yet (the harness creates it after starting)
			}
		}),
	);
	return grew.some(Boolean);
}

async function tick($: Engine): Promise<void> {
	if (ticking || !engaged) return;
	ticking = true;
	try {
		await readTails($);
		await refreshView($); // also ticks the ages
	} finally {
		ticking = false;
	}
}

/** Steps in the band, or cards in their pane. */
async function setView($: Engine, mode: ViewMode): Promise<void> {
	await update($, viewState, (v) => ({ ...v, mode }));
	if (mode === "steps") return $.ui.close({ id: CARDS });
	const r = await $.ui.open({ id: CARDS, title: "routr workers" });
	if (!r.isPlaced) $.ui.toast("the cards pane waits for a wider terminal");
}

async function poll($: Engine): Promise<void> {
	if (polling) return;
	polling = true;
	try {
		await sync($);
		if (!engaged) engaged = await hasState($);
		if (!engaged) return;
		const r = await routr($, ["list", "--json"]);
		if (r.code !== 0) {
			const err = r.missing ? "routr not found: put it on PATH or set ROUTR_BIN" : `routr list failed: ${(r.stderr || r.stdout).trim().split("\n")[0] ?? ""}`;
			if (err !== lastError) $.ui.toast(err);
			lastError = err;
			return;
		}
		lastError = "";
		let workers: Worker[];
		try {
			workers = parseList(r.stdout);
		} catch (err) {
			$.ui.log(`routr list --json: ${err instanceof Error ? err.message : String(err)}`, { to: "debug" });
			return;
		}
		await pickerStands($, workers);
		const c = coord;
		const { wake, keep } = c.snapshot(workers);
		await saveClaims($, keep);
		await refreshView($);
		if (wake.length > 0) await deliver($, c, wake);
	} finally {
		polling = false;
	}
}

// ── Waking the model ──

/** What `routr result ID` prints, else what the list carried. */
async function resultText($: Engine, w: Worker): Promise<string> {
	const r = await routr($, ["result", String(w.id)]);
	if (r.code === 0 && r.stdout.trim()) return r.stdout;
	return w.final ?? (r.stdout || r.stderr).trim();
}

/** Who answers a question and how (`routr show ID --context --json`); undefined if routr could not say. */
async function questionContext($: Engine, w: Worker): Promise<QuestionContext | undefined> {
	const r = await routr($, ["show", String(w.id), "--context", "--json"]);
	return r.code === 0 ? parseContext(r.stdout) : undefined;
}

/**
 * Wakes the model with `items`: into its running turn when a worker needs
 * someone ($.session.append: the model reads it at its next request), else
 * as a turn of its own ($.prompt.submit, not awaited: it resolves only when
 * that turn starts). A failed delivery is held for the next poll.
 */
async function deliver($: Engine, c: Coordinator, items: WakeItem[]): Promise<void> {
	const want = c.wants(items);
	const results = new Map<number, string>();
	const contexts = new Map<number, QuestionContext>();
	await Promise.all([
		...want.results.map(async (w) => results.set(w.id, await resultText($, w))),
		...want.contexts.map(async (w) => {
			const q = await questionContext($, w);
			if (q) contexts.set(w.id, q);
		}),
	]);
	const wk = c.compose(items, results, contexts);
	if (!wk) return;
	const failed = (err: unknown): void => {
		const { held, dropped } = c.deliveryFailed(items);
		const why = err instanceof Error ? err.message : String(err);
		const lost = dropped.length ? `; gave up on worker ${dropped.map((i) => i.worker.id).join(", ")} (routr list shows it)` : "";
		$.ui.toast(`could not deliver a result: ${why}${held.length ? "; retrying" : ""}${lost}`);
	};
	if (wk.intoTurn) {
		try {
			const r = await $.session.append({ message: { type: "user", content: [{ type: "text", text: wk.message.content }] } });
			if ("deny" in r && r.deny) return failed(r.deny);
			c.delivered(items, true);
		} catch (err) {
			failed(err);
		}
		return;
	}
	c.delivered(items, false);
	$.prompt.submit({ text: wk.message.content }).catch(failed);
}

/** How Claude Code's turn ended, in the core's words. */
function turnEnd(reason: TurnCompleteReason): TurnEnd {
	return reason === "aborted" ? "aborted" : reason === "answer" ? "answer" : "error";
}

/** A background shell's task notification: its task id and whether it completed (exit 0). */
function taskNotice(text: string): { task: string; completed: boolean } | undefined {
	const task = /<task-id>([^<]+)<\/task-id>/.exec(text)?.[1];
	if (!task) return undefined;
	return { task, completed: /<status>completed<\/status>/.test(text) };
}

// ── The picker ──

/** A notice from the core. Claude Code heads what a plugin shows with its name, so the core's "routr: " (and the CLI's) goes. */
function notify($: Engine, n: Notice): void {
	const text = n.text.replace(/^(routr: )+/, "");
	if (n.level === "info") $.ui.toast(text);
	else $.ui.log(text);
}

/** The picker's rows: a header, the question, its options, "Open its tab" and "Later". */
function pickerRows(p: Picker): number {
	// a header that wraps once, the question, its options, "Open its tab", "Later"
	if (p.stage === "reading") return 2;
	return p.stage === "choose" ? p.askers.length + 3 : 2 + p.text.length + Math.max(1, p.options.length) + 2;
}

async function showPicker($: Engine, p: Picker): Promise<void> {
	await Promise.all([
		update($, pickerState, () => p),
		$.ui.open({ id: PICKER, title: "routr: answer", focus: true, closeOnEscape: true, holdToasts: true, rows: pickerRows(p) }),
	]);
}

async function closePicker($: Engine): Promise<void> {
	await update($, pickerState, () => null);
	await $.ui.close({ id: PICKER });
}

/**
 * `/routr answer [ID]` and the band's buttons: the picker for a worker's
 * question, or with several waiting and no ID, which worker first. Nothing
 * opens by itself (question.ts says why).
 */
async function openPicker($: Engine, id?: number): Promise<void> {
	const ws = waiting(coord.workers).filter((w) => id === undefined || w.id === id);
	if (ws.length === 0) return $.ui.toast(id === undefined ? "no worker waits for an answer" : `${id} asks you nothing`);
	// Opened before anything is awaited: Claude Code places a pane that answers a press at any width, a later one only from 144 columns.
	const one = ws.length === 1 ? ws[0]! : undefined;
	await showPicker($, one ? { stage: "reading", id: one.id } : { stage: "choose", askers: ws.map(asker), shown: ws.map((w) => [w.id, questionKey(w)!]) });
	if (one) await showQuestion($, one);
}

/** Worker w's question, newer than the last list, with the options that last. */
async function showQuestion($: Engine, w: Worker): Promise<void> {
	const q = question(w, await questionContext($, w));
	if (q) {
		const head = q.asksAbout ? `${q.who} asks about ${q.asksAbout}` : `${q.who} asks:`;
		const options = q.options.map((o, i) => `${o}${q.lasting.includes(i + 1) ? LASTS : ""}`);
		return showPicker($, { stage: "question", id: q.id, head, text: q.text, options, fingerprint: q.fingerprint, key: q.key });
	}
	await closePicker($);
	$.ui.toast(`${w.id} asks nothing now`);
	void poll($);
}

async function chooseWorker($: Engine, id: number): Promise<void> {
	const w = coord.workers.find((x) => x.id === id);
	if (w) return showQuestion($, w);
	await closePicker($);
}

/** An option goes as `routr answer ID N --by human --expect FP`: pressed only on the question the person read. */
async function pickOption($: Engine, id: number, n: number, fingerprint: string | undefined): Promise<void> {
	await closePicker($);
	const a = await routr($, answerArgs(id, n, fingerprint));
	notify($, answered(id, a.code, a.code === 0 ? a.stdout : a.stderr || a.stdout, ANSWER_HINT));
	$.clock.after(100, () => void poll($));
}

async function openTab($: Engine, id: number): Promise<void> {
	await closePicker($);
	const s = await routr($, ["show", String(id), "--focus"]);
	if (s.code !== 0) $.ui.log(`routr show ${id}: ${(s.stderr || s.stdout).trim().split("\n")[0] || `exit ${s.code}`}`);
}

/** Closes the picker when a question it shows changed or went away (answered in its tab, by the model, by Jev). */
async function pickerStands($: Engine, workers: readonly Worker[]): Promise<void> {
	const { value: p } = await $.state.get({ plugin: "routr", key: "picker" });
	if (!p || p.stage === "reading") return; // showQuestion reads it afresh
	const shown = new Map(p.stage === "choose" ? p.shown : [[p.id, p.key]]);
	if (stillAsked(shown, workers)) return;
	await closePicker($);
	notify($, p.stage === "question" ? closed(p.id, workers, ANSWER_HINT) : { text: `the questions changed. ${ANSWER_HINT} shows them.`, level: "info" });
}

// ── /routr ──

/**
 * `/routr` with one of SUBCOMMANDS: the CLI's output is shown here, not to
 * the model. `answer` with no option opens the picker; `wake` and `view`
 * change this session.
 */
async function command($: Engine, args: string): Promise<void> {
	const [cmd = "list", ...rest] = args.trim().split(/\s+/).filter(Boolean);
	if (cmd === "wake") {
		const m = rest[0];
		if (m === "mine" || m === "all" || m === "off") wakeMode = coord.wakeMode = m;
		$.ui.log(`waking the model for ${wakeMode === "mine" ? "workers this session started" : wakeMode === "all" ? "every worker" : "no worker"}`);
		return;
	}
	if (cmd === "view") {
		const v = rest[0];
		const now = (await $.state.get({ plugin: "routr", key: "view" })).value?.mode ?? "steps";
		await setView($, v === "steps" || v === "cards" ? v : now === "steps" ? "cards" : "steps");
		return;
	}
	const answerUsage = "usage: /routr answer [ID [N]] (N = the option number; without it, a picker)";
	if (cmd === "answer" && rest[1] === undefined) {
		if (rest[0] !== undefined && !/^\d+$/.test(rest[0])) return $.ui.log(answerUsage);
		engaged = true;
		await poll($);
		return openPicker($, rest[0] === undefined ? undefined : Number(rest[0]));
	}
	if (cmd !== "list" && !/^\d+$|^[\w.:-]+$/.test(rest[0] ?? "")) return $.ui.log(`usage: /routr ${cmd} ID`);
	if (cmd === "answer" && !(/^\d+$/.test(rest[0] ?? "") && /^\d+$/.test(rest[1] ?? ""))) return $.ui.log(answerUsage);
	// an answer typed from the wake text or the hint: only on the question the mod last saw
	const argv =
		cmd === "list"
			? ["list", ...rest]
			: cmd === "answer"
				? answerArgs(Number(rest[0]), Number(rest[1]), coord.workers.find((w) => w.id === Number(rest[0]))?.fingerprint)
				: [cmd, rest[0]!];
	const r = await routr($, argv);
	if (r.missing) return $.ui.log("routr not found: put it on PATH or set ROUTR_BIN");
	engaged = true;
	if (cmd === "answer" && r.code === 4) {
		notify($, answered(Number(rest[0]), r.code, r.stderr, ANSWER_HINT));
	} else {
		if (cmd === "cancel") coord.userCancelled(`routr ${argv.join(" ")}`, r.stdout);
		const text = [r.stdout, r.stderr].map((s) => s.trim()).filter(Boolean).join("\n");
		$.ui.log(`$ routr ${argv.join(" ")}\n${text || (r.code === 0 ? "(no output)" : `exit ${r.code}`)}`);
	}
	void poll($);
}

export const register: Register = (on) => {
	on("session.start", async ($, e, next) => {
		cwd = e.cwd;
		await sync($);
		$.clock.every(POLL_MS, () => void poll($));
		$.clock.every(TAIL_MS, () => void tick($));
		void poll($);
		return next(e);
	});

	on("session.end", ($, e, next) => {
		coord.end(); // a /clear: the next poll or turn starts over under the new id
		return next(e);
	});

	on("prompt.compose", async ($, e, next) => {
		const r = await next(e);
		return { sections: [...r.sections.filter((s) => s.id !== SECTION_ID), { id: SECTION_ID, text: SECTION, scope: "session" }] };
	});

	on("turn.start", async ($, e, next) => {
		await sync($);
		coord.turnStarted();
		return next(e);
	});

	on("turn.step", async function* ($, e, next) {
		coord.stepStarted();
		return yield* next(e);
	});

	on("turn.complete", async ($, e, next) => {
		const r = await next(e);
		if (coord.turnEnded(turnEnd(e.reason))) $.clock.after(300, () => void poll($));
		return r;
	});

	// A `routr wait` (or `--wait`) the model runs hands it these results itself:
	// their news is held until the call returns. A background one returns at
	// once; its task notification says when it ended.
	on("tool.call", { tool: "Bash" }, async ($, e, next) => {
		const command = e.command;
		if (!/routr|ROUTR_BIN/.test(command)) return next(e);
		engaged = true;
		if (e.run_in_background === true) {
			const r = await next(e);
			const task = /\bID:\s*([A-Za-z0-9]+)/.exec(r.text ?? "")?.[1];
			if (task) {
				background.set(task, command);
				coord.backgroundStarted(task, command);
			}
			return r;
		}
		const c = coord;
		c.shellStarted(e.tool_use_id, command);
		try {
			const r = await next(e);
			if (r.deny !== undefined) c.shellAborted(e.tool_use_id);
			else await saveClaims($, c.shellEnded(e.tool_use_id, command, r.text ?? "") ?? []);
			$.clock.after(200, () => void poll($));
			return r;
		} catch (err) {
			c.shellAborted(e.tool_use_id);
			throw err;
		}
	}).catch(($, e, next) => next(e)); // never in the way: a failure here lets the call run as it would (or replays it)

	// A background shell's end reaches the conversation as a task notification:
	// a turn of its own when idle, or a row folded into the running turn.
	on("session.append", ($, e, next) => {
		if (e.origin.kind === "task-notification" && !e.agentId) {
			const text = e.message.content.map((b) => (b.type === "text" ? b.text : "")).join("\n");
			const n = taskNotice(text);
			const command = n && background.get(n.task);
			if (n && command !== undefined) {
				background.delete(n.task);
				coord.backgroundEnded(n.task, command, n.completed);
				$.clock.after(200, () => void poll($));
			}
		}
		return next(e);
	}).catch(($, e, next) => next(e));

	// /routr is the routr skill's command, which Claude Code names after the skill and holds
	// against a plugin's own: routr:routr as this plugin ships it, routr for a copy of the
	// user's. The mod answers the subcommands; anything else runs the skill with those words.
	on("command.run", { command: ["routr", "routr:routr"] }, async ($, e, next) => {
		if (!SUBCOMMANDS.includes(e.args.trim().split(/\s+/)[0] || "list")) return next(e);
		await command($, e.args);
		return {};
	});

	// The steps above the prompt, and a button for each worker waiting on the person (no hotkey: a digit typed into an empty prompt would press it).
	on("ui.render", { component: "AbovePrompt" }, async ($, e, next) => {
		if (e.props.hasSurvey) return next(e);
		const v = await read($, viewState);
		const lines = v.mode === "steps" ? v.steps : []; // a band taller than its room scrolls
		if (lines.length === 0 && v.waiting.length === 0) return next(e);
		const { Box, Text, Button } = $.ui.resolve(e);
		const buttons = v.waiting.map((a) => <Button key={`answer-${a.id}`} label={`answer ${a.id}`} onPress={() => void openPicker($, a.id)} />);
		return (
			<Box flexDirection="column" width={e.props.bodyColumns}>
				{lines.map((l) => row(Text, l))}
				{buttons.length > 0 ? [<Box flexDirection="row" gap={1}>{buttons}</Box>] : []}
			</Box>
		);
	});

	on("ui.render", { component: "Pane", requestId: CARDS }, async ($, e) => {
		const v = await read($, viewState);
		const lines = v.cards;
		const { Box, Text } = $.ui.resolve(e);
		return (
			<Box flexDirection="column" width={e.props.bodyColumns}>
				{lines.length > 0 ? lines.map((l) => row(Text, l)) : [<Text dimColor>No routr workers to show.</Text>]}
			</Box>
		);
	});

	// The picker: digits pick an option, t opens the worker's tab, l or Esc is Later.
	on("ui.render", { component: "Pane", requestId: PICKER }, async ($, e) => {
		const p = await read($, pickerState);
		const { Box, Text, Button } = $.ui.resolve(e);
		// digits for the first nine; the ring starts on the first, as Pi's select does
		const key = (i: number) => ({ ...(i < 9 ? { hotkey: String(i + 1) } : {}), ...(i === 0 ? { autoFocus: true as const } : {}) });
		const later = <Button key="later" plain hotkey="l" label="Later" role="dismiss" onPress={() => void closePicker($)} />;
		if (!p) return <Box flexDirection="column">{[<Text dimColor>No question to answer.</Text>, later]}</Box>;
		if (p.stage === "reading") return <Box flexDirection="column">{[<Text dimColor>Reading {p.id}'s question…</Text>, later]}</Box>;
		if (p.stage === "choose") {
			return (
				<Box flexDirection="column" width={e.props.bodyColumns}>
					<Text bold>{p.askers.length} workers wait for an answer. Which one?</Text>
					{p.askers.map((a, i) => (
						<Button key={`worker-${a.id}`} plain {...key(i)} label={a.label} onPress={() => void chooseWorker($, a.id)} />
					))}
					{later}
				</Box>
			);
		}
		const q = p;
		const none = q.options.length === 0 ? [<Text dimColor>It has no options to pick here: answer it in its tab.</Text>] : [];
		return (
			<Box flexDirection="column" width={e.props.bodyColumns}>
				<Text bold>{q.head}</Text>
				{q.text.map((l) => <Text>{l}</Text>)}
				{none}
				{q.options.map((o, i) => (
					<Button
						key={`option-${i + 1}`}
						plain
						{...key(i)}
						label={`${i < 9 ? "" : `${i + 1}. `}${o}`}
						onPress={() => void pickOption($, q.id, i + 1, q.fingerprint)}
					/>
				))}
				<Button key="tab" plain hotkey="t" label="Open its tab" onPress={() => void openTab($, q.id)} />
				{later}
			</Box>
		);
	});

	on("ui.close", async ($, e, next) => {
		if (e.id === PICKER) await update($, pickerState, () => null);
		if (e.id === CARDS && e.origin.kind === "person") await update($, viewState, (v): View => ({ ...v, mode: "steps" }));
		return next(e);
	});
};
