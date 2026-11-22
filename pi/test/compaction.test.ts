import { test } from "node:test";
import assert from "node:assert/strict";
import { atBudget, due, keptHead, piCompacts, summaryInstructions, TRIMMED, truncation } from "../src/compaction.ts";
import { HEAD_NOTE_START, markHeadEnd, type MainAgent } from "../src/context.ts";
import { buildSessionProjection, convertToLlm, estimateTokens } from "@earendil-works/pi-coding-agent";
import { stream as anthropic } from "@earendil-works/pi-ai/api/anthropic-messages";
// Pi's own, not exported: piCompacts must say what it says
import { prepareCompaction } from "../node_modules/@earendil-works/pi-coding-agent/dist/core/compaction/compaction.js";
import type { Worker } from "../../frontend/core/index.ts";

// Hand-built session entries, typed loosely as in context.test.ts. Pi's
// estimate is a token per 4 characters, so `size` is in tokens.
type Entry = Record<string, unknown> & { id: string };
const cut = truncation as unknown as (branch: readonly Entry[], main: MainAgent, opts: { tokensBefore: number; workers: readonly Worker[]; budget?: number }) => any;
const instructions = summaryInstructions as unknown as (branch: readonly Entry[], workers: readonly Worker[]) => string;
const kept = keptHead as unknown as (branch: readonly Entry[]) => any[] | undefined;
const budgetAt = atBudget as unknown as (branch: readonly Entry[], main: MainAgent, tokens: number | null | undefined) => number | undefined;
const compacts = piCompacts as unknown as (branch: readonly Entry[], keepRecentTokens: number) => boolean;
const piPrepares = (branch: readonly Entry[], keepRecentTokens: number): boolean =>
	(prepareCompaction as any)(branch, { enabled: true, reserveTokens: 16384, keepRecentTokens }) !== undefined;

// keep_head off: the tests of the note as it is without a head; HEAD turns it on
const MAIN: MainAgent = { context: "truncate", at: 150_000, keep_turns: 3, keep_tokens: 40_000, keep_head: false, head_tokens: 40_000, strip_reasoning: true };
const HEAD: Partial<MainAgent> = { keep_head: true };

/** Claude Opus 5.5 as Pi's model list has it: effort per message, system messages mid-conversation */
const OPUS = {
	id: "claude-opus-5-5",
	name: "Claude Opus 5.5",
	api: "anthropic-messages",
	provider: "anthropic",
	baseUrl: "https://api.anthropic.com",
	reasoning: true,
	input: ["text", "image"],
	cost: { input: 4, output: 20, cacheRead: 0.2, cacheWrite: 5 },
	contextWindow: 1_000_000,
	maxTokens: 128_000,
	compat: { supportsMidConvoEffort: true, supportsMidConvoSystemMessages: true, supportsMidConvoToolChanges: true, forceAdaptiveThinking: true, supportsTemperature: false, supportsStrictTools: true },
};
const OPUS_MSG = { role: "assistant", api: "anthropic-messages", provider: "anthropic", model: "claude-opus-5-5", providerThinkingLevel: "high" };
const BASH = { name: "bash", description: "Run a command", parameters: { type: "object", properties: { command: { type: "string" } }, required: ["command"] } };

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
		/** a truncation's result as Pi records it */
		applied: (r: any) => add({ type: "compaction", summary: r.summary, firstKeptEntryId: r.firstKeptEntryId, tokensBefore: r.tokensBefore, details: r.details, fromHook: true }),
		/** a step with thinking: the model's reasoning, then a tool call, as `api` records it */
		think: (id: string, size: number, api = "anthropic-messages") =>
			msg({ ...OPUS_MSG, api, content: [{ type: "thinking", thinking: pad("hmm", size), thinkingSignature: "sig" }, { type: "toolCall", id, name: "bash", arguments: { command: "cat x" } }], stopReason: "toolUse" }),
		/** the model's own message, as Claude wrote it */
		claude: (content: object[], stopReason = "stop") => msg({ ...OPUS_MSG, content, stopReason }),
		/** Pi's record of the prompt and tools, at session start */
		system: () => add({ type: "message", message: { role: "system", content: "You are the main agent.", toolsAdded: [BASH], timestamp: 0 } }),
		/** a message an extension sent at session start */
		custom: (text: string) => add({ type: "custom_message", customType: "intro", content: text, display: true }),
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

