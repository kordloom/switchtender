// Tests that every paid control in the app says so before it is used, which is the consistency rule
// the operator set: if a feature is gated, the product must say so, everywhere, the same way.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// TestPolicyDialogMarksItsTeamFields pins the five inputs whose use makes a rule Team.
//
// markTier was added for the evidence pack and the drift reconcile and then not applied here, so
// the policy dialog offered five Team-only controls with nothing marking them. A Community reader
// chose Deny, scoped the rule to an agent, set a risk floor, pressed Save, and learned the tier
// from a 403 after composing the whole rule: the same shape of refusal markTier exists to prevent.
test("the policy dialog marks every field that makes a rule Team", async () => {
	const { app, document, clock } = loadPage("policies", {
		parts: ALL_PARTS,
		routes: {
			"/v1/policies": reply({ policies: [] }),
			"/v1/inventories": reply({ inventories: [] }),
		},
	});
	app.wirePolicyForm();
	await clock.flush();

	// Exactly the set policy.Advanced() tests: deny, actor kind, named actor, risk floor, and
	// distinct approver.
	for (const id of ["policy-effect", "policy-actor-kind", "policy-actor", "policy-min-risk",
		"policy-distinct-approver"]) {
		const el = document.getElementById(id);
		assert.ok(el, id + " is missing from the dialog");
		const label = el.closest(".field-label") || el.parentElement;
		assert.ok(label.querySelector(".tier-tag"),
			id + " is Team-gated but carries no tier marker, so a Community reader meets the gate " +
			"as a refusal after composing the rule");
	}

	// A field that is free must not be marked, or the marker stops meaning anything.
	const free = document.getElementById("policy-name");
	const freeLabel = free.closest(".field-label") || free.parentElement;
	assert.equal(freeLabel.querySelector(".tier-tag"), null,
		"a field that is not gated was marked Team, which makes every other marker untrustworthy");
});
