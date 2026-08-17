// Tests for reaching the host matrix and the task timeline without a mouse, and for reading them
// without seeing color. Every cell was a bare div: no role, no tab stop, no key handler, and its
// outcome carried only in a background color and a hover title. A keyboard user could not open a
// single cell's detail, and a screen reader was read a grid of empty boxes, which is the whole result
// of the run.
import { test } from "node:test";
import assert from "node:assert/strict";

import { sandboxOf, evalIn } from "./loader.mjs";
import { loadPage } from "./pages.mjs";
import { fire } from "./dom.mjs";

// events is one run over two hosts and two tasks, with a failure so the outcomes differ.
const t = (n) => "2026-03-01T12:00:0" + n + "Z";
const events = [
	{ type: "task_start", task: "install nginx", time: t(1) },
	{ type: "runner_ok", host: "web-1", task: "install nginx", changed: true, time: t(2) },
	{ type: "runner_failed", host: "web-2", task: "install nginx", rc: 2, message: "apt broke", time: t(2) },
	{ type: "task_start", task: "start nginx", time: t(3) },
	{ type: "runner_ok", host: "web-1", task: "start nginx", time: t(4) },
	{ type: "runner_skipped", host: "web-2", task: "start nginx", time: t(4) },
];

// mountMatrix renders the grid and timeline for the events above and returns the page handle.
function mountMatrix() {
	const page = loadPage("detail", { vars: { RunID: "run_1" } });
	const app = page.app;
	const model = app.buildModel(events);
	evalIn(app, "detailState = { runId: 'run_1', run: { id: 'run_1' }, events: [], model: null };");
	app.detailState.events = events;
	app.detailState.model = model;
	app.renderMatrix(model);
	app.renderTimeline(model);
	return page;
}

test("every matrix cell is a labeled, focusable control", () => {
	const page = mountMatrix();
	const cells = [...page.document.querySelectorAll(".cell")];
	assert.equal(cells.length, 4, "the grid did not render");

	for (const cell of cells) {
		const label = cell.getAttribute("aria-label") || "";
		assert.match(label, /web-\d/, "a cell has no accessible name naming its host: " + label);
		assert.match(label, /nginx/, "a cell's name does not say which task it is: " + label);
		assert.match(label, /changed|ok|failed|skipped/,
			"a cell's outcome is carried by color alone: " + label);
		assert.equal(cell.getAttribute("role"), "button",
			"a cell announces as nothing, so nothing says it can be opened");
	}
	// One cell is the grid's tab stop; the arrow keys reach the rest. A grid of a thousand cells must
	// not put a thousand stops in the tab order.
	const stops = cells.filter((c) => c.getAttribute("tabindex") === "0");
	assert.equal(stops.length, 1, "the grid should hold exactly one tab stop, found " + stops.length);
});

test("the arrow keys walk the grid and Enter opens a cell's detail", () => {
	const page = mountMatrix();
	const win = sandboxOf(page.app);
	const cells = [...page.document.querySelectorAll(".cell")];
	const first = cells.find((c) => c.getAttribute("tabindex") === "0");
	assert.equal(first.dataset.host, "web-1", "the tab stop is not the first cell");

	first.focus();
	// A key press lands on the focused cell and bubbles to the table's handler, which is the path a
	// real keyboard takes.
	const press = (key) => fire(sandboxOf(page.app).document.activeElement, "keydown", { key });
	press("ArrowRight");
	assert.equal(win.document.activeElement.dataset.task, "start nginx",
		"the right arrow did not move along the row");
	assert.equal(win.document.activeElement.getAttribute("tabindex"), "0",
		"the tab stop did not follow the focus");
	assert.equal(first.getAttribute("tabindex"), "-1", "the grid now holds two tab stops");

	press("ArrowDown");
	assert.equal(win.document.activeElement.dataset.host, "web-2",
		"the down arrow did not move down the column");

	// At the edge the focus stays put rather than wrapping to the far side of the grid.
	press("ArrowDown");
	assert.equal(win.document.activeElement.dataset.host, "web-2", "the focus wrapped past the edge");

	press("Enter");
	const drill = page.document.getElementById("drill");
	assert.equal(drill.hidden, false, "Enter did not open the cell's detail");
	assert.match(page.document.getElementById("drill-body").textContent, /web-2/,
		"the detail that opened is not the focused cell's");
});

test("a timeline bar is a labeled control a keyboard can open", () => {
	const page = mountMatrix();
	const bars = [...page.document.querySelectorAll(".tl-bar")];
	assert.ok(bars.length >= 2, "the timeline did not render");
	for (const bar of bars) {
		assert.equal(bar.getAttribute("role"), "button", "a timeline bar announces as nothing");
		assert.equal(bar.tabIndex, 0, "a timeline bar cannot be reached by keyboard");
		assert.match(bar.getAttribute("aria-label") || "", /nginx/,
			"a timeline bar has no accessible name");
	}
	fire(bars[0], "keydown", { key: "Enter" });
	assert.equal(page.document.getElementById("drill").hidden, false,
		"Enter on a timeline bar opened nothing");
});
