// What the routr mod keeps in $.state: what its band, cards pane and picker
// draw. hooks/register.tsx lays it out from its polls and handlers (the
// lines through the core's stepLines and cardLines); the drawings only read
// it. Self-contained: the engine refuses a contract that imports.

export type ViewMode = "steps" | "cards";

/** A worker waiting for the person's answer, as the picker's list names it. */
export interface Asker {
	id: number;
	/** "7 plan · claude asks about Bash: go test ./..." */
	label: string;
}

export interface View {
	/** steps in the band, or cards in a pane */
	mode: ViewMode;
	/** the band's lines and the pane's, as the core laid them out, each painted run marked (register.tsx, runs()) */
	steps: string[];
	cards: string[];
	waiting: Asker[];
}

/** The picker on screen: which worker first, or one worker's question (while routr reads it, `reading`). */
export type Picker =
	| { stage: "reading"; id: number }
	| {
			stage: "choose";
			askers: Asker[];
			/** id → questionKey of each one listed */
			shown: [number, string][];
	  }
	| {
			stage: "question";
			id: number;
			/** "7 plan · claude asks about Bash: go test ./..." */
			head: string;
			text: string[];
			/** the labels, in routr's order: option N is labels[N-1] */
			options: string[];
			fingerprint?: string;
			/** questionKey of the question shown */
			key: string;
	  };

declare module "claude-code" {
	interface PluginState {
		routr: { view: View; picker: Picker | null };
	}
}
