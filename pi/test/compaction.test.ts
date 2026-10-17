import { test } from "node:test";
import assert from "node:assert/strict";
import { due, summaryInstructions, truncation } from "../src/compaction.ts";
import type { MainAgent } from "../src/context.ts";
import type { Worker } from "../../frontend/core/index.ts";

// Hand-built session entries, typed loosely as in context.test.ts. Pi's
// estimate is a token per 4 characters, so `size` is in tokens.
type Entry = Record<string, unknown> & { id: string };
const cut = truncation as unknown as (branch: readonly Entry[], main: MainAgent, opts: { tokensBefore: number; workers: readonly Worker[] }) => any;
const instructions = summaryInstructions as unknown as (branch: readonly Entry[], workers: readonly Worker[]) => string;

const MAIN: MainAgent = { context: "truncate", at: 150_000, keep_turns: 3, keep_tokens: 40_000, strip_reasoning: true };

function session() {
	let n = 0;
	const branch: Entry[] = [];
	const add = (x: Record<string, unknown>): string => {
		const id = `e${++n}`;
		branch.push({ id, parentId: n > 1 ? `e${n - 1}` : null, timestamp: new Date(n * 1000).toISOString(), ...x });
		return id;
	};
	const pad = (text: string, size = 0) => text + " ".repeat(Math.max(0, size * 4 - text.length));
	const msg = (message: Record<string, unknown>) => add({ type: "message", message: { timestamp: n + 1, ...message } });
	return {
		branch,
		user: (text: string, size?: number) => msg({ role: "user", content: pad(text, size) }),
		wake: (text: string, size?: number) => add({ type: "custom_message", customType: "routr-wake", content: pad(text, size), display: true }),
		say: (text: string, size?: number) => msg({ role: "assistant", content: [{ type: "text", text: pad(text, size) }], stopReason: "stop" }),
		call: (id: string, size?: number) =>
			msg({ role: "assistant", content: [{ type: "toolCall", id, name: "bash", arguments: { command: pad("ls", size) } }], stopReason: "toolUse" }),
		result: (id: string, size?: number, toolName = "bash", details?: unknown) =>
			msg({ role: "toolResult", toolCallId: id, toolName, content: [{ type: "text", text: pad("ok", size) }], isError: false, details }),
		bash: (command: string, output: string) => msg({ role: "bashExecution", command, output, exitCode: 0, cancelled: false, truncated: false }),
		claims: () => add({ type: "custom", customType: "routr-claims", data: { ids: [1], chains: [], group: "run:1" } }),
		compaction: (firstKeptEntryId: string) => add({ type: "compaction", summary: "earlier", firstKeptEntryId, tokensBefore: 1 }),
	};
}

/** a turn: the incoming message, a tool call, and the answer */
function turn(s: ReturnType<typeof session>, text: string, size = 100, incoming: "user" | "wake" = "user"): string {
	const id = incoming === "user" ? s.user(text) : s.wake(text);
	s.call(`${id}c`, size);
	s.result(`${id}c`, size);
	s.say(`done with ${text}`);
	return id;
}

const run = (s: ReturnType<typeof session>, main: Partial<MainAgent> = {}, workers: Worker[] = []) =>
	cut(s.branch, { ...MAIN, ...main }, { tokensBefore: 160_000, workers });

test("due: past at after a turn, unless context is off or at is -1", () => {
	assert.equal(due(MAIN, 150_000), true);
	assert.equal(due(MAIN, 149_999), false);
	assert.equal(due(MAIN, null), false, "unknown right after a compaction");
	assert.equal(due(MAIN, undefined), false);
	assert.equal(due({ ...MAIN, context: "off" }, 900_000), false);
	assert.equal(due({ ...MAIN, at: -1 }, 900_000), false);
	assert.equal(due({ ...MAIN, context: "summarize" }, 150_000), true);
});

