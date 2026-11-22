// The coordinator, driven by front-end events alone: turns, the model's shell
// calls, snapshots of `routr list`, and deliveries. Every front end gets the
// same answers from it (pi/src/index.ts, and the Claude Code mod).
import { test } from "node:test";
import assert from "node:assert/strict";
import { Coordinator, type CoordinatorOptions } from "../coordinator.ts";
import type { Place } from "../claims.ts";
import type { State, Worker } from "../model.ts";
import type { WakeItem } from "../wake.ts";

const T0 = Date.parse("2026-10-13T10:00:00Z");
const S = "S1";

/** A coordinator for session S1 on a clock the test moves. */
function setup(opts: CoordinatorOptions = {}) {
	let now = T0;
	const c = new Coordinator({ sessionId: S, now: () => now, ...opts });
	return { c, tick: (ms: number) => void (now += ms), at: () => now };
}

const worker = (id: number, state: State, extra: Partial<Worker> = {}): Worker => ({
	id,
	role: "task",
	harness: "claude",
	state,
	callerSession: S,
	created: T0,
	...extra,
});

const ids = (items: readonly WakeItem[]): string[] => items.map((i) => `${i.worker.id}:${i.kind}`);
const none = new Map();

test("the first snapshot is the baseline; news after it wakes for this session's workers only", () => {
	const { c } = setup();
	assert.deepEqual(c.snapshot([worker(1, "done"), worker(2, "running"), worker(3, "running", { callerSession: "other" })]).wake, []);
	const out = c.snapshot([worker(1, "done"), worker(2, "done"), worker(3, "done", { callerSession: "other" })]);
	assert.deepEqual(ids(out.wake), ["2:done"], "1 was done before; 3 is another session's");
	assert.deepEqual(ids(c.snapshot([worker(2, "done")]).wake), [], "once");
});

test("workers started in one turn are reported together; a chain is one group", () => {
	const { c, tick } = setup();
	c.snapshot([]);
	c.turnStarted();
	tick(1000);
	const a = worker(1, "running", { created: T0 + 1000 });
	const b = worker(2, "running", { created: T0 + 1000 });
	c.snapshot([a, b]);
	c.turnEnded();
	assert.equal(c.group(a), "run:1");
	assert.deepEqual(ids(c.snapshot([{ ...a, state: "done" }, b]).wake), [], "held while its sibling runs");
	assert.equal(c.pending().length, 1);
	assert.deepEqual(ids(c.snapshot([{ ...a, state: "done" }, { ...b, state: "done" }]).wake), ["1:done", "2:done"]);
	assert.equal(c.group(worker(3, "running", { chain: "plan-1" })), "chain:plan-1");
});

test("news held past its hold cap goes out alone", () => {
	const { c, tick } = setup({ maxHoldMs: 60_000 });
	c.snapshot([]);
	c.turnStarted();
	const a = worker(1, "running", { created: T0 });
	const b = worker(2, "running", { created: T0 });
	c.snapshot([a, b]);
	c.turnEnded();
	assert.deepEqual(ids(c.snapshot([{ ...a, state: "done" }, b]).wake), []);
	tick(60_000);
	assert.deepEqual(ids(c.snapshot([{ ...a, state: "done" }, b]).wake), ["1:done"]);
});

test("a result the model already read is not woken for", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running")]);
	c.shellStarted("t1", "routr wait 5");
	assert.deepEqual(ids(c.snapshot([worker(5, "done")]).wake), [], "held: the running wait returns it");
	assert.deepEqual(c.shellEnded("t1", "routr wait 5", "Done.\n"), []);
	assert.deepEqual(ids(c.snapshot([worker(5, "done")]).wake), [], "the wait returned it");
	// read before the snapshot saw it finish
	c.snapshot([worker(6, "running")]);
	c.shellEnded("t2", "routr result 6", "Done.\n");
	assert.deepEqual(ids(c.snapshot([worker(6, "done")]).wake), []);
});

test("a wait that ends without returning (aborted, or the turn ended) leaves the news to wake", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running"), worker(6, "running")]);
	c.shellStarted("t1", "routr wait 5");
	c.shellStarted("t2", "routr wait 6");
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "done")]).wake), []);
	assert.equal(c.shellAborted("t1"), true);
	assert.equal(c.shellAborted("t1"), false, "once");
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "done")]).wake), ["5:done"]);
	assert.equal(c.turnEnded(), true);
	assert.equal(c.turnEnded(), false);
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "done")]).wake), ["6:done"]);
});

