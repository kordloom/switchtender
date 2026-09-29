// Render and interaction tests for the overview activity chart's controls, mounted against the real
// overview template. The math lives in activitywindow.test.mjs; this proves the wiring: the controls
// build, the window pills re-bucket, the bars-or-line toggle swaps the drawing, and the filter box
// narrows the note through its debounce.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { makeEvent } from "./dom.mjs";

const PARTS = ["01-boot.js", "15-overview-doctor.js"];

// recentRuns returns a few runs inside the last few hours, so the default twelve-hour window holds
// them whatever wall-clock moment the test runs at.
function recentRuns() {
	const now = Date.now();
	const ago = (h) => new Date(now - h * 3600 * 1000).toISOString();
	return [
		{ created_at: ago(1), status: "succeeded", playbook: "deploy-web.yml", hosts: ["web01"] },
		{ created_at: ago(1), status: "failed", playbook: "deploy-db.yml", hosts: ["db01"] },
		{ created_at: ago(3), status: "succeeded", playbook: "deploy-web.yml", hosts: ["web02"] },
	];
}

test("the activity controls build and the default window draws twelve hourly bars", () => {
	const { app, document } = loadPage("overview", { parts: PARTS });
	app.renderActivity(recentRuns());

	assert.equal(document.getElementById("activity-panel").hidden, false, "the panel stayed hidden");
	assert.equal(document.querySelectorAll(".activity-windows .seg-btn").length, 6, "six window pills");
	assert.equal(document.querySelectorAll(".activity-chart-toggle .seg-btn").length, 2, "bars and line");
	assert.ok(document.querySelector(".activity-filter"), "the filter box is missing");
	assert.equal(document.querySelectorAll("#activity .activity-col").length, 12, "12h is twelve bars");
	assert.equal(document.querySelectorAll("#activity .activity-svg").length, 0, "bars is the default, not line");
});

test("clicking a window pill re-buckets to that many columns", () => {
	const { app, document } = loadPage("overview", { parts: PARTS });
	app.renderActivity(recentRuns());

	const day = document.querySelector('.activity-windows .seg-btn[data-window="24"]');
	day.click();
	assert.ok(day.classList.contains("active"), "the clicked pill is not marked active");
	assert.equal(document.querySelectorAll("#activity .activity-col").length, 24, "24h is twenty-four bars");

	const week = document.querySelector('.activity-windows .seg-btn[data-window="168"]');
	week.click();
	assert.equal(document.querySelectorAll("#activity .activity-col").length, 7, "7d is seven daily bars");
});

test("the line toggle swaps the bars for an svg with a line and a dot per column", () => {
	const { app, document } = loadPage("overview", { parts: PARTS });
	app.renderActivity(recentRuns());

	document.querySelector('.activity-chart-toggle .seg-btn[data-chart="line"]').click();
	const chart = document.getElementById("activity");
	assert.ok(chart.classList.contains("as-line"), "the line class is not set on the chart");
	assert.equal(chart.querySelectorAll("svg.activity-svg").length, 1, "no svg was drawn");
	assert.equal(chart.querySelectorAll(".act-line.ok").length, 1, "the total line is missing");
	assert.equal(chart.querySelectorAll(".act-dot-hit").length, 12, "one drill-down dot per column");
	assert.ok(chart.querySelector(".act-line.fail"), "a failed run should draw the failed line");

	// Back to bars, and the svg is gone again.
	document.querySelector('.activity-chart-toggle .seg-btn[data-chart="bars"]').click();
	assert.equal(document.querySelectorAll("#activity .activity-svg").length, 0, "line view lingered");
});

test("the filter narrows the note after its debounce", async () => {
	const { app, document, clock } = loadPage("overview", { parts: PARTS });
	app.renderActivity(recentRuns());

	const filter = document.querySelector(".activity-filter");
	filter.value = "db01";
	filter.dispatchEvent(makeEvent("input"));
	await clock.tick(200);

	const note = document.getElementById("activity-note").textContent;
	assert.match(note, /1 of 3 match/, "the note did not reflect the filtered count: " + note);
	// Filtering counts, it does not drop columns.
	assert.equal(document.querySelectorAll("#activity .activity-col").length, 12, "a column vanished under the filter");
});
