import { test } from "node:test";
import assert from "node:assert/strict";
import { parseList, type Paint, type Worker } from "../model.ts";
import { stepLines, toSteps, type StepOptions } from "../steps.ts";

const T0 = Date.parse("2026-10-01T10:00:00Z");
const iso = (ms: number): string => new Date(T0 + ms).toISOString();
const MIN = 60_000;
const plain: Paint = (_t, s) => s;
const opts = (o: Partial<StepOptions> = {}): StepOptions => ({ now: T0 + 10 * MIN, lingerMs: 90_000, mine: () => false, answerHint: "alt+q answer", ...o });

// Shaped like `routr list --json`: what `flow start` queues, and the
// watcher's foreach expansion (worker 4 → copies 5–8).
const chain = parseList(
	JSON.stringify([
		{ id: 1, chain: "oauth-3", step: "plan", role: "plan", harness: "claude", model: "opus", effort: "high", state: "done", created: iso(0), started: iso(0), finished: iso(4 * MIN), updated: iso(4 * MIN) },
		{ id: 2, chain: "oauth-3", step: "review", role: "review", harness: "codex", state: "done", verdict: "pass", round: 2, on_fail: { resume: 1, brief: "fix", max: 3 }, created: iso(0), started: iso(4 * MIN), finished: iso(6 * MIN), updated: iso(6 * MIN) },
		{ id: 3, chain: "oauth-3", step: "implement", role: "implement", harness: "claude", state: "done", fanout: { glob: "parts/*.md", max_items: 10, items: [5, 6, 7, 8] }, created: iso(0) },
		{ id: 5, chain: "oauth-3", step: "implement", role: "implement", harness: "claude", parallel: 2, state: "done", created: iso(6 * MIN), started: iso(6 * MIN), finished: iso(8 * MIN) },
		{ id: 6, chain: "oauth-3", step: "implement", role: "implement", harness: "claude", parallel: 2, state: "done", created: iso(6 * MIN), started: iso(6 * MIN), finished: iso(8 * MIN) },
		{ id: 7, chain: "oauth-3", step: "implement", role: "implement", harness: "claude", parallel: 2, state: "running", created: iso(6 * MIN), started: iso(8 * MIN), transcript: "/t" },
		{ id: 8, chain: "oauth-3", step: "implement", role: "implement", harness: "claude", parallel: 2, state: "waiting", created: iso(6 * MIN) },
		{ id: 9, chain: "oauth-3", step: "test", role: "run", state: "pending", created: iso(0) },
		{ id: 10, chain: "oauth-3", step: "merge", role: "implement", harness: "claude", gate: "you", state: "gated", created: iso(0) },
	]),
);

test("a chain: one row per step, foreach copies folded into ×N", () => {
	const blocks = toSteps(chain, opts());
	assert.equal(blocks.length, 1);
	assert.deepEqual(stepLines(blocks, plain), [
		"! oauth-3 · 2/5 steps · 10m00s",
		"  ✔ plan          claude·opus·high · 4m00s",
		"  ✔ review        codex · pass · round 2/3 · 2m00s",
		"  ▶ implement ×4  ✔2 ▶1 ○1 · max 2 at once · 4m00s",
		"  ○ test          run",
		"  ⏸ merge         claude",
		"      ⎿ waits at a gate → /routr approve 10 to start it",
	]);
});

test("the running worker of a single-worker step shows its latest tool call", () => {
	const ws = chain.map((w): Worker => (w.id === 10 ? { ...w, state: "running", started: T0 + 9 * MIN } : w));
	const lines = stepLines(toSteps(ws, opts({ activity: new Map([[10, ["Read a.go", "Edit b.go"]]]) })), plain);
	assert.deepEqual(lines.slice(-2), ["  ▶ merge         claude · 1m00s", "      ⎿ Edit b.go"]);
});

test("a foreach step before it expands is ×?, one that matched nothing ×0", () => {
	const ws = parseList(
		JSON.stringify([
			{ id: 1, chain: "c", step: "split", role: "plan", harness: "claude", state: "running", created: iso(0), started: iso(0) },
			{ id: 2, chain: "c", step: "each", role: "implement", harness: "claude", state: "waiting", after: [1], fanout: { glob: "x/*", max_items: 5 }, created: iso(0) },
			{ id: 3, chain: "c", step: "none", role: "review", harness: "codex", state: "done", fanout: { glob: "y/*", max_items: 5, allow_empty: true }, created: iso(0), finished: iso(0) },
		]),
	);
	const lines = stepLines(toSteps(ws, opts()), plain);
	assert.equal(lines[2], "  ○ each ×?  claude");
	assert.equal(lines[3], "  ✔ none ×0  no files matched");
});

