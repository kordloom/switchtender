// Tests for how a schedule fire that matched no hosts reads. Such a fire is skipped rather than
// failed, so it must not look like a failure, and a schedule that keeps skipping must not look like
// one that runs: the list carries a badge, the dialog warns before Save, a refused launch links to
// the host preview, and that link opens the preview.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply, sequence } from "./net.mjs";
import { fire } from "./dom.mjs";

// SCHEDULES are one schedule per state the last-fire cell can show.
const SCHEDULES = [
	{
		id: "sch_once", name: "skipped once", cron: "0 2 * * *", enabled: true,
		last_skip: "no hosts matched", skipped_fires: 1, last_run_at: "2026-10-01T02:00:00Z",
	},
	{
		id: "sch_streak", name: "skipping", cron: "0 3 * * *", enabled: true,
		last_skip: "no hosts matched", skipped_fires: 3, last_run_at: "2026-10-01T03:00:00Z",
		last_run_id: "run_old",
	},
	{
		id: "sch_failed", name: "failing", cron: "0 4 * * *", enabled: true,
		last_error: "credential has no secret yet", last_run_at: "2026-10-01T04:00:00Z",
	},
];

// rowOf returns the schedules table row whose name cell starts with name.
function rowOf(doc, name) {
	return Array.from(doc.getElementById("schedules").querySelectorAll("tr"))
		.find((tr) => tr.querySelector("td").textContent.startsWith(name));
}

test("a skipped fire reads as a skip and a run of them carries the badge", async () => {
	const page = loadPage("schedules", {
		routes: {
			"/v1/templates": reply({ templates: [] }),
			"/v1/schedules": reply({ schedules: SCHEDULES }),
		},
	});
	await page.app.loadSchedules();
	await page.clock.flush();
	const doc = page.document;

	const once = rowOf(doc, "skipped once");
	assert.ok(once, "the schedule that skipped once is not listed");
	const chip = once.querySelector(".chip.skipped");
	assert.ok(chip, "a skipped fire is not shown as skipped");
	assert.equal(chip.textContent, "skipped: no hosts matched");
	assert.equal(once.querySelector(".chip.failed"), null, "a skipped fire is painted as a failure");
	assert.equal(once.querySelector(".schedule-skip-badge"), null,
		"one skip carries the badge meant for a run of them");

	const streak = rowOf(doc, "skipping");
	const badge = streak.querySelector(".schedule-skip-badge");
	assert.ok(badge, "three skips in a row carry no badge");
	assert.equal(badge.textContent, "matched no hosts for the last 3 fires");
	assert.ok(streak.querySelector('a[href="/ui/runs/run_old"]'),
		"the last run that did start is no longer linked");

	const failed = rowOf(doc, "failing");
	assert.ok(failed.querySelector(".chip.failed"), "a failed fire no longer reads as one");
	page.net.assertClean();
});

test("the badge threshold is the server's", async () => {
	const page = loadPage("schedules", { routes: {} });
	assert.equal(page.app.SKIP_BADGE_FIRES, 3);
	const at = page.app.scheduleSkipBadge({ skipped_fires: 3 });
	assert.ok(at, "the badge is missing at the threshold");
	assert.equal(page.app.scheduleSkipBadge({ skipped_fires: 2 }), null,
		"the badge shows below the threshold");
});

