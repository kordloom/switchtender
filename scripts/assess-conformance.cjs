// Checks that the browser assessment produces the same report as the command.
//
// The page and the command are two builds of one reader and one renderer, and the whole argument
// for the page is that its numbers cannot disagree with what a run later says. Two builds with
// nothing between them is how that stops being true: this is the thing between them. It loads the
// wasm the deploy is about to publish, assesses a fixture with it, and requires the result to match
// what the command printed for the same file.
//
// Usage: node scripts/assess-conformance.cjs <assess.wasm> <wasm_exec.js> <export.json> <expected.txt>
"use strict";

const fs = require("fs");
const path = require("path");

const [wasmPath, execPath, exportPath, expectedPath] = process.argv.slice(2);
if (!wasmPath || !execPath || !exportPath || !expectedPath) {
	console.error("usage: node assess-conformance.cjs <assess.wasm> <wasm_exec.js> <export.json> <expected.txt>");
	process.exit(2);
}

require(path.resolve(execPath));

// The Source line names where the file came from, which is a path for the command and a phrase for
// the browser. It is the one line that is meant to differ, so it is dropped from both sides rather
// than the comparison being loosened.
function body(text) {
	return text.split("\n").filter((l) => !l.trim().startsWith("Source:")).join("\n").trimEnd();
}

(async () => {
	const go = new Go();
	const { instance } = await WebAssembly.instantiate(fs.readFileSync(wasmPath), go.importObject);
	// main blocks forever so the exported functions stay alive, so this is deliberately not awaited.
	go.run(instance);
	// Give the Go runtime the tick it needs to publish them.
	await new Promise((r) => setTimeout(r, 0));

	if (typeof globalThis.switchtenderAssess !== "function") {
		console.error("the wasm did not publish switchtenderAssess");
		process.exit(1);
	}
	const formats = globalThis.switchtenderFormats();
	if (!formats.includes("awx")) {
		console.error("the wasm does not offer the awx format: " + formats.join(", "));
		process.exit(1);
	}

	// The page hands the reader the file's bytes, so this does too.
	const exportBytes = fs.readFileSync(exportPath);
	const got = globalThis.switchtenderAssess("awx", exportBytes);
	if (!got || !got.ok) {
		console.error("the wasm refused the fixture: " + (got && got.error));
		process.exit(1);
	}
	// The page leads with a sentence the module decides. A build that stopped sending it would leave
	// the page to write one, which is the second implementation this check exists to rule out.
	if (typeof got.headline !== "string" || got.headline === "" ||
		(got.headlineKind !== "gap" && got.headlineKind !== "clear")) {
		console.error("the wasm did not decide the headline: " + JSON.stringify({
			headline: got.headline, headlineKind: got.headlineKind,
		}));
		process.exit(1);
	}
	const want = fs.readFileSync(expectedPath, "utf8");
	if (body(got.report) !== body(want)) {
		console.error("the browser assessment does not match the command's report for the same export.\n");
		const g = body(got.report).split("\n");
		const w = body(want).split("\n");
		for (let i = 0; i < Math.max(g.length, w.length); i++) {
			if (g[i] !== w[i]) {
				console.error("first difference at line " + (i + 1) + ":");
				console.error("  command: " + (w[i] === undefined ? "(no line)" : JSON.stringify(w[i])));
				console.error("  browser: " + (g[i] === undefined ? "(no line)" : JSON.stringify(g[i])));
				break;
			}
		}
		process.exit(1);
	}

	// An export saved as UTF-16, the way Windows PowerShell writes a file by default, has to read
	// the same as the plain file, since the command reads it the same.
	const utf16 = Buffer.concat([Buffer.from([0xff, 0xfe]),
		Buffer.from(exportBytes.toString("utf8"), "utf16le")]);
	const wide = globalThis.switchtenderAssess("awx", utf16);
	if (!wide || !wide.ok || body(wide.report) !== body(got.report)) {
		console.error("the browser assessment reads a UTF-16 copy of the export differently: " +
			(wide && wide.error ? wide.error : "the reports differ"));
		process.exit(1);
	}

	// A malformed export is an ordinary thing to drop on a public page, so the refusal path is
	// checked too. A build that panicked here would take every later assessment on the page with it.
	const junk = globalThis.switchtenderAssess("awx", "{ not json");
	if (!junk || junk.ok !== false || !junk.error) {
		console.error("the wasm did not refuse a malformed export cleanly: " + JSON.stringify(junk));
		process.exit(1);
	}

	console.log("assess conformance: the browser report matches the command in UTF-8 and UTF-16, " +
		"and junk is refused");
	process.exit(0);
})().catch((e) => {
	console.error("assess conformance failed: " + (e && e.stack ? e.stack : e));
	process.exit(1);
});
