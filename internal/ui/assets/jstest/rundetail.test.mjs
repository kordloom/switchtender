// Tests for the run detail streaming paths in 22-run-detail.js: the resume cursor a live stream
// opens with, and the split parent's reconcile when its stream ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

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
	page.streamURL = (path) => path;
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

test("openStream opens the live stream with a cursor even from an empty history", () => {
	const handle = mountDetail();
	handle.page.openStream("run_1", 0);
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