test("the schedule dialog warns when the template's inventory matches nothing now", async () => {
	const page = loadPage("schedules", {
		routes: [
			["/v1/templates", reply({ templates: [
				{ id: "tpl_patch", name: "patch", inventory_id: "inv_window" },
				{ id: "tpl_static", name: "deploy", inventory_id: "inv_fleet" },
			] })],
			["/v1/inventories/inv_window/preview", sequence(
				reply({ kind: "smart", hosts: [], count: 0 }),
				reply({ kind: "smart", hosts: ["web1", "web2"], count: 2 }),
			)],
			["/v1/inventories", reply({ inventories: [
				{ id: "inv_window", name: "patch window", kind: "smart" },
				{ id: "inv_fleet", name: "fleet", content: "[web]\nweb1\n" },
			] })],
		],
	});
	page.app.wireScheduleForm();
	await page.clock.flush();
	const doc = page.document;
	const pick = doc.getElementById("schedule-template");
	const hint = doc.getElementById("schedule-inventory-preview");

	pick.value = "tpl_patch";
	fire(pick, "change");
	await page.clock.flush();
	assert.equal(hint.hidden, false, "nothing warns that the inventory matches no hosts");
	assert.equal(hint.className, "warn-note");
	assert.match(hint.textContent, /patch window/);
	assert.match(hint.textContent, /matches no hosts right now/);
	assert.match(hint.textContent, /skipped, not failed/);

	fire(pick, "change");
	await page.clock.flush();
	assert.equal(hint.className, "field-hint", "a matching inventory still reads as a warning");
	assert.match(hint.textContent, /matches 2 hosts right now/);

	pick.value = "tpl_static";
	fire(pick, "change");
	await page.clock.flush();
	assert.equal(hint.hidden, true, "an inventory of fixed hosts is previewed as if composed");
	assert.equal(page.net.calledWith("/preview").length, 2,
		"an inventory of fixed hosts was sent to the composed preview");
	page.net.assertClean();
});

test("a launch refused for matching no hosts links to the host preview", async () => {
	const page = loadPage("schedules", { routes: {} });
	const status = page.document.createElement("span");
	const url = "/ui/inventories?preview=inv_window";
	const err = new Error("composed inventory resolved to no hosts: patch window matched nothing. " +
		"See which hosts it matches now: " + url);
	err.previewURL = url;
	page.app.showLaunchFailure(status, err);
	const link = status.querySelector("a");
	assert.ok(link, "the refusal names the preview without linking to it");
	assert.equal(link.getAttribute("href"), url);
	assert.equal(status.textContent,
		"Launch failed: composed inventory resolved to no hosts: patch window matched nothing. " +
		"See which hosts it matches now: " + url);

	const plain = page.document.createElement("span");
	page.app.showLaunchFailure(plain, new Error("credential has no secret yet"));
	assert.equal(plain.querySelector("a"), null, "an ordinary refusal grew a link");
	assert.equal(plain.textContent, "Launch failed: credential has no secret yet");
});

test("a refused launch carries the preview link from the server's answer", async () => {
	const page = loadPage("schedules", {
		routes: [["/v1/templates/tpl_patch/launch", reply({
			error: "no hosts. See which hosts it matches now: /ui/inventories?preview=inv_window",
			preview_url: "/ui/inventories?preview=inv_window",
		}, { status: 400 })]],
	});
	await assert.rejects(page.app.postAction("/templates/tpl_patch/launch"), (err) => {
		assert.equal(err.previewURL, "/ui/inventories?preview=inv_window");
		return true;
	});
});

test("the inventories page opens the preview a link names", async () => {
	const page = loadPage("inventories", {
		search: "?preview=inv_window",
		routes: [
			["/v1/inventories/inv_window/preview", reply({
				kind: "smart", hosts: [], count: 0, inputs: [],
			})],
			["/v1/inventories", reply({ inventories: [
				{ id: "inv_fleet", name: "fleet", content: "[web]\nweb1\n" },
				{ id: "inv_window", name: "patch window", kind: "smart", host_filter: "name=web9" },
			] })],
		],
	});
	await page.app.loadInventories();
	await page.clock.flush();
	assert.equal(page.net.calledWith("/v1/inventories/inv_window/preview").length, 1,
		"the preview the link names was not opened");
	const drill = page.document.getElementById("drill");
	assert.ok(drill, "no drawer opened");
	assert.match(drill.textContent, /patch window: hosts right now/);
	page.net.assertClean();
});
