// The extension end to end against a mock ExtensionAPI and test/fake-routr:
// no Pi, no model. The live run in a real terminal is test/smoke.zsh.

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { dirname, join } from "node:path";
import { fileURLToPath } from "node:url";

const here = dirname(fileURLToPath(import.meta.url));
const fake = join(here, "fake-routr");

type Handler = (event: any, ctx: any) => any;

function mockPi() {
	const handlers = new Map<string, Handler[]>();
	const sent: { message: any; options: any }[] = [];
	const entries: { customType: string; data: any }[] = [];
	const commands = new Map<string, any>();
	const shortcuts = new Map<string, any>();
	const renderers = new Map<string, any>();
	const pi = {
		on: (name: string, h: Handler) => handlers.set(name, [...(handlers.get(name) ?? []), h]),
		sendMessage: (message: any, options: any) => sent.push({ message, options }),
		sendUserMessage: (text: string) => sent.push({ message: { user: text }, options: {} }),
		appendEntry: (customType: string, data: any) => entries.push({ customType, data }),
		registerCommand: (name: string, c: any) => commands.set(name, c),
		registerShortcut: (key: string, s: any) => shortcuts.set(key, s),
		registerMessageRenderer: (t: string, r: any) => renderers.set(t, r),
		registerEntryRenderer: (t: string, r: any) => renderers.set(t, r),
	};
	const fire = (name: string, event: any, ctx: any) => (handlers.get(name) ?? []).map((h) => h(event, ctx));
	return { pi, fire, sent, entries, commands, shortcuts, renderers };
}

/** A ui.select on screen: the test answers it with pick, or the extension closes it (closed). */
interface Shown {
	title: string;
	items: string[];
	pick: (label: string | undefined) => void;
	closed: boolean;
}

function mockCtx(cwd: string) {
	const widgets = new Map<string, any>();
	const status = new Map<string, string | undefined>();
	const notes: string[] = [];
	const selects: Shown[] = [];
	const theme = { fg: (_c: string, t: string) => t, bold: (t: string) => t, bg: (_c: string, t: string) => t };
	const ctx = {
		cwd,
		mode: "tui",
		hasUI: true,
		isIdle: () => true,
		sessionManager: { getSessionId: () => "S1", getEntries: () => [] },
		ui: {
			theme,
			setWidget: (k: string, v: any) => widgets.set(k, v),
			setStatus: (k: string, v: string | undefined) => status.set(k, v),
			notify: (m: string) => notes.push(m),
			select: (title: string, items: string[], opts?: { signal?: AbortSignal }) =>
				new Promise<string | undefined>((pick) => {
					const s: Shown = { title, items, pick, closed: false };
					selects.push(s);
					opts?.signal?.addEventListener("abort", () => ((s.closed = true), pick(undefined)), { once: true });
				}),
		},
	};
	const widgetText = (): string => {
		const f = widgets.get("routr");
		return f ? f(null, theme).render(200).join("\n") : "";
	};
	return { ctx, widgets, status, notes, selects, widgetText, theme };
}

let debug: (() => string) | undefined;

async function until<T>(what: string, f: () => T | undefined, ms = 20_000): Promise<T> {
	const end = Date.now() + ms;
	for (;;) {
		const v = f();
		if (v) return v;
		if (Date.now() > end) throw new Error(`timed out waiting for ${what}${debug ? `\n${debug()}` : ""}`);
		await new Promise((r) => setTimeout(r, 100));
	}
}

