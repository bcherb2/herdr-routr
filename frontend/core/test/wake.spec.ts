import { test } from "node:test";
import assert from "node:assert/strict";
import type { QuestionContext, State, Worker } from "../model.ts";
import { buildWakeMessage, DEFAULT_HOLD_MS, holdFromEnv, WakeTracker, wakeKind, type WakeContext } from "../wake.ts";

const w = (id: number, state: State, extra: Partial<Worker> = {}): Worker => ({ id, role: "task", harness: "codex", state, ...extra });

const mineCtx = (ids: number[], groups: Record<number, string> = {}): WakeContext => ({
	isMine: (x) => ids.includes(x.id),
	group: (x) => (x.chain ? `chain:${x.chain}` : groups[x.id]),
});

test("wakeKind", () => {
	assert.equal(wakeKind(w(1, "done"), "running"), "done");
	assert.equal(wakeKind(w(1, "gated"), undefined), "gated");
	assert.equal(wakeKind(w(1, "running"), "pending"), undefined);
	assert.equal(wakeKind(w(1, "cancelled"), "running"), undefined);
	assert.equal(wakeKind(w(1, "closed"), "running"), "closed", "tab gone before routr done");
	assert.equal(wakeKind(w(1, "closed"), "done"), undefined, "normal end, reported as done");
	assert.equal(wakeKind(w(1, "closed"), undefined), undefined);
	const talk = { interactive: true };
	assert.equal(wakeKind(w(1, "running", talk), "running"), undefined, "its first turn is the user's to read");
	assert.equal(wakeKind(w(1, "running", { ...talk, turn: 2, replies: 1 }), "running"), undefined, "a follow-up in flight");
	assert.equal(wakeKind(w(1, "running", { ...talk, turn: 2, replies: 2 }), "running"), "reply");
	assert.equal(wakeKind(w(1, "running", { ...talk, turn: 2, replies: 2, queued: 1 }), "running"), undefined, "another one goes next");
	assert.equal(wakeKind(w(1, "running", { turn: 2, replies: 2 }), "running"), undefined, "not interactive: it settles into done or maybe_done");
});

test("first snapshot is a baseline; later news for my workers wakes once", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2]);
	assert.deepEqual(t.update([w(1, "done", { finished: 1 }), w(2, "running")], ctx), [], "old news is not woken for");
	assert.deepEqual(t.update([w(1, "done", { finished: 1 }), w(2, "running")], ctx), []);
	const got = t.update([w(1, "done", { finished: 1 }), w(2, "done", { finished: 2 })], ctx);
	assert.deepEqual(got.map((i) => [i.worker.id, i.kind]), [[2, "done"]]);
	assert.deepEqual(t.update([w(1, "done", { finished: 1 }), w(2, "done", { finished: 2 })], ctx), [], "no repeat");
});

test("others' workers don't wake the model, and turning on wake-all doesn't replay them", () => {
	const t = new WakeTracker();
	t.update([w(9, "running")], mineCtx([]));
	assert.deepEqual(t.update([w(9, "done")], mineCtx([])), []);
	assert.deepEqual(t.update([w(9, "done")], mineCtx([9])), []);
});

test("a follow-up (back to running) makes the next finish new news", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1]);
	t.update([w(1, "running")], ctx);
	assert.equal(t.update([w(1, "done", { finished: 10 })], ctx).length, 1);
	t.update([w(1, "running")], ctx);
	assert.equal(t.update([w(1, "done", { finished: 10 })], ctx).length, 1, "same stamp, but it ran again");
	// a finish we missed the running state of still counts: the stamp changed
	assert.equal(t.update([w(1, "done", { finished: 20 })], ctx).length, 1);
	// done → closed is the normal end
	assert.equal(t.update([w(1, "closed", { finished: 20 })], ctx).length, 0);
});

