// The main agent's model fallback: when its model fails in a way waiting
// won't fix (credit, quota, a login), move to the next model of the
// [[fallback]] rule routr has for it (`routr fallback MODEL --error MSG`).

/** `routr fallback --json`: rule 0 and no models when no rule matches; lasting with --error. */
export interface FallbackRule {
	rule: number;
	to: string[];
	lasting?: boolean;
}

/** A Pi model named by a pi: entry, and the thinking level after its last colon, if any. */
export interface PiRef {
	provider: string;
	id: string;
	thinking?: string;
}

const THINKING = new Set(["off", "minimal", "low", "medium", "high", "xhigh"]);

export function parseFallback(stdout: string): FallbackRule | undefined {
	let v: { rule?: unknown; to?: unknown; lasting?: unknown };
	try {
		v = JSON.parse(stdout) as typeof v;
	} catch {
		return undefined;
	}
	if (!Number.isInteger(v.rule) || !Array.isArray(v.to) || !v.to.every((m) => typeof m === "string")) return undefined;
	return { rule: v.rule as number, to: v.to as string[], lasting: typeof v.lasting === "boolean" ? v.lasting : undefined };
}

/** pi:PROVIDER/ID[:THINKING]; undefined for another harness's model. */
export function piRef(entry: string): PiRef | undefined {
	if (!entry.startsWith("pi:")) return undefined;
	let rest = entry.slice(3);
	let thinking: string | undefined;
	const colon = rest.lastIndexOf(":");
	if (colon > 0 && THINKING.has(rest.slice(colon + 1))) {
		thinking = rest.slice(colon + 1);
		rest = rest.slice(0, colon);
	}
	const slash = rest.indexOf("/");
	if (slash <= 0 || slash === rest.length - 1) return undefined;
	const ref: PiRef = { provider: rest.slice(0, slash), id: rest.slice(slash + 1) };
	return thinking ? { ...ref, thinking } : ref;
}

/** A Pi model as routr names it: pi:PROVIDER/ID. */
export function modelName(m: { provider: string; id: string }): string {
	return `pi:${m.provider}/${m.id}`;
}

/** How a run failed: its error, and the model that failed (pi:PROVIDER/ID, when the message names it). */
export interface Failure {
	error: string;
	model?: string;
}

/** The failure the run ended with, from its last assistant message; undefined when it answered or was stopped. */
export function failure(messages: readonly unknown[]): Failure | undefined {
	for (let i = messages.length - 1; i >= 0; i--) {
		const m = messages[i] as { role?: string; stopReason?: string; errorMessage?: string; provider?: string; model?: string } | undefined;
		if (m?.role !== "assistant") continue;
		if (m.stopReason !== "error") return undefined;
		const error = m.errorMessage || "an API error";
		return m.provider && m.model ? { error, model: modelName({ provider: m.provider, id: m.model }) } : { error };
	}
	return undefined;
}

/**
 * Where the main agent is in a rule's list. A fallback that fails too moves
 * down the list of the rule its chain started with, never its own rule: an
 * Opus chain doesn't drift to what a cheap model falls back to, and no
 * chain loops.
 */
export class Chain {
	private rule = 0;
	private to: string[] = [];
	private at = -1; // the entry in use; -1: the model the chain started from
	private current = "";

	/** What follows `failing` in its chain; undefined when it is in none (ask routr). */
	next(failing: string): { rule: number; candidates: string[] } | undefined {
		if (this.rule === 0 || failing !== this.current) return undefined;
		return { rule: this.rule, candidates: this.to.slice(this.at + 1) };
	}

	/** A chain starts: `model` failed, and routr gave rule `rule`'s list. */
	start(model: string, rule: number, to: readonly string[]): void {
		this.rule = rule;
		this.to = [...to];
		this.at = -1;
		this.current = model;
	}

	/** The main agent moved to `entry` of the list. */
	moved(entry: string): void {
		this.at = this.to.indexOf(entry);
		this.current = entry.replace(/:(off|minimal|low|medium|high|xhigh)$/, "");
	}
}
