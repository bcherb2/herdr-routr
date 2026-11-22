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
// Ids are per registry, and the model may run routr against another repo's
// (`cd ../other && routr start …`). So each claim says where it was made
// (Place): the registry a "queued" line or a record's `run_dir` names, else
// where the command line put the call (`cd`, `--root`, ROUTR_STATE_DIR).
// Claims in the session's own working directory are the top-level fields;
// the rest are `elsewhere`, for the front end to resolve to a registry.
//
// A command that already waited for the result (`routr wait`, `--wait`,
// `routr result`) and returned it marks those workers as `seen`, so the
// extension doesn't wake the model with a result it just read. While such a
// command runs, `waitCover` says which workers it will return.

/**
 * Where a routr call keeps its registry, when that may not be the session's:
 * the registry itself (ROUTR_STATE_DIR, or the one a "queued" line or a
 * record's `run_dir` names), a repo root (`--root`), or the directory the
 * call ran in (`cd`), whose registry the CLI's rule finds
 * (skills/routr/cli.md, "Where state lives"). Paths are as written: a
 * relative one is against the session's working directory, and `~` is the
 * user's home. The front end resolves it (Coordinator `locate`).
 */
export type Place = { registry: string } | { root: string } | { cwd: string };

/** Where a call is: the session's working directory (undefined), a Place, or somewhere only the shell knows (null). */
type Where = Place | undefined | null;

/** A cache key for a Place. */
export function placeKey(at: Place): string {
	return JSON.stringify(at);
}

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
	/** the same, made at another place than the session's working directory; absent when there are none. Claims made where only the shell knows are dropped. */
	elsewhere?: PlacedClaims[];
}

