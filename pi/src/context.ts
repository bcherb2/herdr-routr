// What the main agent's model sees of its own session: the request-time
// rewrites applied by the `context` hook. The session file keeps everything.

import type { ContextEvent } from "@earendil-works/pi-coding-agent";

type Message = ContextEvent["messages"][number];

/** The [main] config section as `routr main --json` prints it; -1 is no limit. */
export interface MainAgent {
	context: "truncate" | "summarize" | "off";
	at: number;
	keep_turns: number;
	keep_tokens: number;
	/** a truncation keeps the session's first turn as it was, in front of its note */
	keep_head: boolean;
	/** at most this many tokens of that first turn */
	head_tokens: number;
	strip_reasoning: boolean;
}

/**
 * The built-in [main] with the ROUTR_MAIN_* overrides applied: what holds
 * when routr can't say (not found, older than `routr main`, or a config it
 * rejects). As in routr, 0 is the default and -1 no limit; a value it
 * can't read is the default.
 */
export function mainFromEnv(env: NodeJS.ProcessEnv = process.env): MainAgent {
	const num = (v: string | undefined, def: number): number => {
		const n = /^[+-]?\d+$/.test(v ?? "") ? Number(v) : 0;
		return n === 0 || n < -1 ? def : n;
	};
	const context = env.ROUTR_MAIN_CONTEXT;
	const on = (v: string | undefined): boolean => !["0", "false", "off"].includes((v ?? "").toLowerCase());
	return {
		context: context === "truncate" || context === "summarize" || context === "off" ? context : "truncate",
		at: num(env.ROUTR_MAIN_AT, 150_000),
		keep_turns: num(env.ROUTR_MAIN_KEEP_TURNS, 3),
		keep_tokens: num(env.ROUTR_MAIN_KEEP_TOKENS, 40_000),
		keep_head: on(env.ROUTR_MAIN_KEEP_HEAD),
		head_tokens: num(env.ROUTR_MAIN_HEAD_TOKENS, 40_000),
		strip_reasoning: on(env.ROUTR_MAIN_STRIP_REASONING),
	};
}

/**
 * `routr main --json` output, or undefined if it is not that shape. A
 * routr from before keep_head and head_tokens leaves them out: their
 * defaults.
 */
export function parseMain(stdout: string): MainAgent | undefined {
	let v: Partial<MainAgent>;
	try {
		v = JSON.parse(stdout) as Partial<MainAgent>;
	} catch {
		return undefined;
	}
	const head = { keep_head: v.keep_head ?? true, head_tokens: v.head_tokens || 40_000 };
	const ints = [v.at, v.keep_turns, v.keep_tokens, head.head_tokens].every((n) => Number.isInteger(n) && (n as number) >= -1);
	const ok = ["truncate", "summarize", "off"].includes(v.context as string) && ints && typeof v.strip_reasoning === "boolean" && typeof head.keep_head === "boolean";
	return ok ? ({ ...v, ...head } as MainAgent) : undefined;
}

/**
 * The messages with reasoning dropped from every assistant message before the
 * latest incoming one (yours, a routr wake, a compaction summary): Pi replays
 * each earlier thinking block, and DeepSeek bills it as input on every later
 * turn. The turn in progress keeps its reasoning, which DeepSeek needs for its
 * tool loop. Each new incoming message strips the turn before it, so that
 * turn is sent uncached once; everything older is already stripped and stays
 * cached. A message you steer in mid-turn counts as incoming too. undefined
 * when there is nothing to drop.
 */
export function stripOldReasoning(messages: readonly Message[]): Message[] | undefined {
	let start = -1; // the latest message the model did not write
	for (let i = messages.length - 1; i >= 0; i--) {
		const role = messages[i]!.role;
		if (role !== "assistant" && role !== "toolResult") {
			start = i;
			break;
		}
	}
	let out: Message[] | undefined;
	for (let i = 0; i < start; i++) {
		const m = messages[i]!;
		if (m.role !== "assistant") continue;
		const kept = m.content.filter((c) => c.type !== "thinking");
		// only reasoning: emptied, Pi would drop the message and two user messages would meet
		if (kept.length === m.content.length || kept.length === 0) continue;
		out ??= messages.slice();
		out[i] = { ...m, content: kept };
	}
	return out;
}

