// The steps view: workers as a todo-style list, one block per chain and one
// row per step, a foreach step's copies folded into one "×N" row. The other
// view is the cards in model.ts; both read the same `routr list --json`.

import { ACTIVE, age, engine, FINISHED, NEEDS_PERSON, note, tone, visible, type Paint, type Tone, type Worker } from "./model.ts";

export interface StepRow {
	name: string;
	/** a foreach step: its copies, "?" until the watcher has expanded it; undefined = one worker */
	copies?: number | "?";
	/** copies by tone */
	tally: Partial<Record<Tone, number>>;
	tone: Tone;
	gated: boolean;
	/** harness·model·effort of a single worker, "run" or "judge" for those steps */
	engine: string;
	/** verdict, "round 2/3", "max 2 at once" */
	details: string[];
	age: string;
	note?: string;
	activity: string[];
	finished: boolean;
}

export interface StepBlock {
	/** the chain, or undefined for workers started on their own */
	chain?: string;
	rows: StepRow[];
	tone: Tone;
	age: string;
	mine: boolean;
	finished: boolean;
}

export interface StepOptions {
	now: number;
	/** keep a finished chain (or ad hoc worker) on screen this long */
	lingerMs: number;
	mine: (w: Worker) => boolean;
	/** how the user answers a question, at the end of its note (Pi: "alt+q answer") */
	answerHint: string;
	/** the agent run a worker was started in: ad hoc workers of one role started together share a row */
	group?: (w: Worker) => string | undefined;
	activity?: ReadonlyMap<number, readonly string[]>;
}

/** Rolled up over copies: a person first, then work in progress, then how it ended. */
function rollup(tones: readonly Tone[]): Tone {
	const has = (t: Tone) => tones.includes(t);
	if (has("alert")) return "alert";
	if (has("run") || (has("wait") && tones.some((t) => t !== "wait"))) return "run";
	if (has("wait")) return "wait";
	if (has("fail")) return "fail";
	if (has("ok")) return "ok";
	return "muted";
}

function stepEngine(w: Worker): string {
	return w.role === "run" || w.role === "judge" ? w.role : engine(w);
}

/** From the first start to the last finish, or now while any copy is at work. */
function span(ws: readonly Worker[], now: number): string {
	if (ws.length === 1) return ws[0]!.started === undefined && !FINISHED.has(ws[0]!.state) ? "" : age(ws[0]!, now);
	const starts = ws.map((w) => w.started).filter((t): t is number => t !== undefined);
	if (starts.length === 0) return "";
	const done = ws.every((w) => FINISHED.has(w.state) || w.state === "needs_you");
	const end = done ? Math.max(...ws.map((w) => w.finished ?? w.updated ?? now)) : now;
	return age({ ...ws[0]!, started: Math.min(...starts), state: done ? "done" : "running", finished: end }, now);
}

function toRow(name: string, all: readonly Worker[], o: StepOptions): StepRow {
	// An expanded foreach worker only waits on its copies: the copies are the step.
	const ws = all.filter((w) => !w.fanout?.items?.length);
	const unexpanded = ws.find((w) => w.fanout && w.fanout.items === undefined && w.state !== "done");
	const tones = ws.map(tone);
	const t = rollup(tones);
	const tally: Partial<Record<Tone, number>> = {};
	for (const x of tones) tally[x] = (tally[x] ?? 0) + 1;
	const foreach = all.some((w) => w.fanout !== undefined || w.parallel !== undefined);
	const copies = unexpanded ? "?" : foreach || ws.length > 1 ? ws.filter((w) => !w.fanout).length : undefined;
	const one = ws.length === 1 ? ws[0]! : undefined;
	const details: string[] = [];
	if (one?.verdict) details.push(one.verdict);
	const looped = ws.find((w) => w.round !== undefined && w.round > 1 && w.roundMax);
	if (looped) details.push(`round ${looped.round}/${looped.roundMax}`);
	const par = ws.find((w) => w.parallel)?.parallel;
	if (par && typeof copies === "number" && copies > par && t !== "ok" && t !== "fail") details.push(`max ${par} at once`);
	if (copies === 0) details.push("no files matched");
	// a done step's verdict is in its details already
	const noted = ws.find((w) => tone(w) === t && w.state !== "done" && note(w, o.answerHint) !== undefined);
	const acts = one && (ACTIVE.has(one.state) || one.state === "blocked") ? (o.activity?.get(one.id) ?? []) : [];
	return {
		name,
		copies,
		tally,
		tone: t,
		gated: t === "alert" && ws.every((w) => w.state === "gated" || !NEEDS_PERSON.has(w.state)),
		engine: one && copies !== 0 ? stepEngine(one) : "",
		details,
		age: copies === 0 ? "" : span(ws, o.now),
		note: noted ? note(noted, o.answerHint) : undefined,
		activity: acts.slice(-1),
		finished: ws.every((w) => FINISHED.has(w.state)),
	};
}

