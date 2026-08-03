// Tests for the stream helpers in 20-held-copy-stream.js: the token-carrying stream URL and the
// resume cursor.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

test("streamURL leaves the path bare when no token is stored", () => {
	const app = loadParts(["01-boot.js", "07-nav-theme.js", "20-held-copy-stream.js"]);
	assert.equal(app.streamURL("/runs/run_1/events/stream"), "/v1/runs/run_1/events/stream");
});

test("streamURL appends the stored token, encoded, with the right separator", () => {
	const app = loadParts(["01-boot.js", "07-nav-theme.js", "20-held-copy-stream.js"]);
	app.localStorage.setItem("st_token", "tok abc+/=");
	// A bare path gets a query string.
	assert.equal(
		app.streamURL("/runs/run_1/events/stream"),
		"/v1/runs/run_1/events/stream?access_token=tok%20abc%2B%2F%3D",
	);
	// A path that already carries a query gets an ampersand.
	assert.equal(
		app.streamURL("/runs/run_1/events/stream?after=5"),
		"/v1/runs/run_1/events/stream?after=5&access_token=tok%20abc%2B%2F%3D",
	);
});

test("lastSeq returns the highest store sequence, zero when none carry one", () => {
	const app = loadParts(["01-boot.js", "07-nav-theme.js", "20-held-copy-stream.js"]);
	const tests = [
		{ In: [], Want: 0 }, // Test 0: No events.
		{ In: [{ type: "task_start" }, {}], Want: 0 }, // Test 1: No sequence numbers.
		{ In: [{ seq: 3 }, {}, { seq: 9 }, { seq: 2 }], Want: 9 }, // Test 2: Mixed.
		{ In: [{ seq: 0 }], Want: 0 }, // Test 3: A zero sequence is no cursor.
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.lastSeq(tc.In), tc.Want, "test " + i);
	}
});

// linkStub returns a stand-in for one of the run detail links that records what was assigned to it
// and holds on to the click handler, so a test can follow the link the way a reader would.
function linkStub() {
	return {
		dataset: {},
		clicks: [],
		addEventListener(name, fn) { if (name === "click") this.clicks.push(fn); },
		click() { return this.clicks[0]({ preventDefault() {} }); },
	};
}

// downloadHarness loads the detail parts and wires the run download links against stubs for
// everything that would leave the page: the network, the blob plumbing, and the new tab.
function downloadHarness(runId, token) {
	const app = loadParts(["01-boot.js", "07-nav-theme.js", "08-auth-status.js", "20-held-copy-stream.js"]);
	const els = { "full-log": linkStub(), "export-events": linkStub() };
	const calls = [];
	app.document.getElementById = (id) => els[id] || null;
	app.localStorage.setItem("st_token", token);
	app.fetch = (url, opts) => {
		calls.push({ url, opts });
		return Promise.resolve({ ok: true, status: 200, text: () => Promise.resolve("bytes") });
	};
	app.Blob = class Blob {};
	app.URL = { createObjectURL: () => "blob:local", revokeObjectURL: () => {} };
	app.open = () => ({});
	// The blob handle is released on a timer the page can afford to wait out and a test cannot.
	app.setTimeout = (fn) => fn();
	app.downloadBlob = () => {};
	app.wireRunDownloads(runId);
	return { app, els, calls };
}

test("the run download links never carry the token in an href", async () => {
	const token = "tok_secret";
	const { els, calls } = downloadHarness("run_1", token);

	// The links are left exactly as the template wrote them. An href built from the token puts a
	// live credential in the DOM, in the address bar of the tab it opens, and in anything the
	// reader copies out of the page.
	for (const [id, el] of Object.entries(els)) {
		assert.equal(el.href, undefined, id + " was given an href");
	}

	await els["full-log"].click();
	await els["export-events"].click();

	const wanted = ["/v1/runs/run_1/logs", "/v1/runs/run_1/events?download=1"];
	assert.deepEqual(calls.map((c) => c.url), wanted);
	for (const call of calls) {
		assert.equal(call.opts.headers.Authorization, "Bearer " + token);
		assert.ok(!String(call.url).includes("access_token"), "token rode along in " + call.url);
	}
});
