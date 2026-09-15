// Tests for the runs list keeping its filters in the address bar.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// ROUTES answers the runs page's reads with a small, fixed list.
const ROUTES = {
	"/v1/runs": reply({ runs: [], count: 0, has_more: false }),
	"/v1/runs/summary": reply({ total: 0, succeeded: 0, failed: 0, active: 0 }),
};

// TestFiltersReachTheURL pins that a narrowed list can be reloaded, bookmarked, and shared.
//
// The filters lived only in the controls: the address bar read /ui/runs while the table showed four
// of seventeen runs, a reload silently restored all of them, and a copied URL sent the recipient to
// the unfiltered list. The app hands out ?q= deep links itself, so the page read a filter from a URL
// it would never write back.
test("filtering the runs list puts the filter in the address bar", async () => {
	const { app, document, clock } = loadPage("runs", { parts: ALL_PARTS, routes: ROUTES });
	document.getElementById("runs-status").value = "failed";
	document.getElementById("runs-search").value = "db01";
	await app.loadRuns();
	await clock.flush();
	const url = new URLSearchParams(app.location.search);
	assert.equal(url.get("status"), "failed", "the status filter never reached the URL");
	assert.equal(url.get("q"), "db01", "the search text never reached the URL");
	assert.deepEqual(app.location.navigations, [],
		"syncing the address bar counted as navigating away from the page");
});

// TestClearingAFilterClearsTheURL pins that the URL follows the controls back down, or a cleared
// filter would be restored by the next reload.
test("clearing a filter clears it from the address bar", async () => {
	const { app, document, clock } = loadPage("runs", { parts: ALL_PARTS, routes: ROUTES });
	document.getElementById("runs-status").value = "failed";
	await app.loadRuns();
	await clock.flush();
	assert.equal(new URLSearchParams(app.location.search).get("status"), "failed");

	document.getElementById("runs-status").value = "";
	await app.loadRuns();
	await clock.flush();
	assert.equal(new URLSearchParams(app.location.search).get("status"), null,
		"a cleared filter stayed in the URL, so a reload would put it back");
});

// TestURLFiltersSeedTheControls pins the other direction: a shared link has to produce the list it
// describes, not the unfiltered one with a misleading address bar.
test("a shared filtered URL seeds the controls", async () => {
	const { app, document } = loadPage("runs", {
		parts: ALL_PARTS, routes: ROUTES, search: "?status=failed&tool=terraform",
	});
	app.wireRunsFilters();
	assert.equal(document.getElementById("runs-status").value, "failed",
		"a shared link did not restore the status it carried");
	assert.equal(document.getElementById("runs-tool").value, "terraform",
		"a shared link did not restore the tool it carried");
});
