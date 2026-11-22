// The extension end to end against a mock ExtensionAPI and test/fake-routr:
// no Pi, no model. The live run in a real terminal is test/smoke.zsh.

import { test } from "node:test";
import assert from "node:assert/strict";
import { execFileSync } from "node:child_process";
import { mkdtempSync, readFileSync, realpathSync } from "node:fs";
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
		// Pi cuts nothing shorter than this; the sessions here are small (a test can set its own)
		getSettings: () => ({ compaction: { keepRecentTokens: 100 } }),
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
		sessionManager: { getSessionId: () => "S1", getEntries: () => [], getBranch: (): any[] => [] },
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

// The model queued a worker in another repo (cd ../other && routr start …):
// the extension lists that repo's registry too, and wakes for it, and never
// for the worker with the same id in its own.
test("a worker queued in another repo's registry wakes the model; the same id at home does not", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const other = mkdtempSync(join(tmpdir(), "routr-pi-other-"));
	const otherDir = join(other, ".routr");
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "3", PI_SESSION_ID: "S-other" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "3", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards" });
	// home's own worker 7: another session's
	execFileSync(fake, ["start", "--role", "task", "home's"], { cwd, env, stdio: "ignore" });

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	const calls = (d: string) => readFileSync(join(d, "fake-calls.log"), "utf8");
	debug = () => `widget:\n${u.widgetText()}\nwakes: ${JSON.stringify(wakes().map((s) => s.message.content))}\nnotes: ${u.notes.join(" | ")}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await until("home's 7 on its card", () => /7 task/.test(u.widgetText()));
	m.fire("agent_start", {}, u.ctx);
	// the model's shell: cd there and start one, recorded to no session (the claims find it)
	const { PI_SESSION_ID: _, ...noSession } = env;
	const out = execFileSync("sh", ["-c", `${fake} start --role review x 2>&1`], { cwd: other, env: { ...noSession, ROUTR_STATE_DIR: otherDir }, encoding: "utf8" });
	assert.match(out, new RegExp(`^routr: queued worker 7 review .*; registry ${otherDir}\\n7\\n$`));
	const command = `cd ${other} && routr start --role review x`;
	m.fire("tool_call", { toolName: "bash", toolCallId: "c1", input: { command } }, u.ctx);
	m.fire("tool_result", { toolName: "bash", toolCallId: "c1", input: { command }, content: [{ type: "text", text: out }] }, u.ctx);
	assert.deepEqual(m.entries.at(-1), { customType: "routr-claims", data: { ids: [7], chains: [], group: "run:1", registry: realpathSync(otherDir) } });
	m.fire("agent_end", { messages: [{ role: "assistant", stopReason: "stop" }] }, u.ctx);

	const card = await until("the other repo's 7 on its card", () => (/7 review @routr-pi-other-\w+/.test(u.widgetText()) ? u.widgetText() : undefined));
	assert.match(card, /7 task · claude/, "home's 7 too, apart");
	const wake = await until("the wake for the other repo's 7", () => wakes()[0]);
	assert.match(wake.message.content, /^\[routr\] worker 7 · review · codex·gpt-6-astra·high @routr-pi-other-\w+ is done, verdict fail\./);
	assert.ok(wake.message.content.includes(`run routr commands about it as \`cd ${realpathSync(other)} && routr …\``), wake.message.content);
	assert.match(wake.message.content, /Two findings; one blocks implementation\./, "its result, read from its registry");
	assert.match(calls(otherDir), /list --json[\s\S]*result 7/);
	await until("home's 7 done", () => /7 task · claude.* done/.test(u.widgetText()));
	await new Promise((r) => setTimeout(r, 2000));
	assert.equal(wakes().length, 1, "home's 7 is another session's: no wake");
	const lists = calls(otherDir).split("list --json").length;
	await new Promise((r) => setTimeout(r, 6000));
	assert.equal(calls(otherDir).split("list --json").length, lists, "its worker is done with: that registry is no longer listed");
});

