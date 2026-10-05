// Tests for whether a pull request review plan's run page says what its pull request was last told
// and when reporting to it is failing.
//
// The server retries a forge that refuses or cannot be reached, and the pull request keeps showing
// the last report that landed meanwhile. Without the run saying so, a plan looked finished and fine
// while its pull request still read "Plan running".
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

// planRun returns a finished review plan carrying report.
function planRun(report) {
	return {
		id: "run_r", tool: "terraform", command: "infra", status: "succeeded", source: "review",
		pull_request_report: report,
	};
}

test("a failing report names the pull request, the forge's answer, and the next attempt", () => {
	const page = loadPage("detail");
	page.app.renderHeader(planRun({
		pull_request: 7, phase: "running", done: false, attempts: 3,
		retry_at: new Date(Date.now() + 30000).toISOString(),
		last_error: "read the pull request's head: GET /repos/acme/infra/pulls/7 answered 502: " +
			"bad gateway",
	}));
	const callout = page.document.getElementById("run-report");
	assert.ok(!callout.hidden, "the callout is hidden while reports are failing");
	for (const want of ["pull request #7 is failing", "answered 502", "3 attempts have failed",
		"The next is in "]) {
		assert.ok(callout.textContent.includes(want),
			"the callout lacks " + JSON.stringify(want) + ": " + callout.textContent);
	}
	const header = page.document.getElementById("run-header").textContent;
	assert.ok(header.includes("#7") && header.includes("last told: plan running"),
		"the header does not name the pull request and what it was told: " + header);
});

test("a report that gave up says no more attempts will be made", () => {
	const page = loadPage("detail");
	page.app.renderHeader(planRun({
		pull_request: 7, phase: "running", done: true, attempts: 40,
		last_error: "gave up after 24h0m0s of failed attempts: GET /user answered 401: " +
			"Bad credentials",
	}));
	const callout = page.document.getElementById("run-report");
	assert.ok(!callout.hidden, "the callout is hidden after reporting gave up");
	assert.ok(callout.textContent.includes("pull request #7 stopped"),
		"the callout does not say reporting stopped: " + callout.textContent);
	assert.ok(callout.textContent.includes("No more attempts will be made"),
		"the callout does not say nothing more is tried: " + callout.textContent);
});

test("a report that landed shows what was told and no callout", () => {
	const page = loadPage("detail");
	page.app.renderHeader(planRun({
		pull_request: 7, phase: "succeeded", status_state: "success", done: true,
		reported_at: new Date().toISOString(),
	}));
	assert.ok(page.document.getElementById("run-report").hidden,
		"a report that landed raises a failure callout");
	const header = page.document.getElementById("run-header").textContent;
	assert.ok(header.includes("last told: plan succeeded"),
		"the header does not say what the pull request was told: " + header);
});

test("a run that is not a review plan says nothing about a pull request", () => {
	const page = loadPage("detail");
	page.app.renderHeader({ id: "run_p", playbook: "site.yml", status: "succeeded" });
	assert.ok(page.document.getElementById("run-report").hidden,
		"a plain run shows a pull request callout");
	assert.ok(!page.document.getElementById("run-header").textContent.includes("Pull request"),
		"a plain run shows a pull request field");
});
