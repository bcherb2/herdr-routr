// What one front-end session decides about waking its model, so that every
// front end decides it the same way. It owns:
//   - which workers are this session's: routr's `caller.session`, else the
//     claims read from the model's own shell calls (claims.ts);
//   - which wake a worker joins: its chain, else the model's turn it was
//     started in;
//   - what a running `routr wait` (or `--wait`) will hand the model itself;
//   - which news a cancel, or a snapshot newer than the results fetched for
//     it, made moot;
//   - when news goes out: a worker that needs a person at once, into a
//     running turn too; the rest once the model's turn is over, so news the
//     model reads itself meanwhile is dropped, not repeated. After the user
//     interrupted a turn, all of it waits until their next turn ends: Esc
//     means they want to type, and a wake turn would take that from them.
//     While the front end compacts the model's context, all of it waits.
// A front end turns its harness's events into these calls, fetches the
// results it is asked for, and delivers the message. The coordinator runs
// no process and reads no file; one lives for one session.

import { parseClaims, waitCover, type WaitCover } from "./claims.ts";
import { ACTIVE, NEEDS_PERSON, type QuestionContext, type Worker } from "./model.ts";
import { buildWakeMessage, WakeTracker, WANTS_RESULT, type WakeItem, type WakeMessage } from "./wake.ts";

/** Whose news wakes the model: this session's workers, every worker, or none. */
export type WakeMode = "mine" | "all" | "off";

/** How the model's turn ended: it answered, the user interrupted it, or an error ended it. */
export type TurnEnd = "answer" | "aborted" | "error";

/** Claims a front end keeps with its session, so they outlive a restart; handed back as `restored`. */
export interface ClaimsEntry {
	ids: number[];
	chains: string[];
	group: string;
}

export interface CoordinatorOptions {
	/** the session id routr records as `caller.session` */
	sessionId?: string;
	wakeMode?: WakeMode;
	/** news waits on its group at most this long (holdFromEnv) */
	maxHoldMs?: number;
	/** the clock, epoch ms */
	now?: () => number;
	/** the ClaimsEntry records kept with this session, oldest first */
	restored?: readonly Partial<ClaimsEntry>[];
}

/** What a snapshot returns: news to deliver now, and claims to keep with the session. */
export interface SnapshotOutcome {
	wake: WakeItem[];
	keep: ClaimsEntry[];
}

/**
 * A wake ready to deliver. `intoTurn`: the model's turn is running and a
 * worker waits on a person, so it goes into that turn (Pi: steer; Claude
 * Code: $.session.append); otherwise it starts a turn of its own.
 */
export interface Wake {
	message: WakeMessage;
	intoTurn: boolean;
}

/** the CLI stamps `created`; allow for its clock and rounding against ours */
const CLOCK_SLACK_MS = 2000;

/** a shell command that may be a routr call; the rest are ignored */
const ROUTR_CALL = /routr|ROUTR_BIN/;

export class Coordinator {
	wakeMode: WakeMode;
	private readonly sessionId: string | undefined;
	private readonly now: () => number;
	private readonly tracker: WakeTracker;
	private snapshotWorkers: Worker[] = [];
	private run = 0;
	/** the model's turns in this session, oldest first: workers started in one are reported together */
	private readonly runStarts: { group: string; at: number }[] = [];
	private readonly claimedIds = new Map<number, string>(); // id → group
	private readonly claimedChains = new Set<string>();
	/**
	 * workers cancelled from this session (the model's `routr cancel`, the user's `/routr cancel`):
	 * id → the follow-up turn it was at. A finished worker survives a cancel and `routr send`
	 * resumes it; news from a later turn is new.
	 */
	private readonly cancelledHere = new Map<number, number>();
	/** running shell calls that will hand the model workers' results: call id → what they cover */
	private readonly inflight = new Map<string, WaitCover & { since: number }>();
	private turnRunning = false;
	/** the user interrupted a turn, and has not finished one since */
	private interrupted = false;
	/** the front end is compacting the model's context: a wake turn now would race it */
	private paused = false;
	/** news held until the session is idle: worker id → its news */
	private readonly deferred = new Map<number, WakeItem>();
	/** news put into the running turn, and whether a model request has started since (the model read it) */
	private inTurn: { items: WakeItem[]; read: boolean }[] = [];
	private ended = false;