// The overnight run of 2026-11-03: the model started workers in another repo
// as `ID=$(routr start … 2>log); echo "x=$ID $(cat log)"`, so no line of the
// output starts "routr: queued". The registry the call ran in, and routr's
// caller.session, still say whose they are.
test("a worker queued in another repo wakes the model though the output names it on no line of its own", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-ext-"));
	const other = mkdtempSync(join(tmpdir(), "routr-pi-other-"));
	const otherDir = join(other, ".routr");
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "3", PI_SESSION_ID: "S1" };
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "3", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards" });

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	debug = () => `widget:\n${u.widgetText()}\nwakes: ${JSON.stringify(wakes().map((s) => s.message.content))}\nnotes: ${u.notes.join(" | ")}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await new Promise((r) => setTimeout(r, 300));
	m.fire("agent_start", {}, u.ctx);
	const command = `ID=$(routr start --root ${other} --role review x 2>/tmp/e1); echo "x=$ID $(cat /tmp/e1)"`;
	const out = execFileSync("sh", ["-c", `ID=$(${fake} start --role review x 2>&1 >/dev/null); echo "x=7 $ID"`], { cwd: other, env: { ...env, ROUTR_STATE_DIR: otherDir }, encoding: "utf8" });
	assert.match(out, /^x=7 routr: queued worker 7 review/);
	m.fire("tool_call", { toolName: "bash", toolCallId: "c1", input: { command } }, u.ctx);
	m.fire("tool_result", { toolName: "bash", toolCallId: "c1", input: { command }, content: [{ type: "text", text: out }] }, u.ctx);
	assert.deepEqual(m.entries.at(-1), { customType: "routr-claims", data: { ids: [], chains: [], group: "run:1", registry: realpathSync(otherDir) } });
	m.fire("agent_end", { messages: [{ role: "assistant", stopReason: "stop" }] }, u.ctx);

	const wake = await until("the wake for the other repo's 7", () => wakes()[0]);
	assert.match(wake.message.content, /^\[routr\] worker 7 · review .* is done/);
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

test("the main agent asks for Pi's long prompt cache unless PI_CACHE_RETENTION says otherwise; a worker doesn't", async (t) => {
	const keys = ["ROUTR_ID", "PI_CACHE_RETENTION"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const { default: routr } = await import("../src/index.ts");
	const loaded = (env: Record<string, string>) => {
		for (const k of keys) delete process.env[k];
		Object.assign(process.env, env);
		routr(mockPi().pi as any);
		return process.env.PI_CACHE_RETENTION;
	};
	assert.equal(loaded({}), "long", "the main agent: an hour");
	assert.equal(loaded({ PI_CACHE_RETENTION: "short" }), "short", "set off: left alone");
	assert.equal(loaded({ ROUTR_ID: "12" }), undefined, "a worker: Pi's default");
});

test("the main agent strips earlier reasoning as routr main says: not for a worker, another API, or strip_reasoning false", async (t) => {
	const keys = ["ROUTR_ID", "ROUTR_MAIN_STRIP_REASONING", "FAKE_ROUTR_MAIN", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-main-"));
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: join(cwd, ".routr") });
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const messages = [
		{ role: "user", content: "go", timestamp: 0 },
		{ role: "assistant", content: [{ type: "thinking", thinking: "old" }, { type: "text", text: "started 7" }], timestamp: 0 },
		{ role: "custom", customType: "routr-wake", content: "[routr] worker 7 is done", display: true, timestamp: 0 },
	];
	/** a session started under env; what its first request carries, and what it told the user */
	const session = async (env: Record<string, string>, api = "openai-completions") => {
		for (const k of ["ROUTR_ID", "ROUTR_MAIN_STRIP_REASONING", "FAKE_ROUTR_MAIN"]) delete process.env[k];
		Object.assign(process.env, env);
		const u = mockCtx(cwd);
		u.ctx.mode = "print"; // no polling
		m.fire("session_start", { reason: "startup" }, u.ctx);
		const out = await m.fire("context", { type: "context", messages }, { ...u.ctx, model: { api } })[0];
		await new Promise((r) => setTimeout(r, 50)); // a warning is sent once routr main has answered
		m.fire("session_shutdown", {}, u.ctx);
		return { stripped: out?.messages?.[1].content.length === 1, notes: u.notes };
	};

	assert.deepEqual(await session({}), { stripped: true, notes: [] }, "routr main: the built-in [main]");
	for (const api of ["openai-responses", "openai-codex-responses", "azure-openai-responses", "anthropic-messages"])
		assert.equal((await session({}, api)).stripped, false, `${api}: Responses pairs tool calls with their reasoning; Anthropic ignores it`);
	assert.equal((await session({ ROUTR_ID: "12" })).stripped, false, "a worker keeps its reasoning");
	const off = JSON.stringify({ context: "truncate", at: 150000, keep_turns: 3, keep_tokens: 40000, strip_reasoning: false });
	assert.equal((await session({ FAKE_ROUTR_MAIN: off })).stripped, false, "[main] strip_reasoning = false");

	// A routr older than `routr main`: the defaults, with ROUTR_MAIN_STRIP_REASONING, and no extra warning.
	assert.deepEqual(await session({ FAKE_ROUTR_MAIN: "old" }), { stripped: true, notes: [] });
	assert.equal((await session({ FAKE_ROUTR_MAIN: "old", ROUTR_MAIN_STRIP_REASONING: "0" })).stripped, false);

	// A config routr rejects: the defaults, and the user hears why.
	const bad = await session({ FAKE_ROUTR_MAIN: "bad" });
	assert.equal(bad.stripped, true);
	assert.deepEqual(bad.notes, ['routr: main.context = "compact": use one of truncate, summarize, off; the main agent uses the [main] defaults']);
});

/** A session path for session_before_compact: four turns of the user's, oldest first. */
function branchOf(...turns: string[]) {
	let n = 0;
	const entry = (message: object) => ({ type: "message", id: `e${++n}`, parentId: n > 1 ? `e${n - 1}` : null, timestamp: new Date(n * 1000).toISOString(), message });
	return turns.flatMap((t) => [
		entry({ role: "user", content: t, timestamp: n }),
		entry({ role: "assistant", content: [{ type: "text", text: `${t}: done ${"x".repeat(400)}` }], stopReason: "stop", timestamp: n }),
	]);
}

test("past [main] at, the main agent's context is cut once its run settles: no model call, and wakes wait for the cut", { timeout: 40_000 }, async (t) => {
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-cut-"));
	const main = JSON.stringify({ context: "truncate", at: 1000, keep_turns: 1, keep_tokens: 500, strip_reasoning: true });
	const env = { ...process.env, ROUTR_STATE_DIR: join(cwd, ".routr"), FAKE_ROUTR_RUN_SECS: "2", PI_SESSION_ID: "S1" };
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	delete process.env.ROUTR_ID;
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: env.ROUTR_STATE_DIR, FAKE_ROUTR_RUN_SECS: "2", ROUTR_PI_WAKE: "mine", ROUTR_PI_VIEW: "cards", FAKE_ROUTR_MAIN: main });
	execFileSync(fake, ["list"], { cwd, env }); // creates .routr/state

	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	// ~1300 tokens as Pi estimates them, as many as the provider counted: the cut [main] asks for leaves it well under at
	const branch = branchOf("Read .design/brief.md and carry it out.", ...Array.from({ length: 11 }, (_, i) => `turn ${i + 2}`));
	let tokens = 900;
	const compacts: any[] = [];
	Object.assign(u.ctx, {
		getContextUsage: () => ({ tokens, contextWindow: 1_000_000, percent: 0 }),
		hasPendingMessages: () => false,
		compact: (o: any) => compacts.push(o),
		sessionManager: { ...u.ctx.sessionManager, getBranch: () => branch },
	});
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	const wakes = () => m.sent.filter((s) => s.message.customType === "routr-wake");
	debug = () => `widget:\n${u.widgetText()}\nwakes: ${wakes().length}\nnotes: ${u.notes.join(" | ")}`;

	m.fire("session_start", { reason: "startup" }, u.ctx);
	await new Promise((r) => setTimeout(r, 300)); // the baseline snapshot
	await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(compacts.length, 0, "under at");

	m.fire("agent_start", {}, u.ctx);
	execFileSync(fake, ["start", "--role", "review", "x"], { cwd, env, stdio: "ignore" });
	m.fire("agent_end", { messages: [{ role: "assistant", stopReason: "stop" }] }, u.ctx);
	tokens = 1200;
	await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(compacts.length, 1);
	assert.equal(compacts[0].customInstructions, undefined, "truncate: no summary");
	assert.match(u.notes.join("\n"), /routr: the context is at 1200 tokens, past 1000; keeping the last turn/);
	await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(compacts.length, 1, "one cut at a time");

	// Pi runs the compaction it was asked for: routr answers it
	const [answer] = await Promise.all(m.fire("session_before_compact", { reason: "manual", branchEntries: branch, preparation: { tokensBefore: 1300 } }, u.ctx));
	assert.equal(answer.compaction.firstKeptEntryId, "e23", "the last turn");
	// routr main printed no keep_head (a routr from before it): on, so the first turn is kept in front of the note
	assert.equal(answer.compaction.details.head.endId, "e2");
	assert.match(answer.compaction.summary, /^routr cut the conversation between the session's first turn and its last turns/);
	assert.doesNotMatch(answer.compaction.summary, /brief\.md/, "the first message is in the head, not the note");
	assert.match(answer.compaction.summary, /#7 review — /, "this session's worker");

	await until("7 done on its card", () => /7 review .* done/.test(u.widgetText()));
	await new Promise((r) => setTimeout(r, 1500));
	assert.equal(wakes().length, 0, "a wake turn now would race the cut");
	compacts[0].onComplete({ tokensBefore: 1200, estimatedTokensAfter: 300, details: { ...answer.compaction.details, head: { ...answer.compaction.details.head, tokens: 150 } } });
	m.fire("session_compact", { reason: "manual", fromExtension: true }, u.ctx);
	assert.match(u.notes.join("\n"), /routr: cut the context from 1200 to about 450 tokens/, "Pi's estimate and the first turn sent in front of the note");
	const wake = await until("the wake, once the cut is done", () => wakes()[0]);
	assert.deepEqual(wake.message.details.entries.map((e: any) => e.id), [7]);
});

test("a cut that left the context over [main] at is not tried again every turn: only once it has grown a tenth", async (t) => {
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-again-"));
	delete process.env.ROUTR_ID;
	Object.assign(process.env, {
		ROUTR_BIN: fake,
		ROUTR_STATE_DIR: join(cwd, ".routr"),
		FAKE_ROUTR_MAIN: JSON.stringify({ context: "truncate", at: 1000, keep_turns: 1, keep_tokens: 500, strip_reasoning: true }),
	});
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	const branch = branchOf("brief", ...Array.from({ length: 11 }, (_, i) => `turn ${i + 2}`));
	let tokens = 1200;
	const compacts: any[] = [];
	Object.assign(u.ctx, {
		getContextUsage: () => ({ tokens, contextWindow: 1_000_000, percent: 0 }),
		hasPendingMessages: () => false,
		compact: (o: any) => compacts.push(o),
		sessionManager: { ...u.ctx.sessionManager, getBranch: () => branch },
	});
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	m.fire("session_start", { reason: "startup" }, u.ctx);
	const settle = () => Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	const done = async (prompt: number) => {
		compacts.at(-1).onComplete({ tokensBefore: tokens, details: { by: "routr" } });
		m.fire("session_compact", { type: "session_compact", compactionEntry: { details: { by: "routr" } }, fromExtension: true, reason: "manual", willRetry: false }, u.ctx);
		await Promise.all(m.fire("message_end", { type: "message_end", message: { role: "assistant", stopReason: "stop", content: [], usage: { input: 0, cacheRead: prompt, cacheWrite: 0, output: 50 } } }, u.ctx));
	};

	await settle();
	assert.equal(compacts.length, 1);
	await done(1100); // the provider counted more than the cut was sized for
	tokens = 1150;
	await settle();
	assert.equal(compacts.length, 1, "1150 is within a tenth of the 1100 the cut left");
	tokens = 1250;
	await settle();
	assert.equal(compacts.length, 2, "grown a tenth: cut again");
	await done(600);
	tokens = 1050;
	await settle();
	assert.equal(compacts.length, 3, "a cut that came in under at sets no floor; past at again, cut again");
});

test("which compactions routr answers: Pi's own near the window, never the user's /compact or a worker's; summarize and off leave Pi's", async (t) => {
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-cut-"));
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: join(cwd, ".routr") });
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const branch = branchOf("brief", "a", "b", "c", "d");
	const session = async (env: Record<string, string>, reason: string) => {
		for (const k of ["ROUTR_ID", "FAKE_ROUTR_MAIN"]) delete process.env[k];
		Object.assign(process.env, env);
		const u = mockCtx(cwd);
		u.ctx.mode = "print"; // no polling
		const compacts: any[] = [];
		Object.assign(u.ctx, {
			getContextUsage: () => ({ tokens: 200_000, contextWindow: 1_000_000, percent: 20 }),
			hasPendingMessages: () => false,
			compact: (o: any) => compacts.push(o),
			sessionManager: { ...u.ctx.sessionManager, getBranch: () => branch },
		});
		m.fire("session_start", { reason: "startup" }, u.ctx);
		if (reason === "settled") await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
		const [answer] = await Promise.all(m.fire("session_before_compact", { reason: reason === "settled" ? "manual" : reason, branchEntries: branch, preparation: { tokensBefore: 200_000 } }, u.ctx));
		m.fire("session_shutdown", {}, u.ctx);
		return { answer, compacts };
	};
	const main = (o: object) => JSON.stringify({ context: "truncate", at: 150000, keep_turns: 3, keep_tokens: 40000, strip_reasoning: true, ...o });

	assert.equal((await session({}, "manual")).answer, undefined, "the user's /compact: Pi's summary");
	for (const reason of ["threshold", "overflow"]) assert.equal((await session({}, reason)).answer.compaction.firstKeptEntryId, "e5", `Pi's ${reason} compaction, truncated`);
	assert.equal((await session({}, "settled")).answer.compaction.firstKeptEntryId, "e5");
	assert.equal((await session({ ROUTR_ID: "12" }, "threshold")).answer, undefined, "a worker's context is its work");
	assert.equal((await session({ ROUTR_ID: "12" }, "settled")).compacts.length, 0);

	const sum = await session({ FAKE_ROUTR_MAIN: main({ context: "summarize" }) }, "settled");
	assert.match(sum.compacts[0].customInstructions, /Quote the session's first user message word for word/);
	assert.equal(sum.answer, undefined, "summarize: Pi's summary");
	const off = await session({ FAKE_ROUTR_MAIN: main({ context: "off" }) }, "settled");
	assert.equal(off.compacts.length, 0);
	assert.equal((await session({ FAKE_ROUTR_MAIN: main({ context: "off" }) }, "threshold")).answer, undefined, "off: Pi as it is");
	// nothing older than the turns kept: no cut asked for; Pi's own goes on its way
	const few = main({ keep_turns: -1, keep_tokens: 100_000, at: 150_000 });
	assert.equal((await session({ FAKE_ROUTR_MAIN: few }, "settled")).compacts.length, 0);
	assert.equal((await session({ FAKE_ROUTR_MAIN: few }, "threshold")).answer, undefined);
});

