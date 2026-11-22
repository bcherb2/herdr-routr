// When to wake the model, and what to tell it.
//
// A worker the session started "has news" when it reaches a state the model
// should act on. Workers that were started together (in one prompt's turn,
// or one chain) are reported together: a finished worker is held while its
// siblings are still running, so a 4-way research fan-out wakes the model
// once with 4 results, and a plan → review → implement chain wakes it at the
// gate with the plan and the review. A worker that needs a person, or one
// that failed, releases its group at once, and no news is held longer than
// the hold cap (`ROUTR_WAKE_HOLD`): a long chain still reports each step's
// result within a couple of minutes of it, not when the whole chain stops.

import { ACTIVE, engine, jevDecides, refKey, repoTag, routrIn, workerKey, type QuestionContext, type State, type Worker } from "./model.ts";
import { clip } from "./transcript.ts";

export type WakeKind = "done" | "maybe_done" | "blocked" | "needs_you" | "gated" | "failed" | "closed" | "reply";

const WAKE_STATES: ReadonlySet<State> = new Set<State>(["done", "maybe_done", "blocked", "needs_you", "gated", "failed"]);
const URGENT: ReadonlySet<WakeKind> = new Set<WakeKind>(["blocked", "needs_you", "gated", "failed"]);
/** Kinds whose result text (`routr result ID`) is worth fetching. */
export const WANTS_RESULT: ReadonlySet<WakeKind> = new Set<WakeKind>(["done", "maybe_done", "failed", "needs_you", "reply"]);

export interface WakeItem {
	worker: Worker;
	kind: WakeKind;
}

/** A held item, and when it was held (`WakeTracker` clock). */
interface Held extends WakeItem {
	heldAt: number;
}

/** How long news waits on its group at most, by default. */
export const DEFAULT_HOLD_MS = 120_000;

/**
 * The hold cap from `ROUTR_WAKE_HOLD`: seconds ("90"), or a number with a
 * unit ("1500ms", "90s", "2m"). 0 holds nothing. Unset or unreadable: the
 * default.
 */
export function holdFromEnv(v: string | undefined): number {
	const m = /^\s*(\d+(?:\.\d+)?)\s*(ms|s|m)?\s*$/.exec(v ?? "");
	if (!m) return DEFAULT_HOLD_MS;
	const n = Number(m[1]);
	return Math.round(m[2] === "ms" ? n : m[2] === "m" ? n * 60_000 : n * 1000);
}

/** The wake kind for a worker now, given the state it was last seen in. */
export function wakeKind(w: Worker, prev: State | undefined): WakeKind | undefined {
	// Jev decides its question: it answers (the worker runs on) or hands it
	// over (news then), in seconds. Nothing for the model to do meanwhile.
	if (jevDecides(w)) return undefined;
	if (WAKE_STATES.has(w.state)) return w.state as WakeKind;
	// An interactive worker stays running across a follow-up: its answer is
	// news once every follow-up sent has a reply and none is queued. Its
	// first turn has no follow-up; that is the user's, in its tab.
	if (w.state === "running" && w.interactive && w.turn && w.replies === w.turn && !w.queued) return "reply";
	// done → closed is the normal end and was reported as done. Closed straight
	// from a live state means the tab went away before `routr done`.
	if (w.state === "closed" && prev !== undefined && (ACTIVE.has(prev) || prev === "blocked" || prev === "gated")) {
		return "closed";
	}
	return undefined;
}

/**
 * Identity of one piece of news: a new finish, round, question, follow-up
 * reply or error is new news (a failed worker whose follow-up was dropped
 * fails again with a new error). The reply count matters when the running
 * state of a short follow-up fell between two snapshots: done stays done, with the same
 * finish, and only the count says it answered again. The fingerprint tells
 * two questions with the same words apart ("Do you want to proceed?" about
 * two commands), when routr records the second without a running state
 * between.
 */
