// Tests for how the interface shows the built-in agent hold and the exemption from it. A run an
// agent asks for is held by default, so the hold has to read as a reason rather than as a rule
// nobody can find, and an exemption has to survive an edit and read as what it is in the policy
// list. Mapped to the default effect, an edited exemption came back as a rule holding the very runs
// it was written to let through.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// exemption is a stored exemption for one named agent on one queue.
const exemption = {
	id: "pol_ex", name: "nightly smoke", tool: "bash", command_contains: "smoke", inventory_id: "",
	queue: "staging", actor_kind: "", actor: "release-agent", min_risk: "", effect: "exempt",
	max_destroy: -1, exclude_dry_run: false, created_at: "2026-10-05T12:00:00Z",
};

// hold is a plain rule holding every bash run.
const hold = { ...exemption, id: "pol_hold", name: "hold bash", effect: "", actor: "", queue: "",
	command_contains: "" };

test("an exemption survives the edit round trip as an exemption", async () => {
	const tests = [
		// Test 0: An exemption reads back and saves as exempt.
		{ Rule: exemption, WantEffect: "exempt" },
		// Test 1: A plain rule still reads back and saves as the default, the control.
		{ Rule: hold, WantEffect: "" },
	];
	for (const [testNum, tc] of tests.entries()) {
		const puts = [];
		const page = loadPage("policies", {
			routes: {
				"/v1/inventories": reply({ inventories: [] }),
				["/v1/policies/" + tc.Rule.id]: (req) => {
					puts.push(JSON.parse(req.body));
					return reply(tc.Rule);
				},
			},
		});
		page.app.wirePolicyForm();
		page.app.openPolicyEdit(tc.Rule);
		assert.equal(page.document.getElementById("policy-effect").value, tc.WantEffect,
			`test ${testNum}: the dialog read the effect back wrong`);
		fire(page.document.getElementById("policy-form"), "submit");
		await page.clock.flush();
		assert.equal(puts.length, 1, `test ${testNum}: the edit saved once`);
		assert.equal(puts[0].effect, tc.WantEffect, `test ${testNum}: the edit saved another effect`);
	}
});

test("the policy list names an exemption as one, not as a hold", async () => {
	const page = loadPage("policies", {
		routes: {
			"/v1/policies": reply({ policies: [exemption, hold], count: 2 }),
			"/v1/inventories": reply({ inventories: [] }),
			"/v1/runs": reply({ runs: [] }),
		},
	});
	await page.app.loadPolicies();
	const rows = page.document.getElementById("policies").querySelectorAll("tr");
	assert.equal(rows[0].cells[1].textContent, "exempt");
	assert.ok(rows[0].cells[1].querySelector(".chip").dataset.tip.includes("default hold"));
	assert.equal(rows[1].cells[1].textContent, "hold", "the plain rule still reads as a hold");
});

test("the held label gives the built-in hold as a reason and quotes a named rule", () => {
	const page = loadPage("policies", { routes: {} });
	const tests = [
		// Test 0: The built-in agent hold reads as the reason it is.
		{ Rule: "requested by an agent, held by default",
			Want: "Held for approval: requested by an agent, held by default" },
		// Test 1: A stored rule is quoted by name, the control.
		{ Rule: "hold bash", Want: "Held for approval by \"hold bash\"" },
		// Test 2: Nothing named.
		{ Rule: "", Want: "Held for approval" },
	];
	for (const [testNum, tc] of tests.entries()) {
		assert.equal(page.app.heldLabel(tc.Rule), tc.Want, `test ${testNum}`);
	}
});

test("the empty policy list says agent runs are already held", async () => {
	const page = loadPage("policies", {
		routes: {
			"/v1/policies": reply({ policies: [], count: 0 }),
			"/v1/inventories": reply({ inventories: [] }),
			"/v1/runs": reply({ runs: [] }),
		},
	});
	await page.app.loadPolicies();
	assert.ok(page.document.body.textContent.includes("held for approval by default"),
		"the empty state does not say an agent's run is held by default");
});

// heldNote and exemptNote are what the server records on an agent's run about the built-in hold.
const heldNote = "requested by an agent, held by default";
const exemptNote =
	"requested by an agent, exempt from the default hold by policy \"nightly smoke\"";

test("the runs list does not mark an agent's run as noted for the agent hold", () => {
	const page = loadPage("runs");
	const tests = [
		// Test 0: The hold's own record is not a warning.
		{ In: { id: "run_1", playbook: "site.yml", policy_notes: [heldNote] }, WantTag: false },
		// Test 1: Neither is an exemption's.
		{ In: { id: "run_2", playbook: "site.yml", policy_notes: [exemptNote] }, WantTag: false },
		// Test 2: A policy's warning beside it still marks the run, the control.
		{ In: { id: "run_3", playbook: "site.yml", policy_notes: [exemptNote, "advice (x)"] },
			WantTag: true },
	];
	for (const [testNum, tc] of tests.entries()) {
		const tag = page.app.typeCellEl(tc.In).querySelector(".run-kind.noted");
		assert.equal(Boolean(tag), tc.WantTag, `test ${testNum}: noted mark present`);
		if (tag) {
			assert.ok(!tag.dataset.tip.includes("requested by an agent"),
				`test ${testNum}: the mark names the agent hold: ${tag.dataset.tip}`);
		}
	}
});

test("the run page shows the agent hold apart from policy warnings", () => {
	const tests = [
		// Test 0: An exempt run names the exemption and is not called a warning.
		{ Notes: [exemptNote], WantAgent: true, WantWarning: false },
		// Test 1: A run with only a policy's warning keeps the warning callout, the control.
		{ Notes: ["advice (x)"], WantAgent: false, WantWarning: true },
		// Test 2: Both show, each under its own heading.
		{ Notes: [heldNote, "advice (x)"], WantAgent: true, WantWarning: true },
	];
	for (const [testNum, tc] of tests.entries()) {
		const page = loadPage("detail");
		page.app.renderHeader({ id: "run_a", playbook: "site.yml", status: "succeeded",
			policy_notes: tc.Notes });
		const host = page.document.getElementById("run-policy-notes");
		assert.ok(!host.hidden, `test ${testNum}: the notes are hidden`);
		const text = host.textContent;
		assert.equal(text.includes("Agent hold"), tc.WantAgent, `test ${testNum}: ${text}`);
		assert.equal(text.includes("without holding the run"), tc.WantWarning,
			`test ${testNum}: ${text}`);
		for (const note of tc.Notes) {
			assert.ok(text.includes(note), `test ${testNum}: the note ${note} is not shown`);
		}
	}
});
