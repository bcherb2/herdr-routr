import { test } from "node:test";
import assert from "node:assert/strict";
import { jsonValues, parseClaims, places, waitCover } from "../claims.ts";

const EMPTY_CLAIMS = { ids: [], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] };

test("routr start prints the id", () => {
	assert.deepEqual(parseClaims(`routr start --role review --permissions auto 'Red-team PLAN.md'`, "7\n"), { ids: [7], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
	assert.deepEqual(parseClaims(`"$ROUTR_BIN" start --role research - <<'EOF'\nq\nEOF`, "12"), { ids: [12], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
	assert.deepEqual(parseClaims(`/usr/local/bin/routr start x`, "3"), { ids: [3], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
});

test("several starts in one call", () => {
	const cmd = `for b in briefs/*.md; do routr start --role research - < "$b"; done`;
	assert.deepEqual(parseClaims(cmd, "4\n5\n6\n").ids, [4, 5, 6]);
});

test("a pipe claims the last id (the chain field finds the rest)", () => {
	const cmd = `routr start --role plan 'Write PLAN.md' | routr start --after - --role review 'Red-team {{prev.output}}'`;
	assert.deepEqual(parseClaims(cmd, "9\n").ids, [9]);
});

test("--json output", () => {
	const out = JSON.stringify({ id: 11, chain: "c7", role: "review", state: "pending" });
	assert.deepEqual(parseClaims(`routr start --json x`, out), { ids: [11], chains: ["c7"], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
});

test("flow start prints the chain id", () => {
	assert.deepEqual(parseClaims(`routr flow start plan-to-impl --task "add OAuth" --in DECISIONS.md`, "plan-to-impl-3\n"), {
		ids: [],
		chains: ["plan-to-impl-3"],
		seen: [],
		seenChains: [],
		cancelled: [],
		cancelledChains: [],
	});
});

test("--wait that returned the result claims nothing new", () => {
	const got = parseClaims(`routr start --wait --timeout 9m --role review x`, "The plan is fine.\noutput: REDTEAM.md\nverdict: pass");
	assert.deepEqual(got.ids, []);
});

test("--wait that timed out claims the worker it names", () => {
	const got = parseClaims(`routr start --wait --timeout 9m x`, "still running: 3 review · codex (9m00s). Run: routr wait 3");
	assert.deepEqual(got, { ids: [3], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
});

test("wait and result mark what the model already read", () => {
	assert.deepEqual(parseClaims(`routr wait 3 --timeout 9m`, "Done.\n"), { ids: [], chains: [], seen: [3], seenChains: [], cancelled: [], cancelledChains: [] });
	assert.deepEqual(parseClaims(`routr result 4`, "Final.\n"), { ids: [], chains: [], seen: [4], seenChains: [], cancelled: [], cancelledChains: [] });
	assert.deepEqual(parseClaims(`routr wait 3 --timeout 9m`, "still running: 3 review · codex (4m12s). Run: routr wait 3").ids, [3]);
});

test("send: a follow-up the model waits to hear about, unless it waited", () => {
	assert.deepEqual(parseClaims(`routr send 3 "why point 2?"`, ""), { ids: [3], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
	assert.deepEqual(parseClaims(`routr send --wait 3 "why?"`, "Because."), { ids: [], chains: [], seen: [3], seenChains: [], cancelled: [], cancelledChains: [] });
	assert.deepEqual(parseClaims(`routr send --wait --timeout 9m 3 'why?'`, "Because."), { ids: [], chains: [], seen: [3], seenChains: [], cancelled: [], cancelledChains: [] });
});

test("flag values are not mistaken for ids or subcommands", () => {
	assert.deepEqual(parseClaims(`routr wait --timeout 9m 3`, "Done."), { ids: [], chains: [], seen: [3], seenChains: [], cancelled: [], cancelledChains: [] });
	assert.deepEqual(parseClaims(`routr --root /w wait --timeout=9m 4`, "Done.").elsewhere, [
		{ at: { root: "/w" }, ids: [], chains: [], seen: [4], seenChains: [], cancelled: [], cancelledChains: [] },
	]);
	assert.deepEqual(parseClaims(`routr --root /w flow start review --task x`, "c9").elsewhere, [
		{ at: { root: "/w" }, ids: [], chains: ["c9"], seen: [], seenChains: [], cancelled: [], cancelledChains: [] },
	]);
	assert.deepEqual(parseClaims(`routr start --role start --model 5 x`, "6"), { ids: [6], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
});

test("a bare number from another command in the same call is not claimed", () => {
	assert.deepEqual(parseClaims(`routr start x && wc -l < notes.txt`, "7\n12\n").ids, []);
	assert.deepEqual(parseClaims(`cd /w && routr start x`, "7\n").elsewhere?.[0]?.ids, [7]);
	assert.deepEqual(parseClaims(`mkdir -p research; for t in a b; do routr start --role research "$t"; done`, "4\n5\n").ids, [4, 5]);
	assert.deepEqual(parseClaims(`routr start --role task - <<'EOF'\nwc -l things\nEOF\nwc -l f`, "7\n3\n").ids, [], "command after a heredoc");
	assert.deepEqual(parseClaims(`routr start --json x; echo 99`, JSON.stringify({ id: 7 })).ids, [7], "--json is exact");
});

test("other commands and look-alikes claim nothing", () => {
	const none = { ids: [], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] };
	assert.deepEqual(parseClaims(`routr list`, "3 review codex running\n4\n"), none);
	assert.deepEqual(parseClaims(`echo routr-start 5`, "5"), none);
	assert.deepEqual(parseClaims(`git log --oneline`, "5"), none);
	assert.deepEqual(parseClaims(`cat routr.md`, "5"), none);
});

// Pi's bash tool merges stderr into the output, ahead of stdout.
const QUEUED7 = "routr: queued worker 7 review · codex (codex · gpt-6-astra · high)\n";

test("stderr merged in: the queued line and --json after it", () => {
	assert.deepEqual(parseClaims(`routr start --role review x`, `${QUEUED7}7\n`).ids, [7]);
	const rec = JSON.stringify({ id: 7, chain: "c7", state: "pending" }, null, 2);
	assert.deepEqual(parseClaims(`routr start --json --role review x`, `${QUEUED7}${rec}\n`), {
		ids: [7],
		chains: ["c7"],
		seen: [],
		seenChains: [],
		cancelled: [],
		cancelledChains: [],
	});
	// a loop of --json starts prints one record each
	const two = `${QUEUED7}${JSON.stringify({ id: 7 })}\nroutr: queued worker 8 review · codex (codex)\n${JSON.stringify({ id: 8 })}\n`;
	assert.deepEqual(parseClaims(`for t in a b; do routr start --json "$t"; done`, two).ids, [7, 8]);
});

test("the queued line claims what the stdout heuristic can't", () => {
	// stdout captured into a variable: only stderr reaches the output
	assert.deepEqual(parseClaims(`id=$(routr start --role review x)`, QUEUED7).ids, [7]);
	// another command printed a number too
	assert.deepEqual(parseClaims(`id=$(routr start x); echo "started $id"; wc -l f`, `${QUEUED7}started 7\n12\n`).ids, [7]);
	// a pipe: every step says queued
	const pipe = `${QUEUED7}routr: queued worker 8 review · codex (codex) in chain c7\n8\n`;
	assert.deepEqual(parseClaims(`routr start a | routr start --after - b`, pipe).ids, [7, 8]);
	// flow start: each step, and the chain id on stdout
	const flow = "routr: queued 4 plan · claude (claude · opus) step plan\nroutr: queued 5 review · codex (codex) step review\nplan-3\n";
	assert.deepEqual(parseClaims(`routr flow start plan --task x`, flow), { ids: [4, 5], chains: ["plan-3"], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
});

test("command substitutions and $ROUTR_BIN forms are routr calls", () => {
	assert.deepEqual(parseClaims(`r="$("$ROUTR_BIN" start x)"`, QUEUED7).ids, [7]);
	assert.deepEqual(parseClaims("id=`routr start x`", QUEUED7).ids, [7]);
	assert.deepEqual(parseClaims(`echo "$(routr wait 3)"`, "Done.").seen, [3], "the closing ) is not part of the id");
	assert.deepEqual(parseClaims(`x=routr-start; echo 5`, "5").ids, []);
});

test("a wait on what the call started marks it seen", () => {
	// start --wait returned the result
	const done = `${QUEUED7}Two findings.\noutput: REDTEAM.md\nverdict: fail\n`;
	assert.deepEqual(parseClaims(`routr start --wait --role review x`, done), { ids: [7], chains: [], seen: [7], seenChains: [], cancelled: [], cancelledChains: [] });
	// … and timed out
	const late = `${QUEUED7}still running: 7 review · codex (9m00s). Run: routr wait 7\n`;
	assert.deepEqual(parseClaims(`routr start --wait --role review x`, late), { ids: [7], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] });
	// a pipe into wait -
	const pipe = `${QUEUED7}routr: queued worker 8 review · codex (codex)\nLooks good.\n`;
	assert.deepEqual(parseClaims(`routr start a | routr start --after - b | routr wait -`, pipe).seen, [7, 8]);
	// the output didn't name it (stderr dropped): the extension says what the session started meanwhile
	assert.deepEqual(parseClaims(`routr start --wait x 2>/dev/null`, "Done.\n", [9]).seen, [9]);
	assert.deepEqual(parseClaims(`routr start x 2>/dev/null`, "9\n", [9]).seen, [], "no wait, nothing seen");
	// wait on a chain
	assert.deepEqual(parseClaims(`routr wait --timeout 9m auth-2`, "plan: done\nreview: pass\n").seenChains, ["auth-2"]);
	assert.deepEqual(parseClaims(`routr wait auth-2`, "still running: 5 review. Run: routr wait auth-2").seenChains, []);
	// --json of a wait is what it returned; of list, nothing
	assert.deepEqual(parseClaims(`routr wait --json 3`, JSON.stringify({ id: 3, state: "done" })).seen, [3]);
	assert.deepEqual(parseClaims(`routr list --json`, JSON.stringify([{ id: 3, state: "done" }])).seen, []);
});

test("waitCover: what a running call will return", () => {
	assert.equal(waitCover(`routr start --role review x`), undefined);
	assert.equal(waitCover(`routr result 3`), undefined);
	assert.deepEqual(waitCover(`routr wait --timeout 9m 3`), { ids: [3], chains: [], fresh: false });
	assert.deepEqual(waitCover(`routr wait auth-2`), { ids: [], chains: ["auth-2"], fresh: false });
	assert.deepEqual(waitCover(`routr send --wait 4 "why?"`), { ids: [4], chains: [], fresh: false });
	assert.deepEqual(waitCover(`routr start --wait x`), { ids: [], chains: [], fresh: true });
	assert.deepEqual(waitCover(`routr start a | routr wait -`), { ids: [], chains: [], fresh: true });
	assert.deepEqual(waitCover(`id=$(routr start a); routr wait "$id"`), { ids: [], chains: [], fresh: true });
	assert.deepEqual(waitCover(`routr flow start plan --chain p1 --wait --task x`), { ids: [], chains: ["p1"], fresh: true });
	assert.equal(waitCover(`routr start - <<'EOF'\nthen run routr wait 3\nEOF`), undefined, "a brief is not a command");
});

test("separators and flags inside quotes belong to the brief", () => {
	const fresh = { ids: [], chains: [], fresh: true };
	assert.deepEqual(waitCover(`routr start 'fix a; then b' --wait`), fresh);
	assert.deepEqual(waitCover(`routr start --run 'sleep 15; echo x' --wait`), fresh);
	assert.deepEqual(waitCover(`routr start "a && b | c || d" --wait`), fresh);
	assert.deepEqual(waitCover(`routr start "say \\"hi\\"; then stop" --wait`), fresh, "escaped quotes stay inside");
	assert.equal(waitCover(`routr start 'never use --wait here'`), undefined, "--wait in a brief is not the flag");
	assert.deepEqual(parseClaims(`routr start 'count lines; wc -l f'`, "7\n").ids, [7], "a quoted command prints nothing");
	assert.deepEqual(parseClaims(`routr start --wait 'a; b'`, `${QUEUED7}Done.\n`).seen, [7]);
	assert.deepEqual(parseClaims(`routr wait 3 2>&1`, "Done.").seen, [3], "a redirection is not a separator");
	assert.deepEqual(parseClaims(`routr start x 2>&1 && wc -l f`, "7\n12\n").ids, [], "but && still is");
});

test("jsonValues skips prose and finds records between lines", () => {
	assert.deepEqual(jsonValues(`[routr] note\n{"id": 1, "s": "a } b"}\ntrailing\n[1, 2]`), [{ id: 1, s: "a } b" }, [1, 2]]);
	assert.deepEqual(jsonValues("{ broken"), []);
});

test("cancel claims what it cancelled", () => {
	const one = "routr: cancelled 4 research · claude\n";
	assert.deepEqual(parseClaims(`routr cancel 4`, one).cancelled, [4]);
	assert.deepEqual(parseClaims(`routr cancel 4`, one).ids, [], "not a worker to wait on");
	const chain = "routr: cancelled 5 plan · claude\nroutr: cancelled 6 review · codex\n";
	assert.deepEqual(parseClaims(`routr cancel auth-2`, chain), {
		ids: [],
		chains: [],
		seen: [],
		seenChains: [],
		cancelled: [5, 6],
		cancelledChains: ["auth-2"],
	});
	// nothing cancelled: it had finished
	const none = parseClaims(`routr cancel auth-2`, "routr: nothing to cancel in chain auth-2: it has finished (routr list --all)\n");
	assert.deepEqual([none.cancelled, none.cancelledChains], [[], []]);
	// --json: the record of what it did
	const rec = JSON.stringify({ action: "cancel", message: "cancelled 7 x", workers: [{ id: 7, state: "cancelled" }] }, null, 2);
	assert.deepEqual(parseClaims(`routr cancel --json 7`, rec).cancelled, [7]);
	// the line only counts from a cancel
	assert.deepEqual(parseClaims(`routr result 4`, one).cancelled, []);
	// "comes after it" hints name ids that were not cancelled
	const hint = "routr: cancelled 4 plan · claude\nroutr: 5 review comes after it and will not start; routr cancel 5, or routr cancel auth-2 for the chain\n";
	assert.deepEqual(parseClaims(`routr cancel 4`, hint).cancelled, [4]);
});

test("routr help output claims nothing", () => {
	const help = [
		"usage: routr wait ID|CHAIN|- [--timeout D]",
		"  routr wait ID       waits for a worker; Run: routr wait ID|CHAIN|- to wait again",
		"  routr result ID",
		"Run: routr wait ID",
		"Run: routr wait CHAIN",
	].join("\n");
	const got = parseClaims(`routr help`, help);
	assert.deepEqual([got.ids, got.chains], [[], []]);
	// the real lines still claim
	assert.deepEqual(parseClaims(`routr result 4`, "routr: worker 4 has no final message yet (running). Run: routr wait 4\n").ids, [4]);
	assert.deepEqual(parseClaims(`routr wait auth-2`, "still running: 5 review (running). Run: routr wait auth-2\n").chains, ["auth-2"]);
});

// Ids are per registry: a claim says where it was made.
const at = (place: object, c: Partial<Record<string, unknown>>) => ({ at: place, ids: [], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [], ...c });

test("a queued line's registry note says where the worker is, wherever the call ran", () => {
	const out = "routr: queued worker 141 task · codex (codex · gpt-5); registry /code/vp9enc-rs/.routr\n141\n";
	assert.deepEqual(parseClaims(`cd /code/vp9enc-rs && routr start x`, out), { ...EMPTY_CLAIMS, elsewhere: [at({ registry: "/code/vp9enc-rs/.routr" }, { ids: [141] })] });
	assert.deepEqual(parseClaims(`cd "$REPO" && routr start x`, out).elsewhere, [at({ registry: "/code/vp9enc-rs/.routr" }, { ids: [141] })], "the note even where only the shell knows the directory");
	assert.deepEqual(parseClaims(`routr start x`, out).elsewhere, [at({ registry: "/code/vp9enc-rs/.routr" }, { ids: [141] })], "and in the session's own directory: the front end finds it is its own");
	// flow start: each step's line, and the chain where they are
	const flow = "routr: queued 3 plan · claude (claude) step plan; registry /o/.routr\nroutr: queued 4 review · codex (codex) step review; registry /o/.routr\nplan-2\n";
	assert.deepEqual(parseClaims(`cd /o && routr flow start plan --task x`, flow).elsewhere, [at({ registry: "/o/.routr" }, { ids: [3, 4], chains: ["plan-2"] })]);
	// a routr older than the note: where the call ran
	assert.deepEqual(parseClaims(`cd /o && routr start x`, "routr: queued worker 9 task · claude (claude)\n9\n").elsewhere, [at({ cwd: "/o" }, { ids: [9] })]);
});

test("where a call runs: cd, --root, ROUTR_STATE_DIR", () => {
	assert.deepEqual(parseClaims(`cd ../other && routr start x`, "5\n").elsewhere, [at({ cwd: "../other" }, { ids: [5] })]);
	assert.deepEqual(parseClaims(`cd /a; cd b && routr start x`, "5\n").elsewhere, [at({ cwd: "/a/b" }, { ids: [5] })]);
	assert.deepEqual(parseClaims(`cd ~/code/o && routr result 5`, "x").elsewhere, [at({ cwd: "~/code/o" }, { seen: [5] })]);
	assert.deepEqual(parseClaims(`routr result 5 --root /o`, "x").elsewhere, [at({ root: "/o" }, { seen: [5] })]);
	assert.deepEqual(parseClaims(`cd /a && routr result 5 --root ../o`, "x").elsewhere, [at({ root: "/a/../o" }, { seen: [5] })]);
	assert.deepEqual(parseClaims(`ROUTR_STATE_DIR=/s routr send 5 more`, "").elsewhere, [at({ registry: "/s" }, { ids: [5] })]);
	assert.deepEqual(parseClaims(`export ROUTR_STATE_DIR=/s; routr wait 5`, "Done.").elsewhere, [at({ registry: "/s" }, { seen: [5] })]);
	assert.deepEqual(parseClaims(`cd /o && routr cancel 5`, "routr: cancelled 5 task · claude\n").elsewhere, [at({ cwd: "/o" }, { cancelled: [5] })]);
	// a timed-out wait names the worker in the registry it waited on
	assert.deepEqual(parseClaims(`cd /o && routr wait 5`, "still running: 5 task · claude (9m00s). Run: routr wait 5").elsewhere, [at({ cwd: "/o" }, { ids: [5] })]);
	// a follow-up's line names its registry
	assert.deepEqual(parseClaims(`routr send 5 more`, "routr: queued follow-up 2 for 5 task · claude; it goes into its tab; registry /o/.routr\n").elsewhere, [
		at({ registry: "/o/.routr" }, { ids: [5] }),
	]);
	// --json records name theirs in run_dir
	const rec = JSON.stringify({ id: 6, chain: "c1", state: "pending", run_dir: "/o/.routr/runs/6" });
	assert.deepEqual(parseClaims(`routr start --json x`, rec).elsewhere, [at({ registry: "/o/.routr" }, { ids: [6], chains: ["c1"] })]);
});

test("a call where only the shell knows the directory claims nothing: no id is matched in the wrong registry", () => {
	assert.deepEqual(parseClaims(`cd "$REPO" && routr start x`, "5\n"), EMPTY_CLAIMS);
	assert.deepEqual(parseClaims(`cd - && routr result 5`, "x"), EMPTY_CLAIMS);
	assert.deepEqual(parseClaims(`cd $(git rev-parse --show-toplevel)/../o && routr wait 5`, "x"), EMPTY_CLAIMS);
	assert.deepEqual(parseClaims(`cd "$REPO" && cd /o && routr result 5`, "x").elsewhere, [at({ cwd: "/o" }, { seen: [5] })], "an absolute cd after it is known again");
});

test("places: what a front end resolves", () => {
	assert.deepEqual(places(`cd /o && routr start x; routr result 3 --root /p`, "routr: queued worker 7 task · claude (claude); registry /q/.routr\n"), [
		{ cwd: "/o" },
		{ root: "/p" },
		{ registry: "/q/.routr" },
	]);
	assert.deepEqual(places(`routr start x`, "7\n"), []);
});

test("a wait cover says where it waits", () => {
	assert.deepEqual(waitCover(`routr wait 3`), { ids: [3], chains: [], fresh: false });
	assert.deepEqual(waitCover(`cd /o && routr wait 3; routr wait c2 --root /p`), {
		ids: [],
		chains: [],
		fresh: false,
		elsewhere: [
			{ at: { cwd: "/o" }, ids: [3], chains: [] },
			{ at: { root: "/p" }, ids: [], chains: ["c2"] },
		],
	});
	assert.deepEqual(waitCover(`cd "$X" && routr wait 3`), { ids: [], chains: [], fresh: false }, "where only the shell knows: nothing");
});
