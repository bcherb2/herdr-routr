// `routr list --json` → worker records → card models.
//
// The record shape is internal/registry/types.go (Worker) plus `run_dir`
// and `final_path` that --json adds (skills/routr/cli.md); `final`, the text of
// final.md, only with `list --final` (results are read with `routr result`
// or from `final_path`). Only the fields the cards and the
// wake logic use are read; unknown fields are ignored and a malformed record
// is dropped rather than failing the whole list.

import { clip } from "./transcript.ts";

export type State =
	| "pending"
	| "waiting"
	| "gated"
	| "starting"
	| "running"
	| "blocked"
	| "maybe_done"
	| "done"
	| "closed"
	| "failed"
	| "needs_you"
	| "cancelled";

const STATES: ReadonlySet<string> = new Set<State>([
	"pending",
	"waiting",
	"gated",
	"starting",
	"running",
	"blocked",
	"maybe_done",
	"done",
	"closed",
	"failed",
	"needs_you",
	"cancelled",
]);

/** Still moving on its own: the watcher will change its state. */
export const ACTIVE: ReadonlySet<State> = new Set<State>(["pending", "waiting", "starting", "running"]);
/** Stopped and waiting on a person. */
export const NEEDS_PERSON: ReadonlySet<State> = new Set<State>(["blocked", "gated", "needs_you"]);
/** Finished one way or another. */
export const FINISHED: ReadonlySet<State> = new Set<State>(["done", "maybe_done", "failed", "closed", "cancelled"]);

export interface Worker {
	id: number;
	chain?: string;
	step?: string;
	role: string;
	/** short name for its tab (`routr start --title`); absent = the role */
	title?: string;
	harness: string;
	/** a step the watcher does itself, with no tab: a command (`run`) or a question to Jev (`judge`) */
	tabless?: "run" | "judge";
	model?: string;
	effort?: string;
	state: State;
	verdict?: string;
	/** a failed run or judge step: what comes after it waits for `routr approve` */
	halts?: boolean;
	question?: string;
	/** a blocked worker's menu: option N is the Nth; absent when its question has no menu */
	options?: string[];
	/** a blocked worker's menu, named for `routr answer --expect`; absent when its question has no menu */
	fingerprint?: string;
	/** the tool call its question is about: `pending.summary` ("Bash: rm -rf build") */
	asksAbout?: string;
	/** who answers its questions, decided by routr: human, agent or jev; absent = human */
	answerer?: string;
	/** jev: who Jev handed the question it is blocked on to (the fallback, human or agent); absent while Jev decides */
	handedTo?: string;
	/** why it waits or how it got here; for a question Jev handed over, Jev's pick and confidence */
	note?: string;
	error?: string;
	output?: string;
	transcript?: string;
	gate?: string;
	interactive?: boolean;
	round?: number;
	/** follow-ups delivered (`routr send`); a reply to each is written once it answers */
	turn?: number;
	/** follow-up replies written; turn > replies = one in flight */
	replies?: number;
	/** follow-ups queued, not yet delivered */
	queued?: number;
	/** workers that must be done before this one starts */
	after?: number[];
	/** a foreach step the watcher expands; items = its copies, absent until it has expanded */
	fanout?: { items?: number[] };
	/** a foreach copy: at most this many copies of its step run at once */
	parallel?: number;
	/** a review step's loop cap (on_fail.max) */
	roundMax?: number;
	runDir?: string;
	/** the text of final.md: only from `list --final`, which the extension does not use */
	final?: string;
	finalPath?: string;
	/** the agent that ran `routr start`: its harness and its own session id, when its environment said */
	callerHarness?: string;
	callerSession?: string;
	created?: number; // epoch ms
	updated?: number;
	started?: number;
	finished?: number;
}

type Json = Record<string, unknown>;

function str(v: unknown): string | undefined {
	return typeof v === "string" && v !== "" ? v : undefined;
}

function count(v: unknown): number | undefined {
	return typeof v === "number" && Number.isInteger(v) && v > 0 ? v : undefined;
}

function obj(v: unknown): v is Json {
	return v !== null && typeof v === "object" && !Array.isArray(v);
}

function ints(v: unknown): number[] | undefined {
	return Array.isArray(v) ? v.filter((a): a is number => Number.isInteger(a)) : undefined;
}

function strs(v: unknown): string[] | undefined {
	return Array.isArray(v) && v.length > 0 ? v.map((a) => (typeof a === "string" ? a : "")) : undefined;
}

