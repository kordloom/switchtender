// Tests for what the compare page offers a run that has no baseline, which is most runs on a fresh
// install and was 10 of the demo's 17.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// NO_BASELINE is what the server answers for a run that is the first of its kind.
const NO_BASELINE = reply({ error: "no earlier run of the same source to compare against" },
	{ status: 404 });

// mountCompare opens the compare page for run_1 against the given run list.
function mountCompare(routes) {
	return loadPage("compare", {
		parts: ALL_PARTS,
		vars: { RunID: "run_1" },
		routes: Object.assign({
			"/v1/runs/run_1/compare": NO_BASELINE,
			"/v1/runs/run_1": reply({ id: "run_1", playbook: "site.yml", status: "succeeded" }),
		}, routes || {}),
	});
}

// TestPickerOffersTheRunsThatRanTheSameWork pins that the dead end became a way on.
//
// The page ended on one sentence, no table, no control, only "Back to run", while the standing copy
// above it promised a baseline could be chosen and the loader had supported ?with=<id> all along.
test("a run with no baseline is offered the runs it can be compared against", async () => {
	const { app, document, clock } = mountCompare({
		"/v1/runs": reply({ runs: [
			{ id: "run_1", playbook: "site.yml", status: "succeeded" },
			{ id: "run_2", playbook: "site.yml", status: "failed", created_at: "2026-09-14T10:00:00Z" },
			{ id: "run_3", playbook: "other.yml", status: "succeeded" },
		] }),
	});
	await app.loadCompare();
	await clock.flush();
	const panel = document.getElementById("compare-picker");
	assert.equal(panel.hidden, false, "a run with no baseline was left with nothing to do");
	const links = Array.from(panel.querySelectorAll("a")).map((a) => a.getAttribute("href"));
	assert.ok(links.some((h) => h.includes("compare?with=run_2")),
		"the picker did not offer the other run of the same playbook");
	assert.ok(!links.some((h) => h.includes("with=run_3")),
		"the picker offered a run of a different playbook as a baseline");
	assert.ok(!links.some((h) => h.includes("with=run_1")),
		"the picker offered the run itself as its own baseline");
	assert.ok(links.includes("/ui/runs"), "the picker did not offer a way back to all runs");
});

// TestPickerSaysSoWhenNothingComparable pins the empty case, which must still explain itself.
test("a run with nothing comparable is told so, not left blank", async () => {
	const { app, document, clock } = mountCompare({
		"/v1/runs": reply({ runs: [{ id: "run_1", playbook: "site.yml" }] }),
	});
	await app.loadCompare();
	await clock.flush();
	const panel = document.getElementById("compare-picker");
	assert.equal(panel.hidden, false);
	assert.match(panel.textContent, /nothing to compare this run against yet/i,
		"an install with no comparable run said nothing about why the picker is empty");
});

// TestBrokenResponseIsNotDressedAsNoBaseline pins that a real failure still reads as one. A
// truncated body or an HTML error page behind a 200 must not be reported as a missing baseline.
test("an unreadable comparison is reported as a failure, not as a missing baseline", async () => {
	const { app, document, clock } = mountCompare({
		"/v1/runs/run_1/compare": reply("<!doctype html><title>502</title>", { status: 200 }),
	});
	await app.loadCompare();
	await clock.flush();
	assert.equal(document.getElementById("compare-picker").hidden, true,
		"a broken response opened the baseline picker as though the run simply had no baseline");
	assert.match(document.getElementById("status").textContent, /Comparison unavailable/);
});