// An interactive worker stays running across a follow-up, so its reply is
// news only through the reply count.
test("an interactive worker's reply to a follow-up wakes once, and the next one again", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1]);
	const talk = (turn: number, replies: number) => w(1, "running", { interactive: true, turn, replies });
	t.update([talk(0, 0)], ctx);
	assert.deepEqual(t.update([talk(1, 0)], ctx), [], "in flight");
	assert.deepEqual(t.update([talk(1, 1)], ctx).map((i) => [i.worker.id, i.kind]), [[1, "reply"]]);
	assert.deepEqual(t.update([talk(1, 1)], ctx), [], "no repeat while it stays running");
	assert.deepEqual(t.update([talk(2, 2)], ctx).map((i) => i.kind), ["reply"], "a reply whose turn we never saw in flight");
	t.update([talk(3, 2)], ctx);
	t.markSeen([1]); // routr send --wait returned it
	assert.deepEqual(t.update([talk(3, 3)], ctx), []);
});

test("a follow-up reply is news even when the running state in between was missed", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2]);
	t.update([w(1, "running"), w(2, "running")], ctx);
	t.update([w(1, "maybe_done", { turn: 1, replies: 1 }), w(2, "done", { finished: 5, turn: 1, replies: 1 })], ctx);
	const got = t.update([w(1, "maybe_done", { turn: 2, replies: 2 }), w(2, "done", { finished: 5, turn: 2, replies: 2 })], ctx);
	assert.deepEqual(got.map((i) => [i.worker.id, i.kind]), [[1, "maybe_done"], [2, "done"]]);
});

test("a fan-out wakes once, when the last sibling finishes", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2, 3], { 1: "run:1", 2: "run:1", 3: "run:1" });
	t.update([w(1, "running"), w(2, "running"), w(3, "running")], ctx);
	assert.deepEqual(t.update([w(1, "done"), w(2, "running"), w(3, "running")], ctx), []);
	assert.deepEqual(t.update([w(1, "done"), w(2, "done"), w(3, "running")], ctx), []);
	assert.equal(t.pending().length, 2);
	const got = t.update([w(1, "done"), w(2, "done"), w(3, "done")], ctx);
	assert.deepEqual(got.map((i) => i.worker.id), [1, 2, 3]);
	assert.equal(t.pending().length, 0);
});

test("a failed sibling releases its group at once", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2, 3], { 1: "run:1", 2: "run:1", 3: "run:1" });
	t.update([w(1, "running"), w(2, "running"), w(3, "running")], ctx);
	assert.deepEqual(t.update([w(1, "done"), w(2, "running"), w(3, "running")], ctx), []);
	const got = t.update([w(1, "done"), w(2, "failed", { error: "exited during startup" }), w(3, "running")], ctx);
	assert.deepEqual(got.map((i) => [i.worker.id, i.kind]), [[1, "done"], [2, "failed"]]);
	assert.deepEqual(t.update([w(1, "done"), w(2, "failed", { error: "exited during startup" }), w(3, "done")], ctx).map((i) => i.worker.id), [3]);
});

test("a held result goes out after the hold cap though the chain runs on", () => {
	let now = 1_000_000;
	const t = new WakeTracker({ now: () => now, maxHoldMs: 120_000 });
	const chain = { chain: "bn2" };
	const ctx: WakeContext = { isMine: (x) => x.chain === "bn2", group: (x) => `chain:${x.chain}` };
	const snap = (a: State, b: State) => [w(1, a, { ...chain, finished: a === "done" ? 1 : undefined }), w(2, b, { ...chain, after: [1] })];
	t.update(snap("running", "waiting"), ctx);
	assert.deepEqual(t.update(snap("done", "running"), ctx), [], "the plan is held while implement runs");
	now += 119_000;
	assert.deepEqual(t.update(snap("done", "running"), ctx), [], "still under the cap");
	now += 2_000;
	assert.deepEqual(t.update(snap("done", "running"), ctx).map((i) => [i.worker.id, i.kind]), [[1, "done"]], "over it: out it goes");
	assert.equal(t.pending().length, 0);
	assert.deepEqual(t.update(snap("done", "running"), ctx), [], "once");

	// a cap of 0 holds nothing
	const u = new WakeTracker({ now: () => now, maxHoldMs: 0 });
	u.update(snap("running", "waiting"), ctx);
	assert.deepEqual(u.update(snap("done", "running"), ctx).map((i) => i.worker.id), [1]);
});