function wakeKey(w: Worker): string {
	return [w.state, w.finished ?? "", w.round ?? "", w.question ?? "", w.fingerprint ?? "", w.replies ?? "", w.error ?? ""].join("|");
}

export interface WakeContext {
	/** did this session start the worker (or is waking for all workers on) */
	isMine: (w: Worker) => boolean;
	/** workers in the same group are reported together; undefined = alone */
	group: (w: Worker) => string | undefined;
	/**
	 * Is a tool call running right now that will hand the model this worker's
	 * result (`routr wait`, `start --wait`)? Its news is held until the call
	 * ends; if the call returned it, `markSeen` drops it.
	 */
	covered?: (w: Worker) => boolean;
	/**
	 * Did the session claim the worker after it began? The first list of a
	 * registry is a baseline, but not for these: the first list of another
	 * repo's registry comes after the model queued a worker there.
	 */
	fresh?: (w: Worker) => boolean;
}

/** Which registry a key (workerKey) is in: "" for the session's own. */
function keyRegistry(key: string): string {
	const i = key.lastIndexOf("#");
	return i < 0 ? "" : key.slice(0, i);
}

/**
 * An upstream in one of these states won't become `done` on its own, so a
 * worker queued after it can't start until someone acts.
 */
const STOPPED: ReadonlySet<State> = new Set<State>(["failed", "cancelled", "needs_you", "maybe_done", "blocked", "gated"]);
const QUEUED: ReadonlySet<State> = new Set<State>(["pending", "waiting"]);

export interface WakeTrackerOptions {
	/** the clock, epoch ms */
	now?: () => number;
	/** news waits on its group at most this long; then the group goes out as it is */
	maxHoldMs?: number;
}

/** Which list `update` is fed: the registry (undefined: the session's own), and every worker the front end lists now, of all registries. */
export interface UpdateScope {
	registry?: string;
	all?: readonly Worker[];
}

// Keyed by workerKey: a worker id is only unique in its registry.
export class WakeTracker {
	/** every worker seen, as last seen: its previous state, and upstreams that dropped out of `list` */
	private known = new Map<string, Worker>();
	private notified = new Map<string, string>();
	private held = new Map<string, Held>();
	private suppress = new Map<string, number>(); // key → updates of its registry left
	private failures = new Map<string, { key: string; tries: number }>(); // key → failed deliveries of its news (wakeKey)
	/** registries whose first list was the baseline ("" for the session's own) */
	private baselined = new Set<string>();
	private readonly now: () => number;
	private readonly maxHoldMs: number;

	constructor(opts: WakeTrackerOptions = {}) {
		this.now = opts.now ?? Date.now;
		this.maxHoldMs = opts.maxHoldMs ?? DEFAULT_HOLD_MS;
	}

	private hold(item: WakeItem): void {
		this.held.set(workerKey(item.worker), { worker: item.worker, kind: item.kind, heldAt: this.now() });
	}

	/**
	 * The model already has these workers' results (it ran `routr wait` or
	 * `routr result`); don't wake it for their current news, whether it is
	 * held already or still to come. Keys are workerKey's.
	 */
	markSeen(keys: readonly string[]): void {
		for (const k of keys) {
			if (this.held.delete(k)) continue;
			this.suppress.set(k, 3);
		}
	}

