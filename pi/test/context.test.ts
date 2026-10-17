import { test } from "node:test";
import assert from "node:assert/strict";
import { mainFromEnv, stripOldReasoning } from "../src/context.ts";

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
	assert.deepEqual(mainFromEnv({}), { context: "truncate", at: 150_000, keep_turns: 3, keep_tokens: 40_000, strip_reasoning: true });
	assert.deepEqual(
		mainFromEnv({ ROUTR_MAIN_CONTEXT: "off", ROUTR_MAIN_AT: "-1", ROUTR_MAIN_KEEP_TURNS: "5", ROUTR_MAIN_KEEP_TOKENS: "0", ROUTR_MAIN_STRIP_REASONING: "0" }),
		{ context: "off", at: -1, keep_turns: 5, keep_tokens: 40_000, strip_reasoning: false },
	);
	assert.deepEqual(
		mainFromEnv({ ROUTR_MAIN_AT: "+90000" }),
		{ context: "truncate", at: 90_000, keep_turns: 3, keep_tokens: 40_000, strip_reasoning: true },
	);
	assert.deepEqual(
		mainFromEnv({ ROUTR_MAIN_CONTEXT: "compact", ROUTR_MAIN_AT: "150k", ROUTR_MAIN_KEEP_TURNS: "-2", ROUTR_MAIN_KEEP_TOKENS: "+0" }),
		{ context: "truncate", at: 150_000, keep_turns: 3, keep_tokens: 40_000, strip_reasoning: true },
	);
});