const run = (s: ReturnType<typeof session>, main: Partial<MainAgent> = {}, workers: Worker[] = [], budget?: number) =>
	cut(s.branch, { ...MAIN, ...main }, { tokensBefore: 160_000, workers, budget });

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

// ── the session's first turn, kept in front of the note ───────────────────

/** a first turn: the user's message, two reads and the answer; the ids of its results */
function firstTurn(s: ReturnType<typeof session>, sizes: number[] = [100, 100]): { user: string; results: string[]; end: string } {
	const user = s.user("Read .design/brief.md and carry it out.");
	const results = sizes.map((size, i) => {
		s.call(`r${i}`);
		return s.result(`r${i}`, size);
	});
	const end = s.say("read it; starting");
	return { user, results, end };
}

test("the head: none while the first turn goes on, then the first turn whole, with what came before its message", () => {
	const s = session();
	const intro = s.custom("session started in ~/repo");
	s.user("Read .design/brief.md and carry it out.");
	for (let i = 0; i < 6; i++) {
		s.call(`c${i}`);
		s.result(`c${i}`, 8000);
	}
	const WHOLE = { keep_head: true, head_tokens: -1 };
	const going = run(s, WHOLE);
	assert.equal(going.details.head, undefined, "the first turn has not ended: as without a head");
	assert.equal(going.details.split, true);
	assert.match(going.summary, /^routr cut the earlier conversation/);
	const end = s.say("done reading");
	const starts = ["a", "b", "c", "d"].map((t, i) => turn(s, t, 100, i === 0 ? "wake" : "user"));
	const r = run(s, WHOLE);
	assert.equal(r.details.head.endId, end, "through the answer, up to the wake that came next");
	assert.equal(r.firstKeptEntryId, starts[1]);
	assert.deepEqual(r.details.head.trimmed, []);
	assert.deepEqual(r.details.head.thinkingDropped, []);
	assert.ok(r.details.head.tokens > 48_000 && r.details.head.tokens <= 48_100, String(r.details.head.tokens));
	s.applied(r);
	const msgs = kept(s.branch)!;
	assert.deepEqual(msgs[0], { role: "custom", customType: "intro", content: "session started in ~/repo", display: true, details: undefined, timestamp: 1000 }, "the message before the first user message, as Pi projects it");
	assert.equal(intro, "e1");
	assert.deepEqual(msgs.map((m) => m.role), ["custom", "user", ...Array(6).fill(["assistant", "toolResult"]).flat(), "assistant"]);
	assert.equal(msgs[1].content, "Read .design/brief.md and carry it out.");
});