test("cards, claims and one wake for the worker this session started", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "9" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "9", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards" });
	execFileSync(fake, ["seed"], { cwd, env }); // 2: done long ago, 3: another session's, running

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));

	const skills = m.fire("resources_discover", {}, u.ctx)[0];
	assert.match(skills.skillPaths[0], /skills\/routr$/, "ships the routr skill");
	const opts = { sections: { other: "KEEP" } as Record<string, string> };
	const ret = await m.fire("before_agent_start", { systemPrompt: "BASE", systemPromptOptions: opts }, u.ctx)[0];
	assert.equal(ret, undefined, "a section, never a whole systemPrompt: Pi keeps the cached prefix");
	assert.equal(opts.sections.other, "KEEP");
	assert.match(opts.sections.routr!, /^The routr extension is active/);
	assert.match(opts.sections.routr!, /Leave out --permissions: .* routr gives your workers bypass\.$/, "Pi's workers run as Pi does");

	debug = () => `widget:\n${u.widgetText()}\nnotes: ${u.notes.join(" | ")}`;
	m.fire("session_start", { reason: "startup" }, u.ctx);
	await until("the seeded cards", () => u.widgetText().includes("3 research"));
	assert.doesNotMatch(u.widgetText(), /2 task/, "finished long ago: no card");

	// The model runs `routr start` through bash; the extension claims the printed id.
	m.fire("agent_start", {}, u.ctx);
	const out = execFileSync(fake, ["start", "--role", "review", "Red-team PLAN.md"], { cwd, env, encoding: "utf8" });
	assert.equal(out.trim(), "7");
	m.fire(
		"tool_result",
		{ toolName: "bash", input: { command: "routr start --role review 'Red-team PLAN.md'" }, content: [{ type: "text", text: out }] },
		u.ctx,
	);
	assert.deepEqual(m.entries.at(-1), { customType: "routr-claims", data: { ids: [7], chains: [], group: "run:1" } });
	m.fire("agent_end", { messages: [{ role: "assistant", stopReason: "stop" }] }, u.ctx); // it ends its turn, as told

	const card = await until("the live tool calls on the card", () => {
		const t = u.widgetText();
		return /7 review · codex·gpt-6-astra·high · running/.test(t) && /⎿ Read {2}PLAN\.md/.test(t) ? t : undefined;
	});
	assert.match(card, /^routr · 2 running/);
	assert.match(u.status.get("routr") ?? "", /routr ▶2/);

	const wake = await until("the wake message", () => m.sent.find((s) => s.message.customType === "routr-wake"));
	assert.deepEqual(wake.options, { triggerTurn: true, deliverAs: "followUp" });
	assert.match(wake.message.content, /^\[routr\] worker 7 · review · codex·gpt-6-astra·high is done, verdict fail\./);
	assert.match(wake.message.content, /Two findings; one blocks implementation\.[\s\S]*output: REDTEAM\.md\nverdict: fail/);
	assert.deepEqual(wake.message.details.entries.map((e: any) => e.id), [7]);

	// No second wake, and none for workers 2 and 3.
	await new Promise((r) => setTimeout(r, 3500));
	assert.equal(m.sent.filter((s) => s.message.customType === "routr-wake").length, 1);

	// The wake renders collapsed as a one-line card.
	const text = m.renderers.get("routr-wake")(wake.message, { expanded: false }, u.theme).render(200).join("\n");
	assert.match(text, /◆ routr 7 review codex·gpt-6-astra·high → done · verdict fail/);
	assert.match(text, /⎿ Two findings; one blocks implementation\./);

	// /routr conveniences call the CLI; free text goes to the model with the skill.
	const cmd = m.commands.get("routr");
	await cmd.handler("answer 7 2", u.ctx);
	assert.match(u.notes.at(-1)!, /routr answer 7 2 --by human: ok: answer 7 2/, "not blocked: no fingerprint to expect");
	await cmd.handler("answer 7", u.ctx);
	assert.equal(u.notes.at(-1)!, "routr: 7 asks you nothing");
	await cmd.handler("answer x", u.ctx);
	assert.match(u.notes.at(-1)!, /usage: \/routr answer \[ID \[N\]\]/);
	assert.equal(u.selects.length, 0);
	await cmd.handler("list", u.ctx);
	assert.equal(m.entries.at(-1)!.customType, "routr-output");
	await cmd.handler("have codex red-team PLAN.md", u.ctx);
	assert.deepEqual(m.sent.at(-1)!.message, { user: "Use the routr skill to do this: have codex red-team PLAN.md" });

	m.fire("session_shutdown", {}, u.ctx);
	assert.equal(u.widgets.get("routr"), undefined, "widget cleared");
});