test("a wait that timed out names its worker, and its news still wakes", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running")]);
	c.shellStarted("t1", "routr wait 5");
	c.shellEnded("t1", "routr wait 5", "still running: 5 task · claude (9m00s). Run: routr wait 5\n");
	assert.deepEqual(ids(c.snapshot([worker(5, "done")]).wake), ["5:done"]);
});

test("--wait on what the call starts covers the workers this session starts meanwhile", () => {
	const { c, tick } = setup();
	c.snapshot([]);
	tick(5000);
	c.shellStarted("t1", "routr start --wait 'fix a; then b'");
	const w = worker(7, "running", { created: T0 + 5000 });
	c.snapshot([w]);
	assert.deepEqual(ids(c.snapshot([{ ...w, state: "done" }]).wake), [], "covered: the call returns it");
	// stderr dropped: the output names no id, the coordinator knows what was started meanwhile
	c.shellEnded("t1", "routr start --wait 'fix a; then b' 2>/dev/null", "Fixed.\n");
	assert.deepEqual(ids(c.snapshot([{ ...w, state: "done" }]).wake), []);
	// a worker created before the call is not covered by it
	const old = worker(8, "running", { created: T0 });
	c.snapshot([old]);
	c.shellStarted("t2", "routr start --wait x");
	assert.deepEqual(ids(c.snapshot([{ ...old, state: "done" }]).wake), ["8:done"]);
});

test("claims from the model's shell calls: workers routr didn't record to this session, kept for a restart", () => {
	const { c } = setup();
	c.snapshot([]);
	c.turnStarted();
	const unrecorded = worker(7, "running", { callerSession: undefined });
	assert.equal(c.isMine(unrecorded), false);
	assert.equal(c.shellEnded("t1", "ls", "7\n"), undefined, "not a routr call");
	assert.deepEqual(c.shellEnded("t2", "routr start x", "7\n"), [{ ids: [7], chains: [], group: "run:1" }]);
	assert.deepEqual(c.shellEnded("t3", "routr start x", "7\n"), [], "already claimed");
	assert.equal(c.isMine(unrecorded), true);
	// its chain is claimed with it, once a snapshot shows it
	const out = c.snapshot([{ ...unrecorded, chain: "c7" }]);
	assert.deepEqual(out.keep, [{ ids: [], chains: ["c7"], group: "run:1" }]);
	assert.equal(c.isMine(worker(8, "running", { callerSession: undefined, chain: "c7" })), true);
	// handed back after a restart
	const again = new Coordinator({ sessionId: S, restored: [{ ids: [7], chains: [], group: "run:1" }, { chains: ["c7"] }] });
	assert.equal(again.isMine(unrecorded), true);
	assert.equal(again.isMine(worker(9, "running", { callerSession: undefined, chain: "c7" })), true);
});

test("news a cancel made moot while results were fetched is not composed; a finished result stays", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running"), worker(6, "running")]);
	const items = c.snapshot([worker(5, "blocked", { question: "Proceed?" }), worker(6, "done")]).wake;
	assert.deepEqual(ids(items), ["5:blocked", "6:done"]);
	assert.deepEqual(
		c.wants(items).results.map((w) => w.id),
		[6],
		"fetch the result of what finished",
	);
	c.shellEnded("t1", "routr cancel 5", "routr: cancelled 5 task · claude\n");
	const wake = c.compose(items, new Map([["6", "All done."]]), none)!;
	assert.match(wake.message.content, /worker 6/);
	assert.doesNotMatch(wake.message.content, /worker 5/);
	assert.equal(wake.intoTurn, false, "no turn runs: a turn of its own");
	assert.equal(c.compose(items.slice(0, 1), none, none), undefined);
});

test("the user's own /routr cancel makes news moot the same way", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running")]);
	const items = c.snapshot([worker(5, "blocked", { question: "Proceed?" })]).wake;
	c.userCancelled("routr cancel 5", "routr: cancelled 5 task · claude\n");
	assert.equal(c.compose(items, none, none), undefined);
});