function toBlock(chain: string | undefined, rows: Map<string, Worker[]>, o: StepOptions): StepBlock {
	const ws = [...rows.values()].flat();
	const r = [...rows].map(([name, g]) => toRow(name, g, o));
	return {
		chain,
		rows: r,
		tone: rollup(r.map((x) => x.tone)),
		age: span(ws, o.now),
		mine: ws.some(o.mine),
		finished: r.every((x) => x.finished),
	};
}

/**
 * Blocks to show: every chain with a worker still at work or recently
 * finished, with all its steps, then the workers started on their own.
 */
export function toSteps(workers: readonly Worker[], o: StepOptions): StepBlock[] {
	const byId = [...workers].sort((a, b) => a.id - b.id);
	const chains = new Map<string, Map<string, Worker[]>>();
	const adhoc = new Map<string, Worker[]>();
	for (const w of byId) {
		if (w.chain) {
			const steps = chains.get(w.chain) ?? new Map<string, Worker[]>();
			chains.set(w.chain, steps);
			const name = w.step ?? w.title ?? w.role;
			steps.set(name, [...(steps.get(name) ?? []), w]);
		} else if (visible(w, o.now, o.lingerMs)) {
			const name = w.title ?? w.role;
			const run = o.group?.(w);
			const key = run ? `${run}\u0000${name}` : `alone:${w.id}`;
			adhoc.set(key, [...(adhoc.get(key) ?? []), w]);
		}
	}
	const blocks: StepBlock[] = [];
	for (const [chain, steps] of chains) {
		const ws = [...steps.values()].flat();
		if (ws.some((w) => visible(w, o.now, o.lingerMs))) blocks.push(toBlock(chain, steps, o));
	}
	blocks.sort((a, b) => rank(a) - rank(b));
	if (adhoc.size > 0) {
		// one worker: "12 research", as on its card; a group: "research", with ×N
		const rows = new Map<string, Worker[]>();
		for (const ws of adhoc.values()) {
			const name = ws[0]!.title ?? ws[0]!.role;
			rows.set(ws.length === 1 || rows.has(name) ? `${ws[0]!.id} ${name}` : name, ws);
		}
		blocks.push(toBlock(undefined, rows, o));
	}
	return blocks;
}

function rank(b: StepBlock): number {
	return b.tone === "alert" ? 0 : b.finished ? 2 : 1;
}

const GLYPH: Record<Tone, string> = { run: "▶", wait: "○", alert: "!", ok: "✔", fail: "✘", muted: "⊘" };
const TALLY: readonly Tone[] = ["alert", "fail", "ok", "run", "wait", "muted"];

function glyph(r: { tone: Tone; gated?: boolean }): string {
	return r.gated ? "⏸" : GLYPH[r.tone];
}

const cells = (s: string): number => [...s].length;

const label = (r: StepRow): string => `${r.name}${r.copies !== undefined ? ` ×${r.copies}` : ""}`;

