// How the main agent's context is cut once it passes [main] at: the
// truncation, which needs no model call, and the instructions for Pi's own
// summary. The session file keeps everything either way.

import { buildSessionProjection, estimateTokens, sessionEntryToContextMessages, type CompactionResult, type SessionEntry } from "@earendil-works/pi-coding-agent";
import type { Worker } from "../../frontend/core/index.ts";
import { HEAD_NOTE_START, type MainAgent } from "./context.ts";

type Projected = ReturnType<typeof buildSessionProjection>["entries"][number];
type Message = Projected["messages"][number];

/** What a truncation records with its compaction entry. */
export interface TruncationDetails {
	by: "routr";
	context: "truncate";
	/** whole turns kept; a split turn counts as one */
	turns: number;
	/** the latest turn alone was over keep_tokens: only its last steps were kept */
	split: boolean;
	/** the session's first turn, sent in front of the note; every later truncation copies it */
	head?: Head;
}

/**
 * The session's first turn as a truncation keeps it: what it takes to
 * rebuild it the same on every request, so it stays cached. Taken at the
 * first truncation; later ones copy it unchanged, whatever [main] says by
 * then.
 */
export interface Head {
	/** its last entry: the head is the branch from the start through it */
	endId: string;
	/** head_tokens when it was taken, or its share of a budget (fitted); -1 is no limit */
	max: number;
	/** Pi's estimate of it as kept */
	tokens: number;
	/** entries whose assistant message is kept without its thinking */
	thinkingDropped: string[];
	/** tool result entries whose content is TRIMMED */
	trimmed: string[];
}

/** what a tool result of the head is cut to when the head is over head_tokens */
export const TRIMMED = "[routr trimmed this result to keep the session small; re-read it if you need it]";

/**
 * The APIs whose assistant messages can go without their thinking in an
 * earlier turn: Anthropic takes earlier turns without it, and Chat
 * Completions sends it as plain text (stripOldReasoning drops it the same
 * way). The Responses APIs pair each tool call with its reasoning item by
 * id, and the others are untried, so theirs stays. Read from each message's
 * own api, not the model in use: the shrink is then a function of the head
 * alone and the same whatever model the session moves to.
 */
const THINKING_OPTIONAL = new Set(["anthropic-messages", "openai-completions"]);

const FIRST_MAX = 32_000; // characters of the first message
const TYPED_MAX = 24_000; // characters of what the user wrote since
const TYPED_EACH_MAX = 4_000;
const TODO_DESC_MAX = 300;
const TODO_MAX = 8_000;
const QUESTION_MAX = 200;
/** a cut held to a budget gives the head at most this share of it */
const HEAD_SHARE = 0.5;

/** a cut leaves the request at most this share of [main] at, so the turns after it have room before the next */
const AT_ROOM = 0.7;
/** the most real tokens per estimated one a budget believes */
const MAX_RATIO = 4;

/** Is the context past [main] at, after a turn? `tokens` is Pi's estimate, null right after a compaction. */
export function due(main: MainAgent, tokens: number | null | undefined): boolean {
	return main.context !== "off" && main.at !== -1 && tokens != null && tokens >= main.at;
}

/**
 * The budget, in Pi's estimate (see truncation), that leaves the request
 * after a cut at AT_ROOM of [main] at in real tokens, the system prompt
 * included. `tokens` is the request's size now as the provider counted it.
 * The estimate is a token per 4 characters; tool calls, logs and JSON take
 * more, often twice that, so [main]'s keep_tokens and head_tokens alone
 * can leave a request still over at, and every turn is cut again. The
 * ratio of tokens to Pi's estimate of the same request scales the budget:
 * at least 1, so it never loosens one. undefined when at is -1, or the
 * size is unknown.
 */
