// Tests for counts written beside a noun. Each of these put a bare number in front of a plural, so
// the count most likely on a fresh install, one, read "1 entries" on the audit page, "Imported 1
// objects" after a first import, and "Every 1 minutes" for a schedule written with a step of one.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadParts } from "./loader.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

test("the chain badge counts one entry and one anchor in the singular", () => {
	const { app, document } = loadPage("audit");
	const badge = document.getElementById("audit-badge");
	app.renderVerifyVerdict(badge, { ok: true, count: 1, anchored: 0 });
	assert.equal(badge.textContent, "Chain intact: 1 entry, not anchored");
	app.renderVerifyVerdict(badge, { ok: true, count: 1, anchored: 1 });
	assert.equal(badge.textContent, "Chain verified: 1 entry, 1 anchor held");
	app.renderVerifyVerdict(badge, { ok: true, count: 4, anchored: 2 });
	assert.equal(badge.textContent, "Chain verified: 4 entries, 2 anchors held");
});

test("an import that created one object says so in the singular", async () => {
	const { app, document, clock } = loadPage("migrate", {
		routes: { "/v1/import/": reply({ applied: true, created: 1 }) },
	});
	document.getElementById("migrate-export").value = "{}";
	await app.runMigrate(true);
	await clock.flush();
	assert.equal(document.getElementById("migrate-status").textContent, "Imported 1 object.");
	assert.match(document.querySelector(".migrate-applied").textContent, /^Imported 1 object\. /);
});

test("a checkout of one file, and a file one byte long, are counted in the singular", async () => {
	const { app, document, clock } = loadPage("projects", {
		routes: [
			["/v1/projects/proj_1/files", reply({ files: [{ path: "site.yml", size: 1 }] })],
			["/v1/projects/proj_1/file", reply({ path: "site.yml", size: 1, content: "\n" })],
		],
	});
	await app.openProjectFiles({ id: "proj_1", name: "infra" });
	await clock.flush();
	assert.match(document.getElementById("tree-modal").textContent, /1 file in the cached checkout\./);
	await app.openFileViewer("proj_1", "site.yml");
	await clock.flush();
	assert.match(document.getElementById("file-modal").textContent, /\b1 byte Read only/);
});

test("a stepped cron of one reads every minute or every hour", () => {
	const app = loadParts(["17-cron.js"]);
	assert.equal(app.describeCron("*/1 * * * *"), "Every minute");
	assert.equal(app.describeCron("0 */1 * * *"), "Every hour");
	assert.equal(app.describeCron("*/5 * * * *"), "Every 5 minutes");
});

test("a one-step pipeline on the schedules page is one step", () => {
	const { app, document } = loadPage("schedules");
	const one = { steps: [{ name: "deploy" }] };
	assert.equal(app.scheduleTarget(one), "pipeline, 1 step");
	app.setScheduleGraphNotice(one);
	assert.match(document.getElementById("schedule-graph-notice").textContent,
		/a pipeline of 1 step\./);
});

test("an estate window where one host sat still says one host", async () => {
	const { app, document, clock } = loadPage("estate", {
		routes: { "/v1/estate/diff": reply({ hosts: [], unchanged: 1 }) },
	});
	document.getElementById("estate-from").value = "2026-09-01";
	await app.loadEstate();
	await clock.flush();
	assert.match(document.getElementById("status").textContent, /1 host sat still\./);
	assert.doesNotMatch(document.getElementById("status").textContent, /\(s\)/);
});

test("the too-large matrix notice counts one task in the singular", () => {
	const { app, document } = loadPage("detail");
	app.renderMatrixTooLarge(60000, 1, 60000, 50000);
	assert.match(document.querySelector(".matrix-too-large").textContent,
		/covers 60,000 hosts across 1 task, which is/);
});

test("a workflow of one step exports as one step", () => {
	const { app, document } = loadPage("workflows");
	app.mountWorkflow();
	app.wfState.nodes = [app.wfState.nodes[0]];
	app.wfState.edges = [];
	app.exportWorkflow("json");
	assert.match(document.getElementById("status").textContent, /^Exported 1 step to /);
});

test("a source refreshed every second says every second", () => {
	const app = loadParts(["18-host-page.js"]);
	assert.equal(app.fmtInterval(1), "second");
	assert.equal(app.fmtInterval(45), "45 seconds");
});