test("the hold cap: ROUTR_WAKE_HOLD", () => {
	assert.equal(holdFromEnv(undefined), DEFAULT_HOLD_MS);
	assert.equal(DEFAULT_HOLD_MS, 120_000);
	assert.equal(holdFromEnv("90"), 90_000, "seconds");
	assert.equal(holdFromEnv("90s"), 90_000);
	assert.equal(holdFromEnv("2m"), 120_000);
	assert.equal(holdFromEnv("1500ms"), 1500);
	assert.equal(holdFromEnv("0"), 0);
	assert.equal(holdFromEnv("soon"), DEFAULT_HOLD_MS, "unreadable: the default");
	assert.equal(holdFromEnv("-5"), DEFAULT_HOLD_MS);
});

test("a cancelled worker's held news is dropped", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2], { 1: "g", 2: "g" });
	t.update([w(1, "running"), w(2, "running")], ctx);
	assert.deepEqual(t.update([w(1, "maybe_done"), w(2, "running")], ctx), []);
	assert.equal(t.pending().length, 1);
	assert.deepEqual(t.update([w(1, "cancelled"), w(2, "running")], ctx), []);
	assert.equal(t.pending().length, 0);
	assert.deepEqual(t.update([w(1, "cancelled"), w(2, "done")], ctx).map((i) => i.worker.id), [2], "only the sibling's news");
});

test("drop: the model cancelled it; its news is moot before the next snapshot says so", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2, 3], { 1: "g", 2: "g", 3: "g" });
	t.update([w(1, "running"), w(2, "running"), w(3, "running")], ctx);
	t.update([w(1, "maybe_done"), w(2, "done", { finished: 2 }), w(3, "running")], ctx);
	assert.deepEqual(t.pending().map((i) => i.worker.id), [1, 2]);
	t.drop([1, 2]);
	assert.deepEqual(t.pending().map((i) => i.worker.id), [2], "a done result stays: cancel leaves it done");
	assert.deepEqual(t.update([w(1, "maybe_done"), w(2, "done", { finished: 2 }), w(3, "done")], ctx).map((i) => i.worker.id), [2, 3], "1 does not come back");
	// a worker never seen: nothing to drop, nothing breaks
	t.drop([99]);
	assert.deepEqual(t.pending(), []);
});

test("a second failure with a new error wakes again", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1]);
	t.update([w(1, "running")], ctx);
	assert.equal(t.update([w(1, "failed", { error: "exited during startup" })], ctx).length, 1);
	assert.equal(t.update([w(1, "failed", { error: "exited during startup" })], ctx).length, 0);
	const got = t.update([w(1, "failed", { error: "could not resume session; the follow-up was dropped" })], ctx);
	assert.deepEqual(got.map((i) => i.kind), ["failed"]);
});

test("a chain wakes at the gate with what came before", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([3]); // the pipe printed only the last id…
	const chain = { chain: "auth" };
	const snap = (a: State, b: State, c: State) => [w(1, a, chain), w(2, b, chain), w(3, c, chain)];
	const chainCtx: WakeContext = { ...ctx, isMine: (x) => x.chain === "auth" }; // …the chain field makes the rest mine
	t.update(snap("running", "waiting", "waiting"), chainCtx);
	assert.deepEqual(t.update(snap("done", "running", "waiting"), chainCtx), []);
	assert.deepEqual(t.update(snap("done", "done", "waiting"), chainCtx), []);
	const got = t.update(snap("done", "done", "gated"), chainCtx);
	assert.deepEqual(got.map((i) => [i.worker.id, i.kind]), [
		[1, "done"],
		[2, "done"],
		[3, "gated"],
	]);
});

test("a chain step that fails wakes the model: the steps queued after it can't start", () => {
	const t = new WakeTracker();
	const chain = { chain: "auth" };
	const ctx: WakeContext = { isMine: (x) => x.chain === "auth", group: (x) => `chain:${x.chain}` };
	const snap = (a: State, b: State, c: State) => [
		w(1, a, chain),
		w(2, b, { ...chain, after: [1] }),
		w(3, c, { ...chain, after: [2] }),
	];
	t.update(snap("running", "waiting", "waiting"), ctx);
	const got = t.update(snap("failed", "waiting", "waiting"), ctx);
	assert.deepEqual(got.map((i) => [i.worker.id, i.kind]), [[1, "failed"]], "2 and 3 wait on a failed step, transitively");
	assert.equal(t.pending().length, 0);
});