test("workers recorded to this session are mine; a running routr wait holds their wake", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "3", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "3", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards" });
	execFileSync(fake, ["list"], { cwd, env }); // creates .routr/state

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	const sleep = (ms: number) => new Promise((r) => setTimeout(r, ms));
	const bash = (id: string, command: string, text?: string) => {
		m.fire("tool_call", { toolName: "bash", toolCallId: id, input: { command } }, u.ctx);
		if (text === undefined) return;
		m.fire("tool_result", { toolName: "bash", toolCallId: id, input: { command }, content: [{ type: "text", text }] }, u.ctx);
		m.fire("tool_execution_end", { toolCallId: id }, u.ctx);
	};
	debug = () => `widget:\n${u.widgetText()}\nwakes: ${JSON.stringify(wakes().map((s) => s.message.content))}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await sleep(300); // the baseline snapshot
	m.fire("agent_start", {}, u.ctx);

	// Started in a way the claims can't read: the CLI's caller record makes it mine.
	execFileSync(fake, ["start", "--role", "review", "x"], { cwd, env, stdio: "ignore" });
	bash("c1", "routr start --role review x >/dev/null 2>&1", "");
	assert.equal(m.entries.filter((e) => e.customType === "routr-claims").length, 0, "nothing claimed from the output");
	// The model waits on it; it finishes while the wait runs.
	bash("c2", "routr wait 7");
	await until("7 done on its card", () => /7 review .* done/.test(u.widgetText()));
	await sleep(2000);
	assert.equal(wakes().length, 0, "no wake while the wait that returns it runs");
	m.fire("tool_result", { toolName: "bash", toolCallId: "c2", input: { command: "routr wait 7" }, content: [{ type: "text", text: "Two findings.\nverdict: fail" }] }, u.ctx);
	m.fire("tool_execution_end", { toolCallId: "c2" }, u.ctx);
	await sleep(2000);
	assert.equal(wakes().length, 0, "the wait returned it: no wake");
	m.fire("agent_end", { messages: [] }, u.ctx);

	// A wait that ends without returning the result (aborted): wake then, once the turn is over.
	m.fire("agent_start", {}, u.ctx);
	execFileSync(fake, ["start", "--role", "review", "y"], { cwd, env, stdio: "ignore" });
	bash("c3", "routr start --role review y >/dev/null 2>&1", "");
	bash("c4", "routr wait 8");
	await until("8 done on its card", () => /8 review .* done/.test(u.widgetText()));
	await sleep(2000);
	assert.equal(wakes().length, 0);
	m.fire("tool_execution_end", { toolCallId: "c4" }, u.ctx);
	m.fire("agent_end", { messages: [] }, u.ctx);
	const wake = await until("the wake for 8", () => wakes()[0]);
	assert.deepEqual(wake.message.details.entries.map((e: any) => e.id), [8]);
	await sleep(2000);
	assert.equal(wakes().length, 1);
});

test("news during a turn waits for its end; after the user interrupts it, until their next turn ends", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "2", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "2", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards" });
	execFileSync(fake, ["list"], { cwd, env }); // creates .routr/state

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	debug = () => `widget:\n${u.widgetText()}\nwakes: ${wakes().length}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await new Promise((r) => setTimeout(r, 300)); // the baseline snapshot
	m.fire("agent_start", {}, u.ctx);
	execFileSync(fake, ["start", "--role", "review", "x"], { cwd, env, stdio: "ignore" });
	await until("7 done on its card", () => /7 review .* done/.test(u.widgetText()));
	await new Promise((r) => setTimeout(r, 1500));
	assert.equal(wakes().length, 0, "the turn runs: the news waits");
	m.fire("agent_end", { messages: [{ role: "user" }, { role: "assistant", stopReason: "aborted" }] }, u.ctx);
	await new Promise((r) => setTimeout(r, 2000));
	assert.equal(wakes().length, 0, "the user interrupted it: they are about to type");
	m.fire("agent_start", {}, u.ctx);
	m.fire("agent_end", { messages: [{ role: "assistant", stopReason: "stop" }] }, u.ctx);
	const wake = await until("the wake, once their next turn ended", () => wakes()[0]);
	assert.deepEqual(wake.options, { triggerTurn: true, deliverAs: "followUp" });
	assert.deepEqual(wake.message.details.entries.map((e: any) => e.id), [7]);
});