	/**
	 * Feed one fresh `routr list` snapshot of one registry (scope; the
	 * session's own by default); returns the news to deliver now. A
	 * registry's first snapshot only sets its baseline: what had already
	 * happened before this session was looking is shown on the cards, not
	 * woken for.
	 */
	update(workers: readonly Worker[], ctx: WakeContext, scope: UpdateScope = {}): WakeItem[] {
		const reg = scope.registry ?? "";
		const first = !this.baselined.has(reg);
		for (const w of workers) {
			const id = workerKey(w);
			const prev = this.known.get(id)?.state;
			this.known.set(id, w);
			const kind = wakeKind(w, prev);
			if (w.state === "cancelled") {
				// cancelled: what it had to say before is moot
				this.held.delete(id);
				this.failures.delete(id);
			}
			if (ACTIVE.has(w.state) && !kind) {
				// back at work (a follow-up, or an on_fail round): old news is stale
				this.notified.delete(id);
				this.held.delete(id);
				this.failures.delete(id);
				continue;
			}
			if (!kind) continue;
			const key = wakeKey(w);
			if (this.notified.get(id) === key) continue;
			if ((first && !ctx.fresh?.(w)) || this.suppress.has(id)) {
				this.notified.set(id, key);
				this.suppress.delete(id);
				continue;
			}
			this.notified.set(id, key); // also for others' workers: turning on "wake all" later mustn't replay old news
			if (ctx.isMine(w)) this.hold({ worker: w, kind });
		}
		this.baselined.add(reg);
		for (const [id, left] of this.suppress) {
			if (keyRegistry(id) !== reg) continue;
			if (left <= 1) this.suppress.delete(id);
			else this.suppress.set(id, left - 1);
		}
		return this.release(scope.all ?? workers, ctx);
	}

	/**
	 * Delivering these items failed: hold them again, so the next `update`
	 * releases them, unless the worker has newer news, is back at work, or
	 * the model has read its result meanwhile (`markSeen`). After `maxTries`
	 * failed deliveries of one piece of news it is dropped; the card still
	 * shows the worker's state.
	 */
	requeue(items: readonly WakeItem[], maxTries = 3): { held: WakeItem[]; dropped: WakeItem[] } {
		const held: WakeItem[] = [];
		const dropped: WakeItem[] = [];
		for (const item of items) {
			const id = workerKey(item.worker);
			const key = wakeKey(item.worker);
			const now = this.known.get(id);
			if (this.held.has(id) || this.suppress.has(id) || this.notified.get(id) !== key || (now && ACTIVE.has(now.state) && item.kind !== "reply")) continue;
			const f = this.failures.get(id);
			const tries = (f?.key === key ? f.tries : 0) + 1;
			if (tries >= maxTries) {
				this.failures.delete(id);
				dropped.push(item);
				continue;
			}
			this.failures.set(id, { key, tries });
			this.hold(item);
			held.push(item);
		}
		return { held, dropped };
	}

	/** Items still held back, waiting on siblings. */
	pending(): WakeItem[] {
		return [...this.held.values()].map(unheld);
	}

	/**
	 * The model cancelled these workers (`routr cancel`; keys are
	 * workerKey's): drop their held news, and don't wake for it once the next
	 * snapshot shows them. A finished result stays: cancel leaves a done
	 * worker done.
	 */
	drop(keys: readonly string[]): void {
		for (const id of keys) {
			if (this.held.get(id)?.kind === "done") continue;
			this.held.delete(id);
			this.failures.delete(id);
			const w = this.known.get(id);
			if (w) this.notified.set(id, wakeKey(w));
		}
	}

	/**
	 * A queued worker that can't start until someone acts: an upstream
	 * (transitively) failed, was cancelled, stopped without `routr done`,
	 * waits on a person, or is a run or judge step that failed (it halts
	 * what comes after it). An upstream never seen is assumed to be fine.
	 */
	private stuck(w: Worker, visiting = new Set<string>()): boolean {
		if (!QUEUED.has(w.state) || visiting.has(workerKey(w))) return false;
		visiting.add(workerKey(w));
		for (const id of w.after ?? []) {
			const up = this.known.get(refKey(w.registry, id));
			if (!up) continue;
			if ((STOPPED.has(up.state) && !jevDecides(up)) || up.halts) return true;
			// closed with no finish recorded: its tab went away before it was done
			if (up.state === "closed" && up.finished === undefined) return true;
			if (this.stuck(up, visiting)) return true;
		}
		return false;
	}

