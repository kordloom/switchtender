// Tests for role-aware controls. The server enforces the real policy; the page only decides
// whether a control is worth drawing. A viewer used to see approve, cancel, and launch buttons
// whose every click answered 403, and a token session, whose role the page did not know, was
// treated as admin everywhere. Unknown stays permissive, since an open install and an unscoped
// admin token both carry no role and full authority.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { sandboxOf } from "./loader.mjs";

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