test("news a newer snapshot overtook is not composed", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running")]);
	c.turnStarted();
	const items = c.snapshot([worker(5, "blocked", { question: "Proceed?" })]).wake;
	assert.equal(c.compose(items, none, none)?.intoTurn, true, "a person is needed: into the running turn");
	c.snapshot([worker(5, "running")]); // answered in its tab while the context was fetched
	assert.equal(c.compose(items, none, none), undefined);
});

test("a failed delivery is held for the next snapshot, and given up after three tries", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running")]);
	const done = [worker(5, "done")];
	let items = c.snapshot(done).wake;
	for (let i = 1; i <= 2; i++) {
		assert.deepEqual(ids(c.deliveryFailed(items).held), ["5:done"], `try ${i}`);
		items = c.snapshot(done).wake;
		assert.deepEqual(ids(items), ["5:done"], `redelivered after try ${i}`);
	}
	assert.deepEqual(ids(c.deliveryFailed(items).dropped), ["5:done"]);
	assert.deepEqual(c.snapshot(done).wake, []);
});

test("a failed delivery of news the model has read since is not retried", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running")]);
	const items = c.snapshot([worker(5, "done")]).wake;
	c.shellEnded("t1", "routr result 5", "Done.\n");
	assert.deepEqual(c.deliveryFailed(items).held, []);
	assert.deepEqual(c.snapshot([worker(5, "done")]).wake, []);
});

test("wake mode: off wakes for nobody, all for every worker", () => {
	const { c } = setup();
	c.snapshot([worker(1, "running"), worker(2, "running", { callerSession: "other" })]);
	c.wakeMode = "off";
	assert.deepEqual(c.snapshot([worker(1, "done"), worker(2, "running", { callerSession: "other" })]).wake, []);
	c.wakeMode = "all";
	assert.deepEqual(ids(c.snapshot([worker(1, "done"), worker(2, "done", { callerSession: "other" })]).wake), ["2:done"]);
});

test("after the session ends, nothing fetched for it is composed", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running")]);
	const items = c.snapshot([worker(5, "done")]).wake;
	c.end();
	assert.equal(c.compose(items, new Map([["5", "Done."]]), none), undefined);
	assert.deepEqual(c.snapshot([worker(5, "done"), worker(6, "done")]), { wake: [], keep: [] });
});

test("during a turn a person is needed at once; other news waits for the turn's end, and is dropped if the model reads it meanwhile", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running"), worker(6, "running"), worker(7, "running")]);
	c.turnStarted();
	const now = c.snapshot([worker(5, "blocked", { question: "Proceed?" }), worker(6, "done"), worker(7, "done")]).wake;
	assert.deepEqual(ids(now), ["5:blocked"], "into the running turn");
	assert.deepEqual(ids(c.held()), ["6:done", "7:done"]);
	c.shellEnded("t1", "routr result 6", "Done.\n");
	assert.deepEqual(ids(c.held()), ["7:done"], "the model read 6 itself");
	assert.equal(c.turnEnded(), true, "news waits: take a snapshot soon");
	assert.deepEqual(ids(c.snapshot([worker(5, "blocked", { question: "Proceed?" }), worker(6, "done"), worker(7, "done")]).wake), ["7:done"]);
	assert.deepEqual(c.held(), []);
});

test("after the user interrupts a turn nothing goes, until their next turn ends", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running"), worker(6, "running")]);
	c.turnStarted();
	c.snapshot([worker(5, "done"), worker(6, "running")]);
	assert.equal(c.turnEnded("aborted"), false, "nothing to deliver now");
	assert.deepEqual(c.snapshot([worker(5, "done"), worker(6, "blocked", { question: "Proceed?" })]).wake, [], "not even a question: the user is about to type");
	assert.deepEqual(ids(c.held()), ["5:done", "6:blocked"]);
	c.turnStarted(); // the user's next prompt
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "blocked", { question: "Proceed?" })]).wake), [], "still held during it");
	assert.equal(c.turnEnded("answer"), true);
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "blocked", { question: "Proceed?" })]).wake), ["5:done", "6:blocked"]);
});

