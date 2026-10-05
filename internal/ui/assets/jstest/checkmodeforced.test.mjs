// Tests for whether a held run the gate did not find change free says so where the decision is
// made.
//
// A dry run reads as harmless. Ansible runs any play, block, task, role, or include that sets
// check_mode to false for real even under --check, and Terraform runs the program an external data
// source names while it plans, so an approver shown only "dry run" would release a change believing
// it was a preview.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

test("a held dry run names the work its playbook runs for real", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_f", playbook: "site.yml", status: "pending_approval", dry_run: true,
		held_by_policy: "prod changes",
		hold_note: "This dry run was not shown to change nothing, so \"prod changes\" does not " +
			"exempt it. Two clean fixes: rework the task so check mode is safe, or drop " +
			"exclude_dry_run from \"prod changes\".",
		dry_run_scans: [{
			tool: "ansible", scanner: "ansible-check-mode", version: 1,
			inputs: ["roles/db/tasks/main.yml", "site.yml"],
			findings: [
				"site.yml: task \"Restart web\" sets check_mode to false",
				"roles/db/tasks/main.yml: block \"Migrate\" sets check_mode to \"no\"",
			],
			classification: "not_change_free",
		}],
		risk: { level: "low", reasons: ["dry run that still runs work for real under check mode"] },
	});
	const callout = page.document.getElementById("risk-callout");
	assert.ok(!callout.hidden, "the callout is hidden on a held run");
	assert.ok(callout.textContent.includes("not a preview"),
		"the callout does not say the dry run is not a preview: " + callout.textContent);
	assert.ok(callout.textContent.includes("Restart web"),
		"the forcing task is not named: " + callout.textContent);
	assert.ok(callout.textContent.includes("roles/db/tasks/main.yml"),
		"the file holding the forcing block is not named: " + callout.textContent);
	assert.ok(callout.textContent.includes("Two clean fixes"),
		"the hold note with its fixes is not shown: " + callout.textContent);
	assert.ok(callout.textContent.includes("ansible-check-mode version 1, 2 files"),
		"what the scan read is not shown: " + callout.textContent);
});

test("a held plan names the external data source by its address", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_p", tool: "terraform", command: "infra", status: "pending_approval", dry_run: true,
		held_by_policy: "prod plans",
		dry_run_scans: [{
			tool: "terraform", scanner: "terraform-external", version: 1, inputs: ["infra/main.tf"],
			source: "read at commit 0123456789ab, the project's last synced commit",
			findings: ["module.net.data.external.lookup runs a program during plan " +
				"(infra/.terraform/modules/net/main.tf line 3)"],
			fetch: { command: "terraform get", exit_status: 0 },
			classification: "not_change_free",
		}],
		risk: { level: "medium", reasons: ["plan that may run a program while it plans"] },
	});
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.textContent.includes("Planning it runs these programs"),
		"the callout does not say the plan runs programs: " + callout.textContent);
	assert.ok(callout.textContent.includes("module.net.data.external.lookup"),
		"the address is not named: " + callout.textContent);
	assert.ok(callout.textContent.includes("read at commit 0123456789ab"),
		"the commit the scan read is not shown: " + callout.textContent);
	assert.ok(callout.textContent.includes("modules downloaded first by the gate's terraform get"),
		"the module download is not shown: " + callout.textContent);
	assert.ok(callout.textContent.includes("not change free."),
		"the classification is not shown: " + callout.textContent);
});

test("a held plan the gate could not read in full says what it could not read", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_i", tool: "opentofu", command: "infra", status: "pending_approval", dry_run: true,
		dry_run_scans: [{
			tool: "opentofu", scanner: "terraform-external", version: 1, inputs: ["infra/main.tf"],
			unread: ["module.vpc from \"acme/vpc/aws\" (not downloaded, since the gate's tofu get " +
				"failed with exit status 1: Error: Module not found)"],
			fetch: { command: "tofu get", exit_status: 1,
				error: "the gate's tofu get failed with exit status 1: Error: Module not found" },
			classification: "incomplete",
		}],
		risk: { level: "medium", reasons: [] },
	});
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.textContent.includes("could not read all of its configuration"),
		"the callout does not say the scan was incomplete: " + callout.textContent);
	assert.ok(callout.textContent.includes("could not read module.vpc"),
		"the unread module is not named: " + callout.textContent);
	assert.ok(callout.textContent.includes("Error: Module not found"),
		"why the download failed is not shown: " + callout.textContent);
	assert.ok(callout.textContent.includes("the gate's tofu get did not download its modules " +
		"(exit status 1)"), "the failed download is not shown: " + callout.textContent);
});

test("a held dry run that forces nothing says nothing about forcing", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_c", playbook: "site.yml", status: "pending_approval", dry_run: true,
		dry_run_scans: [{ tool: "ansible", scanner: "ansible-check-mode", version: 1,
			inputs: ["site.yml"], classification: "change_free" }],
		risk: { level: "low", reasons: ["dry run, makes no changes"] },
	});
	const callout = page.document.getElementById("risk-callout");
	assert.ok(!callout.textContent.includes("not a preview"),
		"a clean dry run is described as forcing real work: " + callout.textContent);
});
