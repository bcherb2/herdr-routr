// Which workers did this session start? Read from its own bash tool calls.
//
// The CLI records the caller's session on each worker (`caller.session`,
// from the PI_SESSION_ID that Pi's bash tool exports), and the extension
// matches that first. This is the fallback for workers without it (an agent
// the CLI didn't detect, or an older CLI), and it also says which workers the
// model is waiting on: it watches the model's `routr` commands and their
// output (Pi merges stderr into it):
//   - `routr start …` says "routr: queued worker ID …" on stderr and prints
//     the id alone on stdout (skills/routr/cli.md); in a pipe every step says
//     "queued", and the rest of a chain is found through the `chain` field;
//   - `routr flow start …` prints the chain id;
//   - `routr send ID …` is a follow-up the model is waiting on;
//   - a `--wait` that timed out says "Run: routr wait ID";
//   - `routr cancel ID|CHAIN` says "routr: cancelled ID …" per worker it
//     cancelled, whose news the model no longer wants.
// `--json` output is read as the registry record(s).
//
// A command that already waited for the result (`routr wait`, `--wait`,
// `routr result`) and returned it marks those workers as `seen`, so the
// extension doesn't wake the model with a result it just read. While such a
// command runs, `waitCover` says which workers it will return.

export interface Claims {
	/** workers the call started, or the model now waits to hear about */
	ids: number[];
	chains: string[];
	/** workers whose result the command already returned */
	seen: number[];
	/** chains whose results the command already returned (`routr wait CHAIN`) */
	seenChains: string[];
	/** workers the command cancelled */
	cancelled: number[];
	/** chains the command cancelled (some of their workers, at least) */
	cancelledChains: string[];
}

const EMPTY: Claims = { ids: [], chains: [], seen: [], seenChains: [], cancelled: [], cancelledChains: [] };

// `routr`, `/path/to/routr`, `$ROUTR_BIN`, `${ROUTR_BIN}`, with quotes removed (shellCommands).
const ROUTR_WORD = /^(?:(?:\S*\/)?routr|\$\{?ROUTR_BIN\}?)$/;

/**
 * The simple commands of a shell line, each as its words with quotes
 * removed: split at unquoted `;` `&&` `||` `|` `&`, newlines and
 * parentheses, never inside quotes. A `$(…)` or `` `…` `` adds its own
 * commands, and stands in its word as `$(…)`. Good enough for
 * agent-written shell; not a shell parser.
 */
function shellCommands(line: string): string[][] {
	const out: string[][] = [];
	let i = 0;
	// The commands up to `stop` (the `)` or backtick that closes a substitution), or the end.
	const parse = (stop?: string): void => {
		let words: string[] = [];
		let word: string | undefined;
		const endWord = (): void => {
			if (word !== undefined) words.push(word);
			word = undefined;
		};
		const endCommand = (): void => {
			endWord();
			if (words.length > 0) out.push(words);
			words = [];
		};
		while (i < line.length) {
			const c = line[i]!;
			if (c === stop) {
				i++;
				break;
			}
			if (c === "\\") {
				word = (word ?? "") + (line[i + 1] ?? "");
				i += 2;
			} else if (c === "'") {
				const end = line.indexOf("'", i + 1);
				const to = end < 0 ? line.length : end;
				word = (word ?? "") + line.slice(i + 1, to);
				i = to + 1;
			} else if (c === '"') {
				i++;
				word = (word ?? "") + doubleQuoted();
			} else if (c === "$" && line[i + 1] === "(") {
				i += 2;
				parse(")");
				word = (word ?? "") + "$(…)";
			} else if (c === "`") {
				i++;
				parse("`");
				word = (word ?? "") + "$(…)";
			} else if (c === "&" && (line[i - 1] === ">" || line[i - 1] === "<" || line[i + 1] === ">")) {
				word = (word ?? "") + c; // a redirection: 2>&1, &>file
				i++;
			} else if (";&|()\n".includes(c)) {
				endCommand();
				i++;
			} else if (/\s/.test(c)) {
				endWord();
				i++;
			} else {
				word = (word ?? "") + c;
				i++;
			}
		}
		endCommand();
	};
	// The text of a "…" word, from just inside the quote; its substitutions are parsed as commands.
	const doubleQuoted = (): string => {
		let s = "";
		while (i < line.length) {
			const c = line[i]!;
			if (c === '"') {
				i++;
				break;
			}
			if (c === "\\") {
				s += line[i + 1] ?? "";
				i += 2;
			} else if (c === "$" && line[i + 1] === "(") {
				i += 2;
				parse(")");
				s += "$(…)";
			} else if (c === "`") {
				i++;
				parse("`");
				s += "$(…)";
			} else {
				s += c;
				i++;
			}
		}
		return s;
	};
	parse();
	return out;
}