test("Pi refusing a cut routr thought it would take is said once a session, with why", async (t) => {
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-cut-"));
	for (const k of ["ROUTR_ID", "FAKE_ROUTR_MAIN"]) delete process.env[k];
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: join(cwd, ".routr") });
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	Object.assign(m.pi, { getSettings: () => ({ compaction: { keepRecentTokens: 300 } }) });
	routr(m.pi as any);
	const u = mockCtx(cwd);
	u.ctx.mode = "print";
	const branch = branchOf("brief", "a", "b", "c", "d");
	let tokens = 200_000;
	let tries = 0; // past 300 tokens: routr's dry run passes, and this Pi refuses all the same
	Object.assign(u.ctx, {
		getContextUsage: () => ({ tokens, contextWindow: 1_000_000, percent: 20 }),
		hasPendingMessages: () => false,
		compact: (o: any) => (tries++, o.onError(new Error("Compaction failed: Nothing to compact (session too small)"))),
		sessionManager: { ...u.ctx.sessionManager, getBranch: () => branch },
	});
	m.fire("session_start", { reason: "startup" }, u.ctx);
	for (let i = 0; i < 3; i++) await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(tries, 1, "no retry until the context has grown");
	tokens = 230_000;
	await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(tries, 2, "a tenth more: try again");
	m.fire("session_shutdown", {}, u.ctx);
	const told = u.notes.filter((n) => /nothing to cut yet/.test(n));
	assert.deepEqual(told, ["routr: nothing to cut yet: Pi always keeps the last 300 tokens of conversation (compaction.keepRecentTokens), and [main] at counts the system prompt too"]);
	assert.equal(u.notes.filter((n) => /the context is at/.test(n)).length, 1, "it tries after each turn, quietly: the conversation grows past it");
});