	constructor(opts: CoordinatorOptions = {}) {
		this.sessionId = opts.sessionId;
		this.wakeMode = opts.wakeMode ?? "mine";
		this.now = opts.now ?? Date.now;
		this.tracker = new WakeTracker({ now: this.now, maxHoldMs: opts.maxHoldMs });
		for (const d of opts.restored ?? []) {
			for (const id of d.ids ?? []) this.claimedIds.set(id, d.group ?? "restored");
			for (const ch of d.chains ?? []) this.claimedChains.add(ch);
		}
	}

	/** The workers of the latest snapshot. */
	get workers(): readonly Worker[] {
		return this.snapshotWorkers;
	}

	/** routr recorded this session as the worker's caller. */
	private startedHere(w: Worker): boolean {
		return this.sessionId !== undefined && w.callerSession === this.sessionId;
	}

	/** Is the worker's news this session's to hear? */
	isMine(w: Worker): boolean {
		if (this.wakeMode === "all") return true;
		if (this.wakeMode === "off") return false;
		return this.startedHere(w) || this.claimedIds.has(w.id) || (w.chain !== undefined && this.claimedChains.has(w.chain));
	}

	/** The wake a worker's news joins: its chain, else the turn it was started in; undefined = alone. */
	group(w: Worker): string | undefined {
		if (w.chain) return `chain:${w.chain}`;
		return this.claimedIds.get(w.id) ?? (this.startedHere(w) ? this.runOf(w) : undefined);
	}

	private runOf(w: Worker): string | undefined {
		if (w.created === undefined) return undefined;
		for (let i = this.runStarts.length - 1; i >= 0; i--) if (this.runStarts[i]!.at <= w.created) return this.runStarts[i]!.group;
		return undefined;
	}

	/** Will a running shell call hand the model this worker's result itself? */
	private covered(w: Worker): boolean {
		for (const c of this.inflight.values()) {
			if (c.ids.includes(w.id) || (w.chain !== undefined && c.chains.includes(w.chain))) return true;
			if (c.fresh && w.created !== undefined && w.created >= c.since - CLOCK_SLACK_MS) return true;
		}
		return false;
	}

	/** This session's workers created since `since`. */
	private startedSince(since: number): number[] {
		return this.snapshotWorkers.filter((w) => this.isMine(w) && w.created !== undefined && w.created >= since - CLOCK_SLACK_MS).map((w) => w.id);
	}

	/** News still held back, waiting on the rest of its group or a running wait. */
	pending(): WakeItem[] {
		return this.tracker.pending();
	}

	/** The session ended: news fetched for it is no longer delivered. */
	end(): void {
		this.ended = true;
	}

	// ── events ──────────────────────────────────────────────────────────

	/** The model's turn started. */
	turnStarted(): void {
		this.run++;
		this.runStarts.push({ group: `run:${this.run}`, at: this.now() });
		this.turnRunning = true;
	}

	/** A model request of the running turn started: news put into the turn before it has been read. */
	stepStarted(): void {
		for (const d of this.inTurn) d.read = true;
	}

	/**
	 * The model's turn ended. No call covers anything any more, and news put
	 * into the turn after its last model request is delivered again, as a
	 * turn of its own. True when the next snapshot has something to do
	 * (take it soon).
	 */
	turnEnded(how: TurnEnd = "answer"): boolean {
		this.turnRunning = false;
		this.interrupted = how === "aborted";
		for (const d of this.inTurn) if (!d.read) for (const i of d.items) this.deferred.set(i.worker.id, i);
		this.inTurn = [];
		const covered = this.inflight.size > 0;
		this.inflight.clear();
		return covered || (this.deferred.size > 0 && !this.interrupted);
	}

	/** The front end started compacting the model's context: all news waits, a person's question too. */
	pause(): void {
		this.paused = true;
	}

