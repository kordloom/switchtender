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
import { reply } from "./net.mjs";

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

test("a workflow waiting at an approval step is decided by its step, not as a whole run", () => {
	const page = loadPage("detail");
	sandboxOf(page.app).localStorage.setItem("st_role", "admin");
	page.app.renderHeader({ id: "run_w", playbook: "release", kind: "pipeline",
		status: "pending_approval", started_at: "2026-10-05T12:00:00Z" });
	assert.equal(hidden(page.document, "approve-run"), true);
	assert.equal(hidden(page.document, "reject-run"), true);
	assert.equal(hidden(page.document, "cancel-run"), false, "a paused workflow can still be canceled");
});

test("a workflow held before it started is still decided as a whole run", () => {
	const page = loadPage("detail");
	sandboxOf(page.app).localStorage.setItem("st_role", "admin");
	page.app.renderHeader({ id: "run_w", playbook: "release", kind: "pipeline",
		status: "pending_approval" });
	assert.equal(hidden(page.document, "approve-run"), false);
	assert.equal(hidden(page.document, "reject-run"), false);
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

// shown reports whether an element is in the page and not inside anything hidden, which is as close
// to visible as a DOM without layout gets.
function shown(el) {
	return Boolean(el && el.isConnected && !el.closest("[hidden]"));
}

test("an operator or viewer on the users page is told admins manage users, and nothing is asked", async () => {
	// Every read on this page is admin only on the server, since an account carries personal data
	// and the token list names every credential that reaches the install. The page asked anyway, so
	// an operator who opened it by its address met four refusals, "Failed to load users" and "Could
	// not read the tokens" in role jargon, and New user, Issue token, and export buttons that could
	// only be refused again.
	for (const [role, holder] of [["operator", "an operator"], ["viewer", "a viewer"]]) {
		// The server's own answer to this role, so a page that still asks meets what it would meet.
		const refused = reply({ error: "forbidden" }, { status: 403 });
		const page = loadPage("users", { routes: [[/^\/v1\/(users|tokens)(\?|$)/, refused]], quiet: true });
		sandboxOf(page.app).localStorage.setItem("st_role", role);
		fire(page.document, "DOMContentLoaded");
		await page.clock.flush();
		const doc = page.document;
		const status = doc.getElementById("status");
		const said = status.textContent;
		assert.ok(shown(status), role + ": the page shows no explanation at all");
		assert.match(said, /Users and API tokens are managed by admins\./,
			role + ": the page does not say which role manages users: " + said);
		assert.match(said, new RegExp("You are signed in as " + holder + ", so you cannot"),
			role + ": the page does not say this account cannot manage them: " + said);
		assert.equal(page.net.calls.filter((c) => /^\/v1\/(users|tokens)\b/.test(c.path)).length, 0,
			role + ": the page still asked for what the server refuses this role");
		for (const id of ["user-open", "token-open", "token-list-status", "users", "tokens"]) {
			assert.ok(!shown(doc.getElementById(id)), role + ": #" + id + " is still offered");
		}
		const exports = Array.from(doc.querySelectorAll("button.table-export")).filter(shown);
		assert.equal(exports.length, 0, role + ": export buttons are offered for tables it cannot read");
	}
});

test("an admin on the users page still reads the accounts and the tokens", async () => {
	// The notice is for a role the server refuses. An admin, and a session whose role the page does
	// not know, which is an open install or an unscoped token, get the page as before.
	for (const role of ["admin", ""]) {
		const who = role || "unknown role";
		const root = { id: "user_1", username: "root", role: "admin" };
		const page = loadPage("users", {
			routes: [
				[/^\/v1\/users(\?|$)/, reply({ users: [root] })],
				[/^\/v1\/tokens(\?|$)/, reply({ tokens: [] })],
				[/^\/v1\/runs(\?|$)/, reply({ runs: [] })],
			],
			quiet: true,
		});
		if (role) sandboxOf(page.app).localStorage.setItem("st_role", role);
		fire(page.document, "DOMContentLoaded");
		await page.clock.flush();
		const doc = page.document;
		assert.ok(page.net.calledWith("/v1/users").length > 0, who + ": accounts were not read");
		assert.ok(page.net.calledWith("/v1/tokens").length > 0, who + ": tokens were not read");
		assert.equal(doc.querySelectorAll("#users tr").length, 1, who + ": the account is not listed");
		assert.ok(shown(doc.getElementById("user-open")), who + ": New user is gone");
		assert.ok(shown(doc.getElementById("token-open")), who + ": Issue token is gone");
		assert.doesNotMatch(doc.body.textContent, /managed by admins/, who + ": the page turned it away");
	}
});
