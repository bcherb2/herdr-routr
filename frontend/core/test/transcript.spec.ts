import { test } from "node:test";
import assert from "node:assert/strict";
import { activityLine, clip, parseLine, toolArg, type TranscriptEvent } from "../transcript.ts";

// Same fixtures as internal/transcript/transcript_test.go.
const claudeLines = [
	`{"type":"permission-mode"}`,
	`{"type":"user","message":{"role":"user","content":"Fix the bug"}}`,
	`{"type":"assistant","message":{"model":"claude-sonnet-5","content":[{"type":"thinking","thinking":"…"},{"type":"tool_use","name":"Edit","input":{"file_path":"calc.py"}}]}}`,
	`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"ok"}]}}`,
	`{"type":"assistant","message":{"model":"claude-sonnet-5","content":[{"type":"text","text":"Fixed it."}]}}`,
	`not json`,
];
const codexLines = [
	`{"type":"session_meta","payload":{"id":"x"}}`,
	`{"type":"turn_context","payload":{"model":"gpt-6-astra","effort":"low","approval_policy":"on-request"}}`,
	`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":"sys"}]}}`,
	`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Fix the bug"}]}}`,
	`{"type":"response_item","payload":{"type":"custom_tool_call","name":"exec","input":"python3 t.py"}}`,
	`{"type":"response_item","payload":{"type":"custom_tool_call_output","output":"ok"}}`,
	`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Both "},{"type":"output_text","text":"passed."}]}}`,
	`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"Both passed."}}`,
];
const piLines = [
	`{"type":"session","version":3,"id":"u","timestamp":"2026-09-26T00:00:00Z","cwd":"/w"}`,
	`{"type":"model_change","id":"a","parentId":null,"timestamp":"t","provider":"deepseek","modelId":"deepseek-v4-flash"}`,
	`{"type":"message","id":"b","parentId":"a","timestamp":"t","message":{"role":"user","content":[{"type":"text","text":"Fix the bug"}],"timestamp":1}}`,
	`{"type":"message","id":"c","parentId":"b","timestamp":"t","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"toolCall","id":"x","name":"bash","arguments":{"command":"pytest -q"}}],"model":"deepseek-v4-flash","stopReason":"toolUse"}}`,
	`{"type":"message","id":"d","parentId":"c","timestamp":"t","message":{"role":"toolResult","toolCallId":"x","toolName":"bash","content":[{"type":"text","text":"1 passed"}],"isError":false}}`,
	`{"type":"message","id":"e","parentId":"d","timestamp":"t","message":{"role":"assistant","content":[{"type":"text","text":"All green."}],"model":"deepseek-v4-flash","stopReason":"stop"}}`,
];

const all = (h: string | undefined, lines: string[]): TranscriptEvent[] => lines.flatMap((l) => parseLine(h, l));

test("claude transcript", () => {
	assert.deepEqual(all("claude", claudeLines), [
		{ kind: "user", text: "Fix the bug" },
		{ kind: "tool", model: "claude-sonnet-5", tool: "Edit", text: `{"file_path":"calc.py"}` },
		{ kind: "tool_result" },
		{ kind: "text", model: "claude-sonnet-5", text: "Fixed it." },
	]);
});

test("codex transcript", () => {
	assert.deepEqual(all("codex", codexLines), [
		{ kind: "meta", model: "gpt-6-astra", effort: "low" },
		{ kind: "user", text: "Fix the bug" },
		{ kind: "tool", tool: "exec", text: "python3 t.py" },
		{ kind: "tool_result" },
		{ kind: "text", text: "Both passed." },
		{ kind: "turn_end", text: "Both passed." },
	]);
});

test("codex function_call carries arguments", () => {
	const line = `{"type":"response_item","payload":{"type":"function_call","name":"shell","arguments":"{\\"command\\":[\\"bash\\",\\"-lc\\",\\"ls\\"]}"}}`;
	const [e] = parseLine("codex", line);
	assert.equal(e?.tool, "shell");
	assert.equal(activityLine(e!), "shell  bash -lc ls");
});

test("pi session transcript", () => {
	assert.deepEqual(all("pi", piLines), [
		{ kind: "meta", model: "deepseek-v4-flash" },
		{ kind: "user", text: "Fix the bug" },
		{ kind: "tool", model: "deepseek-v4-flash", tool: "bash", text: `{"command":"pytest -q"}` },
		{ kind: "tool_result" },
		{ kind: "text", model: "deepseek-v4-flash", text: "All green." },
		{ kind: "turn_end", text: "All green." },
	]);
});

test("unknown harness is detected per line", () => {
	assert.deepEqual(all(undefined, [claudeLines[2]!, codexLines[4]!, piLines[3]!]).map((e) => e.tool), ["Edit", "exec", "bash"]);
	assert.deepEqual(all("gemini", [claudeLines[4]!]), [{ kind: "text", model: "claude-sonnet-5", text: "Fixed it." }]);
});

test("toolArg picks the telling argument (ported from Go)", () => {
	const cases: Record<string, string> = {
		[`{"command":"go test ./...","description":"x"}`]: "go test ./...",
		[`{"command":["bash","-lc","ls -la"],"workdir":"/w"}`]: "bash -lc ls -la",
		[`{"file_path":"/w/calc.py","old_string":"a"}`]: "/w/calc.py",
		[`text(await tools.exec_command({cmd:"pwd; rg --files -g 'PLAN.md'", yield_time_ms: 1000}))`]: "pwd; rg --files -g 'PLAN.md'",
		["const r = await Promise.allSettled([ tools.exec_command({cmd:\"git status\"}), tools.exec_command({cmd: \"nl -ba \\\"PLAN.md\\\"\"}) ])"]:
			`git status ; nl -ba "PLAN.md"`,
		["*** Begin Patch\n*** Update File: calc.py"]: "*** Begin Patch *** Update File: calc.py",
	};
	for (const [input, want] of Object.entries(cases)) assert.equal(toolArg(input), want, input);
});

test("activity lines and clipping", () => {
	assert.equal(activityLine({ kind: "tool", tool: "Read", text: `{"file_path":"PLAN.md"}` }), "Read  PLAN.md");
	assert.equal(activityLine({ kind: "text", text: "  Looking\n at it " }), "“Looking at it”");
	assert.equal(activityLine({ kind: "tool_result" }), undefined);
	assert.equal(activityLine({ kind: "text", text: "  " }), undefined);
	assert.equal(clip("ab😀cd", 3), "ab😀…");
});