	/** The compaction is over. True when news waits for the next snapshot (take it soon). */
	resume(): boolean {
		this.paused = false;
		return this.deferred.size > 0 && !this.interrupted;
	}

	/** Is the model's turn running? */
	get busy(): boolean {
		return this.turnRunning;
	}

	/** The model started a shell call: a `routr wait` or `--wait` holds the news it will return until it ends. */
	shellStarted(callId: string, command: string): void {
		if (!ROUTR_CALL.test(command)) return;
		const cover = waitCover(command);
		if (cover) this.inflight.set(callId, { ...cover, since: this.now() });
	}

	/**
	 * The model's shell call returned `output`. Claims the workers it started,
	 * marks what it read as seen, and drops news of what it cancelled. Returns
	 * the claims to keep with the session, or undefined when the call was no
	 * routr call.
	 */
	shellEnded(callId: string, command: string, output: string): ClaimsEntry[] | undefined {
		if (!ROUTR_CALL.test(command)) return undefined;
		const cover = this.inflight.get(callId);
		const c = parseClaims(command, output, cover?.fresh ? this.startedSince(cover.since) : []);
		const keep = this.claim(c.ids, c.chains);
		const chainSeen = this.snapshotWorkers.filter((w) => w.chain !== undefined && c.seenChains.includes(w.chain)).map((w) => w.id);
		this.tracker.markSeen([...c.seen, ...chainSeen]);
		for (const id of [...c.seen, ...chainSeen]) this.deferred.delete(id);
		this.cancel(c.cancelled, c.cancelledChains);
		this.inflight.delete(callId); // what it didn't return wakes the model on the next snapshot
		return keep;
	}

	/** A shell call ended without a result (blocked, aborted): it covers nothing any more. True if it did. */
	shellAborted(callId: string): boolean {
		return this.inflight.delete(callId);
	}

	/**
	 * The model started `command` as a background shell, `taskId`: its tool
	 * call returned at once, and the shell runs on. A `routr wait` (or
	 * `--wait`) in it covers what it will return until it ends.
	 */
	backgroundStarted(taskId: string, command: string): void {
		this.shellStarted(taskId, command);
	}

	/**
	 * Background shell `taskId` ended. `completed`: it exited 0, so its
	 * `routr wait` returned the results it covered, and the harness's own
	 * notice hands them to the model. Otherwise (`routr wait`'s exit 3, a
	 * kill) it returned none, and their news wakes the model.
	 */
	backgroundEnded(taskId: string, command: string, completed: boolean): void {
		if (completed) this.shellEnded(taskId, command, "");
		else this.shellAborted(taskId);
	}

	/** The user ran `routr cancel` from the front end (Pi's `/routr cancel`), which printed `output`. */
	userCancelled(command: string, output: string): void {
		const c = parseClaims(command, output);
		this.cancel(c.cancelled, c.cancelledChains);
	}

	/** Workers cancelled from this session: their held news, and any still to come from this turn, is moot. */
	private cancel(ids: readonly number[], chains: readonly string[]): void {
		const ws = this.snapshotWorkers;
		const all = [...ids, ...ws.filter((w) => w.chain !== undefined && chains.includes(w.chain)).map((w) => w.id)];
		for (const id of all) this.cancelledHere.set(id, ws.find((w) => w.id === id)?.turn ?? 0);
		this.tracker.drop(all);
		for (const id of all) if (this.deferred.get(id)?.kind !== "done") this.deferred.delete(id);
	}

