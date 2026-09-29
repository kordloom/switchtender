// Tests for role-aware controls. The server enforces the real policy; the page only decides
// whether a control is worth drawing. A viewer used to see approve, cancel, and launch buttons
// whose every click answered 403, and a token session, whose role the page did not know, was
// treated as admin everywhere. Unknown stays permissive, since an open install and an unscoped
// admin token both carry no role and full authority.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { sandboxOf } from "./loader.mjs";
import { fire } from "./dom.mjs";

// heldRun is a run waiting for a decision, the state with the most role-gated controls.
const heldRun = { id: "run_h", playbook: "site.yml", status: "pending_approval" };

// mountDetailAs mounts the detail page with the given stored role and renders the held run's
// action row.
function mountDetailAs(role) {
	const page = loadPage("detail");
	if (role) sandboxOf(page.app).localStorage.setItem("st_role", role);
	page.app.renderHeader(heldRun);
	return page.document;
}

// hidden reads a control's hidden state by id.
function hidden(doc, id) {
	return doc.getElementById(id).hidden;
}

test("a viewer sees no approve, reject, or cancel on a held run", () => {
	const doc = mountDetailAs("viewer");
	assert.equal(hidden(doc, "approve-run"), true);
	assert.equal(hidden(doc, "reject-run"), true);
	assert.equal(hidden(doc, "cancel-run"), true);
});

test("an operator can cancel a held run but cannot decide it", () => {
	const doc = mountDetailAs("operator");
	assert.equal(hidden(doc, "cancel-run"), false);
	assert.equal(hidden(doc, "approve-run"), true);
	assert.equal(hidden(doc, "reject-run"), true);
});

test("an admin gets the decision controls", () => {
	const doc = mountDetailAs("admin");
	assert.equal(hidden(doc, "approve-run"), false);
	assert.equal(hidden(doc, "reject-run"), false);
	assert.equal(hidden(doc, "cancel-run"), false);
});

test("an unknown role gates nothing, for open installs and unscoped tokens", () => {
	const doc = mountDetailAs("");
	assert.equal(hidden(doc, "approve-run"), false);
	assert.equal(hidden(doc, "cancel-run"), false);
});

test("roleAtLeast ranks the three roles and lets unknown through", () => {
	const page = loadPage("detail");
	const store = sandboxOf(page.app).localStorage;
	assert.equal(page.app.roleAtLeast("admin"), true, "unknown role must not gate");
	store.setItem("st_role", "viewer");
	assert.equal(page.app.roleAtLeast("operator"), false);
	assert.equal(page.app.roleAtLeast("viewer"), true);
	store.setItem("st_role", "operator");
	assert.equal(page.app.roleAtLeast("operator"), true);
	assert.equal(page.app.roleAtLeast("admin"), false);
});

// mountDownloadsAs mounts the detail page as the given role and user, wires its download controls,
// and offers the run's own evidence the way the page does once the run is read.
function mountDownloadsAs(role, user, run) {
	const page = loadPage("detail");
	const store = sandboxOf(page.app).localStorage;
	if (role) store.setItem("st_role", role);
	if (user) store.setItem("st_user", user);
	page.app.wireRunDownloads(run.id);
	page.app.offerOwnEvidence(run);
	return page.document;
}

test("the operator who launched a run can reach its evidence and receipt", () => {
	// The server serves both to an admin or to the actor who launched the run. The page drew them
	// for admins only, so an operator could not reach the evidence for their own change.
	const doc = mountDownloadsAs("operator", "drew", { id: "run_1", actor: "drew" });
	assert.equal(hidden(doc, "export-evidence"), false, "the launcher cannot see Evidence");
	assert.equal(hidden(doc, "download-receipt"), false, "the launcher cannot see Download receipt");
});

test("another operator does not get controls the server would refuse", () => {
	const doc = mountDownloadsAs("operator", "sam", { id: "run_1", actor: "drew" });
	assert.equal(hidden(doc, "export-evidence"), true);
	assert.equal(hidden(doc, "download-receipt"), true);
});

test("an admin gets the evidence controls for any run", () => {
	const doc = mountDownloadsAs("admin", "root", { id: "run_1", actor: "drew" });
	assert.equal(hidden(doc, "export-evidence"), false);
	assert.equal(hidden(doc, "download-receipt"), false);
});

test("an operator on the audit page is told the trail is for admins, not that verification failed", async () => {
	// The page verified on open for every session, and the server refuses a non-admin, so an
	// operator arriving here saw a red "Verify failed: forbidden" badge, which reads as a broken
	// chain.
	const page = loadPage("audit");
	sandboxOf(page.app).localStorage.setItem("st_role", "operator");
	fire(page.document, "DOMContentLoaded");
	await page.clock.flush();
	const badge = page.document.getElementById("audit-badge");
	assert.equal(badge.hidden, true, "the verify badge is shown: " + badge.textContent);
	assert.doesNotMatch(badge.textContent, /failed/i);
	assert.match(page.document.getElementById("status").textContent, /readable by admins/);
	for (const id of ["audit-verify", "audit-bundle", "audit-register"]) {
		assert.equal(hidden(page.document, id), true, id + " is offered to a session the server refuses");
	}
	assert.equal(page.net.calls.filter((c) => c.url.includes("/audit")).length, 0,
		"the page still asked the server for the audit trail");
});
