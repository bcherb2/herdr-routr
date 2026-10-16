// Harness transcript lines → normalized events, for the live tool-call view.
//
// A port of internal/transcript/transcript.go (Claude and Codex JSONL) plus
// Pi's own session JSONL (docs/session-format.md in the Pi package). Nothing
// parsed here is sent to the model; it only feeds the worker cards.

type EventKind = "user" | "text" | "tool" | "tool_result" | "meta" | "turn_end";

export interface TranscriptEvent {
	kind: EventKind;
	model?: string;
	effort?: string;
	tool?: string;
	/** prose, the tool input (JSON or raw), or the final message */
	text?: string;
}

type Harness = "claude" | "codex" | "pi";

type Json = Record<string, unknown>;

function obj(v: unknown): Json | undefined {
	return v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Json) : undefined;
}

function str(v: unknown): string | undefined {
	return typeof v === "string" ? v : undefined;
}

function parseJson(line: string): Json | undefined {
	try {
		return obj(JSON.parse(line));
	} catch {
		return undefined;
	}
}

/** ~/.claude/projects/<dir>/<session>.jsonl */
function parseClaude(r: Json): TranscriptEvent[] {
	const msg = obj(r.message);
	if (!msg) return [];
	const content = msg.content;
	if (r.type === "user") {
		if (typeof content === "string") return [{ kind: "user", text: content }];
		if (!Array.isArray(content)) return [];
		const out: TranscriptEvent[] = [];
		for (const b of content.map(obj)) {
			if (b?.type === "tool_result") out.push({ kind: "tool_result" });
			else if (b?.type === "text") out.push({ kind: "user", text: str(b.text) ?? "" });
		}
		return out;
	}
	if (r.type === "assistant" && Array.isArray(content)) {
		const model = str(msg.model);
		const out: TranscriptEvent[] = [];
		for (const b of content.map(obj)) {
			if (b?.type === "text") out.push({ kind: "text", model, text: str(b.text) ?? "" });
			else if (b?.type === "tool_use") {
				out.push({ kind: "tool", model, tool: str(b.name), text: JSON.stringify(b.input ?? {}) });
			}
		}
		return out;
	}
	return [];
}

/** ~/.codex/sessions/YYYY/MM/DD/rollout-*-<session>.jsonl */
function parseCodex(r: Json): TranscriptEvent[] {
	const p = obj(r.payload);
	if (!p) return [];
	const ptype = str(p.type) ?? "";
	if (r.type === "turn_context") return [{ kind: "meta", model: str(p.model), effort: str(p.effort) }];
	if (r.type === "response_item" && ptype === "message" && (p.role === "assistant" || p.role === "user")) {
		const text = Array.isArray(p.content) ? p.content.map((c) => str(obj(c)?.text) ?? "").join("") : "";
		return [{ kind: p.role === "user" ? "user" : "text", text }];
	}
	if (r.type === "response_item" && (ptype === "custom_tool_call" || ptype === "function_call")) {
		// custom_tool_call carries `input`; function_call carries `arguments` (a JSON string).
		return [{ kind: "tool", tool: str(p.name), text: str(p.input) ?? str(p.arguments) ?? "" }];
	}
	if (r.type === "response_item" && ptype.endsWith("_call_output")) return [{ kind: "tool_result" }];
	if (r.type === "event_msg" && ptype === "task_complete") {
		return [{ kind: "turn_end", text: str(p.last_agent_message) ?? "" }];
	}
	return [];
}

/** Pi session JSONL: {"type":"message","message":{role,content,…}} */
function parsePi(r: Json): TranscriptEvent[] {
	if (r.type === "model_change") return [{ kind: "meta", model: str(r.modelId) }];
	if (r.type === "thinking_level_change") return [{ kind: "meta", effort: str(r.thinkingLevel) }];
	if (r.type !== "message") return [];
	const m = obj(r.message);
	if (!m) return [];
	if (m.role === "user") {
		if (typeof m.content === "string") return [{ kind: "user", text: m.content }];
		if (!Array.isArray(m.content)) return [];
		const text = m.content.map((c) => str(obj(c)?.text) ?? "").join("");
		return [{ kind: "user", text }];
	}
	if (m.role === "toolResult") return [{ kind: "tool_result" }];
	if (m.role === "assistant" && Array.isArray(m.content)) {
		const model = str(m.model);
		const out: TranscriptEvent[] = [];
		for (const b of m.content.map(obj)) {
			if (b?.type === "text") out.push({ kind: "text", model, text: str(b.text) ?? "" });
			else if (b?.type === "toolCall") {
				out.push({ kind: "tool", model, tool: str(b.name), text: JSON.stringify(b.arguments ?? {}) });
			}
		}
		if (m.stopReason === "stop") {
			const last = out.filter((e) => e.kind === "text").at(-1);
			out.push({ kind: "turn_end", text: last?.text ?? "" });
		}
		return out;
	}
	return [];
}

/** Guess the format from one record, for workers whose harness is unknown. */
export function detect(r: Json): Harness | undefined {
	if (obj(r.payload)) return "codex";
	if (r.type === "message" || r.type === "session") return "pi";
	if ((r.type === "assistant" || r.type === "user") && obj(r.message)) return "claude";
	return undefined;
}

/** One JSONL line → events. Unknown harness: detect per line. Bad JSON → []. */
export function parseLine(harness: string | undefined, line: string): TranscriptEvent[] {
	const r = parseJson(line);
	if (!r) return [];
	const h = harness === "claude" || harness === "codex" || harness === "pi" ? harness : detect(r);
	switch (h) {
		case "claude":
			return parseClaude(r);
		case "codex":
			return parseCodex(r);
		case "pi":
			return parsePi(r);
	}
	return [];
}

const ARG_KEYS = ["command", "cmd", "file_path", "path", "pattern", "url", "query", "description"];
const CODEX_EXEC_CMD = /exec_command\(\{\s*cmd:\s*("(?:[^"\\]|\\.)*")/g;

/** Collapse whitespace and clip to n characters (by code point). */
export function clip(s: string, n: number): string {
	const flat = s.split(/\s+/).filter(Boolean).join(" ");
	const chars = [...flat];
	return chars.length > n ? chars.slice(0, n).join("") + "…" : flat;
}

/**
 * The most telling argument of a tool call (the command, path or pattern),
 * clipped to one line, instead of the raw JSON. Codex sends commands as argv
 * arrays, and its exec tool takes JS that calls tools.exec_command({cmd: "…"}).
 */
export function toolArg(input: string, n = 120): string {
	const fields = parseJson(input);
	if (fields) {
		for (const k of ARG_KEYS) {
			const v = fields[k];
			if (typeof v === "string") return clip(v, n);
			if (Array.isArray(v) && v.every((x) => typeof x === "string")) return clip(v.join(" "), n);
		}
	}
	const cmds: string[] = [];
	for (const m of input.matchAll(CODEX_EXEC_CMD)) {
		try {
			cmds.push(JSON.parse(m[1]!) as string);
		} catch {
			// not a JSON string literal; skip it
		}
	}
	if (cmds.length > 0) return clip(cmds.join(" ; "), n);
	return clip(input, n);
}

/** What a card shows for one event, or undefined if the event isn't shown. */
export function activityLine(e: TranscriptEvent, n = 120): string | undefined {
	if (e.kind === "tool") {
		const arg = toolArg(e.text ?? "", n);
		return arg ? `${e.tool ?? "tool"}  ${arg}` : (e.tool ?? "tool");
	}
	if (e.kind === "text" && e.text?.trim()) return `“${clip(e.text, n)}”`;
	return undefined;
}
