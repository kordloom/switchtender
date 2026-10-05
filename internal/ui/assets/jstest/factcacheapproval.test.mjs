// Tests for whether a held run that serves cached facts says so where the decision is made.
//
// A cached fact stands in for one the play would have gathered, so a play that branches on a fact
// can act on a host as it was when the facts were cached. The approval binds the setting, and the
// approver has to be able to see what they bind.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

test("a held run that serves cached facts states the setting and its timeout", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_fc", playbook: "site.yml", status: "pending_approval",
		use_fact_cache: true, fact_cache_timeout: 600, held_by_policy: "prod changes",
		risk: { level: "low", reasons: [] },
	});
	const callout = page.document.getElementById("risk-callout");
	assert.ok(!callout.hidden, "the callout is hidden on a held run");
	assert.ok(callout.textContent.includes("uses cached facts (timeout 600s)"),
		"the callout does not state the fact cache: " + callout.textContent);
	assert.ok(callout.textContent.includes("binds this setting"),
		"the callout does not say the approval binds the setting: " + callout.textContent);
	const header = page.document.getElementById("run-header");
	assert.ok(header.textContent.includes("uses cached facts (timeout 600s)"),
		"the run header does not state the fact cache: " + header.textContent);
});

test("a fact cache with no timeout says so rather than naming a zero", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_fc0", playbook: "site.yml", status: "pending_approval", use_fact_cache: true,
		risk: { level: "low", reasons: [] },
	});
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.textContent.includes("uses cached facts (no timeout)"),
		"the callout does not say facts of any age are served: " + callout.textContent);
});

test("a held run without the cache says nothing about cached facts", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_plain", playbook: "site.yml", status: "pending_approval", fact_cache_timeout: 600,
		risk: { level: "low", reasons: [] },
	});
	const callout = page.document.getElementById("risk-callout");
	const header = page.document.getElementById("run-header");
	assert.ok(!callout.textContent.includes("cached facts"),
		"a run without the cache is described as using it: " + callout.textContent);
	assert.ok(!header.textContent.includes("cached facts"),
		"a run without the cache has a fact cache field: " + header.textContent);
});