test("a wake whose delivery threw is delivered on the next refresh", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "2", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "2", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards" });
	execFileSync(fake, ["list"], { cwd, env }); // creates .routr/state

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	let fail = 1;
	const send = m.pi.sendMessage;
	m.pi.sendMessage = (message: any, options: any) => {
		if (message.customType === "routr-wake" && fail-- > 0) throw new Error("session busy");
		return send(message, options);
	};
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	debug = () => `widget:\n${u.widgetText()}\nnotes: ${u.notes.join(" | ")}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await new Promise((r) => setTimeout(r, 300)); // the baseline snapshot
	m.fire("agent_start", {}, u.ctx);
	execFileSync(fake, ["start", "--role", "review", "x"], { cwd, env, stdio: "ignore" });
	m.fire("agent_end", { messages: [] }, u.ctx);
	await until("the failed delivery", () => u.notes.find((n) => /could not deliver a result: session busy; retrying/.test(n)));
	const wake = await until("the wake, on the retry", () => wakes()[0]);
	assert.deepEqual(wake.message.details.entries.map((e: any) => e.id), [7]);
	await new Promise((r) => setTimeout(r, 2000));
	assert.equal(wakes().length, 1, "delivered once");
});

test("the steps view: a chain as one row per step, /routr view switches to cards", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "2", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "2", ROUTR_PI_WAKE: "off", ROUTR_PI_VIEW: "steps" });
	execFileSync(fake, ["list"], { cwd, env }); // creates .routr/state

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	debug = () => `widget:\n${u.widgetText()}\nnotes: ${u.notes.join(" | ")}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await new Promise((r) => setTimeout(r, 300));
	assert.equal(execFileSync(fake, ["flow", "start", "demo"], { cwd, env, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim(), "demo-1");

	const before = await until("the chain, implement not expanded yet", () => {
		const w = u.widgetText();
		return /▶ plan/.test(w) && /○ implement ×\?/.test(w) ? w : undefined;
	});
	assert.match(before, /^! demo-1 · 0\/4 steps/);
	assert.match(before, /⏸ merge .*\n\s+⎿ waits at a gate → \/routr approve 13/);
	const fanned = await until("implement as ×3, two at once", () => {
		const w = u.widgetText();
		return /▶ implement ×3 +▶2 ○1 · max 2 at once/.test(w) ? w : undefined;
	});
	assert.match(fanned, /✔ plan .*pass/);
	assert.match(u.status.get("routr") ?? "", /routr ▶\d+ !1/, "the footer count is the same in both views");

	const cmd = m.commands.get("routr");
	assert.deepEqual(cmd.getArgumentCompletions("view s"), [{ value: "view steps", label: "steps" }]);
	assert.deepEqual(cmd.getArgumentCompletions("vi").map((i: any) => i.value), ["view"]);
	await cmd.handler("view cards", u.ctx);
	assert.match(u.notes.at(-1)!, /showing workers as cards/);
	assert.match(u.widgetText(), /^routr · /);
	assert.match(u.widgetText(), /\d+ implement \[demo-1\/implement\]/);
	await cmd.handler("view", u.ctx);
	assert.match(u.widgetText(), /^! demo-1/, "no argument toggles");
});

test("a blocked worker shows what it asks about and the key that answers it, and every row stays one row", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "30", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "30", ROUTR_PI_WAKE: "off", ROUTR_PI_VIEW: "steps" });
	execFileSync(fake, ["list"], { cwd, env });

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	debug = () => `widget:\n${u.widgetText()}\nnotes: ${u.notes.join(" | ")}`;
	const rows = (): string[] => u.widgets.get("routr")?.(null, u.theme).render(200) ?? [];

	m.fire("session_start", { reason: "startup" }, u.ctx);
	execFileSync(fake, ["flow", "start", "demo"], { cwd, env, stdio: "ignore" });
	// the fake's ask blocks the plan step on Claude's permission prompt, a question over four lines
	await until("plan blocked", () => {
		try {
			return execFileSync(fake, ["ask", "7"], { cwd, env, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim() === "blocked 7";
		} catch {
			return undefined;
		}
	});
	// the call it is about, clipped so the key stays on screen
	const hint = /⎿ ⚠ claude 7 asks about Bash: ls \*\.md \| xargs -I \{\} sh -c 'echo "═══ \{\} ═══" && cat … · alt\+q answer$/;
	const blocked = await until("the hint row", () => (rows().some((l) => hint.test(l)) ? rows() : undefined));
	assert.ok(blocked.some((l) => /^\s+! plan /.test(l)));
	assert.ok(blocked.every((l) => !/[\n\r]/.test(l)), "no line breaks inside a row");
	assert.ok(blocked.every((l) => !/proceed|Yes/.test(l)), "the call it is about, not the screen's words");
	assert.match(u.status.get("routr") ?? "", / !2$/, "the question and the merge gate");
	await m.commands.get("routr").handler("view cards", u.ctx);
	assert.ok(rows().some((l) => hint.test(l)), "cards say the same");
	assert.ok(rows().every((l) => !/[\n\r]|proceed/.test(l)));
	assert.equal(u.selects.length, 0, "nothing opens by itself");

	execFileSync(fake, ["answer", "7", "1"], { cwd, env, stdio: "ignore" });
	await until("plan running again", () => (rows().some((l) => /^.*● 7 plan .*running/.test(l)) ? true : undefined));
	assert.match(u.status.get("routr") ?? "", / !1$/, "only the merge gate still needs you");
});

