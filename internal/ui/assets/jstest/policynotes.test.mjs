// Tests for how a warning a policy noted instead of holding shows in the interface.
//
// A Rego policy set to warn: note lets the run go ahead, so nothing about the run's status says it
// was warned about. The run page, the runs list, and the policies page are where a person learns it,
// and each of them used to have nowhere to put it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";

// note is a policy note as the server records it on a run.
const note = "staging-advice (no change ticket on the run, rego sha256:0123456789ab)";

test("a run a policy noted lists the note on its page while nothing holds it", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_n", playbook: "site.yml", status: "succeeded",
		policy_notes: [
			note,
			"step \"deploy\": release-advice (an agent asked, rego sha256:abcdef012345)",
		],
	});
	const host = page.document.getElementById("run-policy-notes");
	assert.ok(!host.hidden, "the notes are hidden on a run that carries two");
	assert.ok(host.textContent.includes("no change ticket on the run"),
		"the first note is not shown: " + host.textContent);
	assert.ok(host.textContent.includes("release-advice"),
		"the step's note is not shown: " + host.textContent);
	assert.ok(host.textContent.includes("without holding the run"),
		"the callout does not say the run was not held for it: " + host.textContent);
	assert.equal(host.querySelectorAll("li").length, 2, "each note is not its own line");
	assert.ok(page.document.getElementById("risk-callout").hidden,
		"a run nothing holds shows the hold callout");
});

test("a held run shows its notes beside the hold that stopped it", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_h", playbook: "site.yml", status: "pending_approval",
		held_by_policy: "production-advice (no change ticket on the run, rego sha256:0123456789ab)",
		policy_notes: [note],
		risk: { level: "medium", reasons: [] },
	});
	assert.ok(!page.document.getElementById("risk-callout").hidden, "the hold callout is hidden");
	const host = page.document.getElementById("run-policy-notes");
	assert.ok(!host.hidden, "a held run hides the note another policy recorded");
	assert.ok(host.textContent.includes("Policy note"), "the callout has no heading: " +
		host.textContent);
});

test("a run with no notes shows no notes callout, and a later render clears an earlier one", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_c", playbook: "site.yml", status: "succeeded", policy_notes: [note],
	});
	page.app.renderHeader({ id: "run_c", playbook: "site.yml", status: "succeeded" });
	const host = page.document.getElementById("run-policy-notes");
	assert.ok(host.hidden, "the callout stayed up after the notes went away");
	assert.equal(host.textContent, "", "the callout kept the stale note: " + host.textContent);
});

test("the runs list marks a noted run and names the first note", () => {
	const page = loadPage("runs");
	const tests = [
		// Test 0: One note, named whole.
		{ In: { id: "run_1", playbook: "site.yml", policy_notes: [note] }, WantTag: true,
			WantMore: "" },
		// Test 1: Several notes say how many more there are.
		{ In: { id: "run_2", playbook: "site.yml", policy_notes: [note, "b", "c"] }, WantTag: true,
			WantMore: "and 2 more" },
		// Test 2: No notes, no mark.
		{ In: { id: "run_3", playbook: "site.yml" }, WantTag: false, WantMore: "" },
	];
	for (const [testNum, tc] of tests.entries()) {
		const cell = page.app.typeCellEl(tc.In);
		const tag = cell.querySelector(".run-kind.noted");
		assert.equal(Boolean(tag), tc.WantTag,
			`test ${testNum}: noted mark present = ${Boolean(tag)}`);
		if (!tag) continue;
		assert.ok(tag.dataset.tip.includes("no change ticket on the run"),
			`test ${testNum}: the mark does not name the note: ${tag.dataset.tip}`);
		if (tc.WantMore) {
			assert.ok(tag.dataset.tip.includes(tc.WantMore),
				`test ${testNum}: the mark does not say how many more: ${tag.dataset.tip}`);
		}
	}
});

test("the policies page says what Rego warnings do and how long one may evaluate", async () => {
	const rule = (rego) => ({
		id: "pol_file_abc", name: "advice", max_destroy: -1, created_at: "2026-10-01T12:00:00Z",
		rego: Object.assign({
			package: "data.staging", syntax: "v1",
			sha256: "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			modules: [{ file: "advice.rego", source: "package staging\n" }],
		}, rego),
	});
	const tests = [
		// Test 0: Notes, with a timeout of its own.
		{ Rego: { warn: "note", timeout: "2s" },
			Want: ["recorded on the run without holding it", "2s"] },
		// Test 1: The default holds.
		{ Rego: { warn: "hold", timeout: "500ms" }, Want: ["hold the run for approval", "500ms"] },
	];
	for (const [testNum, tc] of tests.entries()) {
		const page = loadPage("policies", {
			routes: {
				"/v1/policies": reply({ policies: [rule(tc.Rego)], count: 1 }),
				"/v1/inventories": reply({ inventories: [] }),
				"/v1/runs": reply({ runs: [] }),
			},
		});
		await page.app.loadPolicies();
		const chip = page.document.getElementById("policies").querySelector("tr").cells[1]
			.querySelector(".chip");
		for (const want of tc.Want) {
			assert.ok(chip.dataset.tip.includes(want),
				`test ${testNum}: the Rego chip's tip ${JSON.stringify(chip.dataset.tip)} ` +
				`does not say ${want}`);
		}
	}
});
