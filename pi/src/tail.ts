// Incremental reader for an append-only JSONL file.
//
// Each read() returns the complete lines appended since the last call. A
// line still being written is held back until its newline arrives. The file
// may not exist yet (the harness creates it after starting) or be replaced
// by a shorter one; both are handled.

import { closeSync, openSync, readSync, statSync } from "node:fs";
import { StringDecoder } from "node:string_decoder";

export class LineTail {
	private offset = -1; // -1: not opened yet
	private partial = "";
	private decoder = new StringDecoder("utf8"); // keeps a multibyte char split across reads

	/**
	 * @param path file to follow
	 * @param backlog bytes of existing content to read on first open, so a
	 *   card shows recent activity for a worker that was already running
	 */
	readonly path: string;
	private readonly backlog: number;

	constructor(path: string, backlog = 64 * 1024) {
		this.path = path;
		this.backlog = backlog;
	}

	read(maxBytes = 1024 * 1024): string[] {
		let size: number;
		try {
			size = statSync(this.path).size;
		} catch {
			return [];
		}
		let skipFirst = false;
		if (this.offset < 0 || size < this.offset) {
			this.offset = Math.max(0, size - this.backlog);
			skipFirst = this.offset > 0; // started mid-line
			this.partial = "";
			this.decoder = new StringDecoder("utf8");
		}
		if (size === this.offset) return [];
		const len = Math.min(size - this.offset, maxBytes);
		const buf = Buffer.alloc(len);
		let fd: number | undefined;
		let n = 0;
		try {
			fd = openSync(this.path, "r");
			n = readSync(fd, buf, 0, len, this.offset);
		} catch {
			return [];
		} finally {
			if (fd !== undefined) closeSync(fd);
		}
		this.offset += n;
		const text = this.partial + this.decoder.write(buf.subarray(0, n));
		const lines = text.split("\n");
		this.partial = lines.pop() ?? "";
		if (skipFirst) lines.shift();
		return lines.filter((l) => l.length > 0);
	}
}
