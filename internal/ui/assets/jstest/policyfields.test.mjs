// Tests for the policy dialog's round trip over the full rule vocabulary. The dialog used to know
// five fields, so editing any rule, even just renaming it, rebuilt it server-side without effect,
// actor scoping, risk floor, or destroy threshold: a deny rule for agents came back as a plain
// approval rule with no warning. Every field the API knows must survive an edit untouched.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// denyRule is a fully loaded rule: agent-scoped, risk-floored, denying, with a destroy threshold
// intentionally absent since deny rules cannot carry one.
const denyRule = {
	id: "pol_1", name: "no agent drops", tool: "bash", command_contains: "drop database",
	inventory_id: "", actor_kind: "agent", actor: "prod-remediator", min_risk: "high",
	effect: "deny", max_destroy: -1, exclude_dry_run: false, created_at: "2026-08-16T12:00:00Z",
};

// mountPolicyEdit mounts the policies page, wires the dialog, opens the rule for editing, and
// returns the page plus a recorder for the PUT body.
function mountPolicyEdit(rule) {
	const puts = [];
	const page = loadPage("policies", {
		routes: {
			"/v1/inventories": reply({ inventories: [] }),
			["/v1/policies/" + rule.id]: (req) => {
				puts.push(JSON.parse(req.body));
				return reply(rule);
			},
		},
	});
	page.app.wirePolicyForm();
	page.app.openPolicyEdit(rule);
	return { page, puts };
}

test("editing a rule fills every field the rule carries", () => {
	const { page } = mountPolicyEdit(denyRule);
	const doc = page.document;
	assert.equal(doc.getElementById("policy-effect").value, "deny");
	assert.equal(doc.getElementById("policy-actor-kind").value, "agent");
	assert.equal(doc.getElementById("policy-actor").value, "prod-remediator");
	assert.equal(doc.getElementById("policy-min-risk").value, "high");
	assert.equal(doc.getElementById("policy-max-destroy").value, "");
});

test("saving an edit carries the whole vocabulary back, so nothing is stripped", async () => {
	const { page, puts } = mountPolicyEdit(denyRule);
	fire(page.document.getElementById("policy-form"), "submit");
	await page.clock.flush();
	assert.equal(puts.length, 1, "the edit saved once");
	const sent = puts[0];
	assert.equal(sent.effect, "deny");
	assert.equal(sent.actor_kind, "agent");
	assert.equal(sent.actor, "prod-remediator");
	assert.equal(sent.min_risk, "high");
	assert.equal(sent.name, "no agent drops");
	assert.equal(sent.command_contains, "drop database");
	assert.ok(!("max_destroy" in sent), "an empty threshold stays absent so the check stays off");
});

test("a plan-content threshold survives the edit round trip as a number", async () => {
	const gated = { ...denyRule, id: "pol_2", name: "big teardown", effect: "", actor_kind: "",
		actor: "", min_risk: "", max_destroy: 5 };
	const { page, puts } = mountPolicyEdit(gated);
	assert.equal(page.document.getElementById("policy-max-destroy").value, "5");
	fire(page.document.getElementById("policy-form"), "submit");
	await page.clock.flush();
	assert.equal(puts.length, 1);
	assert.equal(puts[0].max_destroy, 5);
	assert.equal(puts[0].effect, "");
});
