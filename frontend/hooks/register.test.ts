// The Claude Code adapter, through the engine's own kit: the coordinator's
// scenarios (frontend/core/test/coordinator.spec.ts) raised as Claude Code
// events. The test's hooks stand in for the host: process.run is a fake
// routr whose workers the test moves, and the clock, store and env are
// mocked. Run: claude plugin test frontend
import { expect, mock, test } from "claude-code/testing";
import type { On } from "claude-code";

const S1 = "11111111-1111-1111-1111-111111111111";
const S2 = "22222222-2222-2222-2222-222222222222";
const POLL = 2000;

type W = Record<string, unknown> & { id: number; state: string };

/** The host beneath the mod: a fake routr, a session id, and what the mod showed and sent. */
function host(on: On) {
	const clock = mock.clock(on, { now: Date.parse("2026-10-13T10:00:00Z") });
	mock.store(on);
	mock.env(on, {});
	const session = mock.session(on);
	const world = {
		session: S1,
		workers: [] as W[],
		results: new Map<number, string>(),
		calls: [] as string[],
		submitted: [] as string[],
		logged: [] as string[],
		/** the words /routr handed to the routr skill (Claude Code's own run of it, beneath the mod) */
		skill: [] as string[],
		toasts: [] as string[],
		/** panes opened and closed, by id */
		opened: [] as string[],
		closed: [] as string[],
		/** transcript path → its JSONL */
		transcripts: new Map<string, string>(),
		status: undefined as string | undefined,
		/** what the model's shell call returns (Claude Code's Bash tool, beneath the mod) */
		bash: (_e: any): unknown => ({ result: { stdout: "", stderr: "", interrupted: false }, text: "" }),
	};
	on("session.id", () => ({ value: world.session }));
	on("process.run", ($, e) => ({ value: routr(e.argv) }));
	const routr = (argv: readonly string[]) => {
		const [bin, ...args] = argv;
		if (bin === "git") return { exitCode: 128, stdout: "", stderr: "not a git repository", isStdoutTruncated: false, isStderrTruncated: false };
		const ok = (stdout: string) => ({ exitCode: 0, stdout, stderr: "", isStdoutTruncated: false, isStderrTruncated: false });
		if (bin === "tail") return ok(world.transcripts.get(args.at(-1)!) ?? "");
		world.calls.push(args.join(" "));
		if (args[0] === "answer") {
			const w = world.workers.find((x) => x.id === Number(args[1]));
			const expect = args[args.indexOf("--expect") + 1];
			if (w?.state !== "blocked" || (args.includes("--expect") && expect !== w.fingerprint))
				return { exitCode: 4, stdout: "", stderr: `worker ${args[1]} asks another question now`, isStdoutTruncated: false, isStderrTruncated: false };
			return ok(`routr: answered ${args[1]} with option ${args[2]}, by human\n`);
		}
		if (args[0] === "list") return ok(JSON.stringify(world.workers));
		if (args[0] === "result") return ok(world.results.get(Number(args[1])) ?? "");
		if (args[0] === "show") {
			const w = world.workers.find((x) => x.id === Number(args[1]));
			return ok(
				JSON.stringify({
					worker: Number(args[1]),
					question: w?.question,
					options: w?.options,
					fingerprint: w?.fingerprint,
					pending: w?.pending,
					docs: [],
					answer: w?.question ? [`fake routr: the user answers worker ${args[1]}'s question.`] : undefined,
				}),
			);
		}
		return { exitCode: 2, stdout: "", stderr: `fake routr: ${args.join(" ")}`, isStdoutTruncated: false, isStderrTruncated: false };
	};
	on("fs.exists", () => ({ value: true }));
	on("fs.stat", ($, e) => {
		const text = world.transcripts.get(e.path);
		if (text === undefined) throw new Error(`ENOENT: ${e.path}`);
		return { value: { kind: "file", size: text.length, mtimeMs: 0, isLink: false } };
	});
	on("ui.open", ($, e) => {
		world.opened.push(e.id);
		return { value: { isPlaced: true } };
	});
	on("ui.close", ($, e) => {
		world.closed.push(e.id);
		return { value: undefined };
	});
	on("command.run", ($, e) => {
		world.skill.push(`${e.command} ${e.args}`);
		return {};
	});
	on("ui.status", ($, e) => {
		world.status = e.text;
		return { value: undefined };
	});
	on("ui.log", ($, e) => {
		world.logged.push(e.text);
		return { value: undefined };
	});
	on("ui.render", ($, e) => $.ui.resolve(e).Box({})); // the engine's own drawing: nothing
	on("ui.toast", ($, e) => {
		world.toasts.push(e.text);
		return { value: undefined };
	});
	on("tool.call", ($, e) => world.bash(e) as any);
	on("prompt.submit", ($, e) => {
		world.submitted.push(e.text);
		return { text: e.text };
	});
	on("session.start", ($, e) => ({ cwd: e.cwd }));
	on("session.end", ($, e) => ({ sessionId: e.sessionId }));
	on("turn.start", ($, e) => ({ turnId: e.turnId }));
	on("turn.complete", () => ({ text: "" }));
	on("turn.step", async function* ($, e) {
		return { turnId: e.turnId, index: e.index, answer: "", toolUses: [], stopReason: "end_turn", usage: null } as any; // a model request, answered with nothing
	});
	return { clock, session, world };
}

