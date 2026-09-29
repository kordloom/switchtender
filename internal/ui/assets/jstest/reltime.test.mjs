// Tests for refreshRelTimes in 09-audit.js, which keeps relative ages current on a timer.
//
// tdTime builds a time cell as an age plus an appended copy-timestamp button. refreshRelTimes ran
// every twenty seconds and set the cell's textContent, which replaced every child and deleted the
// button, so across the fleet, drift, tasks, host, audit, and other tables every timestamp lost its
// copy control on the first tick. It must rewrite only the age text and leave the button in place.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts([
	"01-boot.js", "07-nav-theme.js", "08-auth-status.js", "09-audit.js", "18-host-page.js",
]);

test("refreshRelTimes keeps the copy button and only updates the age", () => {
	const iso = "2026-08-20T12:00:00Z";
	const cell = app.tdTime(iso);
	// The freshly built cell carries the age text and one copy button.
	const buttonsBefore = cell.querySelectorAll("button").length;
	assert.equal(buttonsBefore, 1, "tdTime should build the cell with a copy button");
	assert.ok(cell.dataset.time === iso && cell.classList.contains("reltime"),
		"the cell should be marked for refresh");

	// Put it in the document so the refresh query finds it, then tick.
	const host = app.document.createElement("table");
	const body = app.document.createElement("tbody");
	const row = app.document.createElement("tr");
	row.appendChild(cell);
	body.appendChild(row);
	host.appendChild(body);
	app.document.body.appendChild(host);

	app.refreshRelTimes();

	assert.equal(cell.querySelectorAll("button").length, 1,
		"the copy button was deleted by the refresh: a reader can no longer copy the timestamp");
	assert.equal(cell.dataset.time, iso, "the refresh must not drop the stored timestamp");
	assert.ok(cell.textContent.trim().length > 0, "the age text should still render");
});
