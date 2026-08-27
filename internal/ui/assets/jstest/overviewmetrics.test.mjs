// Tests for the overview headline cards in 15-overview-doctor.js. The cards say "Total runs" and
// "Success rate", so they have to describe the install rather than the page of runs the API happened
// to return. Counting the page made them describe the newest page instead: past the page size the
// total was flatly wrong and the rate was computed over a sample, while the correct install-wide
// numbers sat unused in the same response.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "15-overview-doctor.js", "16-runs-list.js"]);

// cardValues reads the rendered stat cards back as label -> value.
function cardValues(document) {
	const out = {};
	for (const card of document.getElementById("ov-metrics").children) {
		const value = card.querySelector(".stat-value");
		const label = card.querySelector(".stat-label");
		if (value && label) out[label.textContent] = value.textContent;
	}
	return out;
}

test("the headline cards describe the install, not the page of runs returned", () => {
	const { document } = app;
	document.body.innerHTML = '<div id="ov-metrics"></div>';

	// One page of 200 runs out of an install that has 4823, which is what the API returns.
	const page = [];
	for (let i = 0; i < 200; i++) page.push({ status: i % 2 ? "succeeded" : "failed" });
	const summary = { total: 4823, succeeded: 4600, failed: 223, active: 0 };

	app.renderOverviewMetrics(page, [{ host: "web01" }], summary);
	const got = cardValues(document);

	assert.equal(got["Total runs"], "4823",
		"the total counted the returned page rather than the install");
	assert.equal(got["Failed"], "223", "the failed count came from the page rather than the install");
	assert.equal(got["Success rate"], "95%",
		"the rate was computed over the page sample rather than the install");
});

test("without a summary the cards still describe what was returned", () => {
	const { document } = app;
	document.body.innerHTML = '<div id="ov-metrics"></div>';

	// A server that sends no summary is the only case that ever needed counting, and it still works.
	app.renderOverviewMetrics(
		[{ status: "succeeded" }, { status: "succeeded" }, { status: "failed" }],
		[{ host: "web01" }],
		null,
	);
	const got = cardValues(document);
	assert.equal(got["Total runs"], "3");
	assert.equal(got["Failed"], "1");
	assert.equal(got["Success rate"], "67%");
});