const worker = (id: number, state: string, extra: Record<string, unknown> = {}): W => ({
	id,
	role: "task",
	harness: "claude",
	state,
	caller: { harness: "claude", session: S1 },
	created_at: "2026-10-13T10:00:00Z",
	...extra,
});

const wakes = (submitted: readonly string[]) => submitted.filter((t) => t.includes("[routr]"));
let turns = 0;
const turnStart = ($: any) => $.turn.start({ text: "go", turnId: `t${++turns}` });
const turnEnd = ($: any, reason = "answer") =>
	$.turn.complete({ answer: "", durationMs: 1, isAborted: reason === "aborted", turnId: `t${turns}`, reason });

async function start($: any, h: ReturnType<typeof host>, workers: W[]) {
	h.world.workers = workers;
	await $.session.start({ cwd: "/repo", surface: "terminal", isInteractive: true });
	await h.clock.settle(); // the baseline snapshot
}

test("a worker this session started wakes the idle model once, with its result", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(7, "running"), worker(8, "running", { caller: { harness: "claude", session: "other" } })]);
	expect(h.world.status).toBe("▶2"); // every worker on the cards, as Pi counts them
	h.world.workers = [worker(7, "done"), worker(8, "done", { caller: { harness: "claude", session: "other" } })];
	h.world.results.set(7, "Two findings.\nverdict: fail\n");
	await h.clock.advance(POLL);
	expect(wakes(h.world.submitted)).toEqual([expect.stringContaining("[routr] worker 7 · task · claude is done")]);
	expect(h.world.submitted[0]).toContain("Two findings.");
	await h.clock.advance(3 * POLL);
	expect(wakes(h.world.submitted).length).toBe(1);
});

test("news during a turn waits for its end; a question goes into the turn at once", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "running"), worker(6, "running")]);
	await turnStart($);
	h.world.workers = [worker(5, "blocked", { question: "Proceed?\n1. Yes\n2. No", options: ["Yes", "No"], fingerprint: "f5" }), worker(6, "done")];
	await h.clock.advance(POLL);
	expect(wakes(h.world.submitted)).toEqual([]);
	const appended = JSON.stringify(h.session.appended());
	expect(appended).toContain("worker 5");
	expect(appended).toContain("fake routr: the user answers worker 5's question.");
	expect(appended).toContain("/routr answer 5 N");
	for await (const _ of $.turn.step({ turnId: `t${turns}`, index: 1, model: "m", messageCount: 3 } as any)); // the model's next request reads it
	await turnEnd($);
	await h.clock.advance(POLL);
	expect(wakes(h.world.submitted)).toEqual([expect.stringContaining("[routr] worker 6 · task · claude is done")]);
	expect(wakes(h.world.submitted)[0]).not.toContain("worker 5"); // the model read the question: not sent again
});

