// Tests for how the runs page names what it lists. Its subtitle and its navigation entry called
// every run a playbook execution, and the column naming what each run executed was headed
// "Playbook", while a run is as often a Terraform or OpenTofu directory, a Bash, PowerShell,
// Python, or Go script, or a pipeline of steps.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// ONE_TOOL matches a heading that names what only one kind of run has.
const ONE_TOOL = /playbook|command|script|directory|workspace/i;

test("the runs page and its navigation entry describe every run, not only playbooks", () => {
	const { app, document } = loadPage("runs", { quiet: true });
	const subtitle = document.querySelector(".page-head-text p").textContent.trim();
	const entry = app.NAV_GROUPS.flatMap((g) => g.items).find((it) => it.key === "runs");
	assert.doesNotMatch(subtitle, ONE_TOOL, "the page calls every run one kind: " + subtitle);
	assert.doesNotMatch(entry.desc, ONE_TOOL, "the navigation calls every run one kind: " +
		entry.desc);
	// The entry that leads here and the page it leads to say the same thing.
	assert.ok(subtitle.startsWith(entry.desc),
		"the navigation says \"" + entry.desc + "\" and the page says \"" + subtitle + "\"");
});

test("the column naming what a run executed is headed for every kind of run it lists", async () => {
	const runs = [
		{ id: "run_a", status: "succeeded", tool: "ansible", playbook: "plays/site.yml" },
		{ id: "run_t", status: "succeeded", tool: "terraform", command: "/srv/infra/network" },
		{ id: "run_o", status: "failed", tool: "opentofu", command: "/srv/infra/dns" },
		{ id: "run_b", status: "succeeded", tool: "bash",
			command: "# Rotate the nginx logs\nlogrotate -f /etc/logrotate.conf" },
		{ id: "run_y", status: "succeeded", tool: "python", command: "print('hello from python')" },
		{ id: "run_p", status: "succeeded", kind: "pipeline", playbook: "Provision and deploy",
			steps: [{ tool: "terraform" }, { tool: "ansible" }] },
	];
	const { app, document, net, clock } = loadPage("runs", {
		routes: [[/^\/v1\/runs(\?|$)/, reply({ runs, summary: {} })]],
	});
	await app.loadRuns();
	await clock.flush();
	const rows = Array.from(document.querySelectorAll("#runs tr"));
	assert.equal(rows.length, runs.length);
	const col = Array.from(rows[0].cells).findIndex((c) => c.classList.contains("col-playbook"));
	assert.notEqual(col, -1, "no column names what a run executed");
	assert.deepEqual(rows.map((tr) => tr.cells[col].textContent), [
		"site.yml", "network", "dns", "Rotate the nginx logs", "print('hello from python')",
		"Provision and deploy, 2 steps",
	]);
	const heading = document.querySelectorAll("table.runs thead th")[col].textContent.trim();
	assert.ok(heading, "the column has no heading");
	assert.doesNotMatch(heading, ONE_TOOL,
		"the column is headed \"" + heading + "\" over directories, scripts, and pipelines too");
	net.assertClean();
});

test("the templates table heads the same column the way the runs table does", async () => {
	// A template's what-it-runs cell is built by the same label the runs column uses, so it holds
	// a playbook, a directory, a script's title line, or a workflow's step count just the same, and
	// was headed "Playbook" over all of them.
	const templates = [
		{ id: "tpl_a", name: "site", tool: "ansible", playbook: "plays/site.yml" },
		{ id: "tpl_t", name: "network", tool: "terraform", command: "/srv/infra/network" },
		{ id: "tpl_b", name: "rotate", tool: "bash",
			command: "# Rotate the nginx logs\nlogrotate -f /etc/logrotate.conf" },
		{ id: "tpl_w", name: "provision", steps: [{ tool: "terraform" }, { tool: "ansible" }] },
	];
	const { app, document, clock } = loadPage("jobtemplates", {
		routes: [[/^\/v1\/templates(\?|$)/, reply({ templates })]],
		quiet: true,
	});
	await app.loadTemplates();
	await clock.flush();
	const rows = Array.from(document.querySelectorAll("#templates tr"));
	assert.equal(rows.length, templates.length);
	const col = Array.from(rows[0].cells).findIndex((c) => c.querySelector("button.linkish"));
	assert.notEqual(col, -1, "no column shows what a template runs");
	assert.deepEqual(rows.map((tr) => tr.cells[col].textContent), [
		"site.yml", "network", "Rotate the nginx logs", "workflow, 2 steps",
	]);
	const heading = document.querySelectorAll("table.runs thead th")[col].textContent.trim();
	assert.doesNotMatch(heading, ONE_TOOL,
		"the column is headed \"" + heading + "\" over directories, scripts, and workflows too");
	const runs = loadPage("runs", { quiet: true }).document;
	const runHeadings = Array.from(runs.querySelectorAll("table.runs thead th"))
		.map((th) => th.textContent.trim());
	assert.ok(runHeadings.includes(heading),
		"the templates table says \"" + heading + "\" where the runs table says something else");
});
