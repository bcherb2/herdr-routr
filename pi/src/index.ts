// routr for Pi: worker cards with live tool calls, and waking the model when
// a worker it started has news. See ../README.md.
//
// Everything goes through the routr CLI (`list --json`, `result`, `answer`,
// …); the registry is only watched for changes, never read.

import { existsSync, readFileSync, watch, type FSWatcher } from "node:fs";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";
import { execFileSync } from "node:child_process";
import type { ExtensionAPI, ExtensionContext } from "@earendil-works/pi-coding-agent";
import { Text, truncateToWidth } from "@earendil-works/pi-tui";
import {
	ACTIVE,
	activityLine,
	answerArgs,
	answered,
	cardLines,
	closed,
	Coordinator,
	holdFromEnv,
	jevDecides,
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
	type Paint,
	type QuestionContext,
	type Tone,
	type TurnEnd,
	type WakeEntry,
	type WakeItem,
	type WakeMode,
	type Worker,
} from "../../frontend/core/index.ts";
import { runRoutr, staleBinary, type Stale } from "./cli.ts";
import { ANSWER_KEY, chooser, questionPicker } from "./dialogs.ts";
import { LineTail } from "./tail.ts";

const baseDir = dirname(fileURLToPath(import.meta.url));
/** the checkout this extension and its skill are loaded from (install.sh links them) */
const repoDir = join(baseDir, "..", "..");

const WIDGET = "routr";
const WAKE_TYPE = "routr-wake";
const OUTPUT_TYPE = "routr-output";
const CLAIMS_TYPE = "routr-claims";

const POLL_BUSY_MS = 1500;
const POLL_IDLE_MS = 5000;
const POLL_MISSING_MS = 30_000;
const TAIL_MS = 700;
const LINGER_MS = 90_000;
const ACTIVITY_KEEP = 20;
/** ends a waiting worker's note in the widget */
const ANSWER_HINT = `${ANSWER_KEY} answer`;

type View = "steps" | "cards";

const SUBCOMMANDS = ["list", "show", "hide", "result", "answer", "approve", "cancel", "close", "wake", "view"];
/** the second word of the subcommands that take a fixed one */
const CHOICES: Record<string, string[]> = { wake: ["mine", "all", "off"], view: ["steps", "cards"] };

/** The .routr directory for cwd, by the CLI's rule (skills/routr/cli.md, "Where state lives"). */
function routrDir(cwd: string): string {
	if (process.env.ROUTR_STATE_DIR) return process.env.ROUTR_STATE_DIR;
	try {
		const common = execFileSync("git", ["rev-parse", "--path-format=absolute", "--git-common-dir"], {
			cwd,
			encoding: "utf8",
			stdio: ["ignore", "pipe", "ignore"],
			timeout: 3000,
		}).trim();
		return join(common.endsWith("/.git") ? dirname(common) : cwd, ".routr");
	} catch {
		return join(cwd, ".routr");
	}
}

function skillPaths(): string[] {
	if (process.env.ROUTR_PI_SKILL === "0") return [];
	// packaged copy first (pi/skills/routr), then the repo's (../skills/routr)
	for (const p of [join(baseDir, "..", "skills", "routr"), join(baseDir, "..", "..", "skills", "routr")]) {
		if (existsSync(join(p, "SKILL.md"))) return [p];
	}
	return [];
}

function wakeModeFromEnv(): WakeMode {
	const v = process.env.ROUTR_PI_WAKE;
	return v === "all" || v === "off" ? v : "mine";
}

function viewFromEnv(): View {
	return process.env.ROUTR_PI_VIEW === "cards" ? "cards" : "steps";
}

/** How Pi's agent run ended, from its last assistant message. */
function turnEnd(messages: readonly unknown[]): TurnEnd {
	for (let i = messages.length - 1; i >= 0; i--) {
		const m = messages[i] as { role?: string; stopReason?: string } | undefined;
		if (m?.role !== "assistant") continue;
		return m.stopReason === "aborted" ? "aborted" : m.stopReason === "error" ? "error" : "answer";
	}
	return "answer";
}

