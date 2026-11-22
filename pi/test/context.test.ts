import { test } from "node:test";
import assert from "node:assert/strict";
import { HEAD_NOTE_START, mainFromEnv, markHeadEnd, parseMain, stripOldReasoning } from "../src/context.ts";

// hand-built messages, typed loosely as in extension.test.ts
const strip = stripOldReasoning as (messages: readonly object[]) => any[] | undefined;

const think = (t: string) => ({ type: "thinking", thinking: t, thinkingSignature: "reasoning_content" });
const text = (t: string) => ({ type: "text", text: t });
const call = (id: string) => ({ type: "toolCall", id, name: "bash", arguments: {} });
const user = (t: string) => ({ role: "user", content: t, timestamp: 0 });
const wake = (t: string) => ({ role: "custom", customType: "routr-wake", content: t, display: true, timestamp: 0 });
const asst = (...content: object[]) => ({ role: "assistant", content, stopReason: "toolUse", timestamp: 0 });
const result = (id: string) => ({ role: "toolResult", toolCallId: id, toolName: "bash", content: [text("ok")], isError: false, timestamp: 0 });

test("reasoning before the latest incoming message goes; the current turn keeps its own", () => {
	const messages = [
		user("start the run"),
		asst(think("plan A"), call("1")),
		result("1"),
		asst(think("done for now"), text("started worker 7")),
		wake("[routr] worker 7 is done"),
		asst(think("read the result"), call("2")),
		result("2"),
	];
	const out = strip(messages);
	assert.ok(out);
	assert.deepEqual(out[1], asst(call("1")));
	assert.deepEqual(out[3], asst(text("started worker 7")));
	assert.deepEqual(out[5], messages[5], "the turn in progress: DeepSeek needs its reasoning for the tool loop");
	assert.equal(out.length, messages.length);
});

test("the input is not changed", () => {
	const messages = [user("a"), asst(think("x"), text("y")), user("b")];
	const before = structuredClone(messages);
	strip(messages);
	assert.deepEqual(messages, before);
});

test("an earlier assistant message that is only reasoning is left whole: emptied, it would be dropped and two user messages would meet", () => {
	const messages = [user("a"), asst(think("aborted mid-thought")), user("b")];
	assert.equal(strip(messages), undefined);
});

test("nothing to strip returns undefined, so the request is sent as Pi built it", () => {
	assert.equal(strip([]), undefined);
	assert.equal(strip([user("a"), asst(think("x"), text("y"))]), undefined, "one turn, in progress");
	assert.equal(strip([user("a"), asst(text("y")), user("b")]), undefined, "no reasoning anywhere");
});

test("any message that is not the model's own starts a turn: a compaction summary, your ! command", () => {
	const summary = { role: "compactionSummary", summary: "s", tokensBefore: 1, timestamp: 0 };
	const bash = { role: "bashExecution", command: "ls", output: "", exitCode: 0, cancelled: false, truncated: false, timestamp: 0 };
	for (const incoming of [summary, bash]) {
		const out = strip([user("a"), asst(think("x"), text("y")), incoming]);
		assert.deepEqual(out?.[1], asst(text("y")), incoming.role);
	}
});

test("without routr main (an older routr), every ROUTR_MAIN_* override still applies; one it can't read is the default", () => {
	const DEF = { context: "truncate", at: 150_000, keep_turns: 3, keep_tokens: 40_000, keep_head: true, head_tokens: 40_000, strip_reasoning: true };
	assert.deepEqual(mainFromEnv({}), DEF);
	assert.deepEqual(
		mainFromEnv({ ROUTR_MAIN_CONTEXT: "off", ROUTR_MAIN_AT: "-1", ROUTR_MAIN_KEEP_TURNS: "5", ROUTR_MAIN_KEEP_TOKENS: "0", ROUTR_MAIN_STRIP_REASONING: "0" }),
		{ ...DEF, context: "off", at: -1, keep_turns: 5, strip_reasoning: false },
	);
	assert.deepEqual(mainFromEnv({ ROUTR_MAIN_AT: "+90000" }), { ...DEF, at: 90_000 });
	assert.deepEqual(mainFromEnv({ ROUTR_MAIN_CONTEXT: "compact", ROUTR_MAIN_AT: "150k", ROUTR_MAIN_KEEP_TURNS: "-2", ROUTR_MAIN_KEEP_TOKENS: "+0" }), DEF);
	assert.deepEqual(mainFromEnv({ ROUTR_MAIN_KEEP_HEAD: "0", ROUTR_MAIN_HEAD_TOKENS: "-1" }), { ...DEF, keep_head: false, head_tokens: -1 });
	assert.deepEqual(mainFromEnv({ ROUTR_MAIN_KEEP_HEAD: "Off", ROUTR_MAIN_HEAD_TOKENS: "8000" }), { ...DEF, keep_head: false, head_tokens: 8000 });
	assert.deepEqual(mainFromEnv({ ROUTR_MAIN_KEEP_HEAD: "1", ROUTR_MAIN_HEAD_TOKENS: "0" }), DEF);
});