test("a blocked worker this session started wakes the model at once, with routr's word on who answers", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "30", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "30", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards" });
	execFileSync(fake, ["list"], { cwd, env }); // creates .routr/state

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	debug = () => `widget:\n${u.widgetText()}\nwakes: ${JSON.stringify(wakes().map((s) => s.message.content))}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await new Promise((r) => setTimeout(r, 300)); // the baseline snapshot
	m.fire("agent_start", {}, u.ctx);
	execFileSync(fake, ["start", "--answer", "agent", "x"], { cwd, env, stdio: "ignore" });
	await until("7 running on its card", () => /7 task .* running/.test(u.widgetText()));
	execFileSync(fake, ["ask", "7"], { cwd, env, stdio: "ignore" });
	const wake = await until("the wake for the question", () => wakes()[0]);
	assert.deepEqual(wake.options, { triggerTurn: true, deliverAs: "steer" });
	const text: string = wake.message.content;
	assert.match(text, /^\[routr\] worker 7 · task · claude·sonnet·medium is blocked: Do you want to proceed\?\n1\. Yes\n/);
	assert.match(text, /\nIt asks about Bash: ls \*\.md \| xargs/, "the call it is about, from pending.summary");
	const dot = env.ROUTR_STATE_DIR;
	assert.ok(
		text.endsWith(
			"\nfake routr: agent answers worker 7's question.\n" +
				"fake routr: routr answer 7 N --expect 6a128b4273b95faa\n" +
				"The user can also type `/routr answer 7 N` here.",
		),
		"who answers: routr's lines from routr show 7 --context --json, then /routr answer",
	);
	const calls = readFileSync(join(dot, "fake-calls.log"), "utf8");
	assert.match(calls, / show 7 --context --json\n/);
});

test("alt+q answers a worker's question: options, its tab, later; it closes when the question goes or changes", { timeout: 120_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "300", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "300", ROUTR_PI_WAKE: "off", ROUTR_PI_VIEW: "steps" });
	execFileSync(fake, ["list"], { cwd, env });

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const dot = env.ROUTR_STATE_DIR;
	const fakeRun = (...args: string[]) => execFileSync(fake, args, { cwd, env, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] }).trim();
	const calls = () => readFileSync(join(dot, "fake-calls.log"), "utf8");
	const cmd = m.commands.get("routr");
	const altQ = () => m.shortcuts.get("alt+q").handler(u.ctx) as Promise<void>;
	const shown = (n: number) => until(`dialog ${n}`, () => u.selects[n - 1]);
	const hinted = (what: RegExp) => until(`the hint ${what}`, () => (new RegExp(`⎿ ⚠ claude 7 ${what.source}.* · alt\\+q answer`).test(u.widgetText()) ? true : undefined));
	const running = () => until("7 running again", () => (/▶ 7 task/.test(u.widgetText()) ? true : undefined));
	debug = () => `widget:\n${u.widgetText()}\nnotes: ${u.notes.join(" | ")}\ndialogs: ${JSON.stringify(u.selects.map((s) => [s.title, s.items, s.closed]))}`;

	assert.deepEqual([...m.shortcuts.keys()], ["alt+q"]);
	m.fire("session_start", { reason: "startup" }, u.ctx);
	execFileSync(fake, ["start", "x"], { cwd, env, stdio: "ignore" });
	await until("7 blocked", () => {
		try {
			return fakeRun("ask", "7") === "blocked 7";
		} catch {
			return undefined; // still starting
		}
	});
	await hinted(/asks about Bash: ls \*\.md/);
	const p1 = altQ();
	const one = await shown(1);
	assert.equal(one.title, `7 task · claude asks about Bash: ls *.md | xargs -I {} sh -c 'echo "═══ {} ═══" && cat {}'\nDo you want to proceed?`);
	assert.deepEqual(one.items, [
		"1. Yes",
		"2. Yes, and don't ask again for: xargs -I {} sh -c 'echo \"═══ {} ═══\" && cat {}' (lasts past this call)",
		"3. No, and tell Claude what to do differently (esc)",
		"Open its tab",
		"Later",
	]);
	assert.match(calls(), / show 7 --context --json\n/, "the options that last, from routr");
	one.pick("1. Yes");
	await p1;
	await until("the answer", () => (/ answer 7 1 --by human --expect 6a128b4273b95faa\n/.test(calls()) ? true : undefined));
	await running();
	assert.equal(u.notes.at(-1), "routr: ok: answer 7 1");

	// Answered in its tab while the picker is open (/routr answer, no arguments): it closes.
	fakeRun("ask", "7");
	await hinted(/asks about Bash/);
	const p2 = cmd.handler("answer", u.ctx);
	const two = await shown(2);
	fakeRun("answer", "7", "1");
	await until("dialog 2 closed", () => (two.closed ? true : undefined));
	await p2;
	assert.equal(u.notes.at(-1), "routr: 7 asks nothing now; nothing was pressed.");
	await running();

	// Another question while it is open: it closes, and says so.
	fakeRun("ask", "7");
	await hinted(/asks about Bash/);
	const p3 = altQ();
	const three = await shown(3);
	fakeRun("ask", "7", "--menu", "write");
	await until("dialog 3 closed", () => (three.closed ? true : undefined));
	await p3;
	assert.equal(u.notes.at(-1), "routr: 7 asks another question now. alt+q shows it.");
	await hinted(/asks about Write: \/.*notes\.md/);

	// The screen changes after it opened, before routr records it: the answer presses nothing (exit 4).
	const p4 = altQ();
	const four = await shown(4);
	assert.match(four.title, /^7 task · claude asks about Write: \/.*notes\.md\nDo you want to create notes\.md\?$/);
	assert.equal(four.items[1], "2. Yes, allow all edits during this session (shift+tab) (lasts past this call)");
	fakeRun("ask", "7", "--menu", "bash", "--screen-only");
	four.pick("1. Yes");
	await p4;
	assert.match(calls(), / answer 7 1 --by human --expect c0ffee00c0ffee00\n/);
	assert.equal(u.notes.at(-1), "routr: 7 asks another question now, so nothing was pressed. alt+q shows it.");

	// Typed: /routr answer ID N passes the fingerprint the extension last saw.
	fakeRun("ask", "7"); // routr records what the screen shows
	await hinted(/asks about Bash/);
	fakeRun("ask", "7", "--menu", "write", "--screen-only");
	await cmd.handler("answer 7 3", u.ctx);
	assert.match(calls(), / answer 7 3 --by human --expect 6a128b4273b95faa\n/);
	assert.equal(u.notes.at(-1), "routr: 7 asks another question now, so nothing was pressed. alt+q shows it.");
	fakeRun("ask", "7", "--menu", "bash"); // the screen and routr agree again
	await cmd.handler("answer 7 3", u.ctx);
	assert.match(u.notes.at(-1)!, /^routr answer 7 3 --by human --expect 6a128b4273b95faa: ok: answer 7 3$/);
	await running();

	// No menu: only its tab and later.
	fakeRun("ask", "7", "--menu", "none");
	await hinted(/asks: What should the file be called\?/);
	const p5 = altQ();
	const five = await shown(5);
	assert.equal(five.title, "7 task · claude asks:\nWhat should the file be called?\nIt has no options to pick here: answer it in its tab.");
	assert.deepEqual(five.items, ["Open its tab", "Later"]);
	five.pick("Open its tab");
	await p5;
	assert.match(calls(), / show 7 --focus\n/);

	// Several wait: which worker first, the oldest question at the top; Later answers nothing.
	execFileSync(fake, ["start", "y"], { cwd, env, stdio: "ignore" });
	await until("8 blocked", () => {
		try {
			return fakeRun("ask", "8") === "blocked 8";
		} catch {
			return undefined;
		}
	});
	await until("8's hint", () => (/⚠ claude 8 asks about Bash/.test(u.widgetText()) ? true : undefined));
	const p6 = altQ();
	const six = await shown(6);
	assert.equal(six.title, "2 workers wait for an answer. Which one?");
	assert.deepEqual(six.items, ["7 task · claude asks: What should the file be called?", `8 task · claude asks about Bash: ${"ls *.md | xargs -I {} sh -c 'echo \"═══ {} ═══\" && cat {}'".slice(0, 54)}…`]);
	six.pick(six.items[1]);
	const seven = await shown(7);
	assert.match(seven.title, /^8 task · claude asks about Bash/);
	const answers = calls().split("\n").filter((l) => / answer /.test(l)).length;
	seven.pick("Later");
	await p6;
	await new Promise((r) => setTimeout(r, 500));
	assert.equal(calls().split("\n").filter((l) => / answer /.test(l)).length, answers, "Later presses nothing");
	assert.equal(u.selects.length, 7, "every dialog was opened by a key or a command");
});

test("a routr binary older than this checkout: a warning, and the model passes --permissions bypass", { timeout: 20_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-stale-"));
	const repo = join(here, "..", "..");
	const git = (...args: string[]) => execFileSync("git", ["-C", repo, ...args], { encoding: "utf8" }).trim();
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: join(cwd, ".routr") });
	t.after(() => delete process.env.FAKE_ROUTR_REV);

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const prompt = async (rev: string) => {
		process.env.FAKE_ROUTR_REV = rev;
		const u = mockCtx(cwd);
		u.ctx.mode = "print"; // no polling
		m.fire("session_start", { reason: "startup" }, u.ctx);
		const opts = { sections: {} as Record<string, string> };
		await m.fire("before_agent_start", { systemPrompt: "BASE", systemPromptOptions: opts }, u.ctx)[0];
		m.fire("session_shutdown", {}, u.ctx);
		return { text: opts.sections.routr!, notes: u.notes };
	};

	const first = git("rev-list", "--max-parents=0", "HEAD").split("\n")[0] ?? "";
	let p = await prompt(first);
	assert.match(p.text, /routr's binary is older than this skill: pass --permissions bypass .* run .*\/install\.sh\.$/);
	assert.doesNotMatch(p.text, /Leave out --permissions/);
	assert.ok(p.notes.some((n) => n.includes(`installed binary (${first.slice(0, 12)}) is older than`)), p.notes.join(" | "));

	p = await prompt("old"); // a routr without the version command
	assert.match(p.text, /pass --permissions bypass/);
	assert.ok(p.notes.some((n) => n.includes("installed binary is older than")), p.notes.join(" | "));

	p = await prompt(git("rev-parse", "HEAD"));
	assert.match(p.text, /Leave out --permissions: .* routr gives your workers bypass\.$/);
	assert.deepEqual(p.notes, []);

	p = await prompt(""); // no revision: nothing to compare
	assert.match(p.text, /Leave out --permissions/);
});

test("a wake whose worker was cancelled while results were fetched is not sent", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "30", PI_SESSION_ID: "S1" };
	Object.assign(process.env, {
		ROUTR_BIN: fake,
		ROUTR_STATE_DIR: env.ROUTR_STATE_DIR,
		FAKE_ROUTR_RUN_SECS: "30",
		FAKE_ROUTR_SLOW_CONTEXT_MS: "3000",
		ROUTR_PI_WAKE: "mine",
		ROUTR_PI_VIEW: "cards",
	});
	t.after(() => delete process.env.FAKE_ROUTR_SLOW_CONTEXT_MS);
	execFileSync(fake, ["list"], { cwd, env }); // creates .routr/state

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	const calls = () => readFileSync(join(env.ROUTR_STATE_DIR, "fake-calls.log"), "utf8");
	debug = () => `widget:\n${u.widgetText()}\nwakes: ${JSON.stringify(wakes().map((s) => s.message.content))}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await new Promise((r) => setTimeout(r, 300)); // the baseline snapshot
	m.fire("agent_start", {}, u.ctx);
	execFileSync(fake, ["start", "--answer", "agent", "x"], { cwd, env, stdio: "ignore" });
	await until("7 running on its card", () => /7 task .* running/.test(u.widgetText()));
	execFileSync(fake, ["ask", "7"], { cwd, env, stdio: "ignore" });
	// The wake for the question is being built: it fetches what the question is decided with…
	await until("the wake fetching its context", () => / show 7 --context --json\n/.test(calls()));
	// …while the model cancels the worker.
	const command = "routr cancel 7";
	const out = execFileSync(fake, ["cancel", "7"], { cwd, env, encoding: "utf8" });
	m.fire("tool_result", { toolName: "bash", toolCallId: "c1", input: { command }, content: [{ type: "text", text: out }] }, u.ctx);
	await new Promise((r) => setTimeout(r, 5000));
	assert.equal(wakes().length, 0, "the question went with the worker");
});

