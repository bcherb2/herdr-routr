// Answering a worker's question from a front end: which question a worker
// asks now, which workers wait for the user, the `routr answer` the user's
// pick runs, and what to tell the user afterwards. Each front end shows the
// question its own way (Pi: pi/src/dialogs.ts) and turns the user's pick
// into a Choice. Nothing opens by itself: a dialog that pops up takes the
// keys the user was typing, and 1, y or Enter can pick an option by accident.

import { waitsForUser, type QuestionContext, type Worker } from "./model.ts";

/** title lines of a question; Codex's commands can be long */
const MAX_LINES = 8;

/** What the user picked: option N, its tab, or nothing for now (Later, esc). */
export type Choice = { kind: "option"; n: number } | { kind: "tab" } | { kind: "later" };

/** One worker's question, as the picker shows it. */
export interface Question {
	id: number;
	/** "7 plan · claude" */
	who: string;
	/** the question without its numbered options */
	text: string[];
	/** empty when it has no menu, or one without a fingerprint: nothing to press safely */
	options: string[];
	fingerprint?: string;
	asksAbout?: string;
	/** options, from 1, that save a rule or last past this call */
	lasting: number[];
	/** tells this question from the next one (questionKey) */
	key: string;
}

/** Tells one question from the next: its menu's fingerprint, or without a menu its text. */
function keyOf(fingerprint: string | undefined, question: string | undefined): string {
	return fingerprint ?? `text:${question ?? ""}`;
}

/** The question a worker waits on the user for now; undefined if it waits for nobody. */
export function questionKey(w: Worker | undefined): string | undefined {
	return w && waitsForUser(w) ? keyOf(w.fingerprint, w.question) : undefined;
}

/** Workers whose question the user may answer, the oldest question first. */
export function waiting(workers: readonly Worker[]): Worker[] {
	return workers.filter(waitsForUser).sort((a, b) => (a.updated ?? 0) - (b.updated ?? 0) || a.id - b.id);
}

/** Does each question a dialog shows (worker id → questionKey) still stand in this `routr list`? */
export function stillAsked(shown: ReadonlyMap<number, string>, workers: readonly Worker[]): boolean {
	const byId = new Map(workers.map((w) => [w.id, w]));
	for (const [id, key] of shown) if (questionKey(byId.get(id)) !== key) return false;
	return true;
}

/**
 * Worker w's question. From ctx (`routr show ID --context --json`) when
 * routr gave one: it may be newer than w, and it names the options that
 * last. Else from w's record, with none marked. undefined when ctx says it
 * asks nothing now.
 */
export function question(w: Worker, ctx?: QuestionContext): Question | undefined {
	if (ctx && ctx.question === undefined) return undefined;
	const src = ctx ?? { question: w.question, options: w.options, fingerprint: w.fingerprint, asksAbout: w.asksAbout, lasting: [] };
	const menu = src.options ?? [];
	const numbered = new Set(menu.map((o, i) => `${i + 1}. ${o}`));
	const text = (src.question ?? "").split("\n").filter((l) => l.trim() !== "" && !numbered.has(l.trim()));
	return {
		id: w.id,
		who: `${w.id} ${w.title ?? w.role} · ${w.harness}`,
		text: text.length > MAX_LINES ? [...text.slice(0, MAX_LINES - 1), "…"] : text,
		options: src.fingerprint ? menu : [],
		fingerprint: src.fingerprint,
		asksAbout: src.asksAbout,
		lasting: src.lasting,
		key: keyOf(src.fingerprint, src.question),
	};
}

/** `routr answer` for option n, chosen by the user, pressed only on the question they saw (fingerprint). */
export function answerArgs(id: number, n: number, fingerprint?: string): string[] {
	return ["answer", String(id), String(n), "--by", "human", ...(fingerprint ? ["--expect", fingerprint] : [])];
}

export interface Notice {
	text: string;
	level: "info" | "warning" | "error";
}

/**
 * What to tell the user after `routr answer` from the picker. Exit 4: another
 * question is on screen, and nothing was pressed. `opener` is how this front
 * end opens the picker (Pi: alt+q).
 */
export function answered(id: number, code: number, output: string, opener: string): Notice {
	const line = output.trim().split("\n")[0] ?? "";
	if (code === 0) return { text: `routr: ${line || `answered ${id}`}`, level: "info" };
	if (code === 4) return { text: `routr: ${id} asks another question now, so nothing was pressed. ${opener} shows it.`, level: "warning" };
	return { text: `routr answer ${id}: ${line || `exit ${code}`}`, level: "error" };
}

/** Why the picker for id's question closed by itself: it changed, or went away (answered in its tab, by the model, by Jev). */
export function closed(id: number, workers: readonly Worker[], opener: string): Notice {
	const now = questionKey(workers.find((w) => w.id === id));
	return now
		? { text: `routr: ${id} asks another question now. ${opener} shows it.`, level: "warning" }
		: { text: `routr: ${id} asks nothing now; nothing was pressed.`, level: "info" };
}