/** RFC 3339 → epoch ms. Go's zero time ("0001-01-01T00:00:00Z", which omitempty keeps) → undefined. */
export function parseTime(v: unknown): number | undefined {
	if (typeof v !== "string") return undefined;
	const t = Date.parse(v);
	if (Number.isNaN(t) || t < Date.UTC(1971, 0, 1)) return undefined;
	return t;
}

function toWorker(v: unknown): Worker | undefined {
	if (v === null || typeof v !== "object" || Array.isArray(v)) return undefined;
	const r = v as Json;
	const id = r.id;
	if (typeof id !== "number" || !Number.isInteger(id)) return undefined;
	const state = r.state;
	if (typeof state !== "string" || !STATES.has(state)) return undefined;
	const caller = r.caller !== null && typeof r.caller === "object" ? (r.caller as Json) : undefined;
	return {
		id,
		chain: str(r.chain),
		step: str(r.step),
		role: str(r.role) ?? "task",
		title: str(r.title),
		harness: str(r.harness) ?? "?",
		tabless: str(r.run) ? "run" : obj(r.judge) ? "judge" : undefined,
		model: str(r.model),
		effort: str(r.effort),
		state: state as State,
		verdict: str(r.verdict),
		halts: r.halts === true,
		question: str(r.question),
		options: strs(r.options),
		fingerprint: str(r.fingerprint),
		asksAbout: obj(r.pending) ? str(r.pending.summary) : undefined,
		answerer: str(r.answerer),
		handedTo: str(r.handed_to),
		note: str(r.note),
		error: str(r.error),
		output: str(r.output),
		transcript: str(r.transcript),
		gate: str(r.gate),
		interactive: r.interactive === true,
		round: typeof r.round === "number" ? r.round : undefined,
		turn: count(r.turn),
		replies: count(r.replies),
		queued: Array.isArray(r.inbox) && r.inbox.length > 0 ? r.inbox.length : undefined,
		after: ints(r.after),
		fanout: obj(r.fanout) ? { items: ints(r.fanout.items) } : undefined,
		parallel: count(r.parallel),
		roundMax: obj(r.on_fail) ? count(r.on_fail.max) : undefined,
		runDir: str(r.run_dir),
		final: str(r.final),
		finalPath: str(r.final_path),
		callerHarness: str(caller?.harness),
		callerSession: str(caller?.session),
		created: parseTime(r.created),
		updated: parseTime(r.updated),
		started: parseTime(r.started),
		finished: parseTime(r.finished),
	};
}

/** Parse `routr list --json` stdout: an array, or a single object. */
export function parseList(stdout: string): Worker[] {
	const text = stdout.trim();
	if (text === "" || text === "null") return [];
	const data: unknown = JSON.parse(text);
	const items = Array.isArray(data) ? data : [data];
	return items.map(toWorker).filter((w): w is Worker => w !== undefined);
}

/**
 * What a blocked worker's question is decided with, as `routr show ID
 * --context --json` gives it (skills/routr/cli.md, "What a question is decided
 * with"): the files the model reads before it answers, the options only
 * the user may pick (they save a rule or last past this call), and the
 * question as the registry has it now, which may be newer than the last
 * `routr list`.
 */
export interface QuestionContext {
	/** who answers its question and how, a line each, as `routr wait` says it; empty when it asks nothing, or from a routr older than the field */
	answer: string[];
	/** option numbers, from 1 */
	lasting: number[];
	/** absent when it asks nothing now */
	question?: string;
	options?: string[];
	fingerprint?: string;
	asksAbout?: string;
}

/** Parse `routr show ID --context --json` stdout; undefined if it is not one. */
export function parseContext(stdout: string): QuestionContext | undefined {
	let data: unknown;
	try {
		data = JSON.parse(stdout);
	} catch {
		return undefined;
	}
	if (!obj(data) || !Array.isArray(data.docs)) return undefined;
	return {
		answer: strs(data.answer) ?? [],
		lasting: (ints(data.lasting) ?? []).filter((n) => n > 0),
		question: str(data.question),
		options: strs(data.options),
		fingerprint: str(data.fingerprint),
		asksAbout: obj(data.pending) ? str(data.pending.summary) : undefined,
	};
}

export function formatDuration(ms: number): string {
	const s = Math.max(0, Math.floor(ms / 1000));
	if (s < 60) return `${s}s`;
	const m = Math.floor(s / 60);
	if (m < 60) return `${m}m${String(s % 60).padStart(2, "0")}s`;
	const h = Math.floor(m / 60);
	return `${h}h${String(m % 60).padStart(2, "0")}m`;
}