test("a copy that asks turns the row red and says what it asks and the key that answers it", () => {
	const ws = chain.map((w): Worker => (w.id === 7 ? { ...w, state: "blocked", question: "Do you want to proceed?\n1. Yes\n2. No" } : w));
	const lines = stepLines(toSteps(ws, opts()), plain);
	assert.equal(lines[3], "  ! implement ×4  !1 ✔2 ○1 · max 2 at once · 4m00s");
	assert.equal(lines[4], "      ⎿ ⚠ claude 7 asks: Do you want to proceed? · alt+q answer");
	const about = ws.map((w): Worker => (w.id === 7 ? { ...w, asksAbout: "Bash: go test ./..." } : w));
	assert.equal(stepLines(toSteps(about, opts()), plain)[4], "      ⎿ ⚠ claude 7 asks about Bash: go test ./... · alt+q answer", "the call it is about, when routr knows it");
});

test("a finished chain is one line, then goes; an active one keeps its old steps", () => {
	const done = chain.map((w): Worker => ({ ...w, state: "done", finished: T0 + 10 * MIN, updated: T0 + 10 * MIN }));
	assert.deepEqual(stepLines(toSteps(done, opts()), plain), ["✔ oauth-3 · 5/5 steps · 10m00s"]);
	assert.deepEqual(toSteps(done, opts({ now: T0 + 20 * MIN })), []);
	// plan finished long ago; list still returns it because the chain is active
	const blocks = toSteps(chain, opts({ now: T0 + 120 * MIN }));
	assert.equal(blocks[0]!.rows[0]!.name, "plan");
});

test("ad hoc workers: one role started in one run is one row", () => {
	const ws = parseList(
		JSON.stringify([
			{ id: 11, role: "research", harness: "claude", model: "haiku", state: "done", created: iso(0), started: iso(0), finished: iso(9 * MIN), updated: iso(9 * MIN) },
			{ id: 12, role: "research", harness: "claude", model: "haiku", state: "running", created: iso(0), started: iso(0) },
			{ id: 13, role: "research", harness: "claude", model: "haiku", state: "running", created: iso(0), started: iso(0) },
			{ id: 14, role: "review", harness: "codex", state: "running", created: iso(0), started: iso(0) },
		]),
	);
	const group = (w: Worker) => (w.id < 14 ? "run-1" : "run-2");
	assert.deepEqual(stepLines(toSteps(ws, opts({ group, mine: (w) => w.id === 14 })), plain), [
		"▶ ad hoc · 10m00s",
		"  ▶ research ×3  ✔1 ▶2 · 10m00s",
		"  ▶ 14 review    codex · 10m00s",
	]);
	// without a run to group by, each is its own row
	assert.equal(toSteps(ws, opts())[0]!.rows.length, 4);
});

test("over maxLines, finished steps fold first, then queued ones", () => {
	const tall = [
		...chain,
		...[11, 12, 13].map((id) => ({ id, chain: "oauth-3", step: `extra${id}`, role: "run", harness: "?", state: "pending", created: T0 }) as Worker),
	];
	const lines = stepLines(toSteps(tall, opts()), plain, 8);
	assert.deepEqual(lines, [
		"! oauth-3 · 2/8 steps · 10m00s",
		"  ✔ 2 done: plan, review",
		"  ▶ implement ×4  ✔2 ▶1 ○1 · max 2 at once · 4m00s",
		"  ○ test          run",
		"  ⏸ merge         claude",
		"      ⎿ waits at a gate → /routr approve 10 to start it",
		"  ○ 3 more: extra11, extra12, extra13",
	]);
});

test("a theme's escape codes around an empty field leave no stray separator", () => {
	const ansi: Paint = (_t, s) => `\x1b[2m${s}\x1b[0m`;
	const strip = (s: string) => s.replace(/\x1b\[\d+m/g, "");
	const merge = stepLines(toSteps(chain, opts()), ansi).map(strip).find((l) => l.includes("merge"));
	assert.equal(merge, "  ⏸ merge         claude");
});
