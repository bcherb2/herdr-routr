import { test } from "node:test";
import assert from "node:assert/strict";
import { answering, asks, cardLines, engine, formatDuration, jevDecides, oneRow, parseContext, parseList, parseTime, statusText, toCards, waitsForUser, type Paint } from "../model.ts";

const T0 = Date.parse("2026-09-26T10:00:00Z");
const iso = (ms: number): string => new Date(T0 + ms).toISOString();
const ZERO = "0001-01-01T00:00:00Z"; // Go's zero time survives omitempty

// Shaped like internal/registry/types.go Worker + --json's run_dir.
const listJson = JSON.stringify([
	{
		id: 3,
		role: "review",
		harness: "codex",
		model: "gpt-6-astra",
		effort: "high",
		permissions: "auto",
		cwd: "/w",
		write: false,
		brief: "Red-team PLAN.md",
		state: "running",
		transcript: "/tmp/x.jsonl",
		created: iso(0),
		updated: iso(1000),
		started: iso(2000),
		finished: ZERO,
		run_dir: "/w/.routr/runs/3",
	},
	{
		id: 4,
		chain: "c1",
		step: "dig",
		role: "research",
		harness: "claude",
		model: "haiku",
		effort: "low",
		state: "blocked",
		// as routr reads Claude's permission prompt (internal/worker/screen.go)
		question: "Do you want to proceed?\n1. Yes\n2. Yes, and don't ask again for: curl\n3. No",
		options: ["Yes", "Yes, and don't ask again for: curl", "No"],
		fingerprint: "6a128b4273b95faa",
		pending: { tool: "Bash", summary: "Bash: curl -s https://example.com", input: '{"command":"curl -s https://example.com"}' },
		answerer: "human",
		created: iso(0),
		updated: iso(0),
		started: iso(0),
	},
	{ id: 5, role: "implement", title: "fix-auth", harness: "claude", state: "gated", created: iso(0), updated: iso(0), started: ZERO },
	{ id: 6, role: "task", harness: "pi", state: "done", verdict: "pass", output: "OUT.md", created: iso(0), updated: iso(0), started: iso(0), finished: iso(60_000), after: [3, "x", 4], final: "All done.", final_path: "/w/.routr/runs/6/final.md" },
	{ id: 7, role: "task", harness: "pi", state: "done", created: iso(0), updated: iso(0), finished: iso(-3_600_000) },
	{ id: 8, role: "task", harness: "pi", state: "closed", created: iso(0), updated: iso(0) },
	{ id: 9, role: "task", harness: "pi", state: "exploded", created: iso(0) },
	{ role: "no id", state: "running" },
	{ id: 10, role: "task", harness: "codex", state: "pending", created: iso(0), caller: { harness: "pi", session: "S1" } },
]);

test("parseList keeps valid records and drops malformed ones", () => {
	const ws = parseList(listJson);
	assert.deepEqual(ws.map((w) => w.id), [3, 4, 5, 6, 7, 8, 10]);
	const w3 = ws[0]!;
	assert.equal(w3.started, T0 + 2000);
	assert.equal(w3.finished, undefined, "zero time is unset");
	assert.equal(w3.runDir, "/w/.routr/runs/3");
	assert.equal(ws.at(-1)!.callerSession, "S1");
	assert.equal(ws.at(-1)!.callerHarness, "pi");
	const w4 = ws.find((w) => w.id === 4)!;
	assert.deepEqual([w4.fingerprint, w4.asksAbout, w4.answerer], ["6a128b4273b95faa", "Bash: curl -s https://example.com", "human"]);
	assert.deepEqual(w4.options, ["Yes", "Yes, and don't ask again for: curl", "No"]);
	assert.deepEqual([w3.fingerprint, w3.options, w3.asksAbout, w3.answerer], [undefined, undefined, undefined, undefined], "not blocked; a record from before answerer");
	const w6 = ws.find((w) => w.id === 6)!;
	assert.deepEqual([w6.after, w6.final, w6.finalPath], [[3, 4], "All done.", "/w/.routr/runs/6/final.md"]);
	assert.deepEqual(parseList(""), []);
	assert.deepEqual(parseList("null"), []);
	assert.equal(parseList(JSON.stringify({ id: 1, role: "r", harness: "h", state: "done" })).length, 1);
	assert.throws(() => parseList("{nope"));
});