function painter(theme: ExtensionContext["ui"]["theme"]): Paint {
	const color: Record<Tone | "dim", Parameters<typeof theme.fg>[0]> = {
		run: "accent",
		wait: "muted",
		alert: "error",
		ok: "success",
		fail: "error",
		muted: "dim",
		dim: "dim",
	};
	return (tone, text) => (tone === "bold" ? theme.bold(text) : theme.fg(color[tone], text));
}

export default function routr(pi: ExtensionAPI): void {
	// Per-session state; reset in session_start.
	let ctx: ExtensionContext | undefined;
	let cwd = process.cwd();
	let dotRoutr = "";
	let wakeMode: WakeMode = wakeModeFromEnv();
	/** which workers wake the model, and with what; one per session */
	let coord = new Coordinator({ wakeMode, maxHoldMs: holdFromEnv(process.env.ROUTR_WAKE_HOLD) });
	let view: View = viewFromEnv();
	let engaged = false; // this repo has a registry, or the session ran routr
	let missing = false;
	let lastError = "";
	const tails = new Map<number, { tail: LineTail; harness: string; lines: string[] }>();
	let pollTimer: NodeJS.Timeout | undefined;
	let tailTimer: NodeJS.Timeout | undefined;
	let watcher: FSWatcher | undefined;
	let refreshing = false;
	let again = false;
	let gen = 0; // bumped on session start/shutdown; a refresh from an older session drops its result
	/** the routr binary is older than this extension: settled once session_start's check returns */
	let stale: Promise<Stale | undefined> = Promise.resolve(undefined);
	let debounce: NodeJS.Timeout | undefined;
	/** the answer dialog on screen: the question it shows of each worker (id → questionKey); a refresh closes it when one changes or goes away */
	let dialog: { shown: Map<number, string>; abort: AbortController } | undefined;
	let picking = false; // alt+q or /routr answer is running, dialogs and CLI calls

	function ui(fn: (c: ExtensionContext) => void): void {
		if (!ctx?.hasUI) return;
		try {
			fn(ctx);
		} catch {
			// the context went stale (session replaced); session_shutdown cleans up
		}
	}

	function render(): void {
		const now = Date.now();
		const activity = new Map([...tails].map(([id, t]) => [id, t.lines] as const));
		const workers = coord.workers;
		const mine = (w: Worker): boolean => coord.isMine(w);
		const cards = toCards(workers, { now, lingerMs: LINGER_MS, mine, answerHint: ANSWER_HINT, activity });
		const steps = view === "steps" ? toSteps(workers, { now, lingerMs: LINGER_MS, mine, answerHint: ANSWER_HINT, group: (w) => coord.group(w), activity }) : [];
		ui((c) => {
			if (missing || (view === "steps" ? steps.length : cards.length) === 0) {
				c.ui.setWidget(WIDGET, undefined);
				c.ui.setStatus(WIDGET, missing && engaged ? c.ui.theme.fg("warning", "routr: not found") : undefined);
				return;
			}
			c.ui.setWidget(WIDGET, (_tui, theme) => {
				const lines = view === "steps" ? stepLines(steps, painter(theme)) : cardLines(cards, painter(theme));
				return { render: (width: number) => lines.map((l) => truncateToWidth(oneRow(l), width)), invalidate() {} };
			});
			const s = statusText(cards);
			const alert = cards.some((k) => k.tone === "alert");
			c.ui.setStatus(WIDGET, s ? c.ui.theme.fg(alert ? "error" : "accent", `routr ${s}`) : undefined);
		});
	}

	function syncTails(): void {
		const want = new Set<number>();
		for (const w of coord.workers) {
			if (!w.transcript || !(ACTIVE.has(w.state) || w.state === "blocked")) continue;
			want.add(w.id);
			const cur = tails.get(w.id);
			if (!cur || cur.tail.path !== w.transcript) {
				tails.set(w.id, { tail: new LineTail(w.transcript), harness: w.harness, lines: [] });
			}
		}
		for (const id of tails.keys()) if (!want.has(id)) tails.delete(id);
		if (tails.size > 0 && !tailTimer) {
			tailTimer = setInterval(readTails, TAIL_MS);
			tailTimer.unref?.();
		} else if (tails.size === 0 && tailTimer) {
			clearInterval(tailTimer);
			tailTimer = undefined;
		}
	}

	function readTails(): void {
		for (const t of tails.values()) {
			for (const line of t.tail.read()) {
				for (const e of parseLine(t.harness, line)) {
					const a = activityLine(e);
					if (a) t.lines.push(a);
				}
			}
			if (t.lines.length > ACTIVITY_KEEP) t.lines.splice(0, t.lines.length - ACTIVITY_KEEP);
		}
		render(); // also ticks the ages
	}

	function schedule(ms?: number): void {
		if (pollTimer) clearTimeout(pollTimer);
		// A gated or blocked worker can wait for hours; the registry watch catches its change. Jev decides in seconds,
		// and an open answer dialog must close soon after its question goes.
		const busy =
			coord.workers.some((w) => ACTIVE.has(w.state) || jevDecides(w)) || coord.pending().length > 0 || coord.held().length > 0 || dialog !== undefined;
		const delay = ms ?? (missing ? POLL_MISSING_MS : busy ? POLL_BUSY_MS : POLL_IDLE_MS);
		pollTimer = setTimeout(() => void refresh(), delay);
		pollTimer.unref?.();
	}

	function ensureWatcher(): void {
		if (watcher) return;
		const dir = join(dotRoutr, "state");
		if (!existsSync(dir)) return;
		engaged = true;
		try {
			watcher = watch(dir, (_ev, file) => {
				if (file && !String(file).startsWith("registry.json")) return;
				if (debounce) clearTimeout(debounce);
				debounce = setTimeout(() => void refresh(), 150);
				debounce.unref?.();
			});
			watcher.unref(); // never keep Pi (or a test) alive on its own
			watcher.on("error", () => {
				watcher?.close();
				watcher = undefined;
			});
		} catch {
			watcher = undefined; // polling still works
		}
	}

	async function refresh(): Promise<void> {
		if (!ctx) return;
		if (refreshing) {
			again = true;
			return;
		}
		refreshing = true;
		const mine = gen;
		try {
			ensureWatcher();
			if (!engaged) return;
			// Active workers and those changed within the last hour (skills/routr/cli.md);
			// `--all` would read every run's final.md on every poll.
			const r = await runRoutr(["list", "--json"], cwd);
			if (mine !== gen) return;
			missing = r.missing;
			if (r.missing) return;
			if (r.code !== 0) {
				const err = (r.stderr || r.stdout).trim().split("\n")[0] ?? "";
				if (err !== lastError) ui((c) => c.ui.notify(`routr list failed: ${err}`, "warning"));
				lastError = err;
				return;
			}
			lastError = "";
			let workers: Worker[];
			try {
				workers = parseList(r.stdout);
			} catch (e) {
				const err = `routr list --json: ${e instanceof Error ? e.message : String(e)}`;
				if (err !== lastError) ui((c) => c.ui.notify(err, "warning"));
				lastError = err;
				return;
			}
			if (dialog && !stillAsked(dialog.shown, workers)) dialog.abort.abort();
			const c = coord;
			const { wake: items, keep } = c.snapshot(workers);
			for (const k of keep) pi.appendEntry<ClaimsEntry>(CLAIMS_TYPE, k);
			syncTails();
			if (items.length > 0) {
				wake(c, items).catch((e: unknown) => {
					// held again for the next refresh, so a failed delivery doesn't lose a result
					const { held, dropped } = c.deliveryFailed(items);
					const why = e instanceof Error ? e.message : String(e);
					const retry = held.length ? "; retrying" : "";
					const lost = dropped.length ? `; gave up on worker ${dropped.map((i) => i.worker.id).join(", ")} (routr list shows it)` : "";
					ui((c) => c.ui.notify(`routr: could not deliver a result: ${why}${retry}${lost}`, "error"));
				});
			}
		} finally {
			refreshing = false;
			if (mine !== gen) {
				// the session changed while the CLI ran; the new one owns the timers
				if (again) {
					again = false;
					schedule(0);
				}
				return;
			}
			render();
			if (again) {
				again = false;
				schedule(0);
			} else schedule();
		}
	}

	async function resultText(w: Worker): Promise<string> {
		const r = await runRoutr(["result", String(w.id)], cwd);
		if (r.code === 0 && r.stdout.trim()) return r.stdout;
		if (w.finalPath) {
			try {
				return readFileSync(w.finalPath, "utf8");
			} catch {
				// fall through to the text list carried
			}
		}
		return w.final ?? (r.stdout || r.stderr).trim();
	}

	/** What a question the model answers is decided with; undefined if routr could not say (the wake names the command). */
	async function questionContext(w: Worker): Promise<QuestionContext | undefined> {
		const r = await runRoutr(["show", String(w.id), "--context", "--json"], cwd);
		return r.code === 0 ? parseContext(r.stdout) : undefined;
	}

	async function wake(c: Coordinator, items: WakeItem[]): Promise<void> {
		const want = c.wants(items);
		const results = new Map<number, string>();
		const contexts = new Map<number, QuestionContext>();
		await Promise.all([
			...want.results.map(async (w) => results.set(w.id, await resultText(w))),
			...want.contexts.map(async (w) => {
				const q = await questionContext(w);
				if (q) contexts.set(w.id, q);
			}),
		]);
		// once sent, Pi queues it until the model's turn allows, and nothing recalls it from there
		const wk = c.compose(items, results, contexts);
		if (!wk) return;
		pi.sendMessage(
			{ customType: WAKE_TYPE, content: wk.message.content, display: true, details: wk.message.details },
			{ triggerTurn: true, deliverAs: wk.intoTurn ? "steer" : "followUp" },
		);
		c.delivered(items, wk.intoTurn);
	}

	/** ui.select that a refresh closes when a question it shows (id → questionKey) changes or goes away; closed says it did. */
	async function select(c: ExtensionContext, title: string, items: string[], shown: Map<number, string>): Promise<{ label?: string; closed: boolean }> {
		const abort = new AbortController();
		dialog = { shown, abort };
		schedule(); // the busy pace while it is open
		try {
			const label = await c.ui.select(title, items, { signal: abort.signal });
			return { label, closed: label === undefined && abort.signal.aborted };
		} finally {
			if (dialog?.abort === abort) dialog = undefined;
		}
	}

	/**
	 * alt+q, `/routr answer` and `/routr answer ID`: the picker for a
	 * worker's question. With several waiting and no ID, which worker first.
	 * An option goes as `routr answer ID N --by human --expect FP`. One at a
	 * time, from the key to the answer.
	 */
	async function answerPicker(c: ExtensionContext, id?: number): Promise<void> {
		if (picking) return;
		picking = true;
		try {
			await pickAnswer(c, id);
		} finally {
			picking = false;
		}
	}

	async function pickAnswer(c: ExtensionContext, id?: number): Promise<void> {
		const mine = gen;
		const ws = waiting(coord.workers).filter((w) => id === undefined || w.id === id);
		if (ws.length === 0) {
			c.ui.notify(id === undefined ? "routr: no worker waits for an answer" : `routr: ${id} asks you nothing`, "info");
			return;
		}
		let w = ws[0]!;
		if (ws.length > 1) {
			const d = chooser(ws);
			const r = await select(c, d.title, d.items, new Map(ws.map((x) => [x.id, questionKey(x)!])));
			if (mine !== gen) return;
			if (r.closed) return c.ui.notify(`routr: the questions changed. ${ANSWER_KEY} shows them.`, "info");
			const chosen = d.choose(r.label);
			if (!chosen) return;
			w = chosen;
		}
		// newer than the last list, and it names the options that last
		const q = question(w, await questionContext(w));
		if (mine !== gen) return;
		if (!q) {
			c.ui.notify(`routr: ${w.id} asks nothing now`, "info");
			schedule(0);
			return;
		}
		const d = questionPicker(q);
		const r = await select(c, d.title, d.items, new Map([[w.id, q.key]]));
		if (mine !== gen) return;
		if (r.closed) {
			const n = closed(w.id, coord.workers, ANSWER_KEY);
			return c.ui.notify(n.text, n.level);
		}
		const pick = d.choose(r.label);
		if (pick.kind === "later") return;
		if (pick.kind === "tab") {
			const s = await runRoutr(["show", String(w.id), "--focus"], cwd);
			if (s.code !== 0 && mine === gen) c.ui.notify(`routr show ${w.id}: ${(s.stderr || s.stdout).trim().split("\n")[0] || `exit ${s.code}`}`, "error");
			return;
		}
		const a = await runRoutr(answerArgs(w.id, pick.n, q.fingerprint), cwd);
		if (mine !== gen) return;
		const n = answered(w.id, a.code, a.code === 0 ? a.stdout : a.stderr || a.stdout, ANSWER_KEY);
		c.ui.notify(n.text, n.level);
		schedule(100);
	}

	function stop(): void {
		gen++;
		coord.end();
		dialog?.abort.abort();
		dialog = undefined;
		if (debounce) clearTimeout(debounce);
		debounce = undefined;
		if (pollTimer) clearTimeout(pollTimer);
		if (tailTimer) clearInterval(tailTimer);
		pollTimer = tailTimer = undefined;
		watcher?.close();
		watcher = undefined;
		ui((c) => {
			c.ui.setWidget(WIDGET, undefined);
			c.ui.setStatus(WIDGET, undefined);
		});
	}

	// ── Pi events ────────────────────────────────────────────────────────

	pi.on("resources_discover", () => ({ skillPaths: skillPaths() }));

	pi.on("session_start", (_event, c) => {
		stop();
		ctx = c;
		cwd = c.cwd;
		dotRoutr = routrDir(cwd);
		const restored = c.sessionManager
			.getEntries()
			.flatMap((e) => (e.type === "custom" && e.customType === CLAIMS_TYPE && e.data ? [e.data as Partial<ClaimsEntry>] : []));
		coord = new Coordinator({
			sessionId: c.sessionManager.getSessionId(),
			wakeMode,
			maxHoldMs: holdFromEnv(process.env.ROUTR_WAKE_HOLD),
			restored,
		});
		tails.clear();
		missing = false;
		engaged = existsSync(join(dotRoutr, "state"));
		if (c.mode === "tui" || c.mode === "rpc") schedule(0);
		const mine = gen;
		stale = staleBinary(repoDir, cwd);
		void stale.then((s) => {
			if (s && mine === gen)
				ui((u) => u.ui.notify(`routr: the installed binary${s.revision ? ` (${s.revision})` : ""} is older than ${s.repo}; run ${s.repo}/install.sh`, "warning"));
		});
	});

	pi.on("session_shutdown", () => {
		stop();
		ctx = undefined;
	});

	pi.on("agent_start", () => coord.turnStarted());

	pi.on("agent_end", (event) => {
		// every tool call of the run is over, however it ended; news held for the end of the turn goes now
		if (coord.turnEnded(turnEnd(event.messages))) schedule(200);
	});

	pi.on("before_agent_start", async (event) => {
		const s = await stale;
		const permissions = s
			? ` routr's binary is older than this skill: pass --permissions bypass to routr start and routr flow start, and tell the user to run ${s.repo}/install.sh.`
			: " Leave out --permissions: you run without approvals, so routr gives your workers bypass.";
		// a section, not a whole systemPrompt: Pi records only its changes in the transcript and keeps the cached prefix
		event.systemPromptOptions.sections.routr =
			"The routr extension is active: start routr workers without --wait and end your turn;" +
			" it wakes you with their results (a message starting with [routr])." +
			permissions;
	});

	// A `routr wait` (or `--wait`) the model is running will hand it these
	// results itself: hold their wake until the call returns.
	pi.on("tool_call", (event) => {
		if (event.toolName !== "bash") return;
		const command = typeof event.input.command === "string" ? event.input.command : "";
		coord.shellStarted(event.toolCallId, command);
	});

	pi.on("tool_result", (event) => {
		if (event.toolName !== "bash") return;
		const command = typeof event.input.command === "string" ? event.input.command : "";
		const output = event.content
			.map((b) => (b.type === "text" ? b.text : ""))
			.join("\n");
		const keep = coord.shellEnded(event.toolCallId, command, output);
		if (keep === undefined) return;
		for (const k of keep) pi.appendEntry<ClaimsEntry>(CLAIMS_TYPE, k);
		engaged = true;
		schedule(200);
	});

	// A call that ended without a tool_result (blocked, aborted) covers nothing any more.
	pi.on("tool_execution_end", (event) => {
		if (coord.shellAborted(event.toolCallId)) schedule(200);
	});

	// ── Rendering ────────────────────────────────────────────────────────

	pi.registerMessageRenderer<{ entries: WakeEntry[] }>(WAKE_TYPE, (message, { expanded }, theme) => {
		const entries = message.details?.entries ?? [];
		const paint = painter(theme);
		const lines: string[] = [];
		for (const e of entries) {
			const tone: Tone =
				e.kind === "blocked" || e.kind === "gated" || e.kind === "needs_you"
					? "alert"
					: e.kind === "failed" || e.verdict === "fail"
						? "fail"
						: "ok";
			const verdict = e.verdict ? ` · verdict ${e.verdict}` : "";
			lines.push(
				`${paint(tone, "◆")} ${paint("bold", `routr ${e.title}`)} ${paint("dim", e.engine)} ${paint(tone, `→ ${e.kind.replace("_", " ")}${verdict}`)}`,
			);
			if (!expanded && e.preview) lines.push(`  ${paint("dim", `⎿ ${e.preview}`)}`);
			if (!expanded && e.output) lines.push(`  ${paint("dim", `⎿ ${e.output}`)}`);
		}
		if (expanded || entries.length === 0) {
			const content = typeof message.content === "string" ? message.content : "";
			lines.push(...content.split("\n").map((l) => paint("dim", l)));
		}
		return new Text(lines.join("\n"), 0, 0);
	});

	pi.registerEntryRenderer<{ command: string; text: string; code: number }>(OUTPUT_TYPE, (entry, _opts, theme) => {
		const d = entry.data ?? { command: "routr", text: "", code: 0 };
		const head = theme.fg(d.code === 0 ? "accent" : "error", `$ routr ${d.command}`);
		return new Text(`${head}\n${d.text.trimEnd() || theme.fg("dim", "(no output)")}`, 1, 0);
	});

	// ── Answering, /routr ────────────────────────────────────────────────

	// Unbound in Pi on Linux and macOS (docs/keybindings.md); on Windows and
	// WSL it restores queued messages, and this takes it over (Pi warns).
	pi.registerShortcut(ANSWER_KEY, {
		description: "routr: answer a worker's question",
		handler: (c) => answerPicker(c),
	});

	pi.registerCommand("routr", {
		description: "routr workers: list · show N · hide N · result N · answer [N [K]] · approve N · cancel N · close N · wake mine|all|off · view steps|cards",
		getArgumentCompletions: (prefix) => {
			const [first = "", second] = prefix.trimStart().split(/\s+/);
			const items =
				second === undefined
					? SUBCOMMANDS.filter((s) => s.startsWith(first)).map((s) => ({ value: s, label: s }))
					: (CHOICES[first] ?? []).filter((s) => s.startsWith(second)).map((s) => ({ value: `${first} ${s}`, label: s }));
			return items.length > 0 ? items : null;
		},
		handler: async (args, c) => {
			const words = args.trim().split(/\s+/).filter(Boolean);
			const [cmd = "list", ...rest] = words;
			if (cmd === "wake") {
				const m = rest[0];
				if (m === "mine" || m === "all" || m === "off") wakeMode = coord.wakeMode = m;
				c.ui.notify(`routr: waking the model for ${wakeMode === "mine" ? "workers this session started" : wakeMode === "all" ? "every worker" : "no worker"}`, "info");
				return;
			}
			if (cmd === "view") {
				const v = rest[0];
				if (v === "steps" || v === "cards") view = v;
				else if (v === undefined) view = view === "steps" ? "cards" : "steps";
				c.ui.notify(`routr: showing workers as ${view}`, "info");
				render();
				return;
			}
			if (!SUBCOMMANDS.includes(cmd)) {
				// Free text: hand it to the model with the skill, like the prompt templates do.
				const text = `Use the routr skill to do this: ${args.trim()}`;
				if (c.isIdle()) pi.sendUserMessage(text);
				else pi.sendUserMessage(text, { deliverAs: "followUp" });
				return;
			}
			const answerUsage = "usage: /routr answer [ID [N]] (N = the option number; without it, a picker)";
			if (cmd === "answer" && rest[1] === undefined) {
				if (rest[0] !== undefined && !/^\d+$/.test(rest[0])) c.ui.notify(answerUsage, "warning");
				else await answerPicker(c, rest[0] === undefined ? undefined : Number(rest[0]));
				return;
			}
			const needsId = cmd !== "list";
			if (needsId && !/^\d+$|^[\w.:-]+$/.test(rest[0] ?? "")) {
				c.ui.notify(`usage: /routr ${cmd} ID`, "warning");
				return;
			}
			if (cmd === "answer" && !(/^\d+$/.test(rest[0] ?? "") && /^\d+$/.test(rest[1] ?? ""))) {
				c.ui.notify(answerUsage, "warning");
				return;
			}
			// an answer typed from the wake text or the hint: only on the question the extension last saw
			const argv =
				cmd === "list"
					? ["list", ...rest]
					: cmd === "answer"
						? answerArgs(Number(rest[0]), Number(rest[1]), coord.workers.find((w) => w.id === Number(rest[0]))?.fingerprint)
						: [cmd, rest[0]!];
			const r = await runRoutr(argv, c.cwd);
			if (r.missing) {
				c.ui.notify("routr not found: put it on PATH or set ROUTR_BIN", "error");
				return;
			}
			if (cmd === "answer" && r.code === 4) {
				const n = answered(Number(rest[0]), r.code, r.stderr, ANSWER_KEY);
				c.ui.notify(n.text, n.level);
				schedule(0);
				return;
			}
			if (cmd === "cancel") coord.userCancelled(`routr ${argv.join(" ")}`, r.stdout);
			const text = [r.stdout, r.stderr].map((s) => s.trim()).filter(Boolean).join("\n");
			if (cmd === "list" || cmd === "result" || text.includes("\n")) {
				pi.appendEntry(OUTPUT_TYPE, { command: argv.join(" "), text, code: r.code });
			} else {
				c.ui.notify(`routr ${argv.join(" ")}: ${text.trim() || (r.code === 0 ? "ok" : `exit ${r.code}`)}`, r.code === 0 ? "info" : "error");
			}
			engaged = true;
			schedule(100);
		},
	});
}