/** harness·model·effort, skipping what's unset; "sh" for a run step, "jev" for a judge (registry Worker.Tag). */
export function engine(w: Worker): string {
	if (w.tabless === "run") return "sh";
	if (w.tabless === "judge") return "jev";
	return [w.harness, w.model, w.effort].filter(Boolean).join("·");
}

/** How long it has been running, or ran. */
export function age(w: Worker, now: number): string {
	const from = w.started ?? w.created;
	if (from === undefined) return "";
	const to = FINISHED.has(w.state) || w.state === "needs_you" ? (w.finished ?? w.updated ?? now) : now;
	return formatDuration(to - from);
}

export type Tone = "run" | "wait" | "alert" | "ok" | "fail" | "muted";

export interface Card {
	id: number;
	/** "3 review" or "3 review [auth/plan]" */
	title: string;
	engine: string;
	state: State;
	age: string;
	tone: Tone;
	/** what needs saying under the header: the error, the output, a hint (a question as one line, with the key that answers it) */
	note?: string;
	/** live tool calls / text, oldest first */
	activity: string[];
	mine: boolean;
}

/** Who answers the question a blocked worker asks: its answerer, or whoever Jev handed it to; "jev" while Jev decides. */
export function answering(w: Worker): string {
	const who = w.answerer ?? "human";
	return who === "jev" && w.handedTo ? w.handedTo : who;
}

/** Blocked while Jev decides its question: nobody needs to act yet. */
export function jevDecides(w: Worker): boolean {
	return w.state === "blocked" && answering(w) === "jev";
}

/**
 * Blocked on a question the user may answer: any but one Jev decides. With
 * answer = agent the model decides first, but it hands a question to the
 * user in words, which the extension can't see, and the user may always
 * answer: `--expect` keeps the slower of the two from pressing a key on
 * whatever came next.
 */
export function waitsForUser(w: Worker): boolean {
	return w.state === "blocked" && !jevDecides(w);
}

/** What a blocked worker asks, in one line: the tool call it is about, else its question's first line. */
export function asks(w: Pick<Worker, "asksAbout" | "question">, max = 60): string {
	if (w.asksAbout) return `asks about ${clip(w.asksAbout, max)}`;
	const q = w.question?.split("\n").find((l) => l.trim() !== "");
	return q ? `asks: ${clip(q, max)}` : "asks a question in its tab";
}

export function tone(w: Worker): Tone {
	if (jevDecides(w)) return "run";
	if (NEEDS_PERSON.has(w.state)) return "alert";
	if (w.state === "failed" || (w.state === "done" && w.verdict === "fail")) return "fail";
	if (w.state === "done" || w.state === "maybe_done") return "ok";
	if (w.state === "running" || w.state === "starting") return "run";
	if (w.state === "pending" || w.state === "waiting") return "wait";
	return "muted";
}

/** A worker's one-line note; `answerHint` ends a question's (Pi: "alt+q answer"). */
export function note(w: Worker, answerHint: string): string | undefined {
	switch (w.state) {
		case "blocked":
			if (jevDecides(w)) return "Jev decides its question";
			// the about part is clipped so the key stays on screen
			return `⚠ ${w.harness} ${w.id} ${asks(w)} · ${answerHint}`;
		case "gated":
			return `waits at a gate → /routr approve ${w.id} to start it`;
		case "needs_you":
			return `needs you: ${w.error ?? "the chain cannot go on alone"}`;
		case "failed":
			return w.error ? `failed: ${w.error}` : "failed";
		case "maybe_done":
			return `stopped without routr done${w.output ? `; check ${w.output}` : ""}`;
		case "done":
			return [w.verdict ? `verdict ${w.verdict}` : undefined, w.output].filter(Boolean).join(" · ") || undefined;
	}
	return undefined;
}

export interface CardOptions {
	now: number;
	/** keep finished workers on screen this long after they finish */
	lingerMs: number;
	/** workers this session started */
	mine: (w: Worker) => boolean;
	/** how the user answers a question, at the end of its note (Pi: "alt+q answer") */
	answerHint: string;
	/** recent activity lines per worker id */
	activity?: ReadonlyMap<number, readonly string[]>;
	maxActivity?: number;
}

/** Which workers get a card: everything not finished, plus recently finished ones. */
export function visible(w: Worker, now: number, lingerMs: number): boolean {
	if (!FINISHED.has(w.state)) return true;
	if (w.state === "closed" || w.state === "cancelled") return false;
	const at = w.finished ?? w.updated;
	return at !== undefined && now - at <= lingerMs;
}