/** A `routr` (or "$ROUTR_BIN") invocation's subcommand words, per simple command of a shell line. */
function routrCalls(command: string): string[][] {
	const calls: string[][] = [];
	for (const words of shellCommands(command)) {
		const at = words.findIndex((w) => ROUTR_WORD.test(w));
		if (at >= 0) calls.push(words.slice(at + 1));
	}
	return calls;
}

/** Shell words that print nothing themselves, so a bare number in the output came from routr. */
const QUIET = /^(for|do|done|while|until|if|then|else|elif|fi|cd|export|set|mkdir|true|:|\{|\})(\s|$)/;

/** Drop here-document bodies (a brief on stdin), which are data, not commands. */
function stripHeredocs(command: string): string {
	return command.replace(/<<-?\s*(['"]?)(\w+)\1([^\n]*)\n[\s\S]*?\n\s*\2\s*(?=\n|$)/g, "$3");
}

/** Does anything besides routr in this command produce output? */
function otherOutput(command: string): boolean {
	return shellCommands(stripHeredocs(command)).some(
		(words) => !words.some((w) => ROUTR_WORD.test(w)) && !QUIET.test(words.join(" ")) && !words.every((w) => /^[A-Za-z_]\w*=/.test(w)),
	);
}

/** Flags that take a value (skills/routr/cli.md); the rest are booleans. */
const VALUE_FLAGS = new Set([
	"--role", "--harness", "--model", "--effort", "--permissions", "--cwd", "--output", "--after", "--gate",
	"--chain", "--step", "--param", "--in", "--timeout", "--task", "--root", "--id", "--verdict", "--caller",
]);

/** The positional words of one call: subcommand first, then its arguments. */
function positionals(args: string[]): string[] {
	const out: string[] = [];
	for (let i = 0; i < args.length; i++) {
		const a = args[i]!;
		if (a === "-" || !a.startsWith("-")) out.push(a);
		else if (!a.includes("=") && VALUE_FLAGS.has(a)) i++;
	}
	return out;
}

function sub(args: string[]): string {
	const p = positionals(args);
	return p[0] === "flow" ? p.slice(0, 2).join(" ") : (p[0] ?? "");
}

function flagValue(args: string[], name: string): string | undefined {
	for (let i = 0; i < args.length; i++) {
		if (args[i] === name) return args[i + 1];
		if (args[i]!.startsWith(`${name}=`)) return args[i]!.slice(name.length + 1);
	}
	return undefined;
}

const CHAIN_ID = /^[A-Za-z0-9][\w.:-]*$/;

/** The target of `routr send|wait|result X`: a worker id, a chain, or something only the shell knows ("-", "$id"). */
function target(args: string[]): { id?: number; chain?: string } {
	const v = positionals(args)[1];
	if (v === undefined) return {};
	if (/^\d+$/.test(v)) return { id: Number(v) };
	return CHAIN_ID.test(v) ? { chain: v } : {};
}

/** Does this call block until workers finish, and does it name them (else: the ones it starts or reads from stdin)? */
function waits(args: string[]): { id?: number; chain?: string; fresh: boolean } | undefined {
	const s = sub(args);
	const wait = args.includes("--wait");
	if (s === "wait") {
		const t = target(args);
		return { ...t, fresh: t.id === undefined && t.chain === undefined };
	}
	if (s === "send" && wait) return { id: target(args).id, fresh: false };
	if (s === "start" && wait) return { fresh: true };
	if (s === "flow start" && wait) return { chain: flagValue(args, "--chain"), fresh: true };
	return undefined;
}

/**
 * Every top-level JSON object or array in the text that starts a line.
 * `--json` output shares the tool output with stderr lines ("routr: queued
 * worker …") and, in a loop, with other records.
 */
export function jsonValues(text: string): unknown[] {
	const out: unknown[] = [];
	const starts = /^[ \t]*[[{]/gm;
	let m: RegExpExecArray | null;
	while ((m = starts.exec(text))) {
		const from = m.index + m[0].length - 1;
		const end = balancedEnd(text, from);
		if (end < 0) continue;
		try {
			out.push(JSON.parse(text.slice(from, end)));
			starts.lastIndex = end;
		} catch {
			// not JSON after all; try the next line
		}
	}
	return out;
}

/** Index just past the bracket that closes the one at `from`, or -1. */
function balancedEnd(text: string, from: number): number {
	let depth = 0;
	let inString = false;
	for (let i = from; i < text.length; i++) {
		const c = text[i];
		if (inString) {
			if (c === "\\") i++;
			else if (c === '"') inString = false;
		} else if (c === '"') inString = true;
		else if (c === "{" || c === "[") depth++;
		else if (c === "}" || c === "]") {
			depth--;
			if (depth === 0) return i + 1;
		}
	}
	return -1;
}

function records(values: unknown[]): { ids: number[]; chains: string[] } {
	const ids: number[] = [];
	const chains: string[] = [];
	for (const v of values) {
		for (const it of Array.isArray(v) ? v : [v]) {
			if (it === null || typeof it !== "object") continue;
			const r = it as Record<string, unknown>;
			if (typeof r.id === "number") ids.push(r.id);
			if (typeof r.chain === "string" && r.chain) chains.push(r.chain);
		}
	}
	return { ids, chains };
}

/** "routr: queued worker 7 review · codex …" (start) and "routr: queued 8 plan · claude …" (flow start), on stderr. */
const QUEUED = /^routr: queued (?:worker )?(\d+) /gm;
/** "routr: cancelled 4 research · claude" per worker `routr cancel` cancelled. */
const CANCELLED = /^routr: cancelled (\d+)\b/gm;
/**
 * "still running: …. Run: routr wait 4" (a wait that ran out of time) and
 * "… no final message yet (running). Run: routr wait 4" (`routr result`).
 * Only these: help text names `routr wait ID|CHAIN|-` too.
 */
const WAIT_AGAIN = /Run: routr wait (\S+)\s*$/gm;

/** Workers `routr cancel` cancelled: its "cancelled" lines, or its --json record. */
function cancelledBy(output: string, values: unknown[]): Set<number> {
	const out = new Set<number>();
	for (const m of output.matchAll(CANCELLED)) out.add(Number(m[1]));
	for (const v of values) {
		if (v === null || typeof v !== "object" || Array.isArray(v)) continue;
		const r = v as Record<string, unknown>;
		if (r.action === "cancel") for (const id of records([r.workers]).ids) out.add(id);
	}
	return out;
}

/**
 * Claims made by one bash call.
 * @param command the bash tool's `command` input
 * @param output its text output (stdout and stderr as Pi shows them)
 * @param startedMeanwhile this session's workers created while the call ran:
 *   a wait on what the call started itself returned them, even when the
 *   output doesn't name them
 */
export function parseClaims(command: string, output: string, startedMeanwhile: readonly number[] = []): Claims {
	const calls = routrCalls(stripHeredocs(command));
	if (calls.length === 0) return EMPTY;
	const starts = calls.some((a) => sub(a) === "start");
	const flow = calls.some((a) => sub(a) === "flow start");
	const waited = calls.map(waits).filter((w) => w !== undefined);
	const reads = waited.length > 0 || calls.some((a) => sub(a) === "result");

	const ids = new Set<number>();
	const chains = new Set<string>();
	const seen = new Set<number>();
	const seenChains = new Set<string>();

	for (const a of calls) {
		const s = sub(a);
		const t = target(a);
		if (s === "send" && t.id !== undefined) (a.includes("--wait") ? seen : ids).add(t.id);
		if (s === "result" && t.id !== undefined) seen.add(t.id);
		if (s === "wait") {
			if (t.id !== undefined) seen.add(t.id);
			if (t.chain !== undefined) seenChains.add(t.chain);
		}
	}

	const started = new Set<number>();
	for (const m of output.matchAll(QUEUED)) started.add(Number(m[1]));

	const values = jsonValues(output);
	const cancels = calls.filter((a) => sub(a) === "cancel");
	const cancelled = cancels.length > 0 ? cancelledBy(output, values) : new Set<number>();
	// a chain counts once the output says it cancelled something: "nothing to cancel" cancelled nothing
	const cancelledChains = cancelled.size > 0 ? cancels.flatMap((a) => target(a).chain ?? []) : [];
	if (values.length > 0) {
		const r = records(values);
		for (const id of r.ids) {
			if (starts || flow) started.add(id);
			if (reads) seen.add(id);
		}
		for (const c of r.chains) if (starts || flow) chains.add(c);
	} else {
		const lines = output.split("\n").map((l) => l.trim()).filter(Boolean);
		// Only when nothing else could have printed a bare number (wc -l, a counter …).
		if (starts && !calls.some((a) => a.includes("--wait")) && !otherOutput(command)) {
			for (const l of lines) if (/^\d+$/.test(l)) started.add(Number(l));
		}
		if (calls.some((a) => sub(a) === "flow start" && !a.includes("--wait"))) {
			const last = lines.at(-1);
			if (last && CHAIN_ID.test(last)) chains.add(last);
		}
	}
	for (const id of started) ids.add(id);
	// A wait on what this very command started (`start --wait`, `… | routr wait -`) returned it.
	if (waited.some((w) => w.fresh)) for (const id of [...started, ...startedMeanwhile]) seen.add(id);

	// A wait that ran out of time names the worker to wait on again.
	for (const m of output.matchAll(WAIT_AGAIN)) {
		const v = m[1]!;
		if (v === "ID" || v === "CHAIN" || !CHAIN_ID.test(v)) continue;
		if (/^\d+$/.test(v)) {
			ids.add(Number(v));
			seen.delete(Number(v));
		} else {
			chains.add(v);
			seenChains.delete(v);
		}
	}

	return {
		ids: [...ids],
		chains: [...chains],
		seen: [...seen],
		seenChains: [...seenChains],
		cancelled: [...cancelled],
		cancelledChains: [...new Set(cancelledChains)],
	};
}

/** What a running bash call will hand the model when it returns. */
export interface WaitCover {
	ids: number[];
	chains: string[];
	/** it waits on workers it starts itself, or reads from stdin: any this session starts meanwhile */
	fresh: boolean;
}

/** The workers a `routr wait` / `--wait` call covers, read before it runs; undefined if it waits on none. */
export function waitCover(command: string): WaitCover | undefined {
	const waited = routrCalls(stripHeredocs(command))
		.map(waits)
		.filter((w) => w !== undefined);
	if (waited.length === 0) return undefined;
	return {
		ids: waited.flatMap((w) => (w.id === undefined ? [] : [w.id])),
		chains: waited.flatMap((w) => (w.chain === undefined ? [] : [w.chain])),
		fresh: waited.some((w) => w.fresh),
	};
}
