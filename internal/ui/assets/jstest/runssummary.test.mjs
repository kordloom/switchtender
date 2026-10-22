// Tests for the runs page stat cards in 16-runs-list.js. The cards beneath the total have to add up
// to it. Runs held for a decision had no card, so a page with four of them read 38 total over 30
// succeeded, 4 failed, and 0 active.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "08-auth-status.js", "16-runs-list.js"]);

// cards renders summary and reads the cards back as label -> value, in order.
function cards(summary) {
	const { document } = app;
	document.body.innerHTML = '<div id="summary" hidden></div>';
	app.renderSummary(summary);
	const out = [];
	for (const card of document.getElementById("summary").children) {
		out.push([card.querySelector(".stat-label").textContent, card.querySelector(".stat-value").textContent]);
	}
	return out;
}

// sumBelowTotal adds every card after the total.
function sumBelowTotal(got) {
	return got.slice(1).reduce((n, [, v]) => n + Number(v.replace(/,/g, "")), 0);
}

test("the cards beneath the total add up to it", () => {
	const tests = [
		// Test 0: The demo's numbers, four runs held for a decision.
		{
			In: { total: 38, succeeded: 30, failed: 4, active: 0, awaiting_approval: 4, scope: "install" },
			WantCards: [["Total runs", "38"], ["Succeeded", "30"], ["Failed", "4"], ["Pending approval", "4"],
				["Active", "0"]],
		},
		// Test 1: Canceled, interrupted, and rejected runs get their own card when there are any.
		{
			In: { total: 12, succeeded: 5, failed: 1, active: 2, awaiting_approval: 1 },
			WantCards: [["Total runs", "12"], ["Succeeded", "5"], ["Failed", "1"], ["Pending approval", "1"],
				["Active", "2"], ["Canceled or rejected", "3"]],
		},
		// Test 2: A scoped caller is told the count covers what they can see.
		{
			In: { total: 2, succeeded: 2, failed: 0, active: 0, awaiting_approval: 0, scope: "visible" },
			WantCards: [["Runs you can see", "2"], ["Succeeded", "2"], ["Failed", "0"], ["Pending approval", "0"],
				["Active", "0"]],
		},
	];
	for (const [testNum, tc] of tests.entries()) {
		const got = cards(tc.In);
		assert.deepEqual(got, tc.WantCards, `test ${testNum}`);
		assert.equal(sumBelowTotal(got), tc.In.total, `test ${testNum}: cards do not add up to the total`);
	}
});
