// Tests that every row a list page renders has exactly as many cells as its table declares
// headers. A mismatch is the classic invisible regression: adding a column to a template without
// adding its cell to the loader, or the reverse, shifts every value one column sideways and the
// page still looks like a table. These pages carry the widest tables, so they are where drift
// happens first.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";

// PAGES lists each page's loader, the tbody it fills, and the endpoints that loader reads. The
// harness fails loudly on a request no route claims, so this table also documents what each page
// actually asks the server for.
const PAGES = [
	{
		page: "runs", loader: "loadRuns", body: "runs",
		routes: {
			"/v1/runs": reply({
				runs: [{
					id: "run_1", playbook: "site.yml", inventory: "prod", status: "succeeded",
					created_at: "2026-08-17T12:00:00Z", started_at: "2026-08-17T12:00:00Z",
					ended_at: "2026-08-17T12:01:00Z", source: "api", actor: "operator-jane",
					labels: { env: "prod" },
				}],
				count: 1, summary: { total: 1, succeeded: 1 },
			}),
		},
	},
	{
		page: "audit", loader: "loadAudit", body: "audit",
		routes: {
			"/v1/audit": reply({
				entries: [{
					seq: 2, at: "2026-08-17T12:00:00Z", actor: "approver-pat", actor_type: "session",
					on_behalf_of: "", method: "DECISION", path: "/runs/run_1/decision/approved",
					content_digest: "sha256s:aa:bb", hash: "abcdef0123456789", prev_hash: "0",
				}],
				count: 1,
			}),
		},
	},
	{
		page: "policies", loader: "loadPolicies", body: "policies",
		routes: {
			"/v1/policies": reply({
				policies: [{
					id: "pol_1", name: "no agent drops", tool: "bash",
					command_contains: "drop database", inventory_id: "", actor_kind: "agent",
					actor: "", min_risk: "high", effect: "deny", max_destroy: -1,
					exclude_dry_run: false, created_at: "2026-08-17T12:00:00Z",
				}],
				count: 1,
			}),
			"/v1/inventories": reply({ inventories: [] }),
			"/v1/runs": reply({ runs: [] }),
		},
	},
	{
		page: "jobtemplates", loader: "loadTemplates", body: "templates",
		routes: {
			"/v1/templates": reply({
				templates: [{
					id: "tpl_1", name: "deploy", playbook: "site.yml", inventory: "prod",
					tool: "ansible", created_at: "2026-08-17T12:00:00Z",
				}],
				count: 1,
			}),
			"/v1/inventories": reply({ inventories: [] }),
			"/v1/credentials": reply({ credentials: [] }),
			"/v1/projects": reply({ projects: [] }),
			"/v1/schedules": reply({ schedules: [] }),
			"/v1/triggers": reply({ triggers: [] }),
		},
	},
];

for (const spec of PAGES) {
	test(`${spec.page}: every rendered row matches the header count`, async () => {
		const page = loadPage(spec.page, { routes: spec.routes });
		await page.app[spec.loader]();
		const table = page.document.querySelector("main.content table");
		const headers = table.tHead.rows[0].cells.length;
		const rows = page.document.getElementById(spec.body).querySelectorAll("tr");
		assert.ok(rows.length > 0, `${spec.page} rendered no rows, so the test proves nothing`);
		for (const tr of rows) {
			assert.equal(tr.cells.length, headers,
				`${spec.page} row has ${tr.cells.length} cells for ${headers} headers, so every ` +
				`value past the mismatch sits under the wrong column`);
		}
	});
}
