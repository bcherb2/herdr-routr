// What one front-end session decides about waking its model, so that every
// front end decides it the same way. It owns:
//   - which workers are this session's: routr's `caller.session`, else the
//     claims read from the model's own shell calls (claims.ts);
//   - which registries to list: the session's own, and another repo's while
//     it holds this session's workers at work (the model queued them there:
//     `cd ../other && routr start …`). Worker ids are per registry, so a
//     worker is known by its key (workerKey), never by its id alone;
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

import { parseClaims, places, waitCover, type Place } from "./claims.ts";
import { ACTIVE, FINISHED, NEEDS_PERSON, refKey, workerKey, type QuestionContext, type Worker } from "./model.ts";
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
	/** the registry they are in, when it is another repo's; absent for the session's own */
	registry?: string;
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
	/** the session's own registry, its .routr directory as `locate` gives one; the front end lists it as the registry `snapshot` takes by default */
	home?: string;
	/**
	 * The registry (.routr directory) a routr call at this place keeps its
	 * workers in, as the front end resolves it the CLI's way, one path for
	 * one directory however it was written. undefined when it can't tell:
	 * claims made there are dropped rather than matched in the wrong registry.
	 * By default, a registry the CLI named, or an absolute --root's.
	 */
	locate?: (at: Place) => string | undefined;
}

/** What a running shell call covers: worker and chain keys (refKey), and whether it waits on what it starts. */
interface Cover {
	ids: string[];
	chains: string[];
	fresh: boolean;
	since: number;
}

function defaultLocate(at: Place): string | undefined {
	if ("registry" in at) return at.registry.startsWith("/") ? at.registry : undefined;
	if ("root" in at) return at.root.startsWith("/") ? `${at.root.replace(/\/+$/, "")}/.routr` : undefined;
	return undefined;
}

