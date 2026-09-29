// Checks that the browser assessment's worker survives a file too large for the reader.
//
// A reader that runs out of memory ends its Go program, and before the worker every later assessment
// on the page failed until a reload. This runs the worker script itself, in a scope shaped like a
// worker's, under a memory cap small enough that a large export exhausts it. It requires the worker
// to refuse that export with a reason and then read an ordinary one again, from the module it
// already compiled, without fetching anything a second time.
//
// Usage: node --wasm-max-mem-pages=4096 scripts/assess-worker-check.cjs \
//   <assess-worker.js> <assess.wasm> <wasm_exec.js> <export.json>
"use strict";

const fs = require("fs");
const path = require("path");

const [workerPath, wasmPath, execPath, exportPath] = process.argv.slice(2);
if (!workerPath || !wasmPath || !execPath || !exportPath) {
	console.error("usage: node --wasm-max-mem-pages=4096 assess-worker-check.cjs " +
		"<assess-worker.js> <assess.wasm> <wasm_exec.js> <export.json>");
	process.exit(2);
}

// largeExport is an AWX export far bigger than the capped memory can read: one inventory holding
// enough hosts to need several times the cap once parsed.
function largeExport(mb) {
	const hosts = [];
	const vars = JSON.stringify({ ansible_host: "10.0.0.1", env: "prod", role: "web", rack: "r12" });
	for (let i = 0; hosts.length * 110 < mb * 1048576; i++) {
		hosts.push({ name: "host-" + String(i).padStart(7, "0") + ".example.internal", variables: vars });
	}
	return JSON.stringify({
		projects: [{ name: "infra", scm_type: "git", scm_url: "https://example.invalid/i.git" }],
		inventory: [{ name: "fleet", hosts: hosts }],
		job_templates: [{ name: "Deploy", playbook: "deploy.yml", project: "infra", inventory: "fleet" }],
	});
}

let fetches = 0;
const answers = {};
globalThis.self = globalThis;
globalThis.importScripts = (url) => {
	if (url !== "/assess/wasm_exec.js") { throw new Error("unexpected import " + url); }
	require(path.resolve(execPath));
};
globalThis.fetch = (url) => {
	fetches++;
	if (url !== "/assess/assess.wasm") { return Promise.reject(new Error("unexpected fetch " + url)); }
	return Promise.resolve(new Response(fs.readFileSync(wasmPath)));
};
globalThis.postMessage = (msg) => { answers[msg.id](msg); };

function send(msg) {
	return new Promise((resolve) => {
		answers[msg.id] = resolve;
		globalThis.onmessage({ data: msg });
	});
}

function bytesOf(text) {
	const b = Buffer.from(text, "utf8");
	return b.buffer.slice(b.byteOffset, b.byteOffset + b.byteLength);
}

(async () => {
	require(path.resolve(workerPath));
	const fixture = fs.readFileSync(exportPath, "utf8");

	const formats = await send({ id: 1, kind: "formats" });
	if (!formats.formats || !formats.formats.includes("awx")) {
		throw new Error("the worker did not list the awx format: " + JSON.stringify(formats));
	}
	const first = await send({ id: 2, kind: "assess", format: "awx", bytes: bytesOf(fixture) });
	if (!first.result.ok) { throw new Error("the fixture was refused: " + first.result.error); }

	// The Go runtime prints its out of memory trace as it ends, which is the thing this check causes
	// on purpose. It is held back and shown only if the check fails.
	const held = [];
	const log = console.log;
	const warn = console.warn;
	console.log = (...a) => held.push(a.join(" "));
	console.warn = (...a) => held.push(a.join(" "));
	const large = await send({ id: 3, kind: "assess", format: "awx", bytes: bytesOf(largeExport(24)) });
	console.log = log;
	console.warn = warn;
	if (large.result.ok || !/more memory than the reader/.test(large.result.error)) {
		console.error(held.join("\n"));
		throw new Error("a file too large for the reader was not refused with the reason: " +
			JSON.stringify(large.result).slice(0, 200));
	}
	if (!held.some((line) => line.includes("out of memory"))) {
		throw new Error("the large export was refused without the reader running out of memory, so " +
			"this check did not exercise what it is for: " + held.slice(0, 3).join(" | "));
	}

	const after = await send({ id: 4, kind: "assess", format: "awx", bytes: bytesOf(fixture) });
	if (!after.result.ok || after.result.report !== first.result.report) {
		throw new Error("the worker did not read an ordinary export again after a large one: " +
			JSON.stringify(after.result).slice(0, 200));
	}
	if (fetches !== 1) {
		throw new Error("the worker fetched the reader " + fetches + " times, so replacing it is a " +
			"request the page says it never makes after loading");
	}
	console.log("assess worker: a file too large for the reader is refused, and the next one is read");
	process.exit(0);
})().catch((e) => {
	console.error("assess worker check failed: " + (e && e.stack ? e.stack : e));
	process.exit(1);
});
