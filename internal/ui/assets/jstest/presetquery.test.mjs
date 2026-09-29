// Tests for links that promise a filtered view. A page arriving with ?q= must seed its search
// from it: the held-runs link, a label chip, and a user's fired-runs count all navigate to
// /ui/runs?q=..., and the box used to ignore it and show the unfiltered list. The audit page
// widens its window for a preset search, since a run older than the display default filtered to
// an empty table.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";

test("the runs page seeds its search box from ?q= and sends it to the server", async () => {
	const page = loadPage("runs", {
		search: "?q=status%3Apending_approval",
		routes: { "/v1/runs": reply({ runs: [], count: 0, summary: {} }) },
	});
	page.app.wireRunsSearch();
	await page.app.loadRuns();
	assert.equal(page.document.getElementById("runs-search").value, "status:pending_approval");
	const sent = page.net.calls.find((c) => c.url.includes("/v1/runs"));
	assert.ok(sent.url.includes("q=status%3Apending_approval"), sent.url);
});

test("a preset audit search asks for the server's maximum window", async () => {
	const page = loadPage("audit", {
		search: "?q=run_old",
		routes: { "/v1/audit": reply({ entries: [], count: 0 }) },
	});
	await page.app.loadAudit();
	const sent = page.net.calls.find((c) => c.url.includes("/v1/audit"));
	assert.ok(sent.url.includes("limit=1000"), sent.url);
});