export function atBudget(branch: readonly SessionEntry[], main: MainAgent, tokens: number | null | undefined): number | undefined {
	if (main.at === -1 || !tokens || tokens <= 0) return undefined;
	// the latest system prompt, which every request sends: a cut's projection leaves it out once it is older than the turns kept
	let system = 0;
	for (let i = branch.length - 1; i >= 0; i--) {
		const prompt = sessionEntryToContextMessages(branch[i]!).find((m) => m.role === "system");
		if (!prompt) continue;
		system = estimateTokens(prompt);
		break;
	}
	const messages = buildSessionProjection([...branch]).messages.filter((m) => m.role !== "system");
	const sent = [...(keptHead(branch) ?? []), ...messages].reduce((n, m) => n + estimateTokens(m), 0);
	const ratio = Math.min(MAX_RATIO, Math.max(1, tokens / Math.max(1, system + sent)));
	return Math.max(0, Math.floor((main.at * AT_ROOM) / ratio) - system);
}

/**
 * The truncation of `branch` (the session's path, oldest first): a note
 * standing in for the earlier conversation, then the last keep_turns turns
 * as they were, at most keep_tokens of them, cut where a turn starts. A
 * latest turn over keep_tokens on its own keeps its last steps, cut at a
 * model message, and its incoming message goes in the note. undefined when
 * there is nothing to cut.
 *
 * `budget`, for a cut that must fit a window or come in under [main] at
 * (atBudget): at most this many tokens of head, note and turns kept
 * together (see fitted). A cut that fits it as [main] says is made as
 * without it.
 */
export function truncation(
	branch: readonly SessionEntry[],
	main: MainAgent,
	opts: { tokensBefore: number; workers: readonly Worker[]; budget?: number },
): CompactionResult<TruncationDetails> | undefined {
	const entries = buildSessionProjection([...branch]).entries;
	// the newest compaction is projected first; what follows it is the live context
	const from = entries.findIndex((e) => e.sourceEntry.type === "compaction" && e.messages.length > 0) + 1;
	const attempt = (head: Head | undefined, keep_tokens: number): Attempt | undefined => {
		const inHead = new Set((head && headEntries(branch, head))?.map((e) => e.id));
		let start = from;
		// the head is kept whole, ahead of the note: the turns kept come after it
		while (start < entries.length && inHead.has(entries[start]!.sourceEntry.id)) start++;
		const cut = cutAt(entries, start, { ...main, keep_tokens });
		if (!cut || cut.index <= start) return undefined;
		const kept = new Set([...inHead, ...entries.slice(cut.index).map((e) => e.sourceEntry.id)]);
		const summary = note(branch, kept, opts.workers, cut.turns, cut.split, head);
		const keptTokens = entries.slice(cut.index).reduce((n, e) => n + tokensOf(e), 0);
		const tokens = (head?.tokens ?? 0) + estimateTokens({ role: "compactionSummary", summary, tokensBefore: 0, timestamp: 0 }) + keptTokens;
		return { cut, head, summary, keptTokens, tokens };
	};
	const head = headFor(branch, main);
	let a = attempt(head, main.keep_tokens);
	const { budget } = opts;
	// nothing to cut as [main] says: over the budget if what is sent now is, a head in front of an earlier note too
	const now = (): number => entries.reduce((n, e) => n + tokensOf(e), 0) + (head && from > 0 ? head.tokens : 0);
	if (budget !== undefined && (a ? a.tokens : now()) > budget) a = fitted(branch, head, budget, main.keep_tokens, attempt);
	if (!a) return undefined;
	return {
		summary: a.summary,
		firstKeptEntryId: entries[a.cut.index]!.sourceEntry.id,
		tokensBefore: opts.tokensBefore,
		details: { by: "routr", context: "truncate", turns: a.cut.turns, split: a.cut.split !== undefined, ...(a.head ? { head: a.head } : {}) },
	};
}

/** A cut as truncation would make it with head, and Pi's estimate of what it sends. */
interface Attempt {
	cut: Cut;
	head?: Head;
	summary: string;
	/** the turns kept */
	keptTokens: number;
	/** the head, the note and the turns kept */
	tokens: number;
}

/**
 * The cut that fits budget, when the one [main] makes does not: the head
 * gets at most HEAD_SHARE of it, shrunk as head_tokens shrinks it, and the
 * turns kept get what the head and the note leave. A head that won't go
 * that small is dropped, and so is one that still doesn't fit or leaves
 * nothing to cut: the note quotes the first message instead. The head
 * shrunk or dropped is what the details record, so later cuts copy it and
 * the old one never comes back; its cache is lost, but a cut this forced
 * is for a model whose window it no longer fits, mostly a new one, with
 * nothing cached anyway, or one that would otherwise come back every turn
 * and lose it each time. Short of fitting, the smallest cut.
 */