test("cancelling a chain leaves its finished worker's later questions news: resumed, it wakes the model again", { timeout: 60_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "4", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "4", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards" });
	execFileSync(fake, ["list"], { cwd, env }); // creates .routr/state

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	const bash = (id: string, argv: string[]) => {
		const out = execFileSync(fake, argv, { cwd, env, encoding: "utf8", stdio: ["ignore", "pipe", "ignore"] });
		m.fire("tool_result", { toolName: "bash", toolCallId: id, input: { command: `routr ${argv.join(" ")}` }, content: [{ type: "text", text: out }] }, u.ctx);
		return out;
	};
	debug = () => `widget:\n${u.widgetText()}\nwakes: ${JSON.stringify(wakes().map((s) => s.message.content))}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await new Promise((r) => setTimeout(r, 300)); // the baseline snapshot
	m.fire("agent_start", {}, u.ctx);
	assert.equal(bash("c1", ["flow", "start", "demo"]).trim(), "demo-1");
	// plan (7) finishes, review (8) runs: cancel the chain. Like the CLI, the finished plan stays done.
	await until("plan done, review running", () => /7 plan .* done/.test(u.widgetText()) && /8 review .* running/.test(u.widgetText()));
	assert.match(bash("c2", ["cancel", "demo-1"]), /^routr: cancelled 7 plan/m);
	await until("the rest of the chain gone from the cards", () => !/8 review|13 implement/.test(u.widgetText()));

	// A follow-up resumes the plan worker, and it asks a question: that is news for this turn.
	bash("c3", ["send", "7", "one more thing"]);
	execFileSync(fake, ["ask", "7"], { cwd, env, stdio: "ignore" });
	const wake = await until("the wake for its question", () => wakes().find((s) => /worker 7 · plan .* is blocked\b.*: Do you want to proceed\?/.test(s.message.content)));
	assert.deepEqual(wake.options, { triggerTurn: true, deliverAs: "steer" });
});

test("the skill it ships loads in Pi, with no diagnostics", async () => {
	const { loadSkillsFromDir } = await import("@earendil-works/pi-coding-agent");
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const [dir] = m.fire("resources_discover", {}, {})[0].skillPaths;
	const { skills, diagnostics } = loadSkillsFromDir({ dir, source: "extension" });
	assert.deepEqual(diagnostics, [], "Pi drops a skill whose frontmatter it can't parse");
	assert.deepEqual(skills.map((s) => s.name), ["routr"]);
});