	private release(workers: readonly Worker[], ctx: WakeContext): WakeItem[] {
		const covered = (w: Worker): boolean => ctx.covered?.(w) ?? false;
		const now = this.now();
		const groups = new Map<string, Held[]>();
		for (const item of this.held.values()) {
			const g = ctx.group(item.worker) ?? `alone:${workerKey(item.worker)}`;
			groups.set(g, [...(groups.get(g) ?? []), item]);
		}
		const out: WakeItem[] = [];
		for (const [g, items] of groups) {
			// A running wait that will return some of this news holds the group,
			// unless a person is needed elsewhere in it.
			const urgent = items.some((i) => URGENT.has(i.kind) && !covered(i.worker));
			// held long enough: a chain that runs on for an hour still reports its plan
			const overdue = items.some((i) => !covered(i.worker) && now - i.heldAt >= this.maxHoldMs);
			const busy =
				items.some((i) => covered(i.worker)) ||
				workers.some((w) => (ACTIVE.has(w.state) || jevDecides(w)) && !this.held.has(workerKey(w)) && ctx.isMine(w) && ctx.group(w) === g && !this.stuck(w));
			if (!urgent && !overdue && busy) continue;
			for (const i of items) {
				if (covered(i.worker)) continue;
				this.held.delete(workerKey(i.worker));
				out.push(unheld(i));
			}
		}
		return out.sort((a, b) => a.worker.id - b.worker.id);
	}
}

function unheld(h: Held): WakeItem {
	return { worker: h.worker, kind: h.kind };
}

export interface WakeEntry {
	id: number;
	kind: WakeKind;
	title: string;
	engine: string;
	verdict?: string;
	output?: string;
	/** first line of the result, for the collapsed view */
	preview: string;
}

export interface WakeMessage {
	content: string;
	details: { entries: WakeEntry[] };
}

function describe(w: Worker): string {
	const chain = w.chain ? ` (chain ${[w.chain, w.step].filter(Boolean).join("/")})` : "";
	const title = w.title ? ` "${w.title}"` : "";
	return `worker ${w.id}${title} · ${w.role} · ${engine(w)}${chain}${repoTag(w)}`;
}

/** A routr command about w, run where its registry is: `routr result 7`, or `cd /other && routr result 7` for another repo's worker. */
function cmd(w: Worker, args: string): string {
	return `${routrIn(w)}routr ${args}`;
}

/**
 * Who answers a blocked worker's question, and how: routr's lines (`answer`
 * in `routr show ID --context --json`, the lines `routr wait` prints), then
 * the front end's own way in when there is an option to pick. Without them
 * (the context could not be fetched, or routr is older than the field),
 * `routr wait ID` says it: it returns at once for a blocked worker.
 */
function whoAnswers(w: Worker, ctx?: QuestionContext): string[] {
	if (!ctx?.answer.length) {
		return [`Run \`${cmd(w, `wait ${w.id}`)}\` before you do anything about it: it returns at once and says who answers this question, and how. Until then, choose no option.`];
	}
	// /routr answers in the session's own registry only
	return ctx.fingerprint && !w.registry ? [...ctx.answer, `The user can also type \`/routr answer ${w.id} N\` here.`] : ctx.answer;
}

/**
 * Does a maybe_done worker's `routr result` hold a reply? Not when, past
 * the "maybe done: …" line routr heads it with, there is nothing but its
 * output, verdict and worktree lines (its final message was empty).
 */
function replied(result: string): boolean {
	const body = result.startsWith("maybe done:") ? result.slice(result.indexOf("\n") + 1) : result;
	return body.split("\n").some((l) => l.trim() !== "" && !/^(output|verdict|worktree): /.test(l));
}

function clipBlock(text: string, max: number, w: Worker): string {
	const t = text.trim();
	if (t.length <= max) return t;
	return `${t.slice(0, max).trimEnd()}\n… (clipped; full text: ${cmd(w, `result ${w.id}`)})`;
}

/**
 * The message that wakes the model. `results` holds `routr result ID`
 * stdout for the workers that have one, and `contexts` what a question the
 * model answers is decided with (`routr show ID --context --json`), both by
 * workerKey. Every result is clipped so the total stays near `budget`
 * characters. Another repo's worker gets its commands with the `cd` to it.
 */
