// Shaped like `routr list --json` and `routr show --context --json`: the
// workers the question tests share with Pi's dialog tests (pi/test/dialogs.test.ts).
import { parseContext, parseList, type Worker } from "../model.ts";

const T0 = Date.parse("2026-10-02T10:00:00Z");
const iso = (ms: number): string => new Date(T0 + ms).toISOString();

export const OPTIONS = ["Yes", "Yes, and don't ask again for: go test:*", "No"];
// Shaped like `routr list --json`: two workers on a menu, one on a question with no menu, one Jev decides.
export const [bash, free, jev, older] = parseList(
	JSON.stringify([
		{
			id: 7,
			chain: "demo-1",
			step: "plan",
			role: "plan",
			harness: "claude",
			state: "blocked",
			question: ["Do you want to proceed?", ...OPTIONS.map((o, i) => `${i + 1}. ${o}`)].join("\n"),
			options: OPTIONS,
			fingerprint: "6a128b4273b95faa",
			pending: { tool: "Bash", summary: "Bash: go test ./...", input: "{}" },
			updated: iso(5000),
		},
		{ id: 8, role: "task", title: "name-it", harness: "claude", state: "blocked", question: "What should the file be called?", updated: iso(3000) },
		{ id: 9, role: "task", harness: "claude", state: "blocked", question: "Do you want to proceed?\n1. Yes", options: ["Yes"], fingerprint: "f9", answerer: "jev", updated: iso(0) },
		{ id: 10, role: "review", harness: "codex", state: "blocked", question: "Allow?\n1. Yes\n2. No", options: ["Yes", "No"], fingerprint: "fa", answerer: "agent", updated: iso(1000) },
	]),
) as [Worker, Worker, Worker, Worker];

// routr show 7 --context --json
export const ctx7 = parseContext(
	JSON.stringify({ worker: 7, question: bash.question, options: OPTIONS, fingerprint: "6a128b4273b95faa", lasting: [2], pending: { summary: "Bash: go test ./..." }, docs: [] }),
);

