// Tests for the run detail streaming paths in 22-run-detail.js: the resume cursor a live stream
// opens with, and the split parent's reconcile when its stream ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";
import { mountPage } from "./pages.mjs";

// mountDetail loads the run detail parts against a stubbed page: the stream is a recorder rather
// than an EventSource, and every render and fetch outside the code under test is replaced. The
// returned handle exposes the stream URL, the listeners the code registered, and what it fetched.
function mountDetail(shards) {
	const page = loadParts(["01-boot.js", "20-held-copy-stream.js", "22-run-detail.js"]);
	const handle = { page, json: [], events: [], listeners: {}, rendered: 0, shardRenders: 0 };
	page.getJSON = (path) => {
		handle.json.push(path);
		if (path === "/runs/p1") {
			return Promise.resolve({ id: "p1", kind: "split", status: "running", shard_count: 2 });
		}
		if (path === "/runs/p1/shards") return Promise.resolve({ shards: shards || [] });
		return Promise.reject(new Error("unexpected fetch " + path));
	};
	page.loadAllEvents = (id) => {
		handle.events.push(id);
		return Promise.resolve([{ seq: 1, type: "task_start", host: id }]);
	};
	page.renderDetail = () => { handle.rendered++; };
	page.renderShards = () => { handle.shardRenders++; };
	page.setStatus = () => {};
	page.streamURL = async (path) => path;
	page.EventSource = function (url) {
		handle.url = url;
		this.addEventListener = (name, fn) => { handle.listeners[name] = fn; };
		this.close = () => { handle.closed = true; };
	};
	return handle;
}

test("runStreamPath always carries the resume cursor, zero included", () => {
	const app = loadParts(["01-boot.js", "20-held-copy-stream.js", "22-run-detail.js"]);
	const tests = [
		// Test 0: A run with history resumes after its last sequence.
		{ Id: "run_1", After: 42, Want: "/runs/run_1/stream?after=42" },
		// Test 1: A run with no history yet still sends the cursor, so the stream replays from the
		// start instead of starting at whatever the log already holds.
		{ Id: "run_1", After: 0, Want: "/runs/run_1/stream?after=0" },
		// Test 2: A missing cursor is the same as none seen.
		{ Id: "run_1", After: undefined, Want: "/runs/run_1/stream?after=0" },
		// Test 3: The run id is placed in the path.
		{ Id: "run_9", After: 7, Want: "/runs/run_9/stream?after=7" },
	];
	for (const [i, tc] of tests.entries()) {
		const got = app.runStreamPath(tc.Id, tc.After);
		assert.equal(got, tc.Want, "test " + i);
		assert.ok(got.includes("after="), "test " + i + ": the cursor was dropped");
	}
});

test("openStream opens the live stream with a cursor even from an empty history", async () => {
	const handle = mountDetail();
	// Awaited because the URL now carries a minted ticket rather than the reader's own token, so
	// building it is a request rather than string concatenation.
	await handle.page.openStream("run_1", 0);
	assert.equal(handle.url, "/runs/run_1/stream?after=0");
});

test("a split parent reconciles its matrix from a fresh shard read when the stream ends", async () => {
	const handle = mountDetail([{ id: "s0" }, { id: "s1" }]);
	await handle.page.loadParent("p1");
	// The initial view is one read per shard, and the parent's stream is open.
	assert.deepEqual(handle.events, ["s0", "s1"]);
	assert.ok(handle.listeners.end, "the parent stream registered no end listener");

	await handle.listeners.end();
	assert.ok(handle.closed, "the stream was left open");
	// Every shard is read again, so events written between the first read and the stream connecting
	// are on the page rather than lost.
	assert.deepEqual(handle.events, ["s0", "s1", "s0", "s1"],
		"the parent never re-read its shards, so anything the stream missed stayed missing");
	// The folded model is discarded, or the fresh events would never reach the matrix.
	assert.equal(handle.page.detailState.model, null);
	assert.equal(handle.page.detailState.events.length, 2);
});

// mountForLog loads the detail parts against the real detail template and drives the real entry
// point, loadSingle, so the test exercises the wiring rather than the helper it calls.
function mountForLog(logBody, ok = true) {
	const app = loadParts(["01-boot.js", "20-held-copy-stream.js", "22-run-detail.js"]);
	const document = mountPage(app, "detail", { vars: { RunID: "run_1", MatrixCap: 20000 } });
	app.loadAllEvents = () => Promise.resolve([]);
	app.renderDetail = () => {};
	app.setStatus = () => {};
	app.fetchAuthed = () => Promise.resolve({ ok, text: () => Promise.resolve(logBody) });
	return { app, document };
}

// TestLoadStoredLog pins that a run which has already finished shows the output it produced.
//
// The pane was fed only by the live stream, so a running job showed its output and a finished one
// showed nothing. An Ansible play survived that because its matrix and timeline say what happened.
// A script has neither, since it has no hosts and no tasks, so the page was a header, a row of
// buttons, and empty space where the only thing the run produced should have been.
test("a finished run shows the output it produced", async () => {
	const { app, document } = mountForLog("Fleet capacity report\nweb01 ok\ndb01 hot\n");
	await app.loadSingle({ id: "run_1", status: "succeeded" });
	assert.equal(document.getElementById("log-panel").hidden, false,
		"the output panel stayed hidden for a finished run");
	assert.match(document.getElementById("log").textContent, /Fleet capacity report/);
	assert.equal(document.querySelector("#log-panel h2").textContent, "Output",
		"a finished run's record should not be labelled live");
});

test("an empty or unreadable log leaves the panel hidden rather than showing an empty box", async () => {
	for (const [body, ok, why] of [
		["", true, "an empty log"],
		["   \n\n  ", true, "a log of only whitespace"],
		["anything", false, "a log the server refused"],
	]) {
		const { app, document } = mountForLog(body, ok);
		await app.loadSingle({ id: "run_1", status: "failed" });
		assert.equal(document.getElementById("log-panel").hidden, true,
			why + " should leave the panel hidden");
	}
});

test("only the tail of a long log is loaded, so the pane cannot be flooded", async () => {
	const { app, document } = mountForLog("x".repeat(400000) + "\nLAST LINE\n");
	await app.loadSingle({ id: "run_1", status: "succeeded" });
	const shown = document.getElementById("log").textContent;
	assert.ok(shown.length <= 262144, "loaded " + shown.length + " characters, past the pane's cap");
	assert.match(shown, /LAST LINE/, "the tail is the part worth keeping");
});

test("a run still going is left to the live stream rather than loaded from storage", async () => {
	const { app, document } = mountForLog("stored output that should not appear\n");
	let opened = false;
	app.openStream = () => { opened = true; return Promise.resolve(); };
	await app.loadSingle({ id: "run_1", status: "running" });
	assert.ok(opened, "a live run should open its stream");
	assert.equal(document.getElementById("log-panel").hidden, true,
		"a live run's pane is filled by the stream, not by a stored copy");
});
