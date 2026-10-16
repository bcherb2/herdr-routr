import { test } from "node:test";
import assert from "node:assert/strict";
import { question, waiting } from "../../frontend/core/index.ts";
import { bash, ctx7, free, jev, older } from "../../frontend/core/test/fixtures.ts";
import { chooser, LATER, OPEN_TAB, questionPicker } from "../src/dialogs.ts";

test("the picker: the options as on screen, the one that lasts marked, then its tab and later", () => {
	const q = question(bash, ctx7)!;
	const p = questionPicker(q);
	assert.equal(p.title, "7 plan · claude asks about Bash: go test ./...\nDo you want to proceed?");
	assert.deepEqual(p.items, ["1. Yes", "2. Yes, and don't ask again for: go test:* (lasts past this call)", "3. No", OPEN_TAB, LATER]);
	assert.deepEqual(p.choose("3. No"), { kind: "option", n: 3 });
	assert.deepEqual(p.choose(p.items[1]), { kind: "option", n: 2 }, "the user may pick one that lasts");
	assert.deepEqual(p.choose(OPEN_TAB), { kind: "tab" });
	assert.deepEqual(p.choose(LATER), { kind: "later" });
	assert.deepEqual(p.choose(undefined), { kind: "later" }, "esc, or it closed itself");
	assert.equal(q.key, "6a128b4273b95faa");
});

test("without routr's context: the record's question, nothing marked", () => {
	const p = questionPicker(question(bash)!);
	assert.deepEqual(p.items, ["1. Yes", "2. Yes, and don't ask again for: go test:*", "3. No", OPEN_TAB, LATER]);
});

test("a question with no menu offers only its tab and later", () => {
	const p = questionPicker(question(free)!);
	assert.equal(p.title, "8 name-it · claude asks:\nWhat should the file be called?\nIt has no options to pick here: answer it in its tab.");
	assert.deepEqual(p.items, [OPEN_TAB, LATER]);
	assert.deepEqual(p.choose(OPEN_TAB), { kind: "tab" });
	// a menu without a fingerprint can't be answered safely: no options either
	assert.deepEqual(questionPicker(question({ ...bash, fingerprint: undefined })!).items, [OPEN_TAB, LATER]);
});

test("who waits: all but the one Jev decides, the oldest question first; several get a chooser", () => {
	const ws = waiting([bash, free, jev, older]);
	assert.deepEqual(ws.map((w) => w.id), [10, 8, 7], "the model's question too: the user may always answer");
	const d = chooser(ws);
	assert.equal(d.title, "3 workers wait for an answer. Which one?");
	assert.deepEqual(d.items, [
		"10 review · codex asks: Allow?",
		"8 name-it · claude asks: What should the file be called?",
		"7 plan · claude asks about Bash: go test ./...",
	]);
	assert.equal(d.choose(d.items[2])?.id, 7);
	assert.equal(d.choose(undefined), undefined);
});