test("while Pi would refuse the cut (its keepRecentTokens for this model), routr asks for none and says nothing", async (t) => {
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-cut-"));
	for (const k of ["ROUTR_ID", "FAKE_ROUTR_MAIN"]) delete process.env[k];
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: join(cwd, ".routr") });
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	Object.assign(m.pi, { getSettings: () => ({ compaction: { keepRecentTokens: 100, modelOverrides: { "anthropic/claude-opus-5-5": { keepRecentTokens: 30_000 } } } }) });
	routr(m.pi as any);
	const u = mockCtx(cwd);
	u.ctx.mode = "print";
	let branch = branchOf("brief", "a", "b", "c", "d"); // ~500 tokens of conversation
	const compacts: any[] = [];
	Object.assign(u.ctx, {
		model: { provider: "anthropic", id: "claude-opus-5-5", api: "anthropic-messages" },
		getContextUsage: () => ({ tokens: 200_000, contextWindow: 1_000_000, percent: 20 }),
		hasPendingMessages: () => false,
		compact: (o: any) => compacts.push(o),
		sessionManager: { ...u.ctx.sessionManager, getBranch: () => branch },
	});
	m.fire("session_start", { reason: "startup" }, u.ctx);
	for (let i = 0; i < 3; i++) await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(compacts.length, 0, "the model's override, 30000, not the 100 of every model");
	assert.deepEqual(u.notes, [], "nothing to say: the conversation grows past it");
	branch = branchOf("brief", ...Array.from({ length: 300 }, (_, i) => `turn ${i}`)); // ~32k
	await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(compacts.length, 1);
	assert.match(u.notes.join("\n"), /routr: the context is at 200000 tokens, past 150000; keeping the last 3 turns/);
	m.fire("session_shutdown", {}, u.ctx);
});