/** Will the watcher or a follow-up still change it: at work, waiting on someone, or a follow-up not yet answered. */
function unsettled(w: Worker): boolean {
	return !FINISHED.has(w.state) || (w.queued ?? 0) > 0 || (w.turn ?? 0) > (w.replies ?? 0);
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

// Workers, claims, covers and news are keyed by workerKey (refKey for a chain).
export class Coordinator {
	wakeMode: WakeMode;
	private readonly sessionId: string | undefined;
	private readonly now: () => number;
	private readonly tracker: WakeTracker;
	private readonly home: string | undefined;
	private readonly locate: (at: Place) => string | undefined;
	/** the latest list of each registry; another repo's workers carry `registry` */
	private snapshotWorkers: Worker[] = [];
	private run = 0;
	/** the model's turns in this session, oldest first: workers started in one are reported together */
	private readonly runStarts: { group: string; at: number }[] = [];
	private readonly claimedIds = new Map<string, string>(); // key → group
	private readonly claimedChains = new Set<string>();
	/** keys claimed since the session began (not restored): news in their registry's first list too */
	private readonly claimedNow = new Set<string>();
	/** other repos' registries the session holds claims in → whether one of their lists came since the latest claim */
	private readonly others = new Map<string, boolean>();
	/**
	 * workers cancelled from this session (the model's `routr cancel`, the user's `/routr cancel`):
	 * key → the follow-up turn it was at. A finished worker survives a cancel and `routr send`
	 * resumes it; news from a later turn is new.
	 */
	private readonly cancelledHere = new Map<string, number>();
	/** running shell calls that will hand the model workers' results: call id → what they cover */
	private readonly inflight = new Map<string, Cover>();
	private turnRunning = false;
	/** the user interrupted a turn, and has not finished one since */
	private interrupted = false;
	/** the front end is compacting the model's context: a wake turn now would race it */
	private paused = false;
	/** news held until the session is idle: worker key → its news */
	private readonly deferred = new Map<string, WakeItem>();
	/** news put into the running turn, and whether a model request has started since (the model read it) */
	private inTurn: { items: WakeItem[]; read: boolean }[] = [];
	private ended = false;

	constructor(opts: CoordinatorOptions = {}) {
		this.sessionId = opts.sessionId;
		this.wakeMode = opts.wakeMode ?? "mine";
		this.now = opts.now ?? Date.now;
		this.tracker = new WakeTracker({ now: this.now, maxHoldMs: opts.maxHoldMs });
		this.home = opts.home;
		this.locate = opts.locate ?? defaultLocate;
		for (const d of opts.restored ?? []) {
			const r = this.other(d.registry);
			for (const id of d.ids ?? []) this.claimedIds.set(refKey(r, id), d.group ?? "restored");
			for (const ch of d.chains ?? []) this.claimedChains.add(refKey(r, ch));
			if (r) this.others.set(r, false);
		}
	}

	/** registry, unless it is the session's own (undefined) */
	private other(registry: string | undefined): string | undefined {
		return registry && registry !== this.home ? registry : undefined;
	}

	/** The registry claims at a place are in: undefined for the session's own, null when it can't be told. */
	private registryAt(at: Place | undefined): string | undefined | null {
		if (at === undefined) return undefined;
		const r = this.locate(at);
		return r === undefined ? null : this.other(r);
	}

	/** The workers of the latest snapshots: all of the session's own registry, and this session's of other repos'. */
	get workers(): readonly Worker[] {
		return this.snapshotWorkers.filter((w) => !w.registry || this.isMine(w));
	}

	/**
	 * Other repos' registries to list now, besides the session's own: each
	 * that holds this session's claims, until a list since the latest claim
	 * shows none of its workers there unsettled.
	 */
	elsewhere(): string[] {
		const live = new Set(this.snapshotWorkers.filter((w) => w.registry && this.isMine(w) && unsettled(w)).map((w) => w.registry!));
		return [...this.others].filter(([r, listed]) => !listed || live.has(r)).map(([r]) => r);
	}

	/** routr recorded this session as the worker's caller. */
	private startedHere(w: Worker): boolean {
		return this.sessionId !== undefined && w.callerSession === this.sessionId;
	}

	/** Is the worker's news this session's to hear? */
	isMine(w: Worker): boolean {
		if (this.wakeMode === "all") return true;
		if (this.wakeMode === "off") return false;
		return this.startedHere(w) || this.claimedIds.has(workerKey(w)) || (w.chain !== undefined && this.claimedChains.has(refKey(w.registry, w.chain)));
	}

	/** The wake a worker's news joins: its chain, else the turn it was started in; undefined = alone. */
	group(w: Worker): string | undefined {
		if (w.chain) return `chain:${refKey(w.registry, w.chain)}`;
		return this.claimedIds.get(workerKey(w)) ?? (this.startedHere(w) ? this.runOf(w) : undefined);
	}

	private runOf(w: Worker): string | undefined {
		if (w.created === undefined) return undefined;
		for (let i = this.runStarts.length - 1; i >= 0; i--) if (this.runStarts[i]!.at <= w.created) return this.runStarts[i]!.group;
		return undefined;
	}

	/** Will a running shell call hand the model this worker's result itself? */
	private covered(w: Worker): boolean {
		for (const c of this.inflight.values()) {
			if (c.ids.includes(workerKey(w)) || (w.chain !== undefined && c.chains.includes(refKey(w.registry, w.chain)))) return true;
			if (c.fresh && w.created !== undefined && w.created >= c.since - CLOCK_SLACK_MS) return true;
		}
		return false;
	}

	/** This session's workers created since `since`, by key. */
	private startedSince(since: number): string[] {
		return this.snapshotWorkers.filter((w) => this.isMine(w) && w.created !== undefined && w.created >= since - CLOCK_SLACK_MS).map(workerKey);
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
		for (const d of this.inTurn) if (!d.read) for (const i of d.items) this.deferred.set(workerKey(i.worker), i);
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
		if (!cover) return;
		const c: Cover = { ids: [], chains: [], fresh: cover.fresh, since: this.now() };
		for (const { at, ids, chains } of [{ at: undefined, ids: cover.ids, chains: cover.chains }, ...(cover.elsewhere ?? [])]) {
			const r = this.registryAt(at);
			if (r === null) continue;
			c.ids.push(...ids.map((id) => refKey(r, id)));
			c.chains.push(...chains.map((ch) => refKey(r, ch)));
		}
		this.inflight.set(callId, c);
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
		const keep: ClaimsEntry[] = [];
		// a wait on what the call started itself returned this session's workers created meanwhile
		const seen = cover?.fresh ? this.startedSince(cover.since) : [];
		const seenChains: string[] = [];
		const cancelled: string[] = [];
		const cancelledChains: string[] = [];
		const c = parseClaims(command, output);
		for (const p of [{ ...c, at: undefined }, ...(c.elsewhere ?? [])]) {
			const r = this.registryAt(p.at);
			if (r === null) continue; // a registry it can't tell: no claim is better than one on another repo's worker
			keep.push(...this.claim(r, p.ids, p.chains));
			seen.push(...p.seen.map((id) => refKey(r, id)));
			seenChains.push(...p.seenChains.map((ch) => refKey(r, ch)));
			cancelled.push(...p.cancelled.map((id) => refKey(r, id)));
			cancelledChains.push(...p.cancelledChains.map((ch) => refKey(r, ch)));
		}
		keep.push(...this.watch(places(command, output)));
		const chainSeen = this.snapshotWorkers.filter((w) => w.chain !== undefined && seenChains.includes(refKey(w.registry, w.chain))).map(workerKey);
		this.tracker.markSeen([...seen, ...chainSeen]);
		for (const k of [...seen, ...chainSeen]) this.deferred.delete(k);
		this.cancel(cancelled, cancelledChains);
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

	/** The user ran `routr cancel` from the front end (Pi's `/routr cancel`, in the session's own registry), which printed `output`. */
	userCancelled(command: string, output: string): void {
		const c = parseClaims(command, output);
		this.cancel(c.cancelled.map(String), c.cancelledChains);
	}

	/** Workers cancelled from this session (keys): their held news, and any still to come from this turn, is moot. */
	private cancel(keys: readonly string[], chains: readonly string[]): void {
		const ws = this.snapshotWorkers;
		const all = [...keys, ...ws.filter((w) => w.chain !== undefined && chains.includes(refKey(w.registry, w.chain))).map(workerKey)];
		for (const k of all) this.cancelledHere.set(k, ws.find((w) => workerKey(w) === k)?.turn ?? 0);
		this.tracker.drop(all);
		for (const k of all) if (this.deferred.get(k)?.kind !== "done") this.deferred.delete(k);
	}

	/**
	 * A fresh `routr list` of one registry: the session's own, or another
	 * repo's that `elsewhere` named. Returns the news to deliver now (wants
	 * says what to fetch for it), and claims to keep.
	 */
	snapshot(list: readonly Worker[], registry?: string): SnapshotOutcome {
		if (this.ended) return { wake: [], keep: [] };
		const r = this.other(registry);
		const workers = r ? list.map((w) => (w.registry === r ? w : { ...w, registry: r })) : list;
		this.snapshotWorkers = [...this.snapshotWorkers.filter((w) => w.registry !== r), ...workers];
		if (r) this.others.set(r, true);
		for (const w of workers) {
			const turn = this.cancelledHere.get(workerKey(w));
			if (turn !== undefined && (w.turn ?? 0) > turn) this.cancelledHere.delete(workerKey(w)); // resumed by a follow-up: its news counts again
		}
		const keep: ClaimsEntry[] = [];
		for (const w of workers) {
			const k = workerKey(w);
			if (w.chain && this.claimedIds.has(k) && !this.claimedChains.has(refKey(r, w.chain))) {
				this.claimedChains.add(refKey(r, w.chain));
				if (this.claimedNow.has(k)) this.claimedNow.add(refKey(r, w.chain));
				keep.push({ ids: [], chains: [w.chain], group: this.claimedIds.get(k)!, ...(r ? { registry: r } : {}) });
			}
		}
		const released = this.tracker.update(
			workers,
			{
				isMine: (w) => this.isMine(w),
				group: (w) => this.group(w),
				covered: (w) => this.covered(w),
				fresh: (w) =>
					this.claimedNow.has(workerKey(w)) ||
					(w.chain !== undefined && this.claimedNow.has(refKey(w.registry, w.chain))) ||
					(this.startedHere(w) && this.runOf(w) !== undefined),
			},
			{ registry: r, all: this.snapshotWorkers },
		);
		if (this.wakeMode === "off") {
			this.deferred.clear();
			return { wake: [], keep };
		}
		for (const i of released) this.deferred.set(workerKey(i.worker), i);
		// a person is needed: at once, into a running turn too; the rest once the session is idle; nothing right after an interrupt or during a compaction
		const due = this.paused ? [] : [...this.deferred.values()].filter((i) => !this.interrupted && (!this.turnRunning || NEEDS_PERSON.has(i.worker.state)));
		for (const i of due) this.deferred.delete(workerKey(i.worker));
		return { wake: due.sort((a, b) => a.worker.id - b.worker.id), keep };
	}

	/** News held until the session is idle, or until the user's next turn ends after an interrupt. */
	held(): WakeItem[] {
		return [...this.deferred.values()];
	}

	/** What to fetch before composing a wake: `routr result` for these, `routr show --context --json` (who answers) for those; each in its registry (`registry`). */
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
	compose(items: readonly WakeItem[], results: ReadonlyMap<string, string>, contexts: ReadonlyMap<string, QuestionContext>): Wake | undefined {
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
		const now = this.snapshotWorkers.find((w) => workerKey(w) === workerKey(i.worker));
		const turn = this.cancelledHere.get(workerKey(i.worker));
		if ((turn !== undefined && (i.worker.turn ?? 0) <= turn) || now?.state === "cancelled") return true;
		return now !== undefined && ACTIVE.has(now.state) && i.kind !== "reply";
	}

	/**
	 * List each other repo's registry a routr call ran in, claims or not: the
	 * output may not name what it queued there (captured, redirected,
	 * prefixed), and routr's caller.session says which workers are this
	 * session's. Returns the registries newly listed, to keep.
	 */
	private watch(at: readonly Place[]): ClaimsEntry[] {
		const keep: ClaimsEntry[] = [];
		for (const p of at) {
			const r = this.registryAt(p);
			if (!r) continue;
			if (!this.others.has(r)) keep.push({ ids: [], chains: [], group: `run:${this.run}`, registry: r });
			this.others.set(r, false);
		}
		return keep;
	}

	/** Claims these workers of registry r (undefined: the session's own) for the current turn's group; returns what is new, to keep. */
	private claim(r: string | undefined, ids: readonly number[], chains: readonly string[]): ClaimsEntry[] {
		if (r && (ids.length || chains.length)) this.others.set(r, false); // listed again: a follow-up wakes a finished worker
		const g = `run:${this.run}`;
		const newIds = ids.filter((id) => !this.claimedIds.has(refKey(r, id)));
		const newChains = chains.filter((c) => !this.claimedChains.has(refKey(r, c)));
		for (const id of newIds) {
			this.claimedIds.set(refKey(r, id), g);
			this.claimedNow.add(refKey(r, id));
		}
		for (const c of newChains) {
			this.claimedChains.add(refKey(r, c));
			this.claimedNow.add(refKey(r, c));
		}
		return newIds.length || newChains.length ? [{ ids: newIds, chains: newChains, group: g, ...(r ? { registry: r } : {}) }] : [];
	}
}
