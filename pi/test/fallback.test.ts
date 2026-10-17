import { test } from "node:test";
import assert from "node:assert/strict";
import { Chain, failure, modelName, parseFallback, piRef } from "../src/fallback.ts";

test("parseFallback: routr fallback --json, or undefined", () => {
	assert.deepEqual(parseFallback('{"model":"pi:a/b","rule":2,"from":["*b"],"to":["pi:c/d"],"lasting":true}'), { rule: 2, to: ["pi:c/d"], lasting: true });
	assert.deepEqual(parseFallback('{"model":"pi:a/b","rule":0,"from":[],"to":[]}'), { rule: 0, to: [], lasting: undefined });
	assert.equal(parseFallback("not json"), undefined);
	assert.equal(parseFallback('{"rule":"1","to":[]}'), undefined);
	assert.equal(parseFallback('{"rule":1,"to":[3]}'), undefined);
});

test("piRef: a Pi model from a pi: entry, with a thinking level after a colon", () => {
	assert.deepEqual(piRef("pi:openrouter/anthropic/claude-haiku-5.5"), { provider: "openrouter", id: "anthropic/claude-haiku-5.5" });
	assert.deepEqual(piRef("pi:openrouter/deepseek/deepseek-v4.1-flash:high"), { provider: "openrouter", id: "deepseek/deepseek-v4.1-flash", thinking: "high" });
	assert.deepEqual(piRef("pi:ollama/qwen3:32b"), { provider: "ollama", id: "qwen3:32b" }, "a colon that is no thinking level is the model's");
	assert.equal(piRef("codex:gpt-6.1"), undefined, "another harness: not for a Pi main agent");
	assert.equal(piRef("pi:noslash"), undefined);
	assert.equal(modelName({ provider: "deepseek", id: "deepseek-v4-pro" }), "pi:deepseek/deepseek-v4-pro");
});

test("failure: the error a run ended with and the model that failed; none when it answered or the user stopped it", () => {
	const err = { role: "assistant", stopReason: "error", errorMessage: "402: Insufficient Balance", provider: "deepseek", model: "deepseek-v4-pro" };
	assert.deepEqual(failure([{ role: "user" }, err]), { error: "402: Insufficient Balance", model: "pi:deepseek/deepseek-v4-pro" });
	assert.equal(failure([err, { role: "assistant", stopReason: "stop" }]), undefined);
	assert.equal(failure([{ role: "assistant", stopReason: "aborted", errorMessage: "Operation aborted" }]), undefined);
	assert.deepEqual(failure([{ role: "assistant", stopReason: "error" }]), { error: "an API error" });
	assert.equal(failure([{ role: "user" }]), undefined);
});

test("Chain: a fallback that fails too moves down the original rule's list, never its own", () => {
	const chain = new Chain();
	assert.equal(chain.next("pi:a/pro"), undefined, "a model that started no chain: ask routr");
	chain.start("pi:a/pro", 3, ["pi:b/flash", "codex:x", "pi:c/haiku"]);
	assert.deepEqual(chain.next("pi:a/pro"), { rule: 3, candidates: ["pi:b/flash", "codex:x", "pi:c/haiku"] });
	chain.moved("pi:b/flash");
	assert.deepEqual(chain.next("pi:b/flash"), { rule: 3, candidates: ["codex:x", "pi:c/haiku"] });
	chain.moved("pi:c/haiku");
	assert.deepEqual(chain.next("pi:c/haiku"), { rule: 3, candidates: [] }, "the list is spent");
	assert.equal(chain.next("pi:z/other"), undefined, "the user picked another model: a new chain");
});