/** Claims made at one place. */
export interface PlacedClaims extends Omit<Claims, "elsewhere"> {
	at: Place;
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

/** One `routr` invocation: its subcommand words, and where it runs. */
interface Call {
	args: string[];
	at: Where;
}

/** A `routr` (or "$ROUTR_BIN") invocation per simple command of a shell line, each with where the `cd`s, exports and flags before it put it. */
function routrCalls(command: string): Call[] {
	const calls: Call[] = [];
	let dir: string | undefined | null; // where `cd` went: undefined = nowhere, null = only the shell knows
	let state: string | undefined; // an exported ROUTR_STATE_DIR
	for (const words of shellCommands(command)) {
		const at = words.findIndex((w) => ROUTR_WORD.test(w));
		if (at >= 0) {
			const args = words.slice(at + 1);
			calls.push({ args, at: placeOf(dir, stateDir(words.slice(0, at)) ?? state, args) });
			continue;
		}
		const [verb, ...rest] = words;
		if (verb === "cd" || verb === "pushd") {
			const to = rest.find((w) => !/^-[LPe@]+$/.test(w)) ?? "~";
			dir = to === "-" ? null : under(dir, to);
		} else if (verb === "export") {
			state = stateDir(rest) ?? state;
		}
	}
	return calls;
}

/** The ROUTR_STATE_DIR among assignment words (`ROUTR_STATE_DIR=… routr`, `export ROUTR_STATE_DIR=…`). */
function stateDir(words: readonly string[]): string | undefined {
	let v: string | undefined;
	for (const w of words) if (w.startsWith("ROUTR_STATE_DIR=")) v = w.slice("ROUTR_STATE_DIR=".length);
	return v || undefined;
}

/** Path p as a command gives it, against dir (where `cd` went); null when only the shell knows it. */
function under(dir: string | undefined | null, p: string): string | null {
	if (p.includes("$")) return null; // a variable, or a $(…)
	if (p.startsWith("/") || p === "~" || p.startsWith("~/")) return p;
	if (dir === null) return null;
	return dir === undefined ? p : `${dir.replace(/\/+$/, "")}/${p}`;
}

/** Where a routr call keeps its registry, by the CLI's order: --root, then ROUTR_STATE_DIR, then its directory. */
function placeOf(dir: string | undefined | null, state: string | undefined, args: string[]): Where {
	const root = flagValue(args, "--root");
	if (root !== undefined) {
		const r = under(dir, root);
		return r === null ? null : { root: r };
	}
	if (state !== undefined) {
		const r = under(dir, state);
		return r === null ? null : { registry: r };
	}
	return dir === undefined || dir === null ? dir : { cwd: dir };
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

/** A registry record's id and chain, and its registry when its `run_dir` (<registry>/runs/<id>) names it. */
interface Rec {
	id?: number;
	chain?: string;
	registry?: string;
}

function records(values: unknown[]): Rec[] {
	const out: Rec[] = [];
	for (const v of values) {
		for (const it of Array.isArray(v) ? v : [v]) {
			if (it === null || typeof it !== "object") continue;
			const r = it as Record<string, unknown>;
			const dir = typeof r.run_dir === "string" ? /^(\/.*)\/runs\/\d+\/?$/.exec(r.run_dir)?.[1] : undefined;
			out.push({
				id: typeof r.id === "number" ? r.id : undefined,
				chain: typeof r.chain === "string" && r.chain ? r.chain : undefined,
				registry: dir,
			});
		}
	}
	return out;
}

/** A record's place: the registry its run_dir names, else `at`. */
function recAt(r: Rec, at: Where): Where {
	return r.registry ? { registry: r.registry } : at;
}

/**
 * "routr: queued worker 7 review · codex (…); registry /repo/.routr" (start)
 * and "routr: queued 8 plan · claude (…) step plan; registry …" (flow start),
 * on stderr. A routr older than the registry note ends the line before it.
 */
const QUEUED = /^routr: queued (?:worker )?(\d+) (.*)$/gm;
/** "routr: queued follow-up 2 for 7 review · codex; it goes into its tab; registry /repo/.routr" (send). */
const FOLLOW_UP = /^routr: queued follow-up \d+ for (\d+) (.*)$/gm;
/** The registry note that ends a "queued" line. */
const REGISTRY_NOTE = /; registry (\/.*)$/;
/** "routr: cancelled 4 research · claude" per worker `routr cancel` cancelled. */
const CANCELLED = /^routr: cancelled (\d+)\b/gm;
/**
 * "still running: …. Run: routr wait 4" (a wait that ran out of time) and
 * "… no final message yet (running). Run: routr wait 4" (`routr result`).
 * Only these: help text names `routr wait ID|CHAIN|-` too.
 */
const WAIT_AGAIN = /Run: routr wait (\S+)\s*$/gm;

/** Workers `routr cancel` (run at `at`) cancelled: its "cancelled" lines, or its --json record; each with its place. */
function cancelledBy(output: string, values: unknown[], at: Where): Map<number, Where> {
	const out = new Map<number, Where>();
	for (const m of output.matchAll(CANCELLED)) out.set(Number(m[1]), at);
	for (const v of values) {
		if (v === null || typeof v !== "object" || Array.isArray(v)) continue;
		const r = v as Record<string, unknown>;
		if (r.action === "cancel") for (const rec of records([r.workers])) if (rec.id !== undefined) out.set(rec.id, recAt(rec, at));
	}
	return out;
}

/** Ids a "queued" line (QUEUED, FOLLOW_UP) names, with the registry its note names. */
function registryNotes(output: string, line: RegExp): Map<number, Place | undefined> {
	const out = new Map<number, Place | undefined>();
	for (const m of output.matchAll(line)) {
		const reg = REGISTRY_NOTE.exec(m[2]!)?.[1];
		out.set(Number(m[1]), reg ? { registry: reg } : undefined);
	}
	return out;
}

interface Sets {
	ids: Set<number>;
	chains: Set<string>;
	seen: Set<number>;
	seenChains: Set<string>;
	cancelled: Set<number>;
	cancelledChains: Set<string>;
}

/** Claims grouped by where they were made; those made where only the shell knows are dropped. */
class Tally {
	private readonly by = new Map<string, { at: Place | undefined; sets: Sets }>();

	at(where: Where): Sets | undefined {
		if (where === null) return undefined;
		const k = where === undefined ? "" : placeKey(where);
		let e = this.by.get(k);
		if (!e) {
			e = { at: where, sets: { ids: new Set(), chains: new Set(), seen: new Set(), seenChains: new Set(), cancelled: new Set(), cancelledChains: new Set() } };
			this.by.set(k, e);
		}
		return e.sets;
	}

	claims(): Claims {
		const list = (s: Sets): Omit<Claims, "elsewhere"> => ({
			ids: [...s.ids],
			chains: [...s.chains],
			seen: [...s.seen],
			seenChains: [...s.seenChains],
			cancelled: [...s.cancelled],
			cancelledChains: [...s.cancelledChains],
		});
		const here = this.by.get("");
		const elsewhere: PlacedClaims[] = [];
		for (const { at, sets } of this.by.values()) {
			if (at !== undefined && Object.values(sets).some((x: Set<unknown>) => x.size > 0)) elsewhere.push({ at, ...list(sets) });
		}
		const top = here ? list(here.sets) : { ...EMPTY };
		return elsewhere.length > 0 ? { ...top, elsewhere } : top;
	}
}

/**
 * Claims made by one bash call.
 * @param command the bash tool's `command` input
 * @param output its text output (stdout and stderr as Pi shows them)
 * @param startedMeanwhile this session's workers (of its own registry)
 *   created while the call ran: a wait on what the call started itself
 *   returned them, even when the output doesn't name them
 */
export function parseClaims(command: string, output: string, startedMeanwhile: readonly number[] = []): Claims {
	const calls = routrCalls(stripHeredocs(command));
	if (calls.length === 0) return EMPTY;
	const starts = calls.filter((c) => sub(c.args) === "start");
	const flows = calls.filter((c) => sub(c.args) === "flow start");
	const waited = calls.map((c) => waits(c.args)).filter((w) => w !== undefined);
	const readers = calls.filter((c) => waits(c.args) !== undefined || sub(c.args) === "result");
	// output lines can't be told apart by call: they count where the first call of their kind ran
	const startAt = (starts[0] ?? flows[0])?.at;
	const readAt = readers[0]?.at;
	const t = new Tally();

	const followUps = registryNotes(output, FOLLOW_UP);
	for (const c of calls) {
		const s = sub(c.args);
		const tg = target(c.args);
		const p = t.at(c.at);
		if (s === "send" && tg.id !== undefined) {
			const q = followUps.get(tg.id) ? t.at(followUps.get(tg.id)) : p;
			(c.args.includes("--wait") ? q?.seen : q?.ids)?.add(tg.id);
		}
		if (s === "result" && tg.id !== undefined) p?.seen.add(tg.id);
		if (s === "wait") {
			if (tg.id !== undefined) p?.seen.add(tg.id);
			if (tg.chain !== undefined) p?.seenChains.add(tg.chain);
		}
	}

	const started = new Map<number, Where>();
	const queued = registryNotes(output, QUEUED);
	for (const [id, at] of queued) started.set(id, at ?? startAt);

	const values = jsonValues(output);
	const cancels = calls.filter((c) => sub(c.args) === "cancel");
	const cancelled = cancels.length > 0 ? cancelledBy(output, values, cancels[0]!.at) : new Map<number, Where>();
	for (const [id, at] of cancelled) t.at(at)?.cancelled.add(id);
	// a chain counts once the output says it cancelled something: "nothing to cancel" cancelled nothing
	if (cancelled.size > 0) for (const c of cancels) if (target(c.args).chain !== undefined) t.at(c.at)?.cancelledChains.add(target(c.args).chain!);
	if (values.length > 0) {
		for (const r of records(values)) {
			if (r.id !== undefined && (starts.length || flows.length)) started.set(r.id, recAt(r, startAt));
			if (r.id !== undefined && readers.length) t.at(recAt(r, readAt))?.seen.add(r.id);
			if (r.chain !== undefined && (starts.length || flows.length)) t.at(recAt(r, startAt))?.chains.add(r.chain);
		}
	} else {
		const lines = output.split("\n").map((l) => l.trim()).filter(Boolean);
		// Only when nothing else could have printed a bare number (wc -l, a counter …).
		if (starts.length && !calls.some((c) => c.args.includes("--wait")) && !otherOutput(command)) {
			for (const l of lines) if (/^\d+$/.test(l) && !started.has(Number(l))) started.set(Number(l), startAt);
		}
		const flow = flows.find((c) => !c.args.includes("--wait"));
		if (flow) {
			const last = lines.at(-1);
			// the chain is in the registry its steps' "queued" lines name, when they agree
			const regs = new Set([...queued.values()].map((at) => (at ? placeKey(at) : "")));
			const only = regs.size === 1 ? [...queued.values()][0] : undefined;
			if (last && CHAIN_ID.test(last)) t.at(only ?? flow.at)?.chains.add(last);
		}
	}
	for (const [id, at] of started) t.at(at)?.ids.add(id);
	// A wait on what this very command started (`start --wait`, `… | routr wait -`) returned it.
	if (waited.some((w) => w.fresh)) {
		for (const [id, at] of started) t.at(at)?.seen.add(id);
		for (const id of startedMeanwhile) t.at(undefined)!.seen.add(id);
	}

	// A wait that ran out of time names the worker to wait on again.
	for (const m of output.matchAll(WAIT_AGAIN)) {
		const v = m[1]!;
		const p = t.at(readAt);
		if (v === "ID" || v === "CHAIN" || !CHAIN_ID.test(v) || !p) continue;
		if (/^\d+$/.test(v)) {
			p.ids.add(Number(v));
			p.seen.delete(Number(v));
		} else {
			p.chains.add(v);
			p.seenChains.delete(v);
		}
	}

	return t.claims();
}

/** Every place a bash call's routr calls ran at, or its output names a registry of: what a front end resolves for `locate`. */
export function places(command: string, output = ""): Place[] {
	const out = new Map<string, Place>();
	const add = (at: Where): void => {
		if (at) out.set(placeKey(at), at);
	};
	for (const c of routrCalls(stripHeredocs(command))) add(c.at);
	for (const line of [QUEUED, FOLLOW_UP]) for (const at of registryNotes(output, line).values()) add(at);
	for (const r of records(jsonValues(output))) if (r.registry) add({ registry: r.registry });
	for (const v of jsonValues(output)) {
		const workers = v !== null && typeof v === "object" && !Array.isArray(v) ? (v as Record<string, unknown>).workers : undefined;
		for (const r of records([workers])) if (r.registry) add({ registry: r.registry });
	}
	return [...out.values()];
}

/** What a running bash call will hand the model when it returns. */
export interface WaitCover {
	ids: number[];
	chains: string[];
	/** it waits on workers it starts itself, or reads from stdin: any this session starts meanwhile */
	fresh: boolean;
	/** what it waits on at another place than the session's working directory; absent when nothing */
	elsewhere?: { at: Place; ids: number[]; chains: string[] }[];
}

/** The workers a `routr wait` / `--wait` call covers, read before it runs; undefined if it waits on none. */
export function waitCover(command: string): WaitCover | undefined {
	const waited = routrCalls(stripHeredocs(command)).flatMap((c) => {
		const w = waits(c.args);
		return w ? [{ ...w, at: c.at }] : [];
	});
	if (waited.length === 0) return undefined;
	const at = new Map<string, { at: Place | undefined; ids: number[]; chains: string[] }>();
	for (const w of waited) {
		if (w.at === null) continue; // only the shell knows where
		const k = w.at === undefined ? "" : placeKey(w.at);
		const e = at.get(k) ?? { at: w.at, ids: [], chains: [] };
		at.set(k, e);
		if (w.id !== undefined) e.ids.push(w.id);
		if (w.chain !== undefined) e.chains.push(w.chain);
	}
	const here = at.get("");
	const elsewhere = [...at.values()].flatMap((e) => (e.at === undefined ? [] : [{ at: e.at, ids: e.ids, chains: e.chains }]));
	return {
		ids: here?.ids ?? [],
		chains: here?.chains ?? [],
		fresh: waited.some((w) => w.fresh),
		...(elsewhere.length > 0 ? { elsewhere } : {}),
	};
}