test("keeps the last keep_turns turns whole, cut where a turn starts", () => {
	const s = session();
	const starts = ["one", "two", "three", "four", "five"].map((t, i) => turn(s, t, 100, i === 3 ? "wake" : "user"));
	const r = run(s);
	assert.equal(r.firstKeptEntryId, starts[2]);
	assert.equal(r.tokensBefore, 160_000);
	assert.deepEqual(r.details, { by: "routr", context: "truncate", turns: 3, split: false });
});

test("keep_tokens caps the turns kept; -1 keep_turns keeps as many as fit", () => {
	const s = session();
	const starts = ["a", "b", "c", "d", "e", "f"].map((t) => turn(s, t, 5000)); // ~10k tokens a turn
	assert.equal(run(s, { keep_tokens: 25_000 }).firstKeptEntryId, starts[4], "two fit");
	assert.equal(run(s, { keep_turns: -1, keep_tokens: 45_000 }).firstKeptEntryId, starts[2], "four fit");
	assert.equal(run(s, { keep_turns: 2, keep_tokens: -1 }).firstKeptEntryId, starts[4]);
});

test("nothing to cut: undefined", () => {
	const s = session();
	turn(s, "one");
	turn(s, "two");
	assert.equal(run(s), undefined, "two turns, three kept");
	const t = session();
	turn(t, "only");
	assert.equal(run(t), undefined, "one turn, nothing before it");
});

test("a latest turn over keep_tokens keeps its last steps, cut at a model message, with its incoming message in the note", () => {
	const s = session();
	turn(s, "old");
	s.user("start the big refactor");
	const steps = [1, 2, 3, 4, 5].map((i) => {
		const c = s.call(`c${i}`, 8000);
		s.result(`c${i}`, 8000);
		return c;
	});
	s.say("all done");
	const r = run(s);
	assert.equal(r.firstKeptEntryId, steps[3], "the last two steps (~32k) fit in 40k; three don't");
	assert.equal(r.details.split, true);
	assert.equal(r.details.turns, 1);
	assert.match(r.summary, /The latest turn was too long to keep whole\. It began with this message, and its last steps follow:\n\nstart the big refactor/);
	// a single step over the cap is still kept: the cut never lands on a tool result
	const t = session();
	turn(t, "old");
	t.user("go");
	const big = t.call("x", 60_000);
	t.result("x", 60_000);
	assert.equal(run(t).firstKeptEntryId, big);
});