function rowLines(r: StepRow, width: number, paint: Paint): string[] {
	const pad = " ".repeat(Math.max(0, width - cells(label(r))));
	const tally =
		r.copies !== undefined && r.copies !== "?" && r.copies > 0
			? TALLY.filter((t) => r.tally[t]).map((t) => paint(t, `${GLYPH[t]}${r.tally[t]}`)).join(" ")
			: "";
	// filter before painting: a theme wraps even "" in escape codes
	const parts: [Tone | "dim", string][] = [["dim", r.engine], ...r.details.map((d): [Tone | "dim", string] => [d === "fail" ? "fail" : "dim", d]), ["dim", r.age]];
	const meta = [tally, ...parts.filter(([, t]) => t !== "").map(([tone, t]) => paint(tone, t))].filter((s) => s !== "").join(paint("dim", " · "));
	const name = r.tone === "wait" || r.tone === "muted" ? paint("dim", label(r)) : label(r);
	const out = [`  ${paint(r.tone, glyph(r))} ${name}${meta ? `${pad}  ${meta}` : ""}`];
	for (const a of r.activity) out.push(`      ${paint("dim", `⎿ ${a}`)}`);
	if (r.note) out.push(`      ${paint(r.tone === "alert" || r.tone === "fail" ? r.tone : "dim", `⎿ ${r.note}`)}`);
	return out;
}

/** "✔ 2 done: plan, review" in place of finished rows, or "○ 3 more: …" in place of queued ones. */
function folded(rows: readonly StepRow[], t: Tone, word: string, paint: Paint): string {
	return `  ${paint(t, GLYPH[t])} ${paint("dim", `${rows.length} ${word}: ${rows.map((r) => r.name).join(", ")}`)}`;
}

function blockLines(b: StepBlock, paint: Paint, fold: number): string[] {
	const done = b.rows.filter((r) => r.finished).length;
	const title = b.chain ?? "ad hoc";
	const meta = [b.chain ? `${done}/${b.rows.length} steps` : "", b.age].filter(Boolean).join(" · ");
	const head = `${paint(b.tone, glyph(b))} ${b.mine ? paint("bold", title) : title}${meta ? ` ${paint("dim", `· ${meta}`)}` : ""}`;
	if (b.finished && b.chain) return [head];
	const width = Math.max(...b.rows.map((r) => cells(label(r))));
	let rows = b.rows.map((r) => ({ r, lines: rowLines(r, width, paint) }));
	const out: string[] = [head];
	if (fold >= 1) {
		// the steps that went well before the first one that did not
		const lead = rows.findIndex(({ r }) => !r.finished || r.tone !== "ok");
		const n = lead === -1 ? rows.length : lead;
		if (n > 1) {
			out.push(folded(rows.slice(0, n).map(({ r }) => r), "ok", "done", paint));
			rows = rows.slice(n);
		}
	}
	if (fold >= 2) {
		// the queued steps after the last one at work
		let tail = rows.length;
		while (tail > 1 && rows[tail - 1]!.r.tone === "wait") tail--;
		if (rows.length - tail > 1) {
			const rest = rows.slice(tail).map(({ r }) => r);
			rows = rows.slice(0, tail);
			for (const { lines } of rows) out.push(...lines);
			out.push(folded(rest, "wait", "more", paint));
			return out;
		}
	}
	for (const { lines } of rows) out.push(...lines);
	return out;
}

/**
 * Lines for the widget, unclipped (the caller truncates to the width).
 * Over maxLines, finished steps fold into one line first, then queued ones.
 *
 *   ▶ oauth-3 · 3/5 steps · 9m12s
 *     ✔ plan            claude·opus·high · 4m12s
 *     ✔ review          codex · pass · round 2/3 · 2m03s
 *     ▶ implement ×4    ✔2 ▶1 ○1 · max 2 at once · 3m
 *     ○ test
 *     ⏸ merge
 *         ⎿ waits at a gate → /routr approve 9 to start it
 */
export function stepLines(blocks: readonly StepBlock[], paint: Paint, maxLines = 12): string[] {
	let lines: string[] = [];
	for (let fold = 0; fold <= 2; fold++) {
		lines = blocks.flatMap((b) => blockLines(b, paint, fold));
		if (lines.length <= maxLines) break;
	}
	return lines;
}