test("news put into a turn that ends before the model's next request is delivered again", () => {
	const { c } = setup();
	c.snapshot([worker(5, "running"), worker(6, "running")]);
	c.turnStarted();
	const first = c.snapshot([worker(5, "blocked", { question: "Proceed?" }), worker(6, "running")]).wake;
	c.delivered(first, true);
	c.stepStarted(); // the model read it
	const second = c.snapshot([worker(5, "blocked", { question: "Proceed?" }), worker(6, "blocked", { question: "Delete?" })]).wake;
	assert.deepEqual(ids(second), ["6:blocked"]);
	c.delivered(second, true); // during the turn's last request
	assert.equal(c.turnEnded(), true);
	assert.deepEqual(ids(c.snapshot([worker(5, "blocked", { question: "Proceed?" }), worker(6, "blocked", { question: "Delete?" })]).wake), ["6:blocked"], "6 again, 5 not");
});

test("a background routr wait covers its workers until its shell ends: completed, they are read; otherwise they wake", () => {
	const { c, tick } = setup();
	c.snapshot([worker(5, "running"), worker(6, "running")]);
	c.backgroundStarted("bg1", "routr wait 5");
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "running")]).wake), [], "covered while the shell runs");
	c.backgroundEnded("bg1", "routr wait 5", true);
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "running")]).wake), [], "the shell's notice handed it over");
	c.backgroundStarted("bg2", "routr wait 6");
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "done")]).wake), []);
	c.backgroundEnded("bg2", "routr wait 6", false); // exit 3: still running when it gave up, or killed
	assert.deepEqual(ids(c.snapshot([worker(5, "done"), worker(6, "done")]).wake), ["6:done"]);
	// a background start --wait covers what the session starts meanwhile
	tick(5000);
	c.backgroundStarted("bg3", "routr start --wait 'a; b'");
	const w = worker(7, "running", { created: T0 + 5000 });
	c.snapshot([w]);
	assert.deepEqual(c.snapshot([{ ...w, state: "done" }]).wake, []);
	c.backgroundEnded("bg3", "routr start --wait 'a; b'", true);
	assert.deepEqual(c.snapshot([{ ...w, state: "done" }]).wake, []);
});

test("while the front end compacts the model's context, news waits, a question too, and resume says it is due", () => {
	const { c } = setup();
	c.snapshot([worker(1, "running"), worker(2, "running")]);
	c.pause();
	assert.deepEqual(ids(c.snapshot([worker(1, "done"), worker(2, "blocked", { question: "ok?" })]).wake), [], "a wake turn would race the compaction");
	assert.deepEqual(ids(c.held()), ["1:done", "2:blocked"]);
	assert.equal(c.resume(), true, "take a snapshot soon");
	assert.deepEqual(ids(c.snapshot([worker(1, "done"), worker(2, "blocked", { question: "ok?" })]).wake), ["1:done", "2:blocked"]);
	assert.equal(c.resume(), false, "nothing held");
});

// The model queued workers in another repo's registry (cd ../other && routr start …).
// Ids are per registry: the coordinator knows each worker by both.
const HOME = "/code/acksys-tool/.routr";
const OTHER = "/code/vp9enc-rs/.routr";
const queued = (id: number, reg = OTHER) => `routr: queued worker ${id} task · claude (claude); registry ${reg}\n${id}\n`;
const keys = (items: readonly WakeItem[]): string[] => items.map((i) => `${i.worker.registry ?? ""}#${i.worker.id}:${i.kind}`);

test("a worker queued in another repo's registry is listed there and wakes the model; the same id at home does not", () => {
	const { c } = setup({ home: HOME });
	c.snapshot([worker(141, "running", { callerSession: "other" })]); // home's own 141, another session's
	c.turnStarted();
	assert.deepEqual(c.elsewhere(), []);
	const keep = c.shellEnded("t1", "cd /code/vp9enc-rs && routr start x", queued(141));
	assert.deepEqual(keep, [{ ids: [141], chains: [], group: "run:1", registry: OTHER }], "kept with its registry");
	c.turnEnded();
	assert.deepEqual(c.elsewhere(), [OTHER], "listed: it holds a claim");
	const theirs = worker(141, "running", { callerSession: undefined });
	assert.deepEqual(c.snapshot([{ ...worker(141, "done", { callerSession: "other" }) }]).wake, [], "home's 141 is not this session's");
	assert.deepEqual(c.snapshot([theirs], OTHER).wake, []);
	assert.equal(c.isMine({ ...theirs, registry: OTHER }), true);
	assert.equal(c.isMine(worker(141, "running", { callerSession: undefined })), false, "the id alone matches nothing");
	assert.deepEqual(c.workers.map((w) => `${w.registry ?? ""}#${w.id}`), ["#141", `${OTHER}#141`]);
	const out = c.snapshot([{ ...theirs, state: "done" }], OTHER);
	assert.deepEqual(keys(out.wake), [`${OTHER}#141:done`]);
	assert.deepEqual(c.elsewhere(), [], "nothing at work there: no more lists");
	// a follow-up lists it again
	c.shellEnded("t2", "cd /code/vp9enc-rs && routr send 141 more", "routr: queued follow-up 1 for 141 task · claude; it goes into its tab; registry /code/vp9enc-rs/.routr\n");
	assert.deepEqual(c.elsewhere(), [OTHER]);
});

