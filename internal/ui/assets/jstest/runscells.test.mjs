// Tests for the Started and Duration cells of the runs table in 16-runs-list.js. A run held for
// approval never started and has no end, so its Started cell showed the submission time and its
// Duration cell was empty, which read as data the page failed to load.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts, ALL_PARTS } from "./loader.mjs";

const app = loadParts(ALL_PARTS);

// NOW is the instant the cells are rendered at.
const NOW = new Date("2026-10-22T04:30:00Z");

test("the duration cell says how long a run took, waited, or has run", () => {
	const tests = [
		// Test 0: A finished run shows how long it took.
		{
			In: { status: "succeeded", created_at: "2026-10-22T04:00:00Z", started_at: "2026-10-22T04:00:00Z",
				ended_at: "2026-10-22T04:00:04.2Z" },
			WantText: "4.2s",
		},
		// Test 1: A run held for approval shows how long it has waited, from submission.
		{
			In: { status: "pending_approval", created_at: "2026-10-21T23:16:00Z" },
			WantText: "Waiting 5h 14m",
		},
		// Test 2: A workflow parked at an approval step started, and still counts its wait from submission.
		{
			In: { status: "pending_approval", created_at: "2026-10-21T21:56:00Z", started_at: "2026-10-21T21:56:00Z" },
			WantText: "Waiting 6h 34m",
		},
		// Test 3: A running run shows how long it has run.
		{
			In: { status: "running", created_at: "2026-10-22T04:20:00Z", started_at: "2026-10-22T04:28:00Z" },
			WantText: "Running 2m 0s",
		},
		// Test 4: A queued run that has not started has nothing to time.
		{ In: { status: "pending", created_at: "2026-10-22T04:29:00Z" }, WantText: "" },
	];
	for (const [testNum, tc] of tests.entries()) {
		assert.equal(app.runDurationCell(tc.In, NOW).textContent, tc.WantText, `test ${testNum}`);
	}
});

test("a run that has not started says so and keeps its submission time for sorting", () => {
	const held = app.runStartedCell({ status: "pending_approval", created_at: "2026-10-21T23:16:00Z" });
	assert.equal(held.textContent, "Not started", "a held run reads as started");
	assert.equal(held.dataset.time, "2026-10-21T23:16:00Z", "the submission time is not kept for sorting");
	assert.match(held.dataset.tip, /^Submitted /);

	const started = app.runStartedCell({ status: "succeeded", created_at: "2026-10-22T04:00:00Z",
		started_at: "2026-10-22T04:00:01Z" });
	assert.equal(started.dataset.time, "2026-10-22T04:00:01Z", "a started run does not show its start");
	assert.notEqual(started.textContent, "Not started");
});