function fitted(
	branch: readonly SessionEntry[],
	head: Head | undefined,
	budget: number,
	keepTokens: number,
	attempt: (head: Head | undefined, keep_tokens: number) => Attempt | undefined,
): Attempt | undefined {
	const share = Math.floor(budget * HEAD_SHARE);
	const small = head && head.tokens > share ? takeHead(branch, share) : head;
	const fit = (h: Head | undefined): Attempt | undefined => {
		const room = Math.max(0, budget - (h?.tokens ?? 0));
		let a = attempt(h, keepTokens === -1 ? room : Math.min(keepTokens, room));
		// fewer turns by as much as it is over, until it fits or none fewer can be kept
		while (a && a.tokens > budget) {
			const less = attempt(h, Math.max(0, a.keptTokens - (a.tokens - budget)));
			if (!less || less.cut.index <= a.cut.index) break;
			a = less;
		}
		return a;
	};
	const withHead = fit(small);
	if (!small || (withHead && withHead.tokens <= budget)) return withHead;
	const none = fit(undefined);
	return none && (!withHead || none.tokens < withHead.tokens) ? none : withHead;
}

/**
 * The first turn's messages to send in front of the note, when the latest
 * compaction of branch is a truncation that kept it: as that truncation
 * shaped it, and a copy, so nothing that edits the request reaches the
 * session. undefined otherwise.
 */
export function keptHead(branch: readonly SessionEntry[]): Message[] | undefined {
	for (let i = branch.length - 1; i >= 0; i--) {
		const e = branch[i]!;
		if (e.type !== "compaction") continue;
		const head = (e.details as TruncationDetails | undefined)?.by === "routr" ? (e.details as TruncationDetails).head : undefined;
		const entries = head && headEntries(branch, head);
		if (!head || !entries || entries.length === 0) return undefined;
		const drop = new Set(head.thinkingDropped);
		const trimmed = new Set(head.trimmed);
		return structuredClone(buildSessionProjection(entries).entries.flatMap((p) => shaped(p, drop, trimmed)));
	}
	return undefined;
}

/**
 * Whether Pi takes a compaction of branch at all. Its prepareCompaction
 * (not exported) finds nothing to compact, and Pi says "Nothing to compact",
 * unless the conversation since the last compaction's kept part is past
 * keepRecentTokens: the head sent in front of routr's note does not count.
 * Pi 1.3's rule (findProjectedCutPoint) on the same projection.
 */
export function piCompacts(branch: readonly SessionEntry[], keepRecentTokens: number): boolean {
	if (branch.at(-1)?.type === "compaction") return false; // "Already compacted"
	const entries = buildSessionProjection([...branch]).entries;
	const start = entries.findIndex((e) => e.sourceEntry.type === "compaction" && e.messages.length > 0) + 1;
	const live = (e: Projected): boolean => e.sourceEntry.type !== "compaction";
	const points: number[] = [];
	for (let i = start; i < entries.length; i++) if (live(entries[i]!) && entries[i]!.messages.some((m) => m.role !== "toolResult" && m.role !== "system")) points.push(i);
	if (points.length === 0) return false;
	let cut = points[0]!;
	let tokens = 0;
	for (let i = entries.length - 1; i >= start; i--) {
		const n = tokensOf(entries[i]!);
		if (n === 0) continue;
		tokens += n;
		if (tokens >= keepRecentTokens) {
			cut = points.find((p) => p >= i) ?? points.at(-1)!;
			break;
		}
	}
	while (cut > start && live(entries[cut - 1]!) && entries[cut - 1]!.messages.length === 0) cut--;
	// it summarizes what comes before the cut, a split turn's start included
	return entries.slice(start, cut).some((e) => live(e) && e.messages.some((m) => m.role !== "system"));
}