test("routr main --json: keep_head and head_tokens as routr prints them; a routr from before them gives their defaults", () => {
	const old = { context: "truncate", at: 150_000, keep_turns: 3, keep_tokens: 40_000, strip_reasoning: true };
	assert.deepEqual(parseMain(JSON.stringify(old)), { ...old, keep_head: true, head_tokens: 40_000 });
	assert.deepEqual(parseMain(JSON.stringify({ ...old, keep_head: false, head_tokens: -1 })), { ...old, keep_head: false, head_tokens: -1 });
	assert.deepEqual(parseMain(JSON.stringify({ ...old, keep_head: true, head_tokens: 9000 })), { ...old, keep_head: true, head_tokens: 9000 });
	assert.equal(parseMain(JSON.stringify({ ...old, keep_head: "yes" })), undefined);
	assert.equal(parseMain(JSON.stringify({ ...old, head_tokens: -5 })), undefined);
	assert.equal(parseMain(JSON.stringify({ ...old, head_tokens: 1.5 })), undefined);
});

// ── the head's cache breakpoint, on Anthropic payloads as Pi's converter builds them ──

const EPH = { type: "ephemeral" };
const NOTE = `The conversation history before this point was compacted into the following summary:\n\n<summary>\n${HEAD_NOTE_START} to keep this session small, and this note stands in for it.\n</summary>`;
/** a request whose messages are these, then the note, then the latest incoming message, with Pi's three breakpoints */
const payload = (head: object[], cache: object = EPH) => ({
	model: "claude-opus-5-5",
	system: [{ type: "text", text: "You are the main agent.", cache_control: cache }],
	tools: [{ name: "bash", input_schema: {}, cache_control: cache }],
	messages: [...head, { role: "user", content: [{ type: "text", text: NOTE }] }, { role: "assistant", content: [{ type: "text", text: "ok" }] }, { role: "user", content: [{ type: "text", text: "[routr] worker 9 is done", cache_control: cache }] }],
});
const mark = markHeadEnd as (p: unknown) => any;
const goUser = { role: "user", content: "go" };
const thinking = { type: "thinking", thinking: "x", signature: "s" };

test("the head's breakpoint skips thinking: the text before it, or the message before one that is only thinking", () => {
	const textFirst = mark(payload([goUser, { role: "assistant", content: [{ type: "text", text: "a" }, thinking] }]));
	assert.deepEqual(textFirst.messages[1].content, [{ type: "text", text: "a", cache_control: EPH }, thinking]);
	const onlyThinking = mark(
		payload([goUser, { role: "assistant", content: [{ type: "tool_use", id: "t", name: "bash", input: {} }] }, { role: "user", content: [{ type: "tool_result", tool_use_id: "t", content: "ok" }] }, { role: "assistant", content: [{ type: "redacted_thinking", data: "z" }] }]),
	);
	assert.deepEqual(onlyThinking.messages[2].content, [{ type: "tool_result", tool_use_id: "t", content: "ok", cache_control: EPH }]);
	assert.deepEqual(onlyThinking.messages[3].content, [{ type: "redacted_thinking", data: "z" }]);
});

test("the head's breakpoint: a message in plain text becomes a block, an effort marker in between is passed over, a join in one message is followed", () => {
	const plain = mark(payload([{ role: "user", content: "the brief, all of it" }]));
	assert.deepEqual(plain.messages[0].content, [{ type: "text", text: "the brief, all of it", cache_control: EPH }]);
	const effort = mark(payload([goUser, { role: "assistant", content: [{ type: "text", text: "a" }] }, { role: "system", content: [], output_config: { effort: "high" } }]));
	assert.deepEqual(effort.messages[1].content, [{ type: "text", text: "a", cache_control: EPH }]);
	// a converter that joins the results and the note in one user message
	const joined = mark({
		system: [{ type: "text", text: "s", cache_control: EPH }],
		messages: [goUser, { role: "user", content: [{ type: "tool_result", tool_use_id: "a", content: "1" }, { type: "tool_result", tool_use_id: "b", content: "2" }, { type: "text", text: NOTE }] }],
	});
	assert.deepEqual(joined.messages[1].content[1], { type: "tool_result", tool_use_id: "b", content: "2", cache_control: EPH });
	assert.equal(joined.messages[1].content[2].cache_control, undefined);
});

test("the head's breakpoint is Pi's own, ttl and all: Anthropic refuses a shorter one before a longer", () => {
	const hour = { type: "ephemeral", ttl: "1h" };
	const out = mark(payload([goUser, { role: "assistant", content: [{ type: "text", text: "a" }] }], hour));
	assert.deepEqual(out.messages[1].content[0].cache_control, hour);
});

test("no breakpoint without a kept head, without Pi's own (caching off), or with 4 already", () => {
	const head = [goUser, { role: "assistant", content: [{ type: "text", text: "a" }] }];
	const plainNote = payload(head);
	plainNote.messages[2] = { role: "user", content: [{ type: "text", text: NOTE.replace(HEAD_NOTE_START, "routr cut the earlier conversation") }] };
	assert.equal(mark(plainNote), undefined, "a note without a head");
	assert.equal(mark({ messages: [{ role: "user", content: [{ type: "text", text: NOTE }] }] }), undefined, "nothing before the note");
	const off = JSON.parse(JSON.stringify(payload(head), (k, v) => (k === "cache_control" ? undefined : v)));
	assert.equal(mark(off), undefined, "cacheRetention none: Pi marks nothing, nor does routr");
	assert.equal(mark({ ...payload(head), cache_control: EPH }), undefined, "a top-level breakpoint makes 4");
	const oauth = payload(head);
	oauth.system.unshift({ type: "text", text: "You are Claude Code", cache_control: EPH });
	assert.equal(mark(oauth), undefined, "OAuth: two system blocks marked");
	assert.equal(mark(undefined), undefined);
	assert.equal(mark({ input: [] }), undefined, "not a Messages payload");
	assert.ok(mark(payload(head)), "three: room for one");
});
