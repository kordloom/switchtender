// Tests for the formatting helpers in 08-auth-status.js: durations, path base names, short ids,
// and relative ages.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "08-auth-status.js"]);

// iso returns an ISO timestamp offset from a fixed base, for readable duration cases.
const base = Date.parse("2026-03-01T12:00:00Z");
const iso = (offsetMs) => new Date(base + offsetMs).toISOString();

test("fmtDuration renders the span between two ISO times", () => {
	const tests = [
		// Test 0: A zero span.
		{ Start: iso(0), End: iso(0), Want: "0ms" },
		// Test 1: Sub-second stays in milliseconds.
		{ Start: iso(0), End: iso(500), Want: "500ms" },
		// Test 2: The last millisecond before seconds.
		{ Start: iso(0), End: iso(999), Want: "999ms" },
		// Test 3: One second, one decimal.
		{ Start: iso(0), End: iso(1000), Want: "1.0s" },
		// Test 4: Sub-minute.
		{ Start: iso(0), End: iso(42_500), Want: "42.5s" },
		// Test 5: A minute and a half still reads in seconds; there is no minute unit.
		{ Start: iso(0), End: iso(90_000), Want: "90.0s" },
		// Test 6: Hours read in seconds too.
		{ Start: iso(0), End: iso(2 * 3600_000), Want: "7200.0s" },
		// Test 7: A negative span is nonsense and renders empty.
		{ Start: iso(1000), End: iso(0), Want: "" },
		// Test 8: A missing start renders empty.
		{ Start: "", End: iso(0), Want: "" },
		// Test 9: A missing end renders empty.
		{ Start: iso(0), End: undefined, Want: "" },
		// Test 10: Unparseable times render empty.
		{ Start: "yesterday", End: "today", Want: "" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.fmtDuration(tc.Start, tc.End), tc.Want, "test " + i);
	}
});

test("fmtMs renders a millisecond count", () => {
	const tests = [
		{ In: 0, Want: "0ms" }, // Test 0: Zero.
		{ In: 999, Want: "999ms" }, // Test 1: Millisecond ceiling.
		{ In: 500.4, Want: "500ms" }, // Test 2: Fractional milliseconds round.
		{ In: 1000, Want: "1.0s" }, // Test 3: Seconds floor.
		{ In: 1250, Want: "1.3s" }, // Test 4: One decimal, rounded.
		{ In: 61_000, Want: "61.0s" }, // Test 5: Over a minute stays seconds.
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.fmtMs(tc.In), tc.Want, "test " + i);
	}
});

test("baseName returns the last path segment", () => {
	const tests = [
		{ In: "site.yml", Want: "site.yml" }, // Test 0: No path.
		{ In: "deploy/site.yml", Want: "site.yml" }, // Test 1: Relative path.
		{ In: "/srv/plays/site.yml", Want: "site.yml" }, // Test 2: Absolute path.
		{ In: "dir/", Want: "" }, // Test 3: Trailing slash has no final segment.
		{ In: "", Want: "" }, // Test 4: Empty.
		{ In: null, Want: "" }, // Test 5: Null.
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.baseName(tc.In), tc.Want, "test " + i);
	}
});

test("shortId truncates long identifiers", () => {
	const tests = [
		// Test 0: Fifteen characters is the display limit and stays whole.
		{ In: "run_0123456789a", Want: "run_0123456789a" },
		// Test 1: Sixteen characters truncates to thirteen plus an ellipsis.
		{ In: "run_0123456789ab", Want: "run_012345678…" },
		// Test 2: Empty.
		{ In: "", Want: "" },
		// Test 3: Null.
		{ In: null, Want: "" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.shortId(tc.In), tc.Want, "test " + i);
	}
});

test("relTime renders a short relative age", () => {
	const ago = (ms) => new Date(Date.now() - ms).toISOString();
	assert.equal(app.relTime(ago(0)), "just now");
	assert.equal(app.relTime(ago(30_000)), "30s ago");
	assert.equal(app.relTime(ago(10 * 60_000)), "10m ago");
	assert.equal(app.relTime(ago(5 * 3600_000)), "5h ago");
	assert.equal(app.relTime(ago(3 * 86_400_000)), "3d ago");
	// Older than a month falls back to the locale date.
	const old = ago(45 * 86_400_000);
	assert.equal(app.relTime(old), new Date(old).toLocaleDateString());
	// Empty and unparseable inputs come back as given.
	assert.equal(app.relTime(""), "");
	assert.equal(app.relTime("not a time"), "not a time");
});