	/** A fresh `routr list`: the news to deliver now (wants says what to fetch for it), and claims to keep. */
	snapshot(workers: readonly Worker[]): SnapshotOutcome {
		if (this.ended) return { wake: [], keep: [] };
		this.snapshotWorkers = [...workers];
		for (const w of workers) {
			const turn = this.cancelledHere.get(w.id);
			if (turn !== undefined && (w.turn ?? 0) > turn) this.cancelledHere.delete(w.id); // resumed by a follow-up: its news counts again
		}
		const keep: ClaimsEntry[] = [];
		for (const w of workers) {
			if (w.chain && this.claimedIds.has(w.id) && !this.claimedChains.has(w.chain)) {
				this.claimedChains.add(w.chain);
				keep.push({ ids: [], chains: [w.chain], group: this.claimedIds.get(w.id)! });
			}
		}
		const released = this.tracker.update(workers, {
			isMine: (w) => this.isMine(w),
			group: (w) => this.group(w),
			covered: (w) => this.covered(w),
		});
		if (this.wakeMode === "off") {
			this.deferred.clear();
			return { wake: [], keep };
		}
		for (const i of released) this.deferred.set(i.worker.id, i);
		// a person is needed: at once, into a running turn too; the rest once the session is idle; nothing right after an interrupt or during a compaction
		const due = this.paused ? [] : [...this.deferred.values()].filter((i) => !this.interrupted && (!this.turnRunning || NEEDS_PERSON.has(i.worker.state)));
		for (const i of due) this.deferred.delete(i.worker.id);
		return { wake: due.sort((a, b) => a.worker.id - b.worker.id), keep };
	}

	/** News held until the session is idle, or until the user's next turn ends after an interrupt. */
	held(): WakeItem[] {
		return [...this.deferred.values()];
	}

	/** What to fetch before composing a wake: `routr result` for these, `routr show --context --json` (who answers) for those. */
	wants(items: readonly WakeItem[]): { results: Worker[]; contexts: Worker[] } {
		return {
			results: items.filter((i) => WANTS_RESULT.has(i.kind)).map((i) => i.worker),
			contexts: items.filter((i) => i.kind === "blocked").map((i) => i.worker),
		};
	}

	/**
	 * The wake for `items`, with the results and contexts fetched for them.
	 * Fetching takes a while: news a later snapshot or a cancel made moot
	 * meanwhile is dropped, and nothing is left after the session ended.
	 * Once delivered, nothing recalls it.
	 */
	compose(items: readonly WakeItem[], results: ReadonlyMap<number, string>, contexts: ReadonlyMap<number, QuestionContext>): Wake | undefined {
		if (this.ended) return undefined;
		const fresh = items.filter((i) => !this.moot(i));
		if (fresh.length === 0) return undefined;
		return {
			message: buildWakeMessage(fresh, results, undefined, contexts),
			intoTurn: this.turnRunning && fresh.some((i) => NEEDS_PERSON.has(i.worker.state)),
		};
	}

	/**
	 * The wake for `items` was delivered. `intoTurn`: into the running turn,
	 * where the model reads it at its next request (stepStarted); one that
	 * ended before that delivers it again (turnEnded).
	 */
	delivered(items: readonly WakeItem[], intoTurn: boolean): void {
		if (intoTurn) this.inTurn.push({ items: [...items], read: false });
	}

	/**
	 * Delivering these items failed: they are held for the next snapshot,
	 * unless their news went stale meanwhile, and dropped after a few tries.
	 */
	deliveryFailed(items: readonly WakeItem[]): { held: WakeItem[]; dropped: WakeItem[] } {
		return this.tracker.requeue(items);
	}

	/** News the latest snapshot overtook: its worker was cancelled, or is back at work. A finished result stays news. */
	private moot(i: WakeItem): boolean {
		if (i.kind === "done") return false;
		const now = this.snapshotWorkers.find((w) => w.id === i.worker.id);
		const turn = this.cancelledHere.get(i.worker.id);
		if ((turn !== undefined && (i.worker.turn ?? 0) <= turn) || now?.state === "cancelled") return true;
		return now !== undefined && ACTIVE.has(now.state) && i.kind !== "reply";
	}

	/** Claims these workers for the current turn's group; returns what is new, to keep. */
	private claim(ids: readonly number[], chains: readonly string[]): ClaimsEntry[] {
		const g = `run:${this.run}`;
		const newIds = ids.filter((id) => !this.claimedIds.has(id));
		const newChains = chains.filter((c) => !this.claimedChains.has(c));
		for (const id of newIds) this.claimedIds.set(id, g);
		for (const c of newChains) this.claimedChains.add(c);
		return newIds.length || newChains.length ? [{ ids: newIds, chains: newChains, group: g }] : [];
	}
}
