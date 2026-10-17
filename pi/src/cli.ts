// Running the routr CLI. The extension never touches the registry itself.

import { execFile } from "node:child_process";
import { mainFromEnv, parseMain, type MainAgent } from "./context.ts";

export interface RunResult {
	code: number;
	stdout: string;
	stderr: string;
	/** the binary wasn't found */
	missing: boolean;
}

/** `routr` on PATH, or ROUTR_BIN. */
export function routrBin(env: NodeJS.ProcessEnv = process.env): string {
	return env.ROUTR_BIN?.trim() || "routr";
}

export function runRoutr(args: readonly string[], cwd: string, timeoutMs = 15_000): Promise<RunResult> {
	return new Promise((resolve) => {
		execFile(
			routrBin(),
			[...args],
			{ cwd, timeout: timeoutMs, maxBuffer: 16 * 1024 * 1024, env: process.env },
			(err, stdout, stderr) => {
				const e = err as (NodeJS.ErrnoException & { code?: number | string }) | null;
				const missing = e?.code === "ENOENT";
				const code = e ? (typeof e.code === "number" ? e.code : 1) : 0;
				resolve({ code, stdout: String(stdout), stderr: missing ? `${routrBin()}: not found` : String(stderr), missing });
			},
		);
	});
}

/** What changes the routr binary; the skill and this extension are read live. */
const BUILD_PATHS = ["cmd", "internal", "go.mod", "go.sum"];

/** The binary is older than the checkout at repo: what to tell the user. */
export interface Stale {
	/** the binary's revision, 12 digits, or "" when it predates `routr version` */
	revision: string;
	repo: string;
}

/**
 * Whether the routr binary was built before the commits of repo (this
 * extension's checkout) that change it: `routr version --json` against
 * `git diff <rev> HEAD`. Undefined when it is current or that can't be told:
 * no binary, no revision, or repo is not a checkout that has the revision.
 */
export async function staleBinary(repo: string, cwd: string): Promise<Stale | undefined> {
	const r = await runRoutr(["version", "--json"], cwd, 5000);
	if (r.missing) return undefined;
	if (r.code !== 0) return /unknown command "version"/.test(r.stderr) ? { revision: "", repo } : undefined;
	let revision = "";
	try {
		revision = String((JSON.parse(r.stdout) as { revision?: unknown }).revision ?? "");
	} catch {
		return undefined;
	}
	if (!/^[0-9a-f]{7,64}$/.test(revision)) return undefined;
	const code = await new Promise<number>((resolve) => {
		execFile("git", ["-C", repo, "diff", "--quiet", revision, "HEAD", "--", ...BUILD_PATHS], { timeout: 3000 }, (err) => {
			const e = err as (NodeJS.ErrnoException & { code?: number | string }) | null;
			resolve(e ? (typeof e.code === "number" ? e.code : -1) : 0);
		});
	});
	return code === 1 ? { revision: revision.slice(0, 12), repo } : undefined;
}

/** The main agent's settings, and what went wrong reading them, if the user should hear it. */
export interface MainRead {
	main: MainAgent;
	error?: string;
}

/**
 * `routr main --json`: the [main] config with any ROUTR_MAIN_* override.
 * A routr that is missing or predates the command gives mainFromEnv()
 * quietly (the widget and the stale-binary warning say so already); a
 * config it rejects gives it with the error.
 */
export async function readMain(cwd: string): Promise<MainRead> {
	const r = await runRoutr(["main", "--json"], cwd, 5000);
	if (r.missing || /unknown command "main"/.test(r.stderr)) return { main: mainFromEnv() };
	const main = r.code === 0 ? parseMain(r.stdout) : undefined;
	if (main) return { main };
	const said = r.stderr.trim().split("\n")[0]?.replace(/^routr main: /, "");
	const why = r.code === 0 ? "routr main --json printed something unexpected" : said || `routr main exited ${r.code}`;
	return { main: mainFromEnv(), error: why };
}