test("another repo's first list wakes for what the session queued there, already finished; the rest is a baseline", () => {
	const { c } = setup({ home: HOME });
	c.snapshot([]);
	c.shellEnded("t1", "cd /code/vp9enc-rs && routr start --run 'go test' ", queued(7));
	const out = c.snapshot([worker(6, "done", { callerSession: S }), worker(7, "done", { callerSession: undefined })], OTHER);
	assert.deepEqual(keys(out.wake), [`${OTHER}#7:done`], "6 finished before: shown, not woken for");
});

// The model's shell hid the "queued" lines (ID=$(routr start … 2>log); echo "x=$ID $(cat log)"):
// only the place the call ran says where its workers are, and routr's caller.session whose they are.
test("a start in another repo whose output names no worker still lists that registry, and wakes for this session's workers there", () => {
	const locate = (at: Place) => ("cwd" in at && at.cwd === "/code/vp9enc-rs" ? OTHER : undefined);
	const { c, tick } = setup({ home: HOME, locate });
	c.snapshot([]);
	c.turnStarted();
	tick(1000);
	const hidden = `cxd=1 routr: queued worker 1 cxd · claude (claude); registry ${OTHER}\n`;
	const keep = c.shellEnded("t1", 'cd /code/vp9enc-rs && ID=$(routr start --cwd x - < b.md 2>/tmp/e); echo "cxd=$ID $(cat /tmp/e)"', hidden);
	assert.deepEqual(keep, [{ ids: [], chains: [], group: "run:1", registry: OTHER }], "the registry is kept, to list it after a restart");
	c.turnEnded();
	assert.deepEqual(c.elsewhere(), [OTHER]);
	const mine = worker(1, "running", { created: T0 + 1000 });
	const theirs = worker(2, "running", { created: T0 + 1000, callerSession: "other" });
	assert.deepEqual(c.snapshot([mine, theirs], OTHER).wake, []);
	assert.deepEqual(c.elsewhere(), [OTHER], "listed while this session's worker is at work there");
	const out = c.snapshot([{ ...mine, state: "done" }, { ...theirs, state: "done" }], OTHER);
	assert.deepEqual(keys(out.wake), [`${OTHER}#1:done`], "routr recorded this session as 1's caller; 2 is another's");
	assert.deepEqual(c.elsewhere(), []);
	// the same command again lists it again, though it is no longer new
	assert.deepEqual(c.shellEnded("t2", "cd /code/vp9enc-rs && routr start y >/dev/null 2>&1", ""), []);
	assert.deepEqual(c.elsewhere(), [OTHER]);
});

test("another repo's first list wakes for a worker routr says this session started during a turn, though it finished already", () => {
	const locate = (at: Place) => ("cwd" in at && at.cwd === "/code/vp9enc-rs" ? OTHER : undefined);
	const { c, tick } = setup({ home: HOME, locate });
	c.snapshot([]);
	c.turnStarted();
	tick(1000);
	c.shellEnded("t1", "cd /code/vp9enc-rs && routr start --run 'go test' >/dev/null 2>&1", "");
	c.turnEnded();
	const out = c.snapshot([worker(6, "done", { created: T0 - 60_000 }), worker(7, "done", { created: T0 + 1000 })], OTHER);
	assert.deepEqual(keys(out.wake), [`${OTHER}#7:done`], "6 is from before this session's first turn: a baseline");
});

