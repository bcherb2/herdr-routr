// How the main agent's context is cut once it passes [main] at: the
// truncation, which needs no model call, and the instructions for Pi's own
// summary. The session file keeps everything either way.

import { buildSessionProjection, estimateTokens, type CompactionResult, type SessionEntry } from "@earendil-works/pi-coding-agent";
import type { Worker } from "../../frontend/core/index.ts";
import type { MainAgent } from "./context.ts";

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
}

const FIRST_MAX = 32_000; // characters of the first message
const TYPED_MAX = 24_000; // characters of what the user wrote since
const TYPED_EACH_MAX = 4_000;
const TODO_DESC_MAX = 300;
const TODO_MAX = 8_000;
const QUESTION_MAX = 200;

/** Is the context past [main] at, after a turn? `tokens` is Pi's estimate, null right after a compaction. */
export function due(main: MainAgent, tokens: number | null | undefined): boolean {
	return main.context !== "off" && main.at !== -1 && tokens != null && tokens >= main.at;
}

/**
 * The truncation of `branch` (the session's path, oldest first): a note
 * standing in for the earlier conversation, then the last keep_turns turns
 * as they were, at most keep_tokens of them, cut where a turn starts. A
 * latest turn over keep_tokens on its own keeps its last steps, cut at a
 * model message, and its incoming message goes in the note. undefined when
 * there is nothing to cut.
 */
export function truncation(
	branch: readonly SessionEntry[],
	main: MainAgent,
	opts: { tokensBefore: number; workers: readonly Worker[] },
): CompactionResult<TruncationDetails> | undefined {
	const entries = buildSessionProjection([...branch]).entries;
	// the newest compaction is projected first; what follows it is the live context
	const start = entries.findIndex((e) => e.sourceEntry.type === "compaction" && e.messages.length > 0) + 1;
	const c = cutAt(entries, start, main);
	if (!c || c.index <= start) return undefined;
	const kept = new Set(entries.slice(c.index).map((e) => e.sourceEntry.id));
	const summary = note(branch, kept, opts.workers, c.turns, c.split);
	return {
		summary,
		firstKeptEntryId: entries[c.index]!.sourceEntry.id,
		tokensBefore: opts.tokensBefore,
		details: { by: "routr", context: "truncate", turns: c.turns, split: c.split !== undefined },
	};
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

const incoming = (m: Message): boolean => m.role !== "assistant" && m.role !== "toolResult";
const tokensOf = (e: Projected): number => e.messages.reduce((n, m) => n + estimateTokens(m), 0);
const within = (n: number, limit: number): boolean => limit === -1 || n <= limit;

/** A latest turn cut inside: the entry that began it and its incoming message. */
interface Split {
	id: string;
	message: Message;
}

/** Where to cut entries[start..]: the first kept index, the turns kept, and the split turn if it is one. */
function cutAt(entries: readonly Projected[], start: number, main: MainAgent): { index: number; turns: number; split?: Split } | undefined {
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

function note(branch: readonly SessionEntry[], kept: ReadonlySet<string>, workers: readonly Worker[], turns: number, split?: Split): string {
	const users = branch.flatMap((e) => (e.type === "message" && e.message.role === "user" ? [{ id: e.id, text: textOf(e.message.content) }] : []));
	const first = users[0] && !kept.has(users[0].id) ? users[0] : undefined;
	const typed = users.filter((u) => u !== users[0] && u.id !== split?.id && !kept.has(u.id)).map((u) => u.text);
	const began =
		split?.id === first?.id
			? "It began with the session's first message, above; its last steps follow."
			: `It began with this message, and its last steps follow:\n\n${clip(split ? messageText(split.message) : "", FIRST_MAX)}`;
	const todo = todoList(branch);
	const parts = [
		"routr cut the earlier conversation to keep this session small, and this note stands in for it. The session file still has all of it.",
		first ? `The session began with this message (re-read any file it names):\n\n${clip(first.text, FIRST_MAX)}` : "",
		`What the user wrote since, oldest first:\n\n${typedList(typed)}`,
		todo ? `Your todo list as you last set it:\n\n${todo}` : "",
		`routr workers of this session (routr list for all, routr result ID for a result):\n\n${workerLines(workers)}`,
		"Your work's state lives outside this conversation: the files the first message names, issues in beads (bd list, bd show ID), code in git (git log, git status, git worktree list), and workers in routr. Check there rather than guess at what the cut left out.",
		split ? `The latest turn was too long to keep whole. ${began}` : "",
		split ? "" : turns === 1 ? "The last turn follows as it was." : `The last ${turns} turns follow as they were.`,
	];
	return parts.filter(Boolean).join("\n\n");
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
