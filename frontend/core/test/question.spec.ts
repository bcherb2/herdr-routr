import { test } from "node:test";
import assert from "node:assert/strict";
import { parseContext } from "../model.ts";
import { answerArgs, answered, closed, question, questionKey, stillAsked, waiting } from "../question.ts";
import { bash, ctx7, free, jev, older, OPTIONS } from "./fixtures.ts";

test("a question: from routr's context when it has one, else the record; none when it asks nothing now", () => {
	const q = question(bash, ctx7)!;
	assert.equal(q.who, "7 plan · claude");
	assert.deepEqual(q.text, ["Do you want to proceed?"]);
	assert.deepEqual(q.options, OPTIONS);
	assert.deepEqual(q.lasting, [2]);
	assert.equal(q.key, "6a128b4273b95faa");
	assert.deepEqual(question(bash)!.lasting, [], "without the context nothing is marked");
	assert.equal(question(bash, parseContext(JSON.stringify({ worker: 7, docs: [] }))), undefined);
	// the context is newer than the list: its question wins
	const now = question(bash, parseContext(JSON.stringify({ worker: 7, question: "Create a.go?\n1. Yes\n2. No", options: ["Yes", "No"], fingerprint: "c0ffee", docs: [] })))!;
	assert.deepEqual([now.key, now.text, now.options], ["c0ffee", ["Create a.go?"], ["Yes", "No"]]);
	// no menu, or a menu without a fingerprint: nothing to press safely
	assert.deepEqual(question(free)!.options, []);
	assert.equal(questionKey(free), "text:What should the file be called?");
	assert.deepEqual(question({ ...bash, fingerprint: undefined })!.options, []);
});

test("who waits: all but the one Jev decides, the oldest question first", () => {
	assert.deepEqual(waiting([bash, free, jev, older]).map((w) => w.id), [10, 8, 7], "the model's question too: the user may always answer");
	assert.equal(questionKey(jev), undefined, "Jev decides it");
});

test("a dialog stands while each question it shows does; it closes when one is answered or changes", () => {
	const shown = new Map([[7, questionKey(bash)!]]);
	assert.equal(stillAsked(shown, [bash, free]), true);
	assert.equal(stillAsked(shown, [{ ...bash, state: "running", question: undefined, fingerprint: undefined }]), false, "answered in its tab");
	assert.equal(stillAsked(shown, [{ ...bash, fingerprint: "c0ffee" }]), false, "another question");
	assert.equal(stillAsked(shown, []), false, "gone from the list");
	assert.equal(stillAsked(shown, [{ ...bash, answerer: "jev" }]), false, "Jev decides it now");
	assert.deepEqual(closed(7, [{ ...bash, fingerprint: "c0ffee" }], "alt+q"), { text: "routr: 7 asks another question now. alt+q shows it.", level: "warning" });
	assert.deepEqual(closed(7, [], "alt+q"), { text: "routr: 7 asks nothing now; nothing was pressed.", level: "info" });
});

test("the answer goes by the user, only on the question they saw; exit 4 says it changed", () => {
	assert.deepEqual(answerArgs(7, 2, "6a128b4273b95faa"), ["answer", "7", "2", "--by", "human", "--expect", "6a128b4273b95faa"]);
	assert.deepEqual(answerArgs(7, 2), ["answer", "7", "2", "--by", "human"], "no fingerprint seen: as in its tab");
	assert.deepEqual(answered(7, 0, "answered 7 plan · claude with option 1 (Yes), by human\n", "alt+q"), { text: "routr: answered 7 plan · claude with option 1 (Yes), by human", level: "info" });
	assert.deepEqual(answered(7, 4, "routr answer: 7 plan · claude asks another question now, so no option was chosen. …\nCreate a.go?", "alt+q"), {
		text: "routr: 7 asks another question now, so nothing was pressed. alt+q shows it.",
		level: "warning",
	});
	assert.deepEqual(answered(7, 1, "routr: worker 7 shows no menu now: its question was answered or went away.\n", "alt+q"), {
		text: "routr answer 7: routr: worker 7 shows no menu now: its question was answered or went away.",
		level: "error",
	});
});
