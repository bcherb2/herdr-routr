import { test } from "node:test";
import assert from "node:assert/strict";
import { mkdtempSync, writeFileSync, appendFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { LineTail } from "../src/tail.ts";

test("LineTail follows appends and holds partial lines", () => {
	const path = join(mkdtempSync(join(tmpdir(), "routr-pi-")), "s.jsonl");
	const t = new LineTail(path);
	assert.deepEqual(t.read(), [], "missing file");
	writeFileSync(path, "one\ntw");
	assert.deepEqual(t.read(), ["one"]);
	appendFileSync(path, "o\n");
	assert.deepEqual(t.read(), ["two"]);
	assert.deepEqual(t.read(), []);
	writeFileSync(path, "new\n"); // replaced by a shorter file
	assert.deepEqual(t.read(), ["new"]);
});

test("LineTail starts near the end of a long file, on a line boundary", () => {
	const path = join(mkdtempSync(join(tmpdir(), "routr-pi-")), "s.jsonl");
	writeFileSync(path, Array.from({ length: 100 }, (_, i) => `line-${i}`).join("\n") + "\n");
	const t = new LineTail(path, 40);
	const got = t.read();
	assert.ok(got.length > 0 && got.length < 10, `read ${got.length} lines`);
	assert.equal(got.at(-1), "line-99");
	assert.match(got[0]!, /^line-\d+$/);
});

test("LineTail keeps a multibyte char split across writes", () => {
	const path = join(mkdtempSync(join(tmpdir(), "routr-pi-")), "s.jsonl");
	const bytes = Buffer.from("héllo\n");
	writeFileSync(path, bytes.subarray(0, 2)); // splits "é"
	const t = new LineTail(path);
	assert.deepEqual(t.read(), []);
	appendFileSync(path, bytes.subarray(2));
	assert.deepEqual(t.read(), ["héllo"]);
});