test("a question put into a turn's last request is sent again when the turn ends", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "running")]);
	await turnStart($);
	h.world.workers = [worker(5, "blocked", { question: "Proceed?\n1. Yes\n2. No", options: ["Yes", "No"], fingerprint: "f5" })];
	await h.clock.advance(POLL);
	await turnEnd($); // no model request after the append
	await h.clock.advance(POLL);
	expect(wakes(h.world.submitted)).toEqual([expect.stringContaining("[routr] worker 5 · task · claude is blocked")]);
});

test("after Esc nothing is sent until the user's next turn ends", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "running")]);
	await turnStart($);
	h.world.workers = [worker(5, "done")];
	await h.clock.advance(POLL);
	await turnEnd($, "aborted");
	await h.clock.advance(3 * POLL);
	expect(wakes(h.world.submitted)).toEqual([]);
	await turnStart($);
	await turnEnd($);
	await h.clock.advance(POLL);
	expect(wakes(h.world.submitted).length).toBe(1);
});

test("a foreground routr wait hands the model the result itself: no wake", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "running")]);
	h.world.bash = async () => {
		h.world.workers = [worker(5, "done")]; // finishes while the wait runs
		await h.clock.sleep(3 * POLL);
		return { result: { stdout: "Done.", stderr: "", interrupted: false }, text: "Done." };
	};
	await turnStart($);
	const call = $.tool.call({ tool: "Bash", command: "routr wait 5", description: "wait" } as any);
	await h.clock.advance(4 * POLL);
	await call;
	await turnEnd($);
	await h.clock.advance(2 * POLL);
	expect(wakes(h.world.submitted)).toEqual([]);
});

test("a background routr wait covers its worker until its task notification", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "running"), worker(6, "running")]);
	h.world.bash = (e: any) => {
		const id = e.command.includes(" 5") ? "b5" : "b6";
		const text = `Command running in background with ID: ${id}. Output is being written to: /tmp/${id}.output.`;
		return { result: { stdout: text, stderr: "", interrupted: false }, text };
	};
	await $.tool.call({ tool: "Bash", command: "routr wait 5", run_in_background: true, description: "wait" } as any);
	await $.tool.call({ tool: "Bash", command: "routr wait 6", run_in_background: true, description: "wait" } as any);
	h.world.workers = [worker(5, "done"), worker(6, "done")];
	await h.clock.advance(2 * POLL);
	expect(wakes(h.world.submitted)).toEqual([]);
	const notice = (id: string, status: string) => ({
		message: { type: "user", content: [{ type: "text", text: `<task-notification> <task-id>${id}</task-id> <status>${status}</status> </task-notification>` }] },
		door: "prompt",
		origin: { kind: "task-notification" },
		uuid: `u-${id}`,
	});
	await $.session.append(notice("b5", "completed") as any);
	await $.session.append(notice("b6", "failed") as any); // routr wait's exit 3: it returned nothing
	await h.clock.advance(2 * POLL);
	expect(wakes(h.world.submitted)).toEqual([expect.stringContaining("[routr] worker 6 ")]);
});

test("after /clear the new conversation wakes only for the workers it starts", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "running")]);
	await $.session.end({ reason: "clear", sessionId: S1, resume: {} } as any);
	h.world.session = S2;
	h.world.workers = [worker(5, "done"), worker(9, "running", { caller: { harness: "claude", session: S2 } })];
	await h.clock.advance(POLL);
	h.world.workers = [worker(5, "done"), worker(9, "done", { caller: { harness: "claude", session: S2 } })];
	await h.clock.advance(POLL);
	expect(wakes(h.world.submitted)).toEqual([expect.stringContaining("[routr] worker 9 ")]);
});