test("a run or judge step that failed halts the steps after it, so the news goes out", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2]);
	const chain = { chain: "c" };
	t.update([w(1, "running", chain), w(2, "waiting", { ...chain, after: [1] })], ctx);
	const check = w(1, "done", { ...chain, role: "run", verdict: "fail", halts: true, finished: 1 });
	const got = t.update([check, w(2, "waiting", { ...chain, after: [1] })], ctx);
	assert.deepEqual(got.map((i) => [i.worker.id, i.kind]), [[1, "done"]]);
	assert.match(buildWakeMessage(got, new Map()).content, /verdict fail\.\nThe steps after it wait\..*routr approve 1/);

	// approved past it (no longer halts): the step after it runs, the news waits for it
	const u = new WakeTracker();
	u.update([w(1, "running", chain), w(2, "waiting", { ...chain, after: [1] })], ctx);
	assert.deepEqual(u.update([{ ...check, halts: false }, w(2, "running", { ...chain, after: [1] })], ctx), []);
});

test("maybe_done, cancelled or closed-before-done upstreams stop the queue too", () => {
	for (const up of [w(1, "maybe_done"), w(1, "cancelled"), w(1, "closed")]) {
		const t = new WakeTracker();
		const ctx = mineCtx([1, 2, 5], { 1: "g", 2: "g", 5: "g" });
		t.update([w(1, "running"), w(2, "waiting", { after: [1] }), w(5, "running")], ctx);
		assert.deepEqual(t.update([up, w(2, "waiting", { after: [1] }), w(5, "running")], ctx), [], `${up.state}: 5 is still running`);
		const got = t.update([up, w(2, "waiting", { after: [1] }), w(5, "done")], ctx);
		assert.deepEqual(got.map((i) => i.worker.id), up.state === "cancelled" ? [5] : [1, 5], up.state);
	}
});

test("a sibling waiting on a live upstream or a lock still holds the news", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2, 3], { 1: "g", 2: "g", 3: "g" });
	t.update([w(1, "running"), w(2, "running"), w(3, "waiting", { after: [2] })], ctx);
	assert.deepEqual(t.update([w(1, "done"), w(2, "running"), w(3, "waiting", { after: [2] })], ctx), [], "upstream running");
	assert.deepEqual(t.update([w(1, "done"), w(2, "done"), w(3, "waiting", { after: [2] })], ctx), [], "upstream done: waits for the lock");
	assert.deepEqual(t.update([w(1, "done"), w(2, "done"), w(3, "done")], ctx).map((i) => i.worker.id), [1, 2, 3]);
});

test("an upstream that dropped out of the list is judged as last seen", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([2, 3], { 2: "g", 3: "g" });
	t.update([w(1, "running"), w(2, "running"), w(3, "waiting", { after: [1] })], ctx);
	t.update([w(1, "failed"), w(2, "running"), w(3, "waiting", { after: [1] })], ctx);
	// worker 1 changed over an hour ago: `list` no longer shows it
	assert.deepEqual(t.update([w(2, "done"), w(3, "waiting", { after: [1] })], ctx).map((i) => i.worker.id), [2]);
});

test("a running routr wait holds the news it will return; what it didn't return wakes after", () => {
	const t = new WakeTracker();
	let covering: number[] = [1];
	const ctx: WakeContext = { ...mineCtx([1, 2], { 1: "a", 2: "b" }), covered: (x) => covering.includes(x.id) };
	t.update([w(1, "running"), w(2, "running")], ctx);
	assert.deepEqual(t.update([w(1, "done"), w(2, "done")], ctx).map((i) => i.worker.id), [2], "1 is held for the wait");
	assert.deepEqual(t.update([w(1, "done"), w(2, "done")], ctx), []);
	// the wait returned 1's result
	t.markSeen([1]);
	covering = [];
	assert.deepEqual(t.update([w(1, "done"), w(2, "done")], ctx), []);
	assert.equal(t.pending().length, 0);

	// a wait that ended (timed out) without returning it: wake then
	t.update([w(1, "running")], ctx);
	covering = [1];
	assert.deepEqual(t.update([w(1, "done", { finished: 5 })], ctx), []);
	covering = [];
	assert.deepEqual(t.update([w(1, "done", { finished: 5 })], ctx).map((i) => i.worker.id), [1]);
});