test("after a cut that kept the first turn, every request of the main agent has it in front of the note, with its own cache breakpoint on Anthropic", async (t) => {
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-head-"));
	for (const k of ["ROUTR_ID", "FAKE_ROUTR_MAIN"]) delete process.env[k];
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: join(cwd, ".routr") });
	const { buildSessionProjection } = await import("@earendil-works/pi-coding-agent");
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	u.ctx.mode = "print";
	const branch: any[] = branchOf("Read .design/brief.md and carry it out.", "a", "b", "c", "d");
	Object.assign(u.ctx, { sessionManager: { ...u.ctx.sessionManager, getBranch: () => branch } });
	m.fire("session_start", { reason: "startup" }, u.ctx);
	const [answer] = await Promise.all(m.fire("session_before_compact", { reason: "threshold", branchEntries: branch, preparation: { tokensBefore: 200_000 } }, u.ctx));
	const c = answer.compaction;
	assert.equal(c.details.head.endId, "e2", "the built-in [main]: keep_head on");
	const at = (api: string, extra: object = {}) => ({ ...u.ctx, model: { provider: "p", id: "m", api }, ...extra });
	/** the main agent's next request, as the context hook leaves it */
	const context = async (api: string) => {
		const messages = buildSessionProjection(branch).messages.filter((x: any) => x.role !== "system");
		const out = await m.fire("context", { type: "context", messages }, at(api))[0];
		return { messages, out };
	};
	const before = await context("anthropic-messages");
	assert.equal(before.out, undefined, "no cut yet: nothing to put back");

	branch.push({ type: "compaction", id: "c1", parentId: "e10", timestamp: new Date(11_000).toISOString(), summary: c.summary, firstKeptEntryId: c.firstKeptEntryId, tokensBefore: 200_000, details: c.details, fromHook: true });
	branch.push({ type: "custom_message", id: "w1", parentId: "c1", timestamp: new Date(12_000).toISOString(), customType: "routr-wake", content: "[routr] worker 7 is done", display: true });
	for (const api of ["anthropic-messages", "openai-completions", "openai-responses"]) {
		const { messages, out } = await context(api);
		assert.deepEqual(
			out.messages.map((x: any) => x.role),
			["user", "assistant", "compactionSummary", "user", "assistant", "user", "assistant", "user", "assistant", "custom"],
			api,
		);
		assert.equal(out.messages[0].content, "Read .design/brief.md and carry it out.");
		assert.deepEqual(out.messages.slice(2), messages, "the rest as Pi built it");
		assert.notEqual(out.messages[0], branch[0].message, "a copy: nothing that edits the request reaches the session");
	}
	process.env.ROUTR_ID = "12";
	assert.equal((await context("anthropic-messages")).out, undefined, "a worker's context is its own");
	delete process.env.ROUTR_ID;

	// the request Pi's Anthropic converter builds from it
	const NOTE = `The conversation history before this point was compacted into the following summary:\n\n<summary>\n${c.summary}\n</summary>`;
	const eph = { type: "ephemeral" };
	const payload = {
		system: [{ type: "text", text: "s", cache_control: eph }],
		messages: [
			{ role: "user", content: "Read .design/brief.md and carry it out." },
			{ role: "assistant", content: [{ type: "text", text: "brief: done" }] },
			{ role: "user", content: [{ type: "text", text: NOTE }] },
			{ role: "user", content: [{ type: "text", text: "[routr] worker 7 is done", cache_control: eph }] },
		],
	};
	const req = (api: string) => m.fire("before_provider_request", { type: "before_provider_request", payload }, at(api))[0];
	assert.deepEqual(req("anthropic-messages").messages[1].content, [{ type: "text", text: "brief: done", cache_control: eph }]);
	assert.equal(req("openai-completions"), undefined, "not an Anthropic payload");
	process.env.ROUTR_ID = "12";
	assert.equal(req("anthropic-messages"), undefined, "nor for a worker");
	delete process.env.ROUTR_ID;
	m.fire("session_shutdown", {}, u.ctx);
});