test("parseList reads follow-up counts: turns sent, replies written, follow-ups queued", () => {
	const [w] = parseList(JSON.stringify({ id: 11, role: "plan", harness: "claude", state: "running", interactive: true, turn: 3, replies: 2, inbox: ["and then?"] }));
	assert.deepEqual([w!.turn, w!.replies, w!.queued], [3, 2, 1]);
	const [none] = parseList(JSON.stringify({ id: 12, role: "task", harness: "pi", state: "done" }));
	assert.deepEqual([none!.turn, none!.replies, none!.queued], [undefined, undefined, undefined]);
});

// routr show ID --context --json (internal/watch/context.go, QuestionContext)
test("parseContext reads who answers, the options only the user may pick, and the question now", () => {
	const out = JSON.stringify({
		worker: 4,
		answerer: "agent",
		question: "Do you want to proceed?\n1. Yes\n2. Yes, and don't ask again for: curl\n3. No",
		options: ["Yes", "Yes, and don't ask again for: curl", "No"],
		fingerprint: "6a128b4273b95faa",
		lasting: [2],
		pending: { tool: "Bash", summary: "Bash: curl -s https://example.com", input: "{}" },
		brief: "Look into curl",
		brief_path: "/w/.routr/runs/4/brief.md",
		root: "/w",
		docs: [{ path: "/w/SPEC.md", size: 31 }, { size: 3 }],
		missing: ["docs/gone.md"],
		answer: ["You answer this question, not the user.", "Before you decide, read the project docs, even when the answer looks obvious: /w/SPEC.md"],
	});
	assert.deepEqual(parseContext(out), {
		answer: ["You answer this question, not the user.", "Before you decide, read the project docs, even when the answer looks obvious: /w/SPEC.md"],
		lasting: [2],
		question: "Do you want to proceed?\n1. Yes\n2. Yes, and don't ask again for: curl\n3. No",
		options: ["Yes", "Yes, and don't ask again for: curl", "No"],
		fingerprint: "6a128b4273b95faa",
		asksAbout: "Bash: curl -s https://example.com",
	});
	const none = { answer: [], lasting: [], question: undefined, options: undefined, fingerprint: undefined, asksAbout: undefined };
	assert.deepEqual(parseContext(JSON.stringify({ worker: 4, docs: [] })), none, "asks nothing now: nobody answers, nothing lasts");
	assert.equal(parseContext("routr: no worker 4"), undefined);
	assert.equal(parseContext(JSON.stringify([{ docs: [] }])), undefined);
});

test("parseTime", () => {
	assert.equal(parseTime(ZERO), undefined);
	assert.equal(parseTime("garbage"), undefined);
	assert.equal(parseTime(42), undefined);
	assert.equal(parseTime(iso(5)), T0 + 5);
});

test("formatDuration", () => {
	assert.equal(formatDuration(-5), "0s");
	assert.equal(formatDuration(45_900), "45s");
	assert.equal(formatDuration(252_000), "4m12s");
	assert.equal(formatDuration(3_780_000), "1h03m");
});

test("cards: people first, finished ones linger, closed ones go", () => {
	const ws = parseList(listJson);
	const now = T0 + 254_000;
	const activity = new Map([[3, ["a", "b", "c", "exec  rg -n auth"]]]);
	const cards = toCards(ws, { now, lingerMs: 300_000, mine: (w) => w.id === 3, answerHint: "alt+q answer", activity });
	assert.deepEqual(cards.map((c) => c.id), [4, 5, 3, 10, 6]); // 7 finished an hour ago, 8 closed
	const [c4, c5, c3, c10, c6] = cards;
	assert.equal(c4!.title, "4 research [c1/dig]");
	assert.equal(c4!.tone, "alert");
	assert.equal(c4!.note, "⚠ claude 4 asks about Bash: curl -s https://example.com · alt+q answer", "what it asks about, and the key");
	assert.equal(c5!.title, "5 fix-auth", "a title stands in for the role");
	assert.match(c5!.note!, /\/routr approve 5/);
	assert.equal(c3!.engine, "codex·gpt-6-astra·high");
	assert.equal(c3!.age, "4m12s");
	assert.deepEqual(c3!.activity, ["b", "c", "exec  rg -n auth"], "last 3 only");
	assert.equal(c3!.mine, true);
	assert.equal(c10!.tone, "wait");
	assert.equal(c6!.age, "1m00s", "finished workers show how long they ran");
	assert.equal(c6!.note, "verdict pass · OUT.md");
});

