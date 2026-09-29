// Tests for editing a schedule the interface did not create. An import brings in hundreds of them:
// a crontab line becomes a schedule with a playbook or a command and no template, and an AWX or
// Rundeck schedule brings its own timezone. The dialog knew about neither. It required a template, so
// an imported schedule could be opened and never saved, and it had no timezone field, so a reader
// could not see which zone the cron was read in or set one on a schedule they were creating.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// schedulePage mounts the schedules page with an empty template list and list endpoint.
function schedulePage() {
	return loadPage("schedules", {
		routes: {
			"/v1/templates": reply({ templates: [{ id: "tpl_1", name: "deploy" }] }),
			"/v1/schedules": reply({ schedules: [] }),
		},
	});
}

// save fills the required fields, submits, and returns the request the dialog sent.
async function save(page, method) {
	fire(page.document.getElementById("schedule-form"), "submit");
	await page.clock.flush();
	return page.net.calls.find((c) => c.method === method);
}

test("a schedule's timezone is shown, kept through an edit, and settable on a new one", async () => {
	const page = schedulePage();
	page.app.wireScheduleForm();
	await page.clock.flush();

	const zone = page.document.getElementById("schedule-timezone");
	assert.ok(zone, "the dialog cannot show or set the zone the cron is read in");

	page.app.openScheduleEdit({
		id: "sch_1", name: "nightly", cron: "0 2 * * *", template_id: "tpl_1",
		timezone: "America/Chicago",
	});
	assert.equal(zone.value, "America/Chicago", "the edit dropped the schedule's timezone");

	const put = await save(page, "PUT");
	assert.ok(put, "the edit was never sent");
	assert.equal(JSON.parse(put.body).timezone, "America/Chicago",
		"saving moved when the schedule fires");
});

test("an imported schedule with no template can be edited and saved", async () => {
	const page = schedulePage();
	page.app.wireScheduleForm();
	await page.clock.flush();

	// What a crontab import produces: a cadence, a direct target, and no stored template.
	page.app.openScheduleEdit({
		id: "sch_2", name: "rotate logs", cron: "5 4 * * *", playbook: "/opt/rotate.sh",
		inventory: "prod.ini", timezone: "UTC",
	});
	assert.equal(page.document.getElementById("schedule-template").value, "",
		"an inline schedule was given a template it does not have");
	const playbook = page.document.getElementById("schedule-playbook");
	assert.ok(playbook, "the dialog cannot show what an inline schedule actually runs");
	assert.equal(playbook.value, "/opt/rotate.sh", "the target the schedule fires is not shown");

	const put = await save(page, "PUT");
	assert.ok(put, "an imported schedule still cannot be saved from the interface");
	const body = JSON.parse(put.body);
	assert.equal(body.playbook, "/opt/rotate.sh", "the save dropped what the schedule runs");
	assert.equal(body.inventory, "prod.ini", "the save dropped what the schedule targets");
	assert.equal(page.document.getElementById("schedule-status").textContent, "Saved.");
});

test("a schedule with neither a template nor a target is refused with a reason", async () => {
	const page = schedulePage();
	page.app.wireScheduleForm();
	await page.clock.flush();
	page.document.getElementById("schedule-name").value = "nothing";
	page.document.getElementById("schedule-cron").value = "0 2 * * *";

	fire(page.document.getElementById("schedule-form"), "submit");
	await page.clock.flush();
	assert.equal(page.net.calls.filter((c) => c.method === "POST").length, 0,
		"a schedule that fires nothing was sent to the server");
	assert.match(page.document.getElementById("schedule-status").textContent, /template|run/i,
		"the refusal does not say what is missing");
});