test("the note with a head: the first turn is above, so no first message, no files it names; what the user wrote since still", () => {
	const s = session();
	firstTurn(s);
	s.user("also look at the CI failure");
	s.say("will do");
	["a", "b", "c"].forEach((t) => turn(s, t));
	const sum: string = run(s, HEAD).summary;
	assert.equal(
		sum.split("\n\n")[0],
		`${HEAD_NOTE_START} to keep this session small, and this note stands in for it. The first turn is kept above as it was. The session file still has all of it.`,
	);
	assert.ok(sum.startsWith("routr cut the conversation between the session's first turn and its last turns to keep"));
	assert.doesNotMatch(sum, /The session began with this message|brief\.md|re-read any file/, "the first message is in the head, not repeated");
	assert.match(sum, /What the user wrote since, oldest first:\n\n- also look at the CI failure\n\n/);
	assert.match(sum, /Your work's state lives outside this conversation: issues in beads \(bd list, bd show ID\), code in git/);
	assert.match(sum, /The last 3 turns follow as they were\.$/);
});

test("the head is taken once: later cuts keep it as it was, whatever [main] says by then", () => {
	const s = session();
	firstTurn(s, [30_000, 20_000]);
	["a", "b", "c", "d"].forEach((t) => turn(s, t));
	const r1 = run(s, { keep_head: true, head_tokens: 40_000 });
	assert.equal(r1.details.head.trimmed.length, 1);
	s.applied(r1);
	const before = kept(s.branch);
	["e", "f", "g"].forEach((t) => turn(s, t));
	const r2 = run(s, { keep_head: false, head_tokens: -1 });
	assert.deepEqual(r2.details.head, r1.details.head, "copied, though [main] keeps no head now");
	assert.match(r2.summary, /^routr cut the conversation between the session's first turn/);
	s.applied(r2);
	assert.deepEqual(kept(s.branch), before, "the same messages, so the same cached prefix");
	turn(s, "h");
	assert.deepEqual(run(s, HEAD).details.head, r1.details.head);
	// a first cut that kept no head: none later either
	const t = session();
	firstTurn(t);
	["a", "b", "c", "d"].forEach((x) => turn(t, x));
	const n1 = run(t);
	assert.equal(n1.details.head, undefined);
	t.applied(n1);
	["e", "f"].forEach((x) => turn(t, x));
	assert.equal(run(t, HEAD).details.head, undefined);
	assert.equal(kept(t.branch), undefined);
});

test("a head over head_tokens loses the thinking that can go first, then its largest results; never the user's message, the text or the tool calls", () => {
	const s = session();
	s.user("go");
	const t1 = s.think("t1", 6000);
	const r1 = s.result("t1", 3000);
	const t2 = s.think("t2", 6000, "openai-responses");
	const r2 = s.result("t2", 2000);
	const t3 = s.think("t3", 6000, "openai-completions");
	const r3 = s.result("t3", 1000);
	s.say("done");
	["a", "b", "c", "d"].forEach((t) => turn(s, t));
	// ~24k: dropping the Anthropic and Chat Completions thinking (12k) is enough for 15k
	const a = run(s, { keep_head: true, head_tokens: 15_000 }).details.head;
	assert.deepEqual(a.thinkingDropped, [t1, t3], "a Responses API pairs tool calls with their reasoning: it stays");
	assert.deepEqual(a.trimmed, []);
	assert.ok(a.tokens <= 15_000);
	// 8k: the results too, largest first, until it fits
	const b = run(s, { keep_head: true, head_tokens: 9_000 }).details.head;
	assert.deepEqual(b.thinkingDropped, [t1, t3]);
	assert.deepEqual(b.trimmed, [r1, r2]);
	assert.ok(b.tokens <= 9_000, String(b.tokens));
	const c = session();
	c.branch.push(...structuredClone(s.branch));
	const r = run(c, { keep_head: true, head_tokens: 9_000 });
	assert.match(r.summary, /The first turn is kept above as it was, except 2 tool results, trimmed to keep it small: re-read what they showed if you need it\./);
	c.applied(r);
	const msgs = kept(c.branch)!;
	const byCall = (id: string) => msgs.find((m) => m.role === "toolResult" && m.toolCallId === id);
	assert.deepEqual(byCall("t1").content, [{ type: "text", text: TRIMMED }]);
	assert.deepEqual(byCall("t2").content, [{ type: "text", text: TRIMMED }]);
	assert.equal(byCall("t3").content[0].text.length, 4000, "the smallest fit");
	const calls = msgs.filter((m) => m.role === "assistant");
	assert.deepEqual(calls[0].content.map((x: any) => x.type), ["toolCall"]);
	assert.deepEqual(calls[1].content.map((x: any) => x.type), ["thinking", "toolCall"]);
	assert.deepEqual(calls[2].content.map((x: any) => x.type), ["toolCall"]);
	assert.deepEqual(calls[3].content, [{ type: "text", text: "done" }]);
	assert.equal(msgs[0].content, "go");
	// one result trimmed: said in the singular
	const d = session();
	d.branch.push(...structuredClone(s.branch));
	assert.match(run(d, { keep_head: true, head_tokens: 11_000 }).summary, /except one tool result, trimmed to keep it small: re-read what it showed if you need it\./);
	// the session file is untouched
	assert.equal((s.branch.find((e) => e.id === r1) as any).message.content[0].text.length, 12_000);
});

test("a head that won't fit even trimmed is no head: the note has the first message, as without one; -1 never shrinks", () => {
	const s = session();
	s.user(`brief: ${"B".repeat(50_000 * 4)}`); // 50k tokens of the user's own
	s.call("x");
	s.result("x", 1000);
	s.say("ok");
	["a", "b", "c", "d"].forEach((t) => turn(s, t));
	const r = run(s, HEAD);
	assert.equal(r.details.head, undefined);
	assert.match(r.summary, /^routr cut the earlier conversation/);
	assert.match(r.summary, /The session began with this message \(re-read any file it names\):\n\nbrief: B+…/);
	const all = run(s, { keep_head: true, head_tokens: -1 });
	assert.deepEqual(all.details.head.trimmed, []);
	assert.ok(all.details.head.tokens > 50_000);
	assert.equal(all.details.head.max, -1);
});

test("the cut after a head: the turns kept come after it, and with nothing between them there is nothing to cut", () => {
	const s = session();
	const { end } = firstTurn(s);
	const starts = ["a", "b", "c"].map((t) => turn(s, t));
	assert.equal(run(s, HEAD), undefined, "the head and the 3 turns kept: nothing between");
	assert.equal(run(s).firstKeptEntryId, starts[0], "without a head the first turn goes");
	const more = turn(s, "d");
	const r = run(s, HEAD);
	assert.equal(r.firstKeptEntryId, starts[1]);
	assert.equal(r.details.head.endId, end);
	assert.ok(more);
});

// ── a cut held to a budget: a fallback's, or Pi's after an overflow ─────────

/** Pi's estimate of what the next request sends of branch: the head kept in front, the note, the turns kept */
const sent = (branch: readonly Entry[]): number =>
	[...(kept(branch) ?? []), ...buildSessionProjection(branch as any).messages.filter((m: any) => m.role !== "system")].reduce((n: number, m: any) => n + estimateTokens(m), 0);

test("a budget the cut fits as [main] says changes nothing: a saved head stays as it was", () => {
	const s = session();
	firstTurn(s, [3000, 3000]);
	["a", "b", "c", "d"].forEach((t) => turn(s, t));
	const r1 = run(s, HEAD);
	assert.deepEqual(run(s, HEAD, [], 100_000), r1);
	s.applied(r1);
	const before = kept(s.branch);
	["e", "f"].forEach((t) => turn(s, t));
	const r2 = run(s, HEAD);
	assert.deepEqual(r2.details.head, r1.details.head, "unforced: copied");
	assert.deepEqual(run(s, HEAD, [], 100_000), r2);
	s.applied(r2);
	assert.deepEqual(kept(s.branch), before, "the same messages, so the same cached prefix");
});

test("a budget smaller than the saved head: the head shrunk to half of it, the turns kept to the rest, all of it within", () => {
	const s = session();
	firstTurn(s, [3000, 3000]); // ~6k, as a reviewer's probe had it
	["a", "b", "c", "d"].forEach((t) => turn(s, t, 1000));
	const r1 = run(s, HEAD);
	assert.ok(r1.details.head.tokens > 6000);
	s.applied(r1);
	["e", "f", "g"].forEach((t) => turn(s, t, 1000));
	const copy = () => {
		const c = session();
		c.branch.push(...structuredClone(s.branch));
		return c;
	};
	const w = copy();
	w.applied(run(w, HEAD));
	assert.ok(sent(w.branch) > 5000, "as [main] says it would not fit");
	// the probe: 1000 tokens, the 6k first turn and all
	const p = copy();
	const tiny = run(p, HEAD, [], 1000);
	assert.ok(tiny.details.head.tokens <= 500);
	p.applied(tiny);
	assert.ok(sent(p.branch) <= 1000, String(sent(p.branch)));
	const r = run(s, HEAD, [], 5000);
	assert.equal(r.details.head.endId, r1.details.head.endId, "the same first turn");
	assert.equal(r.details.head.max, 2500);
	assert.equal(r.details.head.trimmed.length, 2);
	assert.ok(r.details.head.tokens <= 2500, String(r.details.head.tokens));
	assert.match(r.summary, /The first turn is kept above as it was, except 2 tool results, trimmed/);
	s.applied(r);
	assert.ok(sent(s.branch) <= 5000, String(sent(s.branch)));
	assert.equal(kept(s.branch)!.filter((m) => m.role === "toolResult" && m.content[0].text === TRIMMED).length, 2, "the request has the head as shrunk");
	// the shrunk head is the one later cuts copy
	["h", "i"].forEach((t) => turn(s, t, 1000));
	assert.deepEqual(run(s, HEAD).details.head, r.details.head);
});

test("a budget the head won't shrink to half of: no head, the note quotes the first message, and later cuts keep none", () => {
	const s = session();
	s.user(`brief: ${"B".repeat(2000 * 4)}`); // 2k tokens of the user's own
	s.call("x");
	s.result("x", 1000);
	s.say("ok");
	["a", "b", "c", "d"].forEach((t) => turn(s, t));
	const r1 = run(s, HEAD);
	assert.ok(r1.details.head);
	s.applied(r1);
	["e", "f", "g"].forEach((t) => turn(s, t));
	const r = run(s, HEAD, [], 3000);
	assert.equal(r.details.head, undefined);
	assert.match(r.summary, /^routr cut the earlier conversation/);
	assert.match(r.summary, /The session began with this message \(re-read any file it names\):\n\nbrief: B+\n/);
	s.applied(r);
	assert.equal(kept(s.branch), undefined);
	assert.ok(sent(s.branch) <= 3000, String(sent(s.branch)));
	turn(s, "h");
	assert.equal(run(s, HEAD).details.head, undefined, "the old head does not come back");
});

test("a budget at the first cut: the head taken shrunk to half of it, else none; what is sent is within", () => {
	const s = session();
	firstTurn(s, [3000, 3000]);
	["a", "b", "c", "d"].forEach((t) => turn(s, t, 1000));
	const r = run(s, { keep_head: true, head_tokens: -1 }, [], 6000);
	assert.equal(r.details.head.max, 3000);
	assert.ok(r.details.head.tokens <= 3000);
	s.applied(r);
	assert.ok(sent(s.branch) <= 6000, String(sent(s.branch)));
	// the turns fit beside the head shrunk, nothing between them: the head goes so there is something to cut
	const t = session();
	firstTurn(t, [3000, 3000]);
	["a", "b", "c"].forEach((x) => turn(t, x));
	assert.equal(run(t, HEAD), undefined);
	const n = run(t, HEAD, [], 5000);
	assert.equal(n.details.head, undefined);
	assert.equal(n.details.turns, 3);
	assert.match(n.summary, /The session began with this message/);
	t.applied(n);
	assert.ok(sent(t.branch) <= 5000, String(sent(t.branch)));
	// and none is needed when what is sent is within already
	const u = session();
	u.branch.push(...structuredClone(t.branch.slice(0, -1)));
	assert.equal(run(u, HEAD, [], 7000), undefined);
});

// ── a cut held under [main] at, in the provider's tokens ────────────────────

/** Pi's estimate of the whole request: the system prompt too */
const wholeRequest = (branch: readonly Entry[]): number => {
	const system = buildSessionProjection(branch as any).messages.filter((m: any) => m.role === "system").at(-1);
	return sent(branch) + (system ? estimateTokens(system as any) : 0);
};

test("atBudget: at's room in Pi's estimate, scaled by how far the provider's count is over it, less the system prompt", () => {
	const s = session();
	s.system();
	firstTurn(s, [1000, 1000]);
	["a", "b", "c", "d"].forEach((t) => turn(s, t, 1000));
	const est = wholeRequest(s.branch);
	const system = wholeRequest(s.branch) - sent(s.branch);
	assert.ok(system > 0);
	const at = { ...MAIN, at: 10_000 };
	assert.equal(budgetAt(s.branch, at, est), 7000 - system, "counted as estimated: 70% of at");
	assert.equal(budgetAt(s.branch, at, est * 2), 3500 - system, "counted twice the estimate: half of that");
	assert.equal(budgetAt(s.branch, at, Math.floor(est / 2)), 7000 - system, "under the estimate: never looser");
	assert.equal(budgetAt(s.branch, at, est * 100), 1750 - system, "at most 4 to 1");
	assert.equal(budgetAt(s.branch, { ...MAIN, at: -1 }, est), undefined);
	assert.equal(budgetAt(s.branch, at, null), undefined);
	assert.equal(budgetAt(s.branch, { ...MAIN, at: 10 }, est), 0, "never below nothing");
	// after a cut the projection has no system message; every request still sends it
	s.applied(run(s, { ...HEAD, keep_turns: 1 }));
	const after = sent(s.branch) + system;
	assert.equal(budgetAt(s.branch, at, after), 7000 - system, "the system prompt still counted");
	assert.equal(budgetAt(s.branch, at, after * 2), 3500 - system);
});

test("the cut [main] asks for, when the provider counts twice Pi's estimate, would leave the request over at: cut to fit instead, and the next turn is under at", () => {
	// as on 2026-10-18: at 90k, the head and the turns keep_tokens allows about all there was, counted 2.2 times over
	const main: MainAgent = { ...MAIN, ...HEAD, at: 9000, keep_turns: -1, keep_tokens: 3000, head_tokens: 4000 };
	const build = () => {
		const s = session();
		s.system();
		firstTurn(s, [900, 900]);
		for (let i = 0; i < 12; i++) turn(s, `wake ${i}`, 120, "wake");
		return s;
	};
	// the system prompt goes with every request, whatever the cut kept
	const system = estimateTokens(build().branch[0]!.message as any);
	const real = (branch: readonly Entry[]): number => Math.round((sent(branch) + system) * 2.2);
	const asSaid = build();
	assert.ok(real(asSaid.branch) >= 9000, String(real(asSaid.branch)));
	asSaid.applied(cut(asSaid.branch, main, { tokensBefore: real(asSaid.branch), workers: [] }));
	assert.ok(real(asSaid.branch) > 9000, `as [main] says: ${real(asSaid.branch)}, still over at, so cut again every turn`);
	const s = build();
	const tokens = real(s.branch);
	s.applied(cut(s.branch, main, { tokensBefore: tokens, workers: [], budget: budgetAt(s.branch, main, tokens) }));
	assert.ok(real(s.branch) <= 9000 * 0.7, `fitted: ${real(s.branch)}`);
	turn(s, "next", 120, "wake");
	assert.ok(real(s.branch) < 9000, `the turn after it: ${real(s.branch)}`);
});

test("piCompacts says what Pi's prepareCompaction says: nothing until the conversation since the last cut is past keepRecentTokens", () => {
	const s = session();
	firstTurn(s, [3000, 3000]);
	const cases: [string, number][] = [];
	const check = (what: string, keep: number) => {
		assert.equal(compacts(s.branch, keep), piPrepares(s.branch, keep), `${what}, keepRecentTokens ${keep}`);
		cases.push([what, keep]);
	};
	for (const keep of [0, 100, 5_000, 6_100, 20_000]) check("one turn", keep);
	assert.equal(compacts(s.branch, 20_000), false);
	assert.equal(compacts(s.branch, 100), true);
	["a", "b", "c", "d"].forEach((t) => turn(s, t, 3000));
	for (const keep of [0, 100, 6_000, 20_000, 30_000, 40_000]) check("five turns", keep);
	assert.equal(compacts(s.branch, 20_000), true);
	const r = run(s, HEAD);
	s.applied(r);
	for (const keep of [0, 100, 20_000]) check("just cut", keep);
	assert.equal(compacts(s.branch, 0), false, "Already compacted");
	turn(s, "e", 3000);
	for (const keep of [0, 100, 6_000, 20_000, 30_000]) check("after the cut", keep);
	assert.equal(compacts(s.branch, 30_000), false, "the head in front of the note does not count, only the turns kept and since (~24k)");
	["f", "g", "h"].forEach((t) => turn(s, t, 3000));
	for (const keep of [6_000, 20_000, 30_000]) check("grown since", keep);
	assert.equal(compacts(s.branch, 20_000), true);
	s.user("split me");
	for (let i = 0; i < 4; i++) {
		s.call(`s${i}`, 4000);
		s.result(`s${i}`, 4000);
	}
	for (const keep of [5_000, 20_000, 60_000]) check("a long turn", keep);
});

// ── the request: Pi's projection, the head spliced in, Pi's Anthropic converter ──

/**
 * What Pi sends Anthropic for branch now: its projection (the context hook
 * sees it without system messages, and Pi puts the current one first
 * again), the head in front, convertToLlm, and the converter's payload, as
 * before_provider_request gets it. The cache retention is set here, as
 * Pi's settings would, so the caller's PI_CACHE_RETENTION doesn't count.
 */
async function request(branch: readonly Entry[], { apiKey = "sk-ant-api03-test", cacheRetention = "short" as "short" | "long" } = {}): Promise<any> {
	const projected = buildSessionProjection(branch as any).messages;
	const visible = projected.filter((m: any) => m.role !== "system");
	assert.equal(visible[0]?.role, "compactionSummary", "after a cut the context starts with the note");
	// Pi's compaction entry carries the prompt and tools (systemMessage); these hand-made ones don't
	const system = (branch.find((e: any) => e.message?.role === "system") as any).message;
	const messages = [system, ...convertToLlm([...(kept(branch) ?? []), ...visible] as any)];
	let payload: any;
	const s = anthropic(OPUS as any, { messages } as any, {
		apiKey,
		cacheRetention,
		onPayload: (p: unknown) => {
			payload = p;
			throw new Error("captured: no request goes out");
		},
	});
	await s.result();
	assert.ok(payload, "the converter built a payload");
	return payload;
}

/** where a payload's breakpoints are: "system.0", "tools.0", "messages.4.1" */
function marks(payload: any): string[] {
	const at: string[] = [];
	for (const key of ["system", "tools"]) (payload[key] ?? []).forEach((b: any, i: number) => b.cache_control && at.push(`${key}.${i}`));
	payload.messages.forEach((m: any, i: number) => Array.isArray(m.content) && m.content.forEach((b: any, j: number) => b.cache_control && at.push(`messages.${i}.${j}`)));
	return at;
}

/** a session whose first turn ends with `last`, cut once */
function cutSession(last: (s: ReturnType<typeof session>) => void) {
	const s = session();
	s.system();
	s.user("Read .design/brief.md and carry it out.");
	s.claude([{ type: "thinking", thinking: "two files", thinkingSignature: "sig1" }, { type: "toolCall", id: "a", name: "bash", arguments: { command: "cat brief.md" } }, { type: "toolCall", id: "b", name: "bash", arguments: { command: "cat rules.md" } }], "toolUse");
	s.result("a", 50);
	s.result("b", 50);
	last(s);
	["one", "two", "three", "four"].forEach((t, i) => turn(s, t, 50, i === 0 ? "wake" : "user"));
	const r = run(s, HEAD);
	assert.ok(r.details.head, "a head");
	s.applied(r);
	turn(s, "five", 50, "wake");
	s.wake("[routr] worker 9 is done"); // the request is for the turn this starts
	return s;
}

test("the request after a cut: a breakpoint on the head's last block, before the note, besides Pi's own three", async () => {
	const s = cutSession((s) => s.claude([{ type: "thinking", thinking: "done", thinkingSignature: "sig2" }, { type: "text", text: "read them; starting" }]));
	const payload = await request(s.branch);
	const before = marks(payload);
	assert.equal(before.length, 3, `system, tools, last message: ${before}`);
	const out: any = markHeadEnd(payload);
	assert.ok(out);
	const added = marks(out).filter((m) => !before.includes(m));
	assert.equal(added.length, 1);
	const [, i, j] = added[0]!.split(".").map(Number);
	const block = out.messages[i!].content[j!];
	assert.deepEqual(block, { type: "text", text: "read them; starting", cache_control: { type: "ephemeral" } });
	const next = out.messages.slice(i! + 1).find((m: any) => m.role !== "system");
	assert.match(next.content[0].text, /<summary>\nroutr cut the conversation between the session's first turn and its last turns/, "the note comes next");
	assert.equal(marks(out).length, 4);
	assert.equal(payload.messages[i!].content[j!].cache_control, undefined, "Pi's payload is not changed in place");
	assert.equal(markHeadEnd(out), undefined, "marked already");
	// the head is the same on the next request, so the same prefix is cached
	s.say("on it");
	s.user("and the next");
	const again: any = markHeadEnd(await request(s.branch));
	assert.deepEqual(again.messages.slice(0, i! + 1), out.messages.slice(0, i! + 1));
});

test("the request after a cut: the breakpoint added has Pi's ttl, an hour with cacheRetention long", async () => {
	const s = cutSession((s) => s.claude([{ type: "text", text: "read them; starting" }]));
	for (const [retention, want] of [
		["short", { type: "ephemeral" }],
		["long", { type: "ephemeral", ttl: "1h" }],
	] as const) {
		const out: any = markHeadEnd(await request(s.branch, { cacheRetention: retention }));
		const at = marks(out);
		assert.equal(at.length, 4, retention);
		for (const m of at) {
			const [key, i, j] = m.split(".");
			const block = key === "messages" ? out.messages[Number(i)].content[Number(j)] : out[key!][Number(i)];
			assert.deepEqual(block.cache_control, want, `${retention}: ${m}`);
		}
	}
});

test("the request after a cut: a head that ends in its tool results (the turn was stopped) marks the last of them, which Pi joins in one user message", async () => {
	const s = cutSession((s) => s.claude([{ type: "thinking", thinking: "half a tho", thinkingSignature: "" }], "aborted"));
	const out: any = markHeadEnd(await request(s.branch));
	const at = marks(out).filter((m) => m.startsWith("messages."));
	const [, i, j] = at[0]!.split(".").map(Number);
	const m = out.messages[i!];
	assert.equal(m.role, "user");
	assert.deepEqual(m.content.map((b: any) => b.type), ["tool_result", "tool_result"], "both results in one message");
	assert.equal(j, 1);
	assert.equal(m.content[1].tool_use_id, "b");
});

test("the request after a cut: an OAuth login has Pi's four breakpoints already, so none is added", async () => {
	const s = cutSession((s) => s.claude([{ type: "text", text: "starting" }]));
	const payload = await request(s.branch, { apiKey: "sk-ant-oat01-test" });
	assert.equal(marks(payload).length, 4);
	assert.equal(markHeadEnd(payload), undefined);
});
