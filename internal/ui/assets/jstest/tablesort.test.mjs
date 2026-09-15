// Tests for the sortable table headers in 02-page-data.js, which every list page mounts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts, ALL_PARTS } from "./loader.mjs";
import { mountPage } from "./pages.mjs";
import { fire } from "./dom.mjs";

// mountRuns puts the given duration strings into the runs table, one row each, and returns the
// page with the sort controls mounted.
function mountRuns(durations) {
	const app = loadParts(ALL_PARTS);
	const document = mountPage(app, "runs");
	const tbody = document.querySelector("main.content table tbody");
	for (const row of Array.from(tbody.rows)) row.remove();
	const columns = document.querySelectorAll("main.content table thead th").length;
	const durationIndex = 8;
	durations.forEach((text) => {
		const tr = document.createElement("tr");
		for (let i = 0; i < columns; i++) {
			const cell = document.createElement("td");
			if (i === durationIndex) cell.textContent = text;
			tr.appendChild(cell);
		}
		tbody.appendChild(tr);
	});
	app.mountTableSort();
	return { app, document, tbody, durationIndex };
}

// readColumn returns the text of one column, top to bottom, as it currently stands.
function readColumn(tbody, index) {
	return Array.from(tbody.rows).map((r) => r.cells[index].textContent);
}

// TestDurationSortsByRealLength pins that sorting a duration column orders by elapsed time.
//
// Durations render in whichever unit reads best, so a fast run says 980ms and a slow one 4.2s.
// Reading the leading number alone made 980 larger than 4.2 and put the three fastest runs on the
// page at the slow end of a column an operator sorts precisely to find the slow ones.
test("a duration column sorts by elapsed time, not by leading digits", () => {
	const { document, tbody, durationIndex } = mountRuns(["4.2s", "980ms", "1.5m", "2h", "30ms"]);
	const header = document.querySelectorAll("main.content table thead th")[durationIndex];
	assert.equal(header.textContent.trim(), "Duration", "the test is sorting the wrong column");
	header.click();
	assert.deepEqual(readColumn(tbody, durationIndex), ["30ms", "980ms", "4.2s", "1.5m", "2h"],
		"ascending duration order is wrong");
	header.click();
	assert.deepEqual(readColumn(tbody, durationIndex), ["2h", "1.5m", "4.2s", "980ms", "30ms"],
		"descending duration order is wrong");
});

// TestPlainNumbersStillSortAsNumbers pins that the duration reading did not capture ordinary counts.
test("a plain number column still sorts as numbers", () => {
	const { document, tbody, durationIndex } = mountRuns(["9", "10", "2"]);
	const header = document.querySelectorAll("main.content table thead th")[durationIndex];
	header.click();
	assert.deepEqual(readColumn(tbody, durationIndex), ["2", "9", "10"], "counts stopped sorting numerically");
});

// TestAppendedRowsJoinTheSort pins that Load more does not leave the table contradicting its header.
//
// Rows arriving later were appended in server order while the sorted header kept its arrow, so two
// clicks, sort then Load more, produced a visibly unsorted table claiming to be sorted, with the row
// numbers renumbered straight down the wrong order.
test("rows appended after a sort join it rather than landing at the bottom", () => {
	const { document, tbody, durationIndex } = mountRuns(["4.2s", "980ms"]);
	const header = document.querySelectorAll("main.content table thead th")[durationIndex];
	header.click();
	assert.deepEqual(readColumn(tbody, durationIndex), ["980ms", "4.2s"]);
	assert.equal(header.getAttribute("aria-sort"), "ascending",
		"the sorted header did not say so to a screen reader");

	// What Load more does: append in server order, then announce it.
	const columns = document.querySelectorAll("main.content table thead th").length;
	for (const text of ["2h", "30ms"]) {
		const tr = document.createElement("tr");
		for (let i = 0; i < columns; i++) {
			const cell = document.createElement("td");
			if (i === durationIndex) cell.textContent = text;
			tr.appendChild(cell);
		}
		tbody.appendChild(tr);
	}
	fire(document.querySelector("main.content table"), "rowsappended");

	assert.deepEqual(readColumn(tbody, durationIndex), ["30ms", "980ms", "4.2s", "2h"],
		"appended rows did not join the active sort");
	assert.equal(header.getAttribute("aria-sort"), "ascending",
		"re-sorting flipped the direction the header was showing");
});
