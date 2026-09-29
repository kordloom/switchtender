// Tests for edits and dialogs that silently dropped what they did not render, and for a filter that
// emptied the table it was filtering. Each of these was proven against the real templates: a facet
// tick that hid every row while claiming matches, a New dialog that kept the previous record's
// cloud credentials, and a template edit that stripped the container image off a non-Ansible run.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// auditEntries builds n entries, the last `deletes` of them DELETE, so a facet on Method selects
// rows that all sit past the first page.
function auditEntries(n, deletes) {
	const out = [];
	for (let i = 0; i < n; i++) {
		const isDelete = i >= n - deletes;
		out.push({
			seq: n - i, at: "2026-08-17T12:00:00Z", actor: "root", actor_type: "session",
			method: isDelete ? "DELETE" : "POST",
			path: isDelete ? "/v1/credentials/cred_" + i : "/v1/runs",
			hash: "abcdef0123456789",
		});
	}
	return out;
}

test("a facet filter shows the rows it matched instead of emptying the table", async () => {
	const page = loadPage("audit", {
		routes: { "/v1/audit": reply({ entries: auditEntries(40, 10), count: 40 }) },
	});
	await page.app.loadAudit();
	page.app.mountTablePager();
	const visible = () => [...page.document.getElementById("audit").querySelectorAll("tr")]
		.filter((tr) => !tr.hidden).length;
	assert.ok(visible() > 0, "the unfiltered table renders rows");

	// Hide every POST row, the way ticking DELETE in the Method facet does.
	for (const tr of page.document.getElementById("audit").querySelectorAll("tr")) {
		tr.dataset.xhide = tr.textContent.includes("DELETE") ? "" : "1";
	}
	const table = page.document.querySelector("main.content table");
	fire(table, "rowsfiltered");
	assert.equal(visible(), 10,
		"the facet hid every row: a filter that reports matches must show them");
});

test("New inventory clears the cloud credential fields the previous record left behind", () => {
	const page = loadPage("inventories", {
		routes: {
			"/v1/credentials": reply({ credentials: [] }),
			"/v1/inventories": reply({ inventories: [] }),
		},
	});
	page.app.wireModal("inventory");
	page.app.wireInventoryForm();
	const doc = page.document;
	// A previous AWS-sourced inventory left its secret id and static keys in the form.
	for (const [id, value] of [["inv-aws-secret-id", "prod/fleet-inventory"],
		["inv-aws-access-key", "AKIAEXAMPLE"], ["inv-aws-secret-key", "s3cr3t-access-key"]]) {
		const el = doc.getElementById(id);
		assert.ok(el, "the dialog declares " + id);
		el.value = value;
	}
	fire(doc.getElementById("inventory-open"), "click");
	for (const id of ["inv-aws-secret-id", "inv-aws-access-key", "inv-aws-secret-key"]) {
		assert.equal(doc.getElementById(id).value, "",
			id + " still holds the previous record's value in a dialog titled Add an inventory");
	}
});

test("a non-Ansible template keeps its execution image through an edit", async () => {
	const stored = {
		id: "tpl_tf", name: "infra", tool: "terraform", command: "infra/prod",
		image: "ghcr.io/kordloom/tf:1.9", pull_credential_id: "cred_reg",
		created_at: "2026-08-17T12:00:00Z",
	};
	const page = loadPage("jobtemplates", {
		routes: {
			"/v1/templates": reply({ templates: [stored], count: 1 }),
			"/v1/projects": reply({ projects: [] }),
			"/v1/credentials": reply({ credentials: [] }),
			"/v1/inventories": reply({ inventories: [] }),
			"/v1/schedules": reply({ schedules: [] }),
			"/v1/triggers": reply({ triggers: [] }),
		},
	});
	page.app.wireTemplateForm();
	page.app.openTemplateEdit(stored);
	// The image field must be reachable for a non-Ansible tool, not hidden as an Ansible concern.
	assert.equal(page.document.getElementById("tpl-field-image").hidden, false,
		"the execution image is hidden for a non-Ansible template, so nobody can see or keep it");
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();
	const puts = page.net.calls.filter((c) => c.method === "PUT" && c.url.includes("/v1/templates/"));
	assert.equal(puts.length, 1, "the edit saved once; status: " +
		page.document.getElementById("tpl-status").textContent);
	const sent = JSON.parse(puts[0].body);
	assert.equal(sent.image, "ghcr.io/kordloom/tf:1.9",
		"the edit stripped the container image, so the next run executes on the host");
	assert.equal(sent.pull_credential_id, "cred_reg");
});