test("the first answer after a cut still close to [main] at is said once a session: raise at, or keep less", async (t) => {
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-big-"));
	for (const k of ["ROUTR_ID", "FAKE_ROUTR_MAIN"]) delete process.env[k];
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: join(cwd, ".routr") });
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	u.ctx.mode = "print";
	m.fire("session_start", { reason: "startup" }, u.ctx);
	const cut = (by = "routr") => m.fire("session_compact", { type: "session_compact", compactionEntry: { details: by ? { by } : undefined }, fromExtension: true, reason: "manual", willRetry: false }, u.ctx);
	const answer = (prompt: number, stopReason = "stop") =>
		Promise.all(m.fire("message_end", { type: "message_end", message: { role: "assistant", stopReason, content: [], usage: { input: 1000, cacheRead: prompt - 3000, cacheWrite: 2000, output: 50 } } }, u.ctx));
	const told = () => u.notes.filter((n) => /still/.test(n));

	await answer(140_000);
	assert.deepEqual(told(), [], "no cut: nothing to say");
	cut();
	await answer(100_000);
	await answer(140_000);
	assert.deepEqual(told(), [], "100k is within 80% of 150000; only the first answer after a cut counts");
	cut("");
	await answer(140_000);
	assert.deepEqual(told(), [], "Pi's own summary, not routr's cut");
	cut();
	await answer(140_000, "error");
	await answer(130_000);
	assert.deepEqual(told(), ["routr: the context after the cut is still 130000 tokens, close to [main] at (150000); raise at, or lower keep_tokens (a lower head_tokens counts from a new session)"], "an answer that failed is passed over");
	cut();
	await answer(130_000);
	assert.equal(told().length, 1, "once a session");
	m.fire("session_shutdown", {}, u.ctx);
});

test("no cut right after the user interrupts a run, and a cut that throws still lets the wakes go", async (t) => {
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-cut-"));
	delete process.env.ROUTR_ID;
	Object.assign(process.env, {
		ROUTR_BIN: fake,
		ROUTR_STATE_DIR: join(cwd, ".routr"),
		FAKE_ROUTR_MAIN: JSON.stringify({ context: "summarize", at: 150000, keep_turns: 3, keep_tokens: 40000, strip_reasoning: true }),
	});
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	routr(m.pi as any);
	const u = mockCtx(cwd);
	u.ctx.mode = "print";
	const compacts: any[] = [];
	let broken = false;
	const branch = branchOf("brief", "a", "b", "c", "d");
	Object.assign(u.ctx, {
		getContextUsage: () => ({ tokens: 200_000, contextWindow: 1_000_000, percent: 20 }),
		hasPendingMessages: () => false,
		compact: (o: any) => compacts.push(o),
		sessionManager: {
			...u.ctx.sessionManager,
			getBranch: () => {
				if (broken) throw new Error("bad branch");
				return branch;
			},
		},
	});
	m.fire("session_start", { reason: "startup" }, u.ctx);
	m.fire("agent_end", { messages: [{ role: "assistant", stopReason: "aborted" }] }, u.ctx);
	await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(compacts.length, 0, "Esc: they are about to type");
	m.fire("agent_end", { messages: [{ role: "assistant", stopReason: "stop" }] }, u.ctx);
	broken = true;
	await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(compacts.length, 0);
	assert.match(u.notes.at(-1)!, /routr: could not cut the context: bad branch/);
	broken = false;
	await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	assert.equal(compacts.length, 1, "not stuck compacting: the next settle cuts");
	m.fire("session_shutdown", {}, u.ctx);
});

/** A main-agent session whose model, registry and compaction the test controls. */
async function fallbackSession(t: any, env: Record<string, string>) {
	const keys = ["ROUTR_ID", "FAKE_ROUTR_MAIN", "FAKE_ROUTR_FALLBACK", "ROUTR_BIN", "ROUTR_STATE_DIR"] as const;
	const saved = Object.fromEntries(keys.map((k) => [k, process.env[k]]));
	t.after(() => {
		for (const k of keys) saved[k] === undefined ? delete process.env[k] : (process.env[k] = saved[k]);
	});
	const cwd = mkdtempSync(join(tmpdir(), "routr-pi-fallback-"));
	for (const k of ["ROUTR_ID", "FAKE_ROUTR_MAIN", "FAKE_ROUTR_FALLBACK"]) delete process.env[k];
	Object.assign(process.env, { ROUTR_BIN: fake, ROUTR_STATE_DIR: join(cwd, ".routr"), ...env });
	const models: Record<string, any> = {
		"deepseek/deepseek-v4-pro": { provider: "deepseek", id: "deepseek-v4-pro", contextWindow: 1_000_000 },
		"openrouter/nokey": { provider: "openrouter", id: "nokey", contextWindow: 200_000, noKey: true },
		"openrouter/anthropic/claude-haiku-5.5": { provider: "openrouter", id: "anthropic/claude-haiku-5.5", contextWindow: 200_000 },
		"openrouter/deepseek/deepseek-v4.1-flash": { provider: "openrouter", id: "deepseek/deepseek-v4.1-flash", contextWindow: 1_000_000 },
		"openrouter/tiny": { provider: "openrouter", id: "tiny", contextWindow: 200 },
	};
	const { default: routr } = await import("../src/index.ts");
	const m = mockPi();
	let model = models["deepseek/deepseek-v4-pro"];
	const thinking: string[] = [];
	Object.assign(m.pi, {
		setModel: async (x: any) => ((model = x), true),
		setThinkingLevel: (l: string) => thinking.push(l),
		getSettings: () => ({}),
	});
	routr(m.pi as any);
	const u = mockCtx(cwd);
	u.ctx.mode = "print";
	const compacts: any[] = [];
	let tokens = 50_000;
	Object.defineProperty(u.ctx, "model", { get: () => model });
	Object.assign(u.ctx, {
		modelRegistry: { find: (p: string, id: string) => models[`${p}/${id}`], hasConfiguredAuth: (x: any) => !x.noKey },
		getContextUsage: () => ({ tokens, contextWindow: 1_000_000, percent: 5 }),
		hasPendingMessages: () => false,
		compact: (o: any) => compacts.push(o),
		sessionManager: { ...u.ctx.sessionManager, getBranch: () => branchOf("brief", "a", "b", "c", "d") },
	});
	m.fire("session_start", { reason: "startup" }, u.ctx);
	t.after(() => m.fire("session_shutdown", {}, u.ctx));
	/** a run that ended in `error` (none: it answered) on `on` (default: the model in use), then settled */
	const runEnds = async (error?: string, on?: { provider: string; id: string }) => {
		m.fire("agent_start", {}, u.ctx);
		const by = on ?? model;
		const last =
			error === undefined ? { role: "assistant", stopReason: "stop" } : { role: "assistant", stopReason: "error", errorMessage: error, provider: by.provider, model: by.id };
		m.fire("agent_end", { messages: [{ role: "user" }, last] }, u.ctx);
		await Promise.all(m.fire("agent_settled", { type: "agent_settled" }, u.ctx));
	};
	const resent = () => m.sent.filter((s) => s.message.customType === "routr-fallback");
	return { m, u, runEnds, resent, compacts, thinking, models, model: () => model, setModel: (x: any) => (model = x), setTokens: (n: number) => (tokens = n) };
}

