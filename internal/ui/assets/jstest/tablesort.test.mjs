// Tests for the sortable table headers in 02-page-data.js, which every list page mounts.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts, ALL_PARTS } from "./loader.mjs";
import { mountPage } from "./pages.mjs";

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