test("/routr list and result show the CLI's output to the user, not the model; other words run the skill", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "done")]);
	h.world.results.set(5, "All done.");
	await $.command.run({ command: "routr", args: "result 5" } as any);
	expect(h.world.logged.at(-1)).toBe("$ routr result 5\nAll done.");
	await $.command.run({ command: "routr:routr", args: "" } as any);
	expect(h.world.logged.at(-1)).toContain("$ routr list");
	expect(h.world.skill).toEqual([]);
	await $.command.run({ command: "routr:routr", args: "have codex review PLAN.md" } as any);
	await h.clock.settle();
	expect(h.world.skill).toEqual(["routr:routr have codex review PLAN.md"]);
	expect(h.world.submitted).toEqual([]);
});

test("the system prompt says the plugin wakes the model", async ($, on) => {
	on("prompt.compose", () => ({ sections: [{ id: "core", text: "You are Claude.", scope: "shared" }] }));
	host(on);
	const r = await $.prompt.compose({ model: "m", promptModel: "m", surfaces: ["terminal"], tools: [], outputStyle: null, traits: [] } as any);
	expect(r.sections.map((s: any) => s.id)).toEqual(["core", "routr:wake"]);
	expect(r.sections[1]?.text).toContain("start routr workers without --wait");
});

const BAND = { hasSurvey: false, isWorking: false, maxRows: 20, bodyColumns: 80, scroll: { offset: 0, bodyRows: 20 }, view: {} };
const PANE = { title: "", isFocused: true, bodyColumns: 80, placement: "inline" as const, scroll: { offset: 0, bodyRows: 20 }, view: {} };
const asking = (id: number, fingerprint: string, extra: Record<string, unknown> = {}) =>
	worker(id, "blocked", { question: "Do you want to proceed?\n1. Yes\n2. No", options: ["Yes", "No"], fingerprint, pending: { summary: "Bash: go test ./..." }, ...extra });
const toolUse = (command: string) =>
	JSON.stringify({ type: "assistant", message: { model: "m", content: [{ type: "tool_use", name: "Bash", input: { command } }] } }) + "\n";
const picker = ($: any) => $.ui.mount({ plugin: "routr", surface: "terminal", component: "Pane", requestId: "routr-answer", props: PANE });

test("the band shows the steps, each working worker's last tool call, and a button for each question", async ($, on) => {
	const h = host(on);
	h.world.transcripts.set("/t/5.jsonl", toolUse("rg -n auth src/") + toolUse("go vet ./..."));
	await start($, h, [worker(5, "running", { title: "review", transcript: "/t/5.jsonl" }), asking(6, "f6")]);
	await h.clock.advance(1000); // a tail tick
	const band = await $.ui.mount({ plugin: "routr", surface: "terminal", component: "AbovePrompt", props: BAND });
	expect(await band.find({ type: "Text", text: /5 review/ })).toBeDefined();
	expect(await band.find({ type: "Text", text: /go vet/ })).toBeDefined();
	expect(await band.find({ type: "Text", text: /rg -n auth/ })).toBeUndefined(); // the last call only, as Pi's steps
	expect((await band.find({ key: "answer-6" }))?.text).toContain("answer 6");
	h.world.transcripts.set("/t/5.jsonl", toolUse("rg -n auth src/") + toolUse("go vet ./...") + toolUse("go test ./..."));
	await h.clock.advance(1000);
	expect(await band.find({ type: "Text", text: /go test/ })).toBeDefined();
	await band.press({ key: "answer-6" });
	expect(h.world.opened).toContain("routr-answer");
});

