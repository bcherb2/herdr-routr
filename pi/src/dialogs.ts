// Pi's dialogs for answering a worker's question. The widget names each
// worker that waits for the user and the key that answers it; the key or
// `/routr answer` opens a picker (ui.select) with the question's options,
// "Open its tab" and "Later". What a question is, and what a pick runs, is
// the core's (frontend/core/question.ts); this file only lays it out for
// ui.select, and index.ts opens the dialogs and runs the CLI.

import { asks, type Choice, type Question, type Worker } from "../../frontend/core/index.ts";

/** The key that opens the picker: index.ts registers it, the widget and notices name it. */
export const ANSWER_KEY = "alt+q";

export const OPEN_TAB = "Open its tab";
export const LATER = "Later";
/** after an option routr lets only the user pick (`lasting`) */
const LASTS = " (lasts past this call)";

export interface Dialog<T> {
	title: string;
	/** the labels for ui.select, all different */
	items: string[];
	/** what the label ui.select returned means; undefined is esc, or the dialog closed */
	choose(label: string | undefined): T;
}

/**
 * The picker for one question:
 *
 *   7 plan · claude asks about Bash: go test ./...
 *   Do you want to proceed?
 *   → 1. Yes
 *     2. Yes, and don't ask again for: go test:* (lasts past this call)
 *     3. No
 *     Open its tab
 *     Later
 *
 * A question with no menu gets only "Open its tab" and "Later".
 */
export function questionPicker(q: Question): Dialog<Choice> {
	const items = [...q.options.map((o, i) => `${i + 1}. ${o}${q.lasting.includes(i + 1) ? LASTS : ""}`), OPEN_TAB, LATER];
	const title = [q.asksAbout ? `${q.who} asks about ${q.asksAbout}` : `${q.who} asks:`, ...q.text];
	if (q.options.length === 0) title.push("It has no options to pick here: answer it in its tab.");
	return {
		title: title.join("\n"),
		items,
		choose: (label) => {
			const i = label === undefined ? -1 : items.indexOf(label);
			if (i >= 0 && i < q.options.length) return { kind: "option", n: i + 1 };
			return label === OPEN_TAB ? { kind: "tab" } : { kind: "later" };
		},
	};
}

/** Several workers wait: which one first, the oldest question at the top (waiting). */
export function chooser(ws: readonly Worker[]): Dialog<Worker | undefined> {
	const items = ws.map((w) => `${w.id} ${w.title ?? w.role} · ${w.harness} ${asks(w)}`);
	return {
		title: `${ws.length} workers wait for an answer. Which one?`,
		items,
		choose: (label) => (label === undefined ? undefined : ws[items.indexOf(label)]),
	};
}

