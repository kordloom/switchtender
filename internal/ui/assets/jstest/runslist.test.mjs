// Tests for the runs list helpers in 16-runs-list.js: the tool label a row shows, where an origin
// chip navigates, the request each page of history is fetched with, and the generation guard that
// keeps a superseded load from touching the table.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "08-auth-status.js", "16-runs-list.js"]);

// mountRunsPage loads the runs list against a stubbed page: the toolbar controls hold the given
// values, the table and its body exist, and everything loadRuns calls outside the code under test
// is replaced by a recorder. The returned handle exposes what the page did.
function mountRunsPage(values, search) {
	const page = loadParts(["01-boot.js", "08-auth-status.js", "16-runs-list.js"]);
	const byID = {};
	for (const [id, value] of Object.entries(values || {})) {
		const el = page.document.createElement("select");
		el.value = value;
		byID[id] = el;
	}
	const tbody = page.document.createElement("tbody");
	byID.runs = tbody;
	const table = page.document.createElement("table");
	table.parentNode = { insertBefore: (node) => node };
	page.location.search = search || "";
	page.document.getElementById = (id) => byID[id] || null;
	page.document.querySelector = (sel) => (sel === "table.runs" ? table : null);

	const handle = { urls: [], status: [], appended: [], table, tbody, page };
	page.getJSON = (url) => {
		handle.urls.push(url);
		return handle.reply(url);
	};
	handle.reply = () => Promise.resolve({ runs: [{ id: "run_1" }], has_more: true, summary: {} });
	page.setStatus = (msg) => { handle.status.push(msg); };
	page.showSkeletonRows = () => {};
	page.showEmpty = (msg) => { handle.status.push(msg); };
	page.renderSummary = () => {};
	page.appendRunRows = (_, runs) => { handle.appended.push(...runs); };
	// The Load more button is created and inserted by the code under test, so it is captured on
	// the way past rather than pre-built, and registered so a later wiring finds the same one.
	const create = page.document.createElement;
	page.document.createElement = (tag) => {
		const el = create(tag);
		if (tag === "button") {
			handle.more = el;
			Object.defineProperty(el, "id", {
				set: (v) => { byID[v] = el; },
				get: () => "runs-more",
			});
		}
		return el;
	};
	return handle;
}

test("toolLabel names what a run executed rather than quoting its source", () => {
	const tests = [
		// Test 0: A playbook shows its file, not its path.
		{ In: { playbook: "deploy/site.yml" }, Want: "site.yml" },
		// Test 1: A bare playbook name stays.
		{ In: { playbook: "site.yml" }, Want: "site.yml" },
		// Test 2: A playbook path with no final segment falls back to the whole path.
		{ In: { playbook: "deploy/" }, Want: "deploy/" },
		// Test 3: The playbook wins over a command when both exist.
		{ In: { playbook: "site.yml", command: "echo hi" }, Want: "site.yml" },
		// Test 4: A command collapses runs of whitespace.
		{ In: { command: "  terraform   apply \n -auto-approve " }, Want: "terraform apply -auto-approve" },
		// Test 5: A 48 character command fits whole.
		{ In: { command: "x".repeat(48) }, Want: "x".repeat(48) },
		// Test 6: A 49 character command truncates to 47 plus an ellipsis.
		{ In: { command: "x".repeat(49) }, Want: "x".repeat(47) + "…" },
		// Test 7: Nothing to show.
		{ In: {}, Want: "" },
		// Test 8: A named workflow keeps its name beside the step count, since the count alone made
		// every workflow in the list read the same.
		{ In: { playbook: "Provision and deploy", steps: [1, 2, 3] },
			Want: "Provision and deploy, 3 steps" },
		// Test 9: An unnamed workflow still says what it is.
		{ In: { steps: [1] }, Want: "workflow, 1 step" },
		// Test 10: A Go program is described by its comment, not by its package clause.
		{ In: { command: "// Report spare capacity per host\npackage main\n\nimport (\n\t\"fmt\"\n)" },
			Want: "Report spare capacity per host" },
		// Test 11: With no comment, the first line that is not ceremony. The body is realistically
		// long, since a command short enough to show whole is shown whole.
		{ In: { command: "package main\n\nimport (\n\t\"encoding/json\"\n\t\"fmt\"\n)\n\n" +
			"type Host struct{ Name string }\n\nfunc main() { fmt.Println(\"spare capacity\") }" },
			Want: "type Host struct{ Name string }" },
		// Test 12: Python skips its imports.
		{ In: { command: "import json\nimport sys\n\n" +
			"want = {\"web01\", \"web02\", \"db01\"}\nprint(sorted(want))" },
			Want: "want = {\"web01\", \"web02\", \"db01\"}" },
		// Test 13: A shell script skips the shebang and its own options.
		{ In: { command: "#!/usr/bin/env bash\nset -euo pipefail\n" +
			"echo \"Rotating application logs\"\nlogrotate -f /etc/logrotate.conf\nsystemctl reload nginx" },
			Want: "echo \"Rotating application logs\"" },
		// Test 14: A shebang is not read as a comment title, and a real comment below it is.
		{ In: { command: "#!/bin/sh\n# Rotate the logs on every web host in the fleet\n" +
			"echo hi\nlogrotate -f /etc/logrotate.conf\nsystemctl reload nginx" },
			Want: "Rotate the logs on every web host in the fleet" },
		// Test 15: A script that is nothing but ceremony still shows something rather than nothing.
		{ In: { command: "import os\nimport sys\nimport json\nimport time\nimport socket\nimport re" },
			Want: "import os import sys import json import time im…" },
		// Test 16: The chosen line is still cut to the row width.
		{ In: { command: "# " + "x".repeat(60) }, Want: "x".repeat(47) + "…" },
		// Test 17: Terraform is pointed at a directory, so it is named by that directory rather
		// than by the whole path, the same way a playbook is named by its file.
		{ In: { tool: "terraform", command: "/tmp/switchtender-demo-assets/infra/network" },
			Want: "network" },
		// Test 18: OpenTofu is the same shape.
		{ In: { tool: "opentofu", command: "envs/prod" }, Want: "prod" },
		// Test 19: A directory tool with a bare name keeps it.
		{ In: { tool: "terraform", command: "infra" }, Want: "infra" },
		// Test 20: A script tool is unaffected by the directory rule, so a bash command that
		// happens to contain slashes is still read as a command.
		{ In: { tool: "bash", command: "cp /etc/hosts /tmp/hosts.bak" },
			Want: "cp /etc/hosts /tmp/hosts.bak" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.toolLabel(tc.In), tc.Want, "test " + i);
	}
});

