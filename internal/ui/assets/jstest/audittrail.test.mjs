// Tests that a run's Audit trail button finds the run's own creation entry.
//
// A creation entry is written in middleware before the handler runs, so the path it commits is the
// collection, /v1/runs, with no run id the server does not yet know. Filtering the trail by the run
// id therefore matched only entries written after the run existed, and a run still held for
// approval has none of those: the demo's flagship held run, reached from the Overview tile, opened
// an empty table while the header above the button named the very chain entry that created it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";

const ENTRIES = {
	entries: [
		{ seq: 47, at: "2026-09-14T10:00:00Z", actor: "admin", method: "RUN",
			path: "/runs/run_other/outcome/succeeded", hash: "aaaa" },
		{ seq: 44, at: "2026-09-14T09:00:00Z", actor: "admin", method: "POST",
			path: "/v1/runs", hash: "bbbb" },
		{ seq: 43, at: "2026-09-14T08:00:00Z", actor: "admin", method: "POST",
			path: "/v1/templates", hash: "cccc" },
	],
};

// visibleSeqs returns the sequence of every row the filter left showing.
function visibleSeqs(document) {
	return Array.from(document.querySelectorAll("#audit tr"))
		.filter((tr) => tr.dataset.fhide !== "1")
		.map((tr) => tr.cells[0].textContent);
}

test("a run with no later entries still finds its creation entry by sequence", async () => {
	const page = loadPage("audit", {
		search: "?q=run_held&seq=44",
		routes: [[/^\/v1\/audit/, reply(ENTRIES)]],
		quiet: true,
	});
	page.app.mountListFilter();
	await page.app.loadAudit();
	await page.clock.tick(300);

	const seqs = visibleSeqs(page.document);
	assert.ok(seqs.includes("44"),
		"the run's creation entry was filtered away, so the trail opened empty for a held run");
	assert.ok(!seqs.includes("43"),
		"an unrelated entry survived, so the sequence keep is not a filter bypass");
});

test("without a sequence the filter still behaves as plain text", async () => {
	const page = loadPage("audit", {
		search: "?q=run_other",
		routes: [[/^\/v1\/audit/, reply(ENTRIES)]],
		quiet: true,
	});
	page.app.mountListFilter();
	await page.app.loadAudit();
	await page.clock.tick(300);

	const seqs = visibleSeqs(page.document);
	assert.deepEqual(seqs, ["47"], "plain text filtering changed shape");
});
