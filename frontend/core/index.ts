// The front-end core: what Pi's extension (pi/) and the Claude Code mod
// share. It parses the CLI's JSON, decides which workers wake the model and
// with what text (the Coordinator), says what a worker's question is and what
// answering it runs, and lays out the steps and cards. It imports neither a
// harness nor Node: each front end runs the processes, reads the files, keeps
// the session's state and draws. Its tests run from pi/ (npm test).
//
// This list is the core's interface; a front end imports from here only.

export {
	ACTIVE,
	asks,
	cardLines,
	jevDecides,
	oneRow,
	parseContext,
	parseList,
	statusText,
	toCards,
	type Card,
	type CardOptions,
	type Paint,
	type QuestionContext,
	type State,
	type Tone,
	type Worker,
} from "./model.ts";
export { holdFromEnv, type WakeEntry, type WakeItem, type WakeKind, type WakeMessage } from "./wake.ts";
export { Coordinator, type ClaimsEntry, type CoordinatorOptions, type SnapshotOutcome, type TurnEnd, type Wake, type WakeMode } from "./coordinator.ts";
export { answerArgs, answered, closed, question, questionKey, stillAsked, waiting, type Choice, type Notice, type Question } from "./question.ts";
export { stepLines, toSteps, type StepBlock, type StepOptions, type StepRow } from "./steps.ts";
export { activityLine, parseLine, type TranscriptEvent } from "./transcript.ts";
