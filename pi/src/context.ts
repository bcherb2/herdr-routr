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
	return {
		context: context === "truncate" || context === "summarize" || context === "off" ? context : "truncate",
		at: num(env.ROUTR_MAIN_AT, 150_000),
		keep_turns: num(env.ROUTR_MAIN_KEEP_TURNS, 3),
		keep_tokens: num(env.ROUTR_MAIN_KEEP_TOKENS, 40_000),
		strip_reasoning: !["0", "false", "off"].includes((env.ROUTR_MAIN_STRIP_REASONING ?? "").toLowerCase()),
	};
}

/** `routr main --json` output, or undefined if it is not that shape. */
export function parseMain(stdout: string): MainAgent | undefined {
	let v: Partial<MainAgent>;
	try {
		v = JSON.parse(stdout) as Partial<MainAgent>;
	} catch {
		return undefined;
	}
	const ints = [v.at, v.keep_turns, v.keep_tokens].every((n) => Number.isInteger(n) && (n as number) >= -1);
	const ok = ["truncate", "summarize", "off"].includes(v.context as string) && ints && typeof v.strip_reasoning === "boolean";
	return ok ? (v as MainAgent) : undefined;
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
