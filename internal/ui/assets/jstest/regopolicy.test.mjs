// Tests for how the policies page shows a Rego policy. A Rego policy carries no criteria of its own,
// so drawn like any other rule it read "any" in every column, which describes a rule that holds
// every run, the opposite of what a reader needs to know about a policy that decides in code.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";

// regoRule is a Rego policy as the API lists it.
const regoRule = {
	id: "pol_file_abc", name: "guardrails", max_destroy: -1, created_at: "2026-10-01T12:00:00Z",
	rego: {
		package: "data.switchtender", syntax: "v1",
		sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		modules: [{ file: "guardrails.rego", source: "package switchtender\n" }],
	},
};

// mountRegoPolicies loads the policies page listing regoRule beside the held runs given.
async function mountRegoPolicies(held) {
	const page = loadPage("policies", {
		routes: {
			"/v1/policies": reply({ policies: [regoRule], count: 1 }),
			"/v1/inventories": reply({ inventories: [] }),
			"/v1/runs": reply({ runs: held }),
		},
	});
	await page.app.loadPolicies();
	return page;
}

test("a Rego policy names its package instead of reading any in every column", async () => {
	const page = await mountRegoPolicies([]);
	const table = page.document.querySelector("main.content table");
	const rows = page.document.getElementById("policies").querySelectorAll("tr");
	assert.equal(rows.length, 1, "the Rego policy drew no row");
	const cells = rows[0].cells;
	assert.equal(cells.length, table.tHead.rows[0].cells.length,
		"the Rego row does not line up with the header");
	assert.equal(cells[1].textContent, "rego", "the effect column does not say Rego decides");
	assert.equal(cells[2].textContent, "data.switchtender", "the package is not named");
	for (let i = 3; i <= 10; i++) {
		assert.equal(cells[i].textContent, "in rego",
			`column ${i} reads "${cells[i].textContent}", which describes a criterion the policy lacks`);
	}
});

test("a run a Rego policy holds is counted against that policy", async () => {
	const page = await mountRegoPolicies([{
		id: "run_1", status: "pending_approval",
		held_by_policy: "guardrails (terraform needs a person, rego sha256:0123456789ab)",
	}]);
	const row = page.document.getElementById("policies").querySelector("tr");
	assert.match(row.textContent, /1 run waiting/,
		"the held run was not counted against the Rego policy that holds it");
});