test("card lines and status", () => {
	const ws = parseList(listJson);
	const cards = toCards(ws, { now: T0 + 254_000, lingerMs: 0, mine: () => false, answerHint: "alt+q answer" });
	const plain: Paint = (_t, s) => s;
	const lines = cardLines(cards, plain);
	assert.equal(lines[0], "routr · 2 need you · 1 running · 1 queued");
	assert.equal(lines[1], "├─ ! 4 research [c1/dig] · claude·haiku·low · blocked · 4m14s");
	assert.equal(lines[2], "│    ⎿ ⚠ claude 4 asks about Bash: curl -s https://example.com · alt+q answer");
	assert.ok(lines.every((l) => !/proceed|\n/.test(l)), "the call it is about, not the screen's words; no line breaks");
	assert.equal(lines.at(-1), "└─ ◦ 10 task · codex · pending · 4m14s");
	assert.equal(statusText(cards), "▶2 !2");
	assert.deepEqual(cardLines([], plain), []);
	assert.equal(statusText([]), undefined);
});

// answer = "jev": while Jev decides, nobody needs to act, and the card says
// so; once Jev handed the question over, it needs a person as any question.
test("a question Jev decides, then hands over", () => {
	const rec = { id: 4, role: "task", harness: "claude", state: "blocked", question: "Do you want to proceed?\n1. Yes\n2. No", fingerprint: "6a128b4273b95faa", answerer: "jev", note: "" };
	const [deciding] = parseList(JSON.stringify(rec));
	const note = "Jev did not answer it: it picked option 1 (Yes) with confidence 0.62, below min_confidence 0.8.";
	const [handed] = parseList(JSON.stringify({ ...rec, handed_to: "agent", note }));
	assert.deepEqual([answering(deciding!), jevDecides(deciding!), deciding!.note], ["jev", true, undefined]);
	assert.deepEqual([answering(handed!), jevDecides(handed!), handed!.handedTo, handed!.note], ["agent", false, "agent", note]);
	const [h, d] = toCards([deciding!, { ...handed!, id: 5 }], { now: T0, lingerMs: 0, mine: () => true, answerHint: "alt+q answer" });
	assert.deepEqual([h!.id, h!.tone, d!.id, d!.tone, d!.note], [5, "alert", 4, "run", "Jev decides its question"], "people first; Jev's card runs");
	assert.equal(statusText([d!]), "▶1");
	assert.equal(answering({ id: 1, role: "task", harness: "pi", state: "blocked" }), "human", "a record from before answerer");
});

// The widget's line for a question the user may answer: the call it is
// about, else the question's first line, clipped so the key stays on screen.
test("the hint: what a blocked worker asks, in one line, and the key", () => {
	const q = { id: 4, role: "task", harness: "codex", state: "blocked" as const };
	assert.equal(asks({ ...q, question: "\nWould you like to run the following command?\n$ go test ./...\n1. Yes" }), "asks: Would you like to run the following command?");
	assert.equal(asks({ ...q, question: undefined }), "asks a question in its tab");
	const long = asks({ ...q, asksAbout: `Bash: ${"x".repeat(100)}` });
	assert.equal(long, `asks about Bash: ${"x".repeat(54)}…`, "clipped to 60");
	const [card] = toCards([{ ...q, question: "What should the file be called?" }], { now: T0, lingerMs: 0, mine: () => true, answerHint: "alt+q answer" });
	assert.equal(card!.note, "⚠ codex 4 asks: What should the file be called? · alt+q answer", "no menu: the same hint; the picker offers its tab");
	// who waits for the user: any question but one Jev decides; the model's (agent) too, as the user may always answer
	assert.equal(waitsForUser({ ...q, answerer: "agent", fingerprint: "f" }), true);
	assert.equal(waitsForUser({ ...q, answerer: "jev", fingerprint: "f" }), false);
	assert.equal(waitsForUser({ ...q, answerer: "jev", handedTo: "human", fingerprint: "f" }), true);
	assert.equal(waitsForUser({ ...q, state: "gated" }), false);
});

test("oneRow keeps a widget line to one terminal row", () => {
	assert.equal(oneRow("failed: exit 1\nsee log\r\nmore"), "failed: exit 1 ↵ see log ↵ more");
	assert.equal(oneRow("a\tb\x07c\x00d\x7f"), "a bcd");
	const red = "\x1b[31m⎿ ok\x1b[39m";
	assert.equal(oneRow(red), red, "colours stay");
});

test("a run step's engine is sh, a judge's jev", () => {
	const [run, judge, worker] = parseList(
		JSON.stringify([
			{ id: 1, state: "running", role: "run", run: "go test ./..." },
			{ id: 2, state: "running", role: "judge", judge: { by: "jev" } },
			{ id: 3, state: "running", role: "task", harness: "claude", model: "opus" },
		]),
	);
	assert.equal(engine(run!), "sh", "no harness: not ?");
	assert.equal(engine(judge!), "jev");
	assert.equal(engine(worker!), "claude·opus");
});