/** What Pi's summary is asked to keep, with the workers and the todo list as they are now. */
export function summaryInstructions(branch: readonly SessionEntry[], workers: readonly Worker[]): string {
	const todo = todoList(branch);
	return [
		"This session runs routr workers, often overnight, and goes on after this summary.",
		"Quote the session's first user message word for word, and name every file it points to.",
		"Keep every routr worker ID with what it is for and its state, every bead ID, branch and worktree, and each decision still open.",
		`routr workers of this session now:\n${workerLines(workers)}`,
		todo ? `The todo list as last set:\n${todo}` : "",
	]
		.filter(Boolean)
		.join("\n\n");
}

// a system message is Pi's prompt state, not a turn
const incoming = (m: Message): boolean => m.role !== "assistant" && m.role !== "toolResult" && m.role !== "system";
const tokensOf = (e: Projected): number => e.messages.reduce((n, m) => n + estimateTokens(m), 0);
const within = (n: number, limit: number): boolean => limit === -1 || n <= limit;

/** The head this truncation keeps: the last truncation's, unchanged, else the first turn if [main] keeps it. */
function headFor(branch: readonly SessionEntry[], main: MainAgent): Head | undefined {
	for (let i = branch.length - 1; i >= 0; i--) {
		const e = branch[i]!;
		const d = e.type === "compaction" ? (e.details as TruncationDetails | undefined) : undefined;
		if (d?.by === "routr") return d.head && headEntries(branch, d.head) ? d.head : undefined;
	}
	return main.keep_head && main.context === "truncate" ? takeHead(branch, main.head_tokens) : undefined;
}

/** The branch from the start through the head's last entry; undefined if that is not on it. */
function headEntries(branch: readonly SessionEntry[], head: Head): SessionEntry[] | undefined {
	const end = branch.findIndex((e) => e.id === head.endId);
	return end < 0 ? undefined : branch.slice(0, end + 1);
}

/**
 * The session's first turn: every entry up to the first incoming one after
 * the first user message, those before that message included (a message
 * at session start). Shrunk to max if it is over: first the thinking that
 * can go (THINKING_OPTIONAL), then the largest tool results, each cut to
 * TRIMMED, until it fits. The user's message, the model's text and its
 * tool calls stay. undefined while the first turn is still going, when a
 * compaction falls inside it, or when it won't fit.
 */
function takeHead(branch: readonly SessionEntry[], max: number): Head | undefined {
	const first = branch.findIndex((e) => e.type === "message" && e.message.role === "user");
	if (first < 0) return undefined;
	const next = branch.findIndex((e, i) => i > first && (e.type === "compaction" || sessionEntryToContextMessages(e).some(incoming)));
	if (next < 0 || branch[next]!.type === "compaction") return undefined;
	const entries = buildSessionProjection(branch.slice(0, next)).entries;
	const drop = new Set<string>();
	const trimmed = new Set<string>();
	const cost = (e: Projected): number => shaped(e, drop, trimmed).reduce((n, m) => n + estimateTokens(m), 0);
	let costs = entries.map(cost);
	let tokens = costs.reduce((a, b) => a + b, 0);
	if (max !== -1 && tokens > max) {
		for (const e of entries) if (e.messages.some(thinkingOptional)) drop.add(e.sourceEntry.id);
		costs = entries.map(cost);
		tokens = costs.reduce((a, b) => a + b, 0);
		const results = entries.flatMap((e, i) => (e.messages.some((m) => m.role === "toolResult") ? [i] : [])).sort((a, b) => costs[b]! - costs[a]! || a - b);
		for (const i of results) {
			if (tokens <= max) break;
			trimmed.add(entries[i]!.sourceEntry.id);
			const c = cost(entries[i]!);
			tokens += c - costs[i]!;
			costs[i] = c;
		}
		if (tokens > max) return undefined;
	}
	const ids = (set: ReadonlySet<string>): string[] => entries.map((e) => e.sourceEntry.id).filter((id) => set.has(id));
	return { endId: branch[next - 1]!.id, max, tokens, thinkingDropped: ids(drop), trimmed: ids(trimmed) };
}