const BALANCE = '402: {"message":"Insufficient Balance (request_id: 88fc6ff6)","type":"unknown_error"}';
const RULES = JSON.stringify({
	"pi:deepseek/deepseek-v4-pro": ["codex:gpt-6.1", "pi:openrouter/nokey", "pi:openrouter/anthropic/claude-haiku-5.5", "pi:openrouter/deepseek/deepseek-v4.1-flash:high"],
});

test("a main agent out of credit moves down its [[fallback]] rule and carries on; a fallback that fails too goes on down the same list", async (t) => {
	const s = await fallbackSession(t, { FAKE_ROUTR_FALLBACK: RULES });
	await s.runEnds(BALANCE);
	assert.equal(s.model().id, "anthropic/claude-haiku-5.5");
	assert.match(s.u.notes.at(-1)!, /^routr: pi:deepseek\/deepseek-v4-pro failed \(402: .*Insufficient Balance.*\); switched to pi:openrouter\/anthropic\/claude-haiku-5\.5, \[\[fallback\]\] rule 1 \(skipped codex:gpt-6\.1: not a Pi model; pi:openrouter\/nokey: no API key in Pi\)$/);
	assert.equal(s.resent().length, 1);
	assert.deepEqual(s.resent()[0]!.options, { triggerTurn: true, deliverAs: "followUp" });
	assert.match(s.resent()[0]!.message.content, /^\[routr\] Your last turn failed on pi:deepseek\/deepseek-v4-pro: 402: .*You are now on pi:openrouter\/anthropic\/claude-haiku-5\.5 \(\[\[fallback\]\] rule 1\)\. Carry on where you left off\.$/);
	assert.equal(s.compacts.length, 0, "50k fits haiku's window");

	await s.runEnds("Overloaded");
	assert.equal(s.model().id, "anthropic/claude-haiku-5.5", "an error that may pass: Pi's own retry, no move");

	await s.runEnds("429 quota exceeded");
	assert.equal(s.model().id, "deepseek/deepseek-v4.1-flash", "haiku failed too: the next of the original list");
	assert.deepEqual(s.thinking, ["high"]);

	await s.runEnds(BALANCE);
	assert.equal(s.resent().length, 2, "the list is spent: nothing resent");
	assert.match(s.u.notes.at(-1)!, /^routr: pi:openrouter\/deepseek\/deepseek-v4\.1-flash failed \(402: .*\), and no model of \[\[fallback\]\] rule 1 is left to take over\. Wakes wait until your next message; \/model switches by hand\.$/);
	await s.runEnds(BALANCE);
	assert.equal(s.u.notes.filter((n) => /is left to take over/.test(n)).length, 2, "the user's own next turn failed too: said again");
});

test("no [[fallback]] rule: the main agent stops and says so; a routr without the command leaves Pi as it is", async (t) => {
	const s = await fallbackSession(t, {});
	await s.runEnds(BALANCE);
	assert.equal(s.model().id, "deepseek-v4-pro");
	assert.match(s.u.notes.at(-1)!, /^routr: pi:deepseek\/deepseek-v4-pro failed \(402: .*\), and no \[\[fallback\]\] rule matches it\. Wakes wait/);
	assert.equal(s.resent().length, 0);
	const old = await fallbackSession(t, { FAKE_ROUTR_FALLBACK: "old" });
	await old.runEnds(BALANCE);
	assert.equal(old.model().id, "deepseek-v4-pro");
	assert.deepEqual(old.u.notes, []);
	const worker = await fallbackSession(t, { FAKE_ROUTR_FALLBACK: RULES, ROUTR_ID: "12" });
	await worker.runEnds(BALANCE);
	assert.equal(worker.model().id, "deepseek-v4-pro", "a worker's model is routr's to change, not its own");
});