test("originHref navigates only when the source has a page", () => {
	const tests = [
		// Test 0: A template origin opens the templates page when the template is known.
		{ In: { source: "template", source_id: "tpl_1" }, Want: "/ui/templates" },
		// Test 1: A template origin with no id has nowhere to go.
		{ In: { source: "template" }, Want: "" },
		// Test 2: A schedule origin opens schedules.
		{ In: { source: "schedule", source_id: "sch_1" }, Want: "/ui/schedules" },
		// Test 3: A schedule origin with no id has nowhere to go.
		{ In: { source: "schedule" }, Want: "" },
		// Test 4: A rerun links to the run it replayed.
		{ In: { source: "rerun", rerun_of: "run_7" }, Want: "/ui/runs/run_7" },
		// Test 5: A rerun with no ancestor has nowhere to go.
		{ In: { source: "rerun" }, Want: "" },
		// Test 6: A drift fix links to the check that proposed it.
		{ In: { source: "reconcile", proposed_from: "run_3" }, Want: "/ui/runs/run_3" },
		// Test 7: An API submission has no page.
		{ In: { source: "api" }, Want: "" },
		// Test 8: A proposed run has no page either.
		{ In: { source: "propose" }, Want: "" },
		// Test 9: No source at all.
		{ In: {}, Want: "" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.originHref(tc.In), tc.Want, "test " + i);
	}
});

test("Load more requests the same filtered set as the first page", async () => {
	const handle = mountRunsPage({
		"runs-status": "failed", "runs-tool": "ansible", "runs-order": "duration",
		"runs-search": "web", "runs-pagesize": "25",
	}, "?after=2026-01-01&before=2026-02-01");

	await handle.page.loadRuns();
	assert.equal(handle.urls.length, 1);
	assert.ok(handle.more, "Load more was never created");

	await handle.more.onclick();
	assert.equal(handle.urls.length, 2, "Load more did not fetch a page");

	// Every parameter but the offset has to match, or the next page is drawn from a different
	// result set than the one its offset counts within.
	const params = (url) => {
		const q = new URLSearchParams(url.slice(url.indexOf("?") + 1));
		const offset = q.get("offset");
		q.delete("offset");
		q.sort();
		return { offset, rest: q.toString() };
	};
	const first = params(handle.urls[0]);
	const more = params(handle.urls[1]);
	assert.equal(more.rest, first.rest, "Load more dropped filters the first page carried");
	assert.equal(first.rest,
		"after=2026-01-01&before=2026-02-01&limit=25&order=duration&q=web&status=failed&tool=ansible");
	assert.equal(first.offset, "0");
	assert.equal(more.offset, "1");
});

test("a superseded load that fails leaves the newer table alone", async () => {
	const handle = mountRunsPage({ "runs-search": "", "runs-pagesize": "20" }, "");
	// The first load never settles until the test says so; the second answers immediately, so the
	// stale failure lands after a newer load has already rendered.
	let failFirst;
	let first = true;
	handle.reply = () => {
		if (!first) return Promise.resolve({ runs: [{ id: "run_2" }], has_more: false, summary: {} });
		first = false;
		return new Promise((_, reject) => { failFirst = () => reject(new Error("network gone")); });
	};

	const stale = handle.page.loadRuns();
	const fresh = handle.page.loadRuns();
	await fresh;
	assert.deepEqual(handle.appended, [{ id: "run_2" }]);
	assert.equal(handle.table.hidden, false);

	failFirst();
	await stale;
	assert.equal(handle.table.hidden, false, "the stale failure hid a table a newer load drew");
	assert.ok(!handle.status.some((s) => s.startsWith("Failed to load runs")),
		"the stale failure showed an error over good data: " + JSON.stringify(handle.status));
});