/** an assistant message whose thinking can go without emptying it */
function thinkingOptional(m: Message): boolean {
	return m.role === "assistant" && THINKING_OPTIONAL.has(m.api) && m.content.some((c) => c.type === "thinking") && m.content.some((c) => c.type !== "thinking");
}

/**
 * A head entry's messages as kept: no system message (Pi sends the current
 * one first), and the shrink recorded applied as recorded, so a head taken
 * earlier is rebuilt the same.
 */
function shaped(e: Projected, drop: ReadonlySet<string>, trimmed: ReadonlySet<string>): Message[] {
	const id = e.sourceEntry.id;
	return e.messages.flatMap((m): Message[] => {
		if (m.role === "system") return [];
		if (m.role === "toolResult" && trimmed.has(id)) return [{ ...m, content: [{ type: "text", text: TRIMMED }] }];
		if (m.role !== "assistant" || !drop.has(id)) return [m];
		const content = m.content.filter((c) => c.type !== "thinking");
		return [content.length > 0 ? { ...m, content } : m];
	});
}

/** A latest turn cut inside: the entry that began it and its incoming message. */
interface Split {
	id: string;
	message: Message;
}

/** A cut: the first kept index, the turns kept, and the split turn if it is one. */
interface Cut {
	index: number;
	turns: number;
	split?: Split;
}

/** Where to cut entries[start..]. */
function cutAt(entries: readonly Projected[], start: number, main: MainAgent): Cut | undefined {
	const starts: number[] = [];
	for (let i = start; i < entries.length; i++) if (entries[i]!.messages.some(incoming)) starts.push(i);
	if (starts.length === 0) return undefined;
	let index = entries.length;
	let tokens = 0;
	let turns = 0;
	for (let k = starts.length - 1; k >= 0 && (main.keep_turns === -1 || turns < main.keep_turns); k--) {
		const size = entries.slice(starts[k], index).reduce((n, e) => n + tokensOf(e), 0);
		if (!within(tokens + size, main.keep_tokens)) break;
		tokens += size;
		index = starts[k]!;
		turns++;
	}
	if (turns > 0) return { index, turns };
	// the latest turn alone is over keep_tokens: its last steps, from a model message so no tool result is orphaned
	const first = starts[starts.length - 1]!;
	let best: number | undefined;
	tokens = 0;
	for (let i = entries.length - 1; i > first; i--) {
		tokens += tokensOf(entries[i]!);
		if (!entries[i]!.messages.some((m) => m.role === "assistant")) continue;
		if (best !== undefined && !within(tokens, main.keep_tokens)) break;
		best = i;
	}
	if (best === undefined) return undefined;
	return { index: best, turns: 1, split: { id: entries[first]!.sourceEntry.id, message: entries[first]!.messages.find(incoming)! } };
}

function note(branch: readonly SessionEntry[], kept: ReadonlySet<string>, workers: readonly Worker[], turns: number, split?: Split, head?: Head): string {
	const users = branch.flatMap((e) => (e.type === "message" && e.message.role === "user" ? [{ id: e.id, text: textOf(e.message.content) }] : []));
	const first = users[0] && !kept.has(users[0].id) ? users[0] : undefined;
	const typed = users.filter((u) => u !== users[0] && u.id !== split?.id && !kept.has(u.id)).map((u) => u.text);
	const began =
		split?.id === first?.id
			? "It began with the session's first message, above; its last steps follow."
			: `It began with this message, and its last steps follow:\n\n${clip(split ? messageText(split.message) : "", FIRST_MAX)}`;
	const todo = todoList(branch);
	const parts = [
		head
			? `${HEAD_NOTE_START} to keep this session small, and this note stands in for it. ${headKept(head)} The session file still has all of it.`
			: "routr cut the earlier conversation to keep this session small, and this note stands in for it. The session file still has all of it.",
		first ? `The session began with this message (re-read any file it names):\n\n${clip(first.text, FIRST_MAX)}` : "",
		`What the user wrote since, oldest first:\n\n${typedList(typed)}`,
		todo ? `Your todo list as you last set it:\n\n${todo}` : "",
		`routr workers of this session (routr list for all, routr result ID for a result):\n\n${workerLines(workers)}`,
		`Your work's state lives outside this conversation: ${head ? "" : "the files the first message names, "}issues in beads (bd list, bd show ID), code in git (git log, git status, git worktree list), and workers in routr. Check there rather than guess at what the cut left out.`,
		split ? `The latest turn was too long to keep whole. ${began}` : "",
		split ? "" : turns === 1 ? "The last turn follows as it was." : `The last ${turns} turns follow as they were.`,
	];
	return parts.filter(Boolean).join("\n\n");
}