test("a covered sibling holds its group, but a person's question still goes out", () => {
	const t = new WakeTracker();
	const ctx: WakeContext = { ...mineCtx([1, 2, 3], { 1: "g", 2: "g", 3: "g" }), covered: (x) => x.id === 1 };
	t.update([w(1, "running"), w(2, "running"), w(3, "running")], ctx);
	assert.deepEqual(t.update([w(1, "done"), w(2, "done"), w(3, "running")], ctx), []);
	const got = t.update([w(1, "done"), w(2, "done"), w(3, "blocked", { question: "Allow?" })], ctx);
	assert.deepEqual(got.map((i) => [i.worker.id, i.kind]), [[2, "done"], [3, "blocked"]]);
	assert.deepEqual(t.pending().map((i) => i.worker.id), [1]);
});

test("a blocked worker releases at once; a new question is new news", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2], { 1: "g", 2: "g" });
	t.update([w(1, "running"), w(2, "running")], ctx);
	assert.equal(t.update([w(1, "blocked", { question: "A?" }), w(2, "running")], ctx).length, 1);
	assert.equal(t.update([w(1, "blocked", { question: "A?" }), w(2, "running")], ctx).length, 0);
	assert.equal(t.update([w(1, "blocked", { question: "B?" }), w(2, "running")], ctx).length, 1);
});

test("an on_fail round makes the held result stale", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2], { 1: "g", 2: "g" });
	t.update([w(1, "running"), w(2, "waiting")], ctx);
	t.update([w(1, "done"), w(2, "running")], ctx);
	assert.equal(t.pending().length, 1);
	t.update([w(1, "running"), w(2, "waiting")], ctx); // the reviewer failed it; the planner resumes
	assert.equal(t.pending().length, 0);
});

test("results the model already read are not woken for", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1]);
	t.update([w(1, "running")], ctx);
	t.markSeen([1]);
	assert.deepEqual(t.update([w(1, "done")], ctx), []);
	// the suppression is used up
	t.update([w(1, "running")], ctx);
	assert.equal(t.update([w(1, "done")], ctx).length, 1);
});

test("wake message: result text, verdict, gate and question wording", () => {
	const items = [
		{ worker: w(3, "done", { role: "review", model: "gpt-6-astra", effort: "high", verdict: "fail", output: "REDTEAM.md" }), kind: "done" as const },
		{ worker: w(4, "blocked", { role: "research", harness: "claude", question: "Allow curl? 1. Yes 2. No" }), kind: "blocked" as const },
		{ worker: w(5, "gated", { role: "implement", title: "fix-auth", chain: "auth", step: "implement" }), kind: "gated" as const },
		{ worker: w(6, "maybe_done", { output: "OUT.md" }), kind: "maybe_done" as const },
		{ worker: w(7, "running", { role: "plan", interactive: true, turn: 2, replies: 2 }), kind: "reply" as const },
	];
	const results = new Map([
		[3, "Two findings.\n1. Token refresh races.\n\noutput: REDTEAM.md\nverdict: fail\n"],
		[6, "x".repeat(5000)],
		[7, "reply to follow-up 2:\nKeep the lock."],
	]);
	const m = buildWakeMessage(items, results, 4000);
	assert.match(m.content, /^\[routr\] worker 3 · review · codex·gpt-6-astra·high is done, verdict fail\.\n\nTwo findings\./);
	assert.match(m.content, /output: REDTEAM\.md\nverdict: fail/);
	assert.match(m.content, /worker 4 · research · claude is blocked: Allow curl\?/);
	assert.match(m.content, /Allow curl\? 1\. Yes 2\. No\nRun `routr wait 4` before you do anything about it/, "no context fetched: the command that says who answers");
	assert.doesNotMatch(m.content, /routr answer 4/, "no menu, no option to press");
	assert.match(m.content, /worker 5 "fix-auth" · implement · codex \(chain auth\/implement\) waits at its gate/);
	assert.match(m.content, /only after the user says yes; never approve on your own/);
	assert.match(m.content, /went idle without running routr done.*check that OUT\.md exists/);
	assert.match(m.content, /\(clipped; full text: routr result 6\)/);
	assert.match(m.content, /worker 7 · plan · codex replied to follow-up 2\. It is still open: `routr send 7 "…"` goes on\.\n\nreply to follow-up 2:\nKeep the lock\./);
	assert.ok(m.content.length < 6000);
	assert.deepEqual(
		m.details.entries.map((e) => [e.id, e.kind, e.preview]),
		[
			[3, "done", "Two findings."],
			[4, "blocked", "Allow curl? 1. Yes 2. No"],
			[5, "gated", ""],
			[6, "maybe_done", "x".repeat(160) + "…"],
			[7, "reply", "reply to follow-up 2:"],
		],
	);
});

