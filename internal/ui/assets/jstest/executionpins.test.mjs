// Tests for whether a held run says, where the decision is made, what the approval binds about the
// image it runs in, the inventory it runs against, and the plan file a gated apply carries out.
//
// An approver releases exactly what they were shown. An image held only to a tag, an inventory that
// is a dynamic source, and an apply that carries out a saved plan each change what that means, so
// the approver has to be able to read each one.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

const digest = "sha256:" + "a".repeat(64);

// held returns a held run with fields merged over a minimal one.
function held(fields) {
	return Object.assign({
		id: "run_pins", playbook: "site.yml", status: "pending_approval", held_by_policy: "prod",
		risk: { level: "low", reasons: [] },
	}, fields);
}

test("an image held to a tag alone says it is not pinned to a digest", () => {
	const page = loadPage("detail");
	page.app.renderHeader(held({ image: "registry.example.com/runner:2" }));
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.textContent.includes("tag not pinned to a digest"),
		"the callout does not say the tag is not pinned: " + callout.textContent);
	const header = page.document.getElementById("run-header");
	assert.ok(header.textContent.includes("tag not pinned to a digest"),
		"the header does not say the tag is not pinned: " + header.textContent);
});

test("an image pinned to a digest says so and raises no warning", () => {
	const page = loadPage("detail");
	page.app.renderHeader(held({ image: "registry.example.com/runner:2@" + digest }));
	const header = page.document.getElementById("run-header");
	assert.ok(header.textContent.includes("pinned to sha256:aaaaaaaaaaaa"),
		"the header does not show the pinned digest: " + header.textContent);
	const callout = page.document.getElementById("risk-callout");
	assert.ok(!callout.textContent.includes("not pinned"),
		"a pinned image is described as unpinned: " + callout.textContent);
});

test("a dynamic inventory source says its hosts resolve at execution", () => {
	const page = loadPage("detail");
	page.app.renderHeader(held({ inventory_id: "inv_1",
		inventory_snapshot: { sealed_sha256: "s", content_sha256: "c", dynamic: true } }));
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.textContent.includes("hosts resolve at execution"),
		"the callout does not say a dynamic source resolves at execution: " + callout.textContent);
	const header = page.document.getElementById("run-header");
	assert.ok(header.textContent.includes("dynamic source: hosts resolve at execution"),
		"the header does not say a dynamic source resolves at execution: " + header.textContent);
});

test("a static inventory snapshot names the hosts the approval binds", () => {
	const page = loadPage("detail");
	page.app.renderHeader(held({ inventory_id: "inv_1",
		inventory_snapshot: { sealed_sha256: "s", content_sha256: "c", hosts: ["web1", "web2"] } }));
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.textContent.includes("2 hosts: web1, web2"),
		"the callout does not name the snapshot's hosts: " + callout.textContent);
	assert.ok(callout.textContent.includes("does not reach it"),
		"the callout does not say a later edit does not reach the run: " + callout.textContent);
});

test("a gated apply says it carries out the saved plan", () => {
	const page = loadPage("detail");
	page.app.renderHeader(held({ tool: "terraform", command: "infra", plan_sha256: "b".repeat(64) }));
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.textContent.includes("runs that plan rather than planning again"),
		"the callout does not say the apply carries out the saved plan: " + callout.textContent);
	const header = page.document.getElementById("run-header");
	assert.ok(header.textContent.includes("Plan file"),
		"the header does not show the plan file: " + header.textContent);
});
