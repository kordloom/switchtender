// Tests for the full-page activity view mounted at /ui/activity: it draws the shared chart, a
// window-tracking outcome summary, and a CSV export of the buckets on screen.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { fire } from "./dom.mjs";

// sample returns three recent runs, one failed, inside the last few hours.
function sample() {
	const now = Date.now();
	const ago = (h) => new Date(now - h * 3600 * 1000).toISOString();
	return [
		{ created_at: ago(1), status: "succeeded", playbook: "deploy-web.yml", hosts: ["web01"] },
		{ created_at: ago(2), status: "failed", playbook: "deploy-db.yml", hosts: ["db01"] },
		{ created_at: ago(3), status: "succeeded", playbook: "deploy-web.yml", hosts: ["web02"] },
	];
}

test("the activity page draws the chart, a window summary, and a working export", async () => {
	const page = loadPage("activity", { routes: { "/v1/runs": { runs: sample() } } });
	await page.app.loadActivityPage();
	await page.clock.flush();
	const doc = page.document;

	assert.equal(doc.getElementById("activity-panel").hidden, false, "the chart panel stayed hidden");
	assert.ok(doc.querySelectorAll("#activity .activity-col").length > 0, "no bars drew");
	assert.equal(doc.getElementById("activity-summary").hidden, false, "the summary stayed hidden");

	const summary = doc.getElementById("activity-summary").textContent;
	assert.match(summary, /Runs in window/, "the summary has no window total");
	assert.match(summary, /Success rate/, "the summary has no success rate");
	assert.match(summary, /Failed/, "the summary has no failed count");

	const btn = doc.getElementById("activity-export");
	assert.ok(btn, "no export button");
	assert.doesNotThrow(() => fire(btn, "click"), "exporting the buckets threw");
});

test("the window control re-buckets the page chart the same as the overview", async () => {
	const page = loadPage("activity", { routes: { "/v1/runs": { runs: sample() } } });
	await page.app.loadActivityPage();
	await page.clock.flush();
	const doc = page.document;

	doc.querySelector('.activity-windows .seg-btn[data-window="6"]').click();
	assert.equal(doc.querySelectorAll("#activity .activity-col").length, 6, "6h is six columns");
	assert.match(doc.getElementById("activity-summary").textContent, /Runs in window/,
		"the summary vanished after changing the window");
});