/** What the note says of the head above it. */
function headKept(head: Head): string {
	const n = head.trimmed.length;
	if (n === 0) return "The first turn is kept above as it was.";
	const what = n === 1 ? "one tool result, trimmed to keep it small: re-read what it showed" : `${n} tool results, trimmed to keep it small: re-read what they showed`;
	return `The first turn is kept above as it was, except ${what} if you need it.`;
}

type Content = string | readonly { type: string; text?: string }[] | undefined;

function textOf(content: Content): string {
	if (typeof content === "string") return content;
	return (content ?? []).map((c) => (c.type === "text" ? (c.text ?? "") : `[${c.type}]`)).join("\n");
}

/** An incoming message as text: its content, or a shell command the user ran. */
function messageText(m: Message): string {
	if ("content" in m) return textOf(m.content as Content);
	if (m.role === "bashExecution") return `$ ${m.command}\n${m.output}`;
	return `[${m.role}]`;
}

function clip(text: string, max: number): string {
	return text.length <= max ? text : `${text.slice(0, max)}… [${text.length - max} more characters in the session file]`;
}

/** The newest that fit in TYPED_MAX, each clipped. */
function typedList(typed: readonly string[]): string {
	if (typed.length === 0) return "- (nothing)";
	const lines: string[] = [];
	let size = 0;
	for (let i = typed.length - 1; i >= 0; i--) {
		const line = `- ${clip(typed[i]!, TYPED_EACH_MAX)}`;
		if (lines.length > 0 && size + line.length > TYPED_MAX) {
			lines.unshift(`[${i + 1} earlier messages are in the session file]`);
			break;
		}
		lines.unshift(line);
		size += line.length;
	}
	return lines.join("\n");
}

interface Task {
	id?: unknown;
	subject?: unknown;
	status?: unknown;
	description?: unknown;
}

/** The latest `todo` tool result's task list (its details; the model saw only "Updated #6"), else its text. */
function todoList(branch: readonly SessionEntry[]): string | undefined {
	for (let i = branch.length - 1; i >= 0; i--) {
		const e = branch[i]!;
		if (e.type !== "message" || e.message.role !== "toolResult" || e.message.toolName !== "todo") continue;
		const tasks = (e.message.details as { tasks?: unknown } | undefined)?.tasks;
		if (!Array.isArray(tasks)) return clip(textOf(e.message.content), TODO_MAX);
		const lines = (tasks as Task[])
			.filter((t) => t.status !== "deleted")
			.map((t) => {
				const desc = typeof t.description === "string" && t.description && t.status !== "completed" ? ` — ${oneLine(t.description, TODO_DESC_MAX)}` : "";
				return `#${String(t.id)} [${String(t.status)}] ${String(t.subject)}${desc}`;
			});
		return clip(lines.join("\n"), TODO_MAX);
	}
	return undefined;
}

function oneLine(text: string, max: number): string {
	const t = text.replace(/\s+/g, " ").trim();
	return t.length <= max ? t : `${t.slice(0, max)}…`;
}

function workerLines(workers: readonly Worker[]): string {
	if (workers.length === 0) return "- none active or changed in the last hour";
	return workers
		.map((w) => {
			const where = w.chain ? ` (${w.chain}${w.step ? `/${w.step}` : ""})` : "";
			const verdict = w.verdict ? `, verdict ${w.verdict}` : "";
			const engine = w.model ? `${w.harness} ${w.model}` : w.harness;
			const asks = w.question ? ` · asks: ${oneLine(w.question, QUESTION_MAX)}` : "";
			return `#${w.id} ${w.title ?? w.role}${where} — ${w.state}${verdict} · ${engine}${asks}`;
		})
		.join("\n");
}