test("a fallback model whose window the context would fill gets it truncated first, whatever [main] context says, then carries on", async (t) => {
	const main = JSON.stringify({ context: "summarize", at: 500000, keep_turns: 3, keep_tokens: 40000, strip_reasoning: true });
	const s = await fallbackSession(t, { FAKE_ROUTR_FALLBACK: RULES, FAKE_ROUTR_MAIN: main });
	s.setTokens(170_000); // over 80% of haiku's 200k
	await s.runEnds(BALANCE);
	assert.equal(s.model().id, "anthropic/claude-haiku-5.5");
	assert.equal(s.compacts.length, 1);
	assert.equal(s.resent().length, 0, "not before the cut");
	const [answer] = await Promise.all(s.m.fire("session_before_compact", { reason: "manual", branchEntries: branchOf("brief", "a", "b", "c", "d"), preparation: { tokensBefore: 170_000 } }, s.u.ctx));
	assert.equal(answer.compaction.firstKeptEntryId, "e5", "truncated, though [main] says summarize");
	s.compacts[0].onComplete({ tokensBefore: 170_000, estimatedTokensAfter: 30_000 });
	assert.equal(s.resent().length, 1, "then on");
	const [after] = await Promise.all(s.m.fire("session_before_compact", { reason: "threshold", branchEntries: branchOf("brief", "a", "b", "c", "d"), preparation: { tokensBefore: 170_000 } }, s.u.ctx));
	assert.equal(after, undefined, "later compactions are [main]'s again");
});

test("the fallback leaves a model the user switched to, skips the model that failed, and says once when routr rejects the rules", async (t) => {
	const s = await fallbackSession(t, { FAKE_ROUTR_FALLBACK: JSON.stringify({ "pi:deepseek/deepseek-v4-pro": ["pi:deepseek/deepseek-v4-pro:high", "pi:openrouter/anthropic/claude-haiku-5.5"] }) });
	const pro = s.model();
	s.setModel(s.models["openrouter/deepseek/deepseek-v4.1-flash"]); // the user's /model while pro's run was failing
	await s.runEnds(BALANCE, pro);
	assert.equal(s.model().id, "deepseek/deepseek-v4.1-flash", "theirs to try");
	assert.equal(s.resent().length, 0);
	s.setModel(pro);
	await s.runEnds(BALANCE);
	assert.equal(s.model().id, "anthropic/claude-haiku-5.5");
	assert.match(s.u.notes.at(-1)!, /\(skipped pi:deepseek\/deepseek-v4-pro:high: the model that failed\)$/);

	const bad = await fallbackSession(t, { FAKE_ROUTR_FALLBACK: "{not json" });
	await bad.runEnds(BALANCE);
	await bad.runEnds(BALANCE);
	assert.equal(bad.u.notes.filter((n) => /^routr: no model fallback: /.test(n)).length, 1, "said once");
	assert.equal(bad.model().id, "deepseek-v4-pro");
});

test("a forced cut keeps at most half the new window, though [main] keeps everything, and says so if the context still won't fit", async (t) => {
	const main = JSON.stringify({ context: "off", at: -1, keep_turns: -1, keep_tokens: -1, strip_reasoning: true });
	const s = await fallbackSession(t, { FAKE_ROUTR_FALLBACK: JSON.stringify({ "pi:deepseek/deepseek-v4-pro": ["pi:openrouter/tiny"] }), FAKE_ROUTR_MAIN: main });
	s.setTokens(170); // over 80% of tiny's 200
	await s.runEnds(BALANCE);
	assert.equal(s.model().id, "tiny");
	const [answer] = await Promise.all(s.m.fire("session_before_compact", { reason: "manual", branchEntries: branchOf("brief", "a", "b"), preparation: { tokensBefore: 170 } }, s.u.ctx));
	assert.ok(answer?.compaction, "a cut, where [main] alone would keep everything");
	assert.equal(answer.compaction.details.split, true, "100 tokens: not even the last turn whole");
	s.setTokens(250);
	s.compacts[0].onComplete({ tokensBefore: 170, estimatedTokensAfter: 250 });
	assert.match(s.u.notes.join("\n"), /routr: the context \(250 tokens\) is still over pi:openrouter\/tiny's window of 200; Pi's overflow compaction takes it from here/);
	assert.equal(s.resent().length, 1, "carried on all the same");
});

test("Pi's compaction after an overflow is held to half the window too; its compaction near the window is [main]'s", async (t) => {
	const main = JSON.stringify({ context: "truncate", at: -1, keep_turns: -1, keep_tokens: -1, strip_reasoning: true });
	const s = await fallbackSession(t, { FAKE_ROUTR_MAIN: main });
	s.setModel(s.models["openrouter/tiny"]);
	const fire = async (reason: string) =>
		(await Promise.all(s.m.fire("session_before_compact", { reason, branchEntries: branchOf("brief", "a", "b"), preparation: { tokensBefore: 250 } }, s.u.ctx)))[0];
	assert.equal(await fire("threshold"), undefined, "[main] keeps everything: Pi's summary");
	const answer = await fire("overflow");
	assert.ok(answer?.compaction, "a cut, where [main] alone would keep everything");
	assert.equal(answer.compaction.details.split, true, "100 tokens: not even the last turn whole");
	assert.equal(answer.compaction.details.head, undefined, "nor the first turn");
});