/** How a truncation's note begins when it keeps the session's first turn in front of it. */
export const HEAD_NOTE_START = "routr cut the conversation between the session's first turn and its last turns";

/** Pi's wrapper around a compaction summary (COMPACTION_SUMMARY_PREFIX in core/messages.ts, not exported). */
const SUMMARY_PREFIX = "The conversation history before this point was compacted into the following summary:\n\n<summary>\n";
/** Anthropic takes at most this many cache breakpoints in a request: system, tools and message blocks. */
const MAX_BREAKPOINTS = 4;
/** blocks Anthropic won't take a breakpoint on */
const UNMARKABLE = new Set(["thinking", "redacted_thinking"]);

interface Block {
	type?: unknown;
	text?: unknown;
	cache_control?: unknown;
}
interface AnthropicMessage {
	role?: unknown;
	content?: unknown;
}

const marked = (x: unknown): boolean => typeof x === "object" && x !== null && (x as Block).cache_control != null;
const blocksOf = (m: AnthropicMessage): Block[] =>
	typeof m.content === "string" ? [{ type: "text", text: m.content }] : Array.isArray(m.content) ? (m.content as Block[]) : [];

/**
 * An Anthropic Messages request (Pi's `anthropic-messages` API, OpenRouter's
 * Claude models too) with a cache breakpoint at the end of the session's
 * first turn, which a truncation keeps in front of its note: on the last
 * block before the note that takes one (not thinking). Pi marks the system
 * prompt, the tools and the last message; Anthropic looks back only 20
 * blocks from each, so without this every cut would write the first turn to
 * the cache again. The note is found by its text rather than by counting
 * messages, as Pi's converter drops empty and aborted messages, adds
 * results for orphaned tool calls, and joins a turn's tool results in one
 * user message. The breakpoint is a copy of Pi's own (its ttl too:
 * Anthropic refuses a shorter one before a longer). undefined when the
 * request has no such note, Pi marks nothing (cacheRetention "none"), the
 * block is marked already, or the request has its 4 breakpoints.
 */
export function markHeadEnd(payload: unknown): unknown {
	const messages = (payload as { messages?: unknown } | null)?.messages;
	if (!Array.isArray(messages)) return undefined;
	const p = payload as { system?: unknown; tools?: unknown };
	const marks: Block[] = [];
	for (const list of [p.system, p.tools]) if (Array.isArray(list)) marks.push(...(list as Block[]).filter(marked));
	// every block but system ones, in order: a system message never sits between the head and its note
	const flat: { i: number; j: number; block: Block }[] = [];
	(messages as AnthropicMessage[]).forEach((m, i) => {
		const blocks = blocksOf(m);
		marks.push(...blocks.filter(marked));
		if (m.role !== "system") blocks.forEach((block, j) => flat.push({ i, j, block }));
	});
	if (marks.length === 0 || marks.length + (marked(payload) ? 1 : 0) >= MAX_BREAKPOINTS) return undefined;
	const note = flat.findIndex(
		({ i, block }) => (messages[i] as AnthropicMessage).role === "user" && block.type === "text" && typeof block.text === "string" && block.text.startsWith(SUMMARY_PREFIX + HEAD_NOTE_START),
	);
	let k = note - 1;
	while (k >= 0 && UNMARKABLE.has(flat[k]!.block.type as string)) k--;
	if (note < 0 || k < 0 || marked(flat[k]!.block)) return undefined;
	const { i, j } = flat[k]!;
	const blocks = blocksOf(messages[i] as AnthropicMessage).map((b, n) => (n === j ? { ...b, cache_control: marks[0]!.cache_control } : b));
	const out = messages.slice();
	out[i] = { ...(messages[i] as object), content: blocks };
	return { ...(payload as object), messages: out };
}