// A failed worker that saved a reply: `routr result` puts its error and the
// command that resumes it above that reply, so the wake keeps the command
// even when it clips a long reply.
test("wake message: a failed worker's recovery command survives the clip", () => {
	const result =
		"failed: 1 task · claude: the harness exited before it ran routr done\n" +
		"routr send 1 … resumes its session\n\n" +
		"x".repeat(6000);
	const m = buildWakeMessage([{ worker: w(1, "failed", { error: "the harness exited before it ran routr done" }), kind: "failed" }], new Map([[1, result]]), 4000);
	assert.match(m.content, /^\[routr\] worker 1 · task · codex failed: the harness exited before it ran routr done\.\n\nfailed: .*\nroutr send 1 … resumes its session\n/);
	assert.match(m.content, /\(clipped; full text: routr result 1\)$/);
});

// Who answers a blocked worker's question, and how, is routr's to say
// (watch.QuestionContext.WhoAnswers, tested in Go): the wake prints the lines
// routr gave (`answer` in routr show ID --context --json), as routr wait
// does, and adds only the front end's own way in. The incident behind this:
// woken with "Answer only with their choice", the model answered four
// prompts on its own; so without routr's lines it chooses nothing.
test("wake message: who answers a blocked worker's question is routr's to say", () => {
	const blocked = (id: number, extra: Partial<Worker> = {}) => ({
		worker: w(id, "blocked", {
			role: "research",
			harness: "claude",
			question: "Do you want to proceed?\n1. Yes\n2. No",
			fingerprint: "6a128b4273b95faa",
			asksAbout: "Bash: rm -rf build",
			...extra,
		}),
		kind: "blocked" as const,
	});
	const head = (id: number) => `[routr] worker ${id} · research · claude is blocked: Do you want to proceed?\n1. Yes\n2. No\nIt asks about Bash: rm -rf build`;
	const answer = [
		"The user answers this question, not you: do not choose an option yourself, even an obvious one.",
		"Tell the user what it asks. They answer in its tab (routr show 4), or tell you an option.",
		"If they tell you option N, run: routr answer 4 N --by human --expect 6a128b4273b95faa",
	];
	const ctx = (extra: Partial<QuestionContext> = {}): ReadonlyMap<number, QuestionContext> =>
		new Map([[4, { answer, lasting: [], question: "Do you want to proceed?\n1. Yes\n2. No", fingerprint: "6a128b4273b95faa", ...extra }]]);
	const msg = (item = blocked(4), contexts: ReadonlyMap<number, QuestionContext> = new Map()) => buildWakeMessage([item], new Map(), undefined, contexts).content;

	assert.equal(msg(blocked(4), ctx()), [head(4), ...answer, "The user can also type `/routr answer 4 N` here."].join("\n"), "routr's lines, then the front end's way in");
	assert.equal(msg(blocked(4), ctx({ fingerprint: undefined })), [head(4), ...answer].join("\n"), "no menu now: nothing to pick here");
	const fallback = "Run `routr wait 4` before you do anything about it: it returns at once and says who answers this question, and how. Until then, choose no option.";
	assert.equal(msg(blocked(4)), `${head(4)}\n${fallback}`, "the context could not be fetched");
	assert.equal(msg(blocked(4), ctx({ answer: [] })), `${head(4)}\n${fallback}`, "a routr older than answer");
	assert.doesNotMatch(msg(blocked(4), ctx()), /Answer only with/);
});