test("a question answered through the picker presses the option on the question the person read", async ($, on) => {
	const h = host(on);
	await start($, h, [asking(5, "f5")]);
	await $.command.run({ command: "routr", args: "answer" } as any);
	expect(h.world.opened).toEqual(["routr-answer", "routr-answer"]); // at once, then again as tall as the question
	const ui = await picker($);
	expect(await ui.find({ type: "Text", text: /5 task · claude asks about Bash: go test/ })).toBeDefined();
	expect((await ui.find({ key: "option-1" }))?.text).toContain("Yes");
	expect((await ui.find({ key: "option-2" }))?.text).toContain("No");
	expect(await ui.find({ type: "Text", text: /^1\. Yes/ })).toBeUndefined(); // the options are buttons, not question text
	await ui.press({ key: "option-1" });
	expect(h.world.calls).toContain("answer 5 1 --by human --expect f5");
	expect(h.world.closed).toContain("routr-answer");
	expect(h.world.toasts.at(-1)).toBe("answered 5 with option 1, by human");
});

test("--expect refuses an answer to a question that changed under the picker", async ($, on) => {
	const h = host(on);
	await start($, h, [asking(5, "f5")]);
	await $.command.run({ command: "routr", args: "answer 5" } as any);
	const ui = await picker($);
	h.world.workers = [asking(5, "f6", { question: "Run rm -rf build?\n1. Yes\n2. No" })]; // before the next poll
	await ui.press({ key: "option-1" });
	expect(h.world.calls).toContain("answer 5 1 --by human --expect f5");
	expect(h.world.logged.at(-1)).toBe("5 asks another question now, so nothing was pressed. /routr answer shows it.");
});

test("the picker closes by itself when its question goes", async ($, on) => {
	const h = host(on);
	await start($, h, [asking(5, "f5")]);
	await $.command.run({ command: "routr", args: "answer" } as any);
	h.world.workers = [worker(5, "running")]; // answered in its tab
	await h.clock.advance(2000);
	expect(h.world.closed).toContain("routr-answer");
	expect(h.world.toasts).toContain("5 asks nothing now; nothing was pressed.");
	expect(h.world.calls.filter((c) => c.startsWith("answer"))).toEqual([]);
});

test("with several questions the picker asks which worker first", async ($, on) => {
	const h = host(on);
	await start($, h, [asking(5, "f5"), asking(6, "f6", { title: "plan" })]);
	await $.command.run({ command: "routr", args: "answer" } as any);
	const ui = await picker($);
	expect((await ui.find({ key: "worker-6" }))?.text).toContain("6 plan · claude asks about Bash");
	await ui.press({ key: "worker-6" });
	await ui.press({ key: "option-2" });
	expect(h.world.calls).toContain("answer 6 2 --by human --expect f6");
});

test("/routr view cards puts the workers in a pane, and steps back in the band", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "running", { title: "review" })]);
	await $.command.run({ command: "routr", args: "view cards" } as any);
	expect(h.world.opened).toEqual(["routr-cards"]);
	const pane = await $.ui.mount({ plugin: "routr", surface: "terminal", component: "Pane", requestId: "routr-cards", props: PANE });
	expect(await pane.find({ type: "Text", text: /routr · 1 running/ })).toBeDefined();
	const band = await $.ui.mount({ plugin: "routr", surface: "terminal", component: "AbovePrompt", props: BAND });
	expect(await band.find({ type: "Text", text: /5 review/ })).toBeUndefined();
	await $.command.run({ command: "routr", args: "view steps" } as any);
	expect(h.world.closed).toEqual(["routr-cards"]);
	expect(await band.find({ type: "Text", text: /5 review/ })).toBeDefined();
});

test("/routr approve, cancel and the rest run the CLI and show its output", async ($, on) => {
	const h = host(on);
	await start($, h, [worker(5, "gated")]);
	await $.command.run({ command: "routr", args: "approve 5" } as any);
	expect(h.world.calls).toContain("approve 5");
	expect(h.world.logged.at(-1)).toContain("$ routr approve 5");
	await $.command.run({ command: "routr", args: "cancel" } as any);
	expect(h.world.logged.at(-1)).toBe("usage: /routr cancel ID");
});