export function buildWakeMessage(
	items: readonly WakeItem[],
	results: ReadonlyMap<string, string>,
	budget = 8000,
	contexts: ReadonlyMap<string, QuestionContext> = new Map(),
): WakeMessage {
	const per = Math.max(1200, Math.floor(budget / Math.max(1, items.length)));
	const blocks: string[] = [];
	const entries: WakeEntry[] = [];
	for (const { worker: w, kind } of items) {
		const result = results.get(workerKey(w))?.trim() ?? "";
		const lines: string[] = [];
		switch (kind) {
			case "done":
				lines.push(`[routr] ${describe(w)} is done${w.verdict ? `, verdict ${w.verdict}` : ""}.`);
				// a worker started on its own has no chain (--after puts both in one): nothing waits on its fail
				if (w.halts && !w.chain) lines.push("No step waits on it: it was started on its own. Tell the user why it failed (below); there is nothing to approve.");
				else if (w.halts) {
					lines.push(
						`The steps after it wait. Tell the user why it failed (below); run \`${cmd(w, `approve ${w.id}`)}\` to go on past it only if the user says so.`,
					);
				}
				break;
			case "maybe_done": {
				const asked = w.unanswered ? `, after a question it asked went unanswered (dismissed or refused in its tab: ${w.unanswered})` : "";
				lines.push(
					`[routr] ${describe(w)} went idle without running routr done${asked}.` +
						` Its last reply ${replied(result) ? "is below" : "was empty"}${w.output ? `; check that ${w.output} exists before relying on it` : ""}.`,
				);
				break;
			}
			case "failed":
				lines.push(`[routr] ${describe(w)} failed${w.error ? `: ${w.error}` : ""}.`);
				break;
			case "closed":
				lines.push(
					`[routr] ${describe(w)} was closed before it ran routr done. ` +
						`\`${cmd(w, `send ${w.id} "…"`)}\` resumes its session.`,
				);
				break;
			case "reply":
				lines.push(`[routr] ${describe(w)} replied to follow-up ${w.turn}. It is still open: \`${cmd(w, `send ${w.id} "…"`)}\` goes on.`);
				break;
			case "blocked":
				lines.push(`[routr] ${describe(w)} is blocked: ${w.question ?? "(see its tab)"}`);
				if (w.asksAbout) lines.push(`It asks about ${w.asksAbout}`);
				lines.push(...whoAnswers(w, contexts.get(workerKey(w))));
				break;
			case "gated":
				lines.push(
					`[routr] ${describe(w)} waits at its gate.`,
					`Summarize what came before for the user and ask whether to start it. Run \`${cmd(w, `approve ${w.id}`)}\` only after the user says yes; never approve on your own.`,
				);
				break;
			case "needs_you":
				lines.push(`[routr] ${describe(w)} needs the user: ${w.question ?? w.error ?? "its chain cannot go on alone"}.`);
				break;
		}
		// another repo's: its routr commands run there (routr's own lines, `answer` above, name none)
		if (w.registry) lines.push(`It is in another repo's registry (${w.registry}): run routr commands about it as \`${cmd(w, "…")}\`.`);
		if (result && WANTS_RESULT.has(kind)) lines.push("", clipBlock(result, per, w));
		else if (w.output && (kind === "done" || kind === "maybe_done")) lines.push(`output: ${w.output}`);
		blocks.push(lines.join("\n"));
		const firstResultLine = result.split("\n").find((l) => l.trim() && !/^(output|verdict):/.test(l));
		entries.push({
			id: w.id,
			kind,
			title: `${w.id} ${w.role}${repoTag(w)}`,
			engine: engine(w),
			verdict: w.verdict,
			output: w.output,
			preview: clip(firstResultLine ?? w.question ?? w.error ?? "", 160),
		});
	}
	return { content: blocks.join("\n\n"), details: { entries } };
}
