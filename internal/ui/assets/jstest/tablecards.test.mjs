// Tests for the card labels in 33-table-cards.js, which give every list table's cells the header
// text and the card slot a phone layout reads them by.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts, ALL_PARTS } from "./loader.mjs";
import { mountPage } from "./pages.mjs";

// mountTable fills a page's first list table with one row of numbered cells and labels it.
function mountTable(page) {
	const app = loadParts(ALL_PARTS);
	const document = mountPage(app, page);
	const table = document.querySelector("main.content table.runs");
	const tbody = table.tBodies[0];
	for (const row of Array.from(tbody.rows)) row.remove();
	const tr = document.createElement("tr");
	const columns = table.tHead.rows[0].cells.length;
	for (let i = 0; i < columns; i++) {
		const cell = document.createElement("td");
		cell.textContent = "v" + i;
		tr.appendChild(cell);
	}
	tbody.appendChild(tr);
	app.mountTableCards();
	return { app, table, row: tr };
}

// slots reads each cell of a row as "slot:label".
function slots(row) {
	return Array.from(row.cells).map((c) => c.dataset.slot + ":" + c.dataset.label);
}

test("a runs row names its status and run, hides the number, and labels every field", () => {
	const { row } = mountTable("runs");
	assert.deepEqual(slots(row), [
		"hide:#", "status:Status", "title:Run", "field:Type", "field:Origin", "field:What it runs",
		"field:Labels", "field:Started", "field:Duration",
	]);
});

test("an unmarked table titles its first column and gives its controls the actions slot", () => {
	const { row } = mountTable("projects");
	assert.deepEqual(slots(row), [
		"title:Name", "field:Repository", "field:Branch", "field:Created", "actions:Actions",
	]);
});

test("a row added later is labeled when the table is labeled again", () => {
	const { app, table, row } = mountTable("projects");
	const later = row.cloneNode(true);
	for (const cell of later.cells) {
		delete cell.dataset.slot;
		delete cell.dataset.label;
	}
	table.tBodies[0].appendChild(later);
	app.labelTableCells(table);
	assert.deepEqual(slots(later), slots(row));
});