// answer = "jev": no wake while Jev decides, nor once it answered (the
// worker runs on); after a hand-over, what Jev made of it, then the text of
// whoever answers now, as routr wait gives it.
test("a question Jev decides wakes the model only once Jev hands it over", () => {
	const q = { harness: "claude", question: "Do you want to proceed?\n1. Yes\n2. No", fingerprint: "6a128b4273b95faa", answerer: "jev" };
	const note = "Jev did not answer it: it picked option 1 (Yes) with confidence 0.62, below min_confidence 0.8.";
	assert.equal(wakeKind(w(1, "blocked", q), "running"), undefined, "Jev decides");
	assert.equal(wakeKind(w(1, "blocked", { ...q, handedTo: "human", note }), "blocked"), "blocked");

	const t = new WakeTracker();
	const ctx = mineCtx([1, 2], { 1: "g", 2: "g" });
	t.update([w(1, "running", q), w(2, "running")], ctx);
	assert.deepEqual(t.update([w(1, "blocked", q), w(2, "done")], ctx), [], "Jev decides: 2's news waits for it, as for a running sibling");
	assert.deepEqual(t.update([w(1, "running", { ...q, question: undefined, fingerprint: undefined }), w(2, "done")], ctx), [], "Jev answered: it runs on");
	t.update([w(1, "blocked", q), w(2, "done")], ctx);
	const got = t.update([w(1, "blocked", { ...q, handedTo: "human", note }), w(2, "done")], ctx);
	assert.deepEqual(got.map((i) => [i.worker.id, i.kind]), [[1, "blocked"], [2, "done"]], "handed over: news");
	assert.deepEqual(t.update([w(1, "blocked", { ...q, handedTo: "human", note }), w(2, "done")], ctx), [], "once");
	// The next question, in the same words but about another command, recorded with no running state between.
	const next = { ...q, fingerprint: "0123456789abcdef", handedTo: "human", note };
	assert.deepEqual(t.update([w(1, "blocked", next), w(2, "done")], ctx).map((i) => i.worker.id), [1], "a new fingerprint is new news");

	// handed over: Jev's note leads routr's lines
	const item = { worker: w(3, "blocked", { ...q, asksAbout: "Bash: rm -rf build", handedTo: "human", note }), kind: "blocked" as const };
	const answer = [note, "The user answers this question, not you: do not choose an option yourself, even an obvious one."];
	const ctxs = new Map([[3, { answer, lasting: [] }]]);
	assert.equal(
		buildWakeMessage([item], new Map(), undefined, ctxs).content,
		["[routr] worker 3 · task · claude is blocked: Do you want to proceed?\n1. Yes\n2. No\nIt asks about Bash: rm -rf build", ...answer].join("\n"),
	);
});

test("a delivery that failed is held again, until newer news or maxTries", () => {
	const t = new WakeTracker();
	const ctx = mineCtx([1, 2]);
	t.update([w(1, "running"), w(2, "running")], ctx);
	const got = t.update([w(1, "done", { finished: 1 }), w(2, "done", { finished: 2 })], ctx);
	assert.equal(got.length, 2);
	assert.deepEqual(t.requeue(got).dropped, [], "nothing dropped on the first failure");
	assert.deepEqual(t.pending().map((i) => i.worker.id), [1, 2]);
	const again = t.update([w(1, "done", { finished: 1 }), w(2, "running")], ctx);
	assert.deepEqual(again.map((i) => i.worker.id), [1], "the next snapshot releases it; 2 is back at work, its news stale");
	assert.deepEqual(t.requeue(again).held.map((i) => i.worker.id), [1]);
	const third = t.update([w(1, "done", { finished: 1 }), w(2, "running")], ctx);
	assert.deepEqual(third.map((i) => i.worker.id), [1]);
	assert.deepEqual(t.requeue(third).dropped.map((i) => i.worker.id), [1], "the third failure gives up");
	assert.deepEqual(t.update([w(1, "done", { finished: 1 }), w(2, "running")], ctx), []);
	// Newer news came in while the delivery was in flight: the old item is not requeued.
	t.update([w(1, "running"), w(2, "running")], ctx);
	const first = t.update([w(1, "done", { finished: 5 }), w(2, "running")], ctx);
	t.update([w(1, "running"), w(2, "running")], ctx);
	assert.deepEqual(t.requeue(first), { held: [], dropped: [] });
	assert.deepEqual(t.pending(), []);
	// The model read the result (routr wait) while the delivery was in flight.
	const read = t.update([w(1, "done", { finished: 6 }), w(2, "running")], ctx);
	t.markSeen([1]);
	assert.deepEqual(t.requeue(read), { held: [], dropped: [] });
	assert.deepEqual(t.update([w(1, "done", { finished: 6 }), w(2, "running")], ctx), []);
});
