// Tests for the runs page stat cards in 16-runs-list.js. The cards beneath the total have to add up
// to it. Runs held for a decision had no card, so a page with four of them read 38 total over 30
// succeeded, 4 failed, and 0 active.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "08-auth-status.js", "16-runs-list.js"]);

// cards renders summary and reads the cards back as label -> value, in order.
function cards(summary, shown, filtered, more) {
	const { document } = app;
	document.body.innerHTML = '<div id="summary" hidden></div>';
	app.renderSummary(summary, shown, filtered, more);
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

// caption renders summary and returns the scope caption above the cards, or null when there is none.
function caption(summary, shown, filtered, more) {
	cards(summary, shown, filtered, more);
	const note = app.document.getElementById("summary-scope");
	return note ? note.textContent : null;
}

test("a filtered table is told apart from the install-wide cards", () => {
	const summary = { total: 38, succeeded: 30, failed: 4, active: 0, awaiting_approval: 4 };
	const tests = [
		// Test 0: A search that matches two runs says the cards cover the install.
		{
			Shown: 2, Filtered: true,
			Want: "Counts cover every run on this install. The table shows the 2 runs matching this search.",
		},
		// Test 1: One matching run is singular.
		{
			Shown: 1, Filtered: true,
			Want: "Counts cover every run on this install. The table shows the 1 run matching this search.",
		},
		// Test 2: More matches wait behind Load more, so the table holds the first of them.
		{
			Shown: 25, Filtered: true, More: true,
			Want: "Counts cover every run on this install. The table shows the first 25 runs matching this search.",
		},
		// Test 3: Counts scoped by grants say they cover what the caller can see, not the install.
		{
			Shown: 2, Filtered: true, Scope: "visible",
			Want: "Counts cover every run you can see. The table shows the 2 runs matching this search.",
		},
	];
	for (const [testNum, tc] of tests.entries()) {
		const scoped = tc.Scope ? { ...summary, scope: tc.Scope } : summary;
		assert.equal(caption(scoped, tc.Shown, tc.Filtered, tc.More), tc.Want, `test ${testNum}`);
	}
});

test("an unfiltered table draws no scope caption", () => {
	const summary = { total: 38, succeeded: 30, failed: 4, active: 0, awaiting_approval: 4 };
	assert.equal(caption(summary, 30, false), null);
});

test("the caption precedes the cards and clears when the filter does", () => {
	const summary = { total: 4, succeeded: 4, failed: 0, active: 0, awaiting_approval: 0 };
	cards(summary, 1, true);
	const { document } = app;
	const note = document.getElementById("summary-scope");
	assert.equal(note.className, "muted");
	assert.equal(note.nextElementSibling, document.getElementById("summary"), "the caption is not above the cards");
	cards(summary, 4, false);
	assert.equal(document.getElementById("summary-scope"), null, "an unfiltered redraw kept the caption");
});

// loadWith runs loadRuns on a stubbed page whose search box holds query and returns the caption the
// page drew, so the test covers the call site that decides whether the table is filtered.
async function loadWith(query) {
	const page = loadParts(["01-boot.js", "08-auth-status.js", "16-runs-list.js"]);
	const byID = {};
	for (const id of ["summary", "runs-search", "runs-pagesize"]) byID[id] = page.document.createElement("div");
	byID["runs-search"].value = query;
	byID.runs = page.document.createElement("tbody");
	byID.summary.parentNode = { insertBefore: (node) => { byID["summary-scope"] = node; return node; } };
	const table = page.document.createElement("table");
	page.location.search = "";
	page.document.getElementById = (id) => byID[id] || null;
	page.document.querySelector = (sel) => (sel === "table.runs" ? table : null);
	page.getJSON = () => Promise.resolve({ runs: [{ id: "run_1" }, { id: "run_2" }], summary: { total: 38 } });
	page.setStatus = () => {};
	page.showSkeletonRows = () => {};
	page.showListControls = () => {};
	page.appendRunRows = () => {};
	page.wireRunsMore = () => {};
	await page.loadRuns();
	return byID["summary-scope"] ? byID["summary-scope"].textContent : null;
}

test("loading a searched table captions the cards and an unsearched one does not", async () => {
	assert.equal(await loadWith("web"),
		"Counts cover every run on this install. The table shows the 2 runs matching this search.");
	assert.equal(await loadWith(""), null);
});
