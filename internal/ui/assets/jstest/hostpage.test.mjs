// Tests for the host page helpers in 18-host-page.js: the status badge and the sync interval
// wording.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "16-runs-list.js", "18-host-page.js"]);

test("badge builds a status span with underscores read as spaces", () => {
	const tests = [
		// Test 0: A one word status.
		{ In: "running", WantClass: "badge running", WantText: "running" },
		// Test 1: Underscores become spaces in the label but stay in the class.
		{ In: "pending_approval", WantClass: "badge pending_approval", WantText: "pending approval" },
		// Test 2: Every underscore is replaced, not just the first.
		{ In: "a_b_c", WantClass: "badge a_b_c", WantText: "a b c" },
	];
	for (const [i, tc] of tests.entries()) {
		const span = app.badge(tc.In);
		assert.equal(span.tagName, "SPAN", "test " + i);
		assert.equal(span.className, tc.WantClass, "test " + i);
		assert.equal(span.textContent, tc.WantText, "test " + i);
	}
});

test("fmtInterval renders the largest whole unit that fits", () => {
	const tests = [
		{ In: 3600, Want: "hour" }, // Test 0: Exactly one hour.
		{ In: 7200, Want: "2 hours" }, // Test 1: Whole hours.
		{ In: 86400, Want: "24 hours" }, // Test 2: A day is still hours.
		{ In: 60, Want: "minute" }, // Test 3: Exactly one minute.
		{ In: 300, Want: "5 minutes" }, // Test 4: Whole minutes.
		{ In: 90, Want: "90 seconds" }, // Test 5: Not a whole minute.
		{ In: 45, Want: "45 seconds" }, // Test 6: Under a minute.
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.fmtInterval(tc.In), tc.Want, "test " + i);
	}
});

test("the host summary counts failures from the field the API actually returns", () => {
	// The rows carry worst, not outcome. Counting a nonexistent field made Failures read zero and
	// Success rate 100% while the table below showed red failed chips: three views of one host
	// disagreed on the demo's first click.
	const host = app.document.createElement("div");
	host.id = "host-summary";
	app.document.body.appendChild(host);
	app.renderHostSummary("web01", [
		{ worst: "failed", changed: 1, duration_seconds: 2 },
		{ worst: "unreachable", changed: 0, duration_seconds: 1 },
		{ worst: "ok", changed: 3, duration_seconds: 4 },
		{ worst: "changed", changed: 2, duration_seconds: 3 },
	]);
	const text = host.textContent;
	assert.ok(text.includes("2") && text.includes("Failures"),
		"two of four runs went bad on this host; the card said: " + text);
	assert.ok(text.includes("50%"),
		"success rate should be 50%, not 100%; the card said: " + text);
});