export function toCards(workers: readonly Worker[], o: CardOptions): Card[] {
	const max = o.maxActivity ?? 3;
	return workers
		.filter((w) => visible(w, o.now, o.lingerMs))
		.sort((a, b) => rank(a) - rank(b) || a.id - b.id)
		.map((w) => {
			const chain = w.chain ? ` [${[w.chain, w.step].filter(Boolean).join("/")}]` : "";
			const acts = ACTIVE.has(w.state) || w.state === "blocked" ? (o.activity?.get(w.id) ?? []) : [];
			return {
				id: w.id,
				title: `${w.id} ${w.title ?? w.role}${chain}`,
				engine: engine(w),
				state: w.state,
				age: age(w, o.now),
				tone: tone(w),
				note: note(w, o.answerHint),
				activity: acts.slice(-max),
				mine: o.mine(w),
			};
		});
}

/** People first, then running, then queued, then finished. */
function rank(w: Worker): number {
	if (jevDecides(w)) return 1;
	if (NEEDS_PERSON.has(w.state)) return 0;
	if (w.state === "running" || w.state === "starting") return 1;
	if (ACTIVE.has(w.state)) return 2;
	return 3;
}

export type Paint = (tone: Tone | "dim" | "bold", text: string) => string;

/**
 * Card lines for the widget, unclipped (the caller truncates to the width).
 *
 *   routr · 2 running · 1 needs you
 *   ├─ ● 3 review · codex·gpt-6-astra·high · running · 4m12s
 *   │    ⎿ exec_command  rg -n auth src/
 *   └─ ! 4 research · claude·haiku·low · blocked · 1m02s
 *        ⎿ ⚠ claude 4 asks about Bash: go test ./... · alt+q answer
 */
export function cardLines(cards: readonly Card[], paint: Paint): string[] {
	if (cards.length === 0) return [];
	const counts = new Map<string, number>();
	for (const c of cards) {
		const k = c.tone === "alert" ? "need you" : c.tone === "run" ? "running" : c.tone === "wait" ? "queued" : "finished";
		counts.set(k, (counts.get(k) ?? 0) + 1);
	}
	const order = ["need you", "running", "queued", "finished"];
	const summary = order
		.filter((k) => counts.has(k))
		.map((k) => {
			const n = counts.get(k)!;
			return k === "need you" ? paint("alert", `${n} ${n === 1 ? "needs you" : "need you"}`) : `${n} ${k}`;
		})
		.join(" · ");
	const lines = [`${paint("bold", "routr")} ${paint("dim", "·")} ${summary}`];
	cards.forEach((c, i) => {
		const last = i === cards.length - 1;
		const branch = paint("dim", last ? "└─" : "├─");
		const cont = paint("dim", last ? "  " : "│ ");
		const glyph = paint(c.tone, GLYPH[c.tone]);
		const who = c.mine ? paint("bold", c.title) : c.title;
		const head = [who, paint("dim", c.engine), paint(c.tone, c.state.replace("_", " ")), paint("dim", c.age)]
			.filter((s) => s !== "")
			.join(paint("dim", " · "));
		lines.push(`${branch} ${glyph} ${head}`);
		for (const a of c.activity) lines.push(`${cont}   ${paint("dim", `⎿ ${a}`)}`);
		if (c.note) lines.push(`${cont}   ${paint(c.tone === "alert" || c.tone === "fail" ? c.tone : "dim", `⎿ ${c.note}`)}`);
	});
	return lines;
}

/**
 * A widget line as one terminal row: Pi counts a line as one row, so a line
 * break inside one (a worker's error, a multi-line command) misplaces every
 * row drawn after it. Breaks become " ↵ ", tabs a space; other control
 * characters go, except ESC, which starts the colours.
 */
export function oneRow(line: string): string {
	return line.replace(/\r\n|\r|\n/g, " ↵ ").replace(/\t/g, " ").replace(/[\x00-\x1a\x1c-\x1f\x7f]/g, "");
}

const GLYPH: Record<Tone, string> = { run: "●", wait: "◦", alert: "!", ok: "✓", fail: "✗", muted: "·" };

/** One-line footer status, or undefined when there's nothing to show. */
export function statusText(cards: readonly Card[]): string | undefined {
	const need = cards.filter((c) => c.tone === "alert").length;
	const run = cards.filter((c) => c.tone === "run" || c.tone === "wait").length;
	if (need === 0 && run === 0) return undefined;
	return [run ? `▶${run}` : "", need ? `!${need}` : ""].filter(Boolean).join(" ");
}