test("the note: the first message verbatim, what the user typed since, the todo list, the workers, where state lives", () => {
	const s = session();
	s.user("Read .design/prompts/overnight.md and carry it out.");
	s.call("t1");
	s.result("t1", 1, "todo", {
		action: "create",
		tasks: [
			{ id: 1, subject: "Phase 0: baseline", status: "completed", description: "run routr doctor" },
			{ id: 2, subject: "Phase 1: fix 5i5", status: "in_progress", description: "x".repeat(500), activeForm: "Fixing" },
			{ id: 3, subject: "gone", status: "deleted" },
		],
	});
	s.say("started");
	s.claims();
	s.wake("[routr] worker 7 done: the result text");
	s.say("ok");
	s.user("also look at the CI failure");
	s.say("will do");
	turn(s, "later");
	turn(s, "kept two");
	s.user("the user's kept message");
	s.say("fine");
	const workers: Worker[] = [
		{ id: 7, role: "task", title: "fix-5i5", harness: "codex", model: "gpt-6", state: "done", verdict: "pass", chain: "plan-1", step: "fix", callerSession: "S" },
		{ id: 9, role: "review", harness: "claude", state: "blocked", question: "Allow\nrm -rf build?" },
	];
	const r = run(s, {}, workers);
	const sum: string = r.summary;
	assert.match(sum, /^routr cut the earlier conversation to keep this session small/);
	assert.match(sum, /The session began with this message \(re-read any file it names\):\n\nRead \.design\/prompts\/overnight\.md and carry it out\.\n/);
	assert.match(sum, /What the user wrote since, oldest first:\n\n- also look at the CI failure\n\n/);
	assert.doesNotMatch(sum, /- later|kept message|kept two/, "the kept turns are there whole");
	assert.doesNotMatch(sum, /worker 7 done/, "a wake is not the user");
	assert.match(sum, /Your todo list as you last set it:\n\n#1 \[completed\] Phase 0: baseline\n#2 \[in_progress\] Phase 1: fix 5i5 — x{300}…\n\n/);
	assert.doesNotMatch(sum, /gone/);
	assert.match(sum, /routr workers of this session \(routr list for all, routr result ID for a result\):\n\n#7 fix-5i5 \(plan-1\/fix\) — done, verdict pass · codex gpt-6\n#9 review — blocked · claude · asks: Allow rm -rf build\?\n/);
	assert.match(sum, /beads \(bd list, bd show ID\)/);
	assert.match(sum, /The last 3 turns follow as they were\.$/);
	// the first message is not repeated in the list, and a long one is cut with a pointer to the session file
	const t = session();
	t.user("F".repeat(40_000));
	t.say("ok");
	turn(t, "a");
	turn(t, "b");
	turn(t, "c");
	const r2 = run(t);
	assert.match(r2.summary, /F{32000}… \[8000 more characters in the session file\]/);
	assert.match(r2.summary, /What the user wrote since, oldest first:\n\n- \(nothing\)/);
	assert.match(r2.summary, /routr workers of this session[^\n]*:\n\n- none active or changed in the last hour/);
	assert.doesNotMatch(r2.summary, /todo list/, "no todo tool used");
});

test("what the user wrote is capped, newest kept", () => {
	const s = session();
	s.user("first");
	s.say("ok");
	for (let i = 0; i < 40; i++) {
		s.user(`message ${i} ${"m".repeat(1000)}`);
		s.say("ok");
	}
	turn(s, "a");
	turn(s, "b");
	turn(s, "c");
	const sum: string = run(s).summary;
	assert.match(sum, /\[\d+ earlier messages are in the session file\]\n- message \d+/);
	assert.match(sum, /- message 39 /);
	assert.doesNotMatch(sum, /- message 0 /);
});

test("after an earlier compaction: turns count from its kept tail, and the note still starts from the first message", () => {
	const s = session();
	s.user("the original brief");
	s.say("ok");
	const keep = turn(s, "a");
	s.compaction(keep);
	const starts = ["b", "c", "d", "e"].map((t) => turn(s, t));
	const r = run(s);
	assert.equal(r.firstKeptEntryId, starts[1]);
	assert.match(r.summary, /began with this message \(re-read any file it names\):\n\nthe original brief/);
	assert.match(r.summary, /- a\n- b\n/);
	// nothing past the compaction's own tail to drop
	const t = session();
	t.user("brief");
	t.say("ok");
	const k = turn(t, "a");
	t.compaction(k);
	turn(t, "b");
	assert.equal(run(t), undefined);
});

test("the summary instructions name the workers and the todo list", () => {
	const s = session();
	s.user("go");
	s.call("t");
	s.result("t", 1, "todo", { tasks: [{ id: 4, subject: "ship it", status: "pending" }] });
	const text = instructions(s.branch, [{ id: 3, role: "task", harness: "pi", state: "running" }]);
	assert.match(text, /Quote the session's first user message word for word/);
	assert.match(text, /#3 task — running · pi/);
	assert.match(text, /#4 \[pending\] ship it/);
});

test("a split turn begun by a shell command the user ran names the command", () => {
	const s = session();
	turn(s, "old");
	s.bash("make bench", "ok");
	s.call("c1", 30_000);
	s.result("c1", 30_000);
	s.say("done");
	assert.match(run(s).summary, /It began with this message, and its last steps follow:\n\n\$ make bench\nok/);
});
