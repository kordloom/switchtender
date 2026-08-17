// Tests for separation of duties in the interface: a rule can ask for a second person, and a run
// held by such a rule does not offer its requester an Approve button whose only future is a refusal.
import { test } from "node:test";
import assert from "node:assert/strict";

import { sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";
import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";

test("the policy dialog can require a second person, and carries it on an edit", async () => {
	const page = loadPage("policies", {
		routes: {
			"/v1/inventories": reply({ inventories: [] }),
			"/v1/policies": reply({ policies: [] }),
		},
	});
	page.app.wirePolicyForm();
	const box = page.document.getElementById("policy-distinct-approver");
	assert.ok(box, "the policy dialog cannot express separation of duties at all");

	// Editing an existing rule shows the requirement it already carries, rather than resetting it.
	page.app.openPolicyEdit({ id: "pol_1", name: "prod", require_distinct_approver: true });
	assert.equal(box.checked, true, "an edit drops the rule's distinct-approver requirement");

	// A save sends the field whether it is on or off: the update handler rebuilds the policy whole,
	// so an omitted field silently turns the control off.
	page.document.getElementById("policy-name").value = "prod";
	fire(page.document.getElementById("policy-form"), "submit");
	await page.clock.flush();
	const put = page.net.calls.find((c) => c.method === "PUT");
	assert.ok(put, "the edit was never sent");
	assert.equal(JSON.parse(put.body).require_distinct_approver, true,
		"the saved rule does not carry the requirement the dialog showed");
});

test("a run you asked for does not offer you its Approve button", async () => {
	const held = {
		id: "run_1", status: "pending_approval", actor: "casey", actor_type: "session",
		held_by_policy: "production apply", require_distinct_approver: true,
		created_at: "2026-03-01T12:00:00Z", tool: "bash", command: "deploy",
	};
	const page = loadPage("detail", {
		vars: { RunID: "run_1" },
		routes: {
			"/v1/runs/run_1": reply(held),
			"/v1/runs/run_1/events": reply({ events: [] }),
			"/v1/auth/me": reply({ name: "casey", role: "admin", actor_type: "session" }),
		},
	});
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_role", "admin");
	win.localStorage.setItem("st_user", "casey");
	page.app.loadDetail("run_1");
	await page.clock.flush();

	const approve = page.document.getElementById("approve-run");
	assert.equal(approve.hidden, true,
		"the requester is offered an Approve button that the server will refuse");
	// Rejecting your own request is allowed, and withdrawing it is the requester's only way out.
	assert.equal(page.document.getElementById("reject-run").hidden, false,
		"the requester cannot withdraw their own request");
	assert.match(page.document.getElementById("status").textContent, /another person|second person|different person/i,
		"nothing explains why approving is unavailable");
});

test("someone else's held run still offers Approve", async () => {
	const held = {
		id: "run_2", status: "pending_approval", actor: "dana", actor_type: "session",
		held_by_policy: "production apply", require_distinct_approver: true,
		created_at: "2026-03-01T12:00:00Z", tool: "bash", command: "deploy",
	};
	const page = loadPage("detail", {
		vars: { RunID: "run_2" },
		routes: {
			"/v1/runs/run_2": reply(held),
			"/v1/runs/run_2/events": reply({ events: [] }),
			"/v1/auth/me": reply({ name: "casey", role: "admin", actor_type: "session" }),
		},
	});
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_role", "admin");
	win.localStorage.setItem("st_user", "casey");
	page.app.loadDetail("run_2");
	await page.clock.flush();

	assert.equal(page.document.getElementById("approve-run").hidden, false,
		"an approver can no longer release someone else's held run");
});