test("claims at a place go where locate says; one it can't tell is dropped; one at home is home's", () => {
	const locate = (at: Place) => ("cwd" in at ? ({ "/code/vp9enc-rs": OTHER, sub: HOME } as Record<string, string>)[at.cwd] : undefined);
	const { c } = setup({ home: HOME, locate });
	c.snapshot([worker(5, "running", { callerSession: undefined })]);
	c.shellEnded("t1", "cd /code/vp9enc-rs && routr start x", "5\n");
	assert.equal(c.isMine(worker(5, "running", { callerSession: undefined })), false);
	assert.equal(c.isMine(worker(5, "running", { callerSession: undefined, registry: OTHER })), true);
	c.shellEnded("t2", "cd /nowhere && routr start x", "6\n");
	assert.equal(c.isMine(worker(6, "running", { callerSession: undefined })), false, "dropped, not matched at home");
	assert.deepEqual(c.elsewhere(), [OTHER]);
	c.shellEnded("t3", "cd sub && routr start x", "8\n");
	assert.equal(c.isMine(worker(8, "running", { callerSession: undefined })), true, "a subdirectory of the same repo");
});

test("a wait, a read and a cancel in another repo count there, not at home", () => {
	const { c } = setup({ home: HOME });
	c.snapshot([worker(5, "running")]);
	c.shellEnded("t0", "cd /code/vp9enc-rs && routr start x", queued(5));
	c.snapshot([worker(5, "running")], OTHER);
	c.shellStarted("t1", "routr wait 5 --root /code/vp9enc-rs");
	const both = () => {
		const a = c.snapshot([worker(5, "done")]).wake;
		return [...a, ...c.snapshot([worker(5, "done")], OTHER).wake];
	};
	assert.deepEqual(keys(both()), ["#5:done"], "the wait covers the other repo's 5 only");
	c.shellEnded("t1", "routr wait 5 --root /code/vp9enc-rs", "Done.\n");
	assert.deepEqual(keys(both()), [], "it returned it");
	// a cancel there drops that one's news only
	c.snapshot([worker(9, "running")]);
	c.shellEnded("t2", "cd /code/vp9enc-rs && routr start x", queued(9));
	c.snapshot([worker(9, "running")], OTHER);
	c.shellEnded("t3", "routr cancel 9 --root /code/vp9enc-rs", "routr: cancelled 9 task · claude\n");
	const home = c.snapshot([worker(9, "blocked", { question: "ok?" })]).wake;
	assert.deepEqual(keys(home), ["#9:blocked"]);
});

test("claims kept with another repo's registry are handed back after a restart, and listed once", () => {
	const c = new Coordinator({ sessionId: S, home: HOME, restored: [{ ids: [3], chains: ["c1"], group: "run:1", registry: OTHER }] });
	assert.deepEqual(c.elsewhere(), [OTHER]);
	assert.equal(c.isMine(worker(3, "done", { callerSession: undefined, registry: OTHER })), true);
	assert.equal(c.isMine(worker(3, "done", { callerSession: undefined })), false);
	assert.equal(c.isMine(worker(4, "done", { callerSession: undefined, chain: "c1", registry: OTHER })), true);
	c.snapshot([]);
	assert.deepEqual(c.snapshot([worker(3, "done", { callerSession: undefined })], OTHER).wake, [], "finished before the restart: a baseline");
	assert.deepEqual(c.elsewhere(), []);
});

test("another repo's chain is told from a chain of the same name at home", () => {
	const { c } = setup({ home: HOME });
	c.snapshot([worker(2, "running", { callerSession: "other", chain: "plan-1" })]);
	c.shellEnded("t1", "cd /code/vp9enc-rs && routr flow start plan --task x", `routr: queued 2 plan · claude (claude) step plan; registry ${OTHER}\nplan-1\n`);
	assert.equal(c.isMine(worker(2, "running", { callerSession: "other", chain: "plan-1" })), false);
	assert.equal(c.isMine(worker(2, "running", { callerSession: "other", chain: "plan-1", registry: OTHER })), true);
	assert.equal(c.group(worker(2, "running", { chain: "plan-1", registry: OTHER })), `chain:${OTHER}#plan-1`);
	assert.equal(c.group(worker(2, "running", { chain: "plan-1" })), "chain:plan-1");
});
