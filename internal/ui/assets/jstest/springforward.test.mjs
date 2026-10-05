// Tests for the schedule dialog's "If the time doesn't exist" setting: on the night the clocks go
// forward a time such as 02:30 does not exist, and the schedule says whether it runs when the clock
// jumps, after the clock change, or not that night. The dialog shows the stored setting, sends it on every
// save, asks the preview with it, and says what happens that night when the preview describes it.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";
import { fire } from "./dom.mjs";

// gap is the night the preview describes for a nightly 02:30 in New York under the later setting.
const gap = {
	transition: "2027-03-14T07:00:00Z", zone: "America/New_York", from: "02:00", to: "03:00",
	setting: "later", fires: ["2027-03-14T07:30:00Z"],
};

// schedulePage mounts the schedules page with a template to pick and a preview that answers with
// the given spring_gap, or none.
function schedulePage(previewGap) {
	const preview = { next: ["2026-10-02T06:30:00Z"] };
	if (previewGap) preview.spring_gap = previewGap;
	return loadPage("schedules", {
		parts: ALL_PARTS,
		routes: {
			// The preview matcher has to come first: "/v1/schedules" also matches its path.
			"/v1/schedules/preview": reply(preview),
			"/v1/schedules/sch_1": reply({ ok: true }),
			"/v1/schedules": reply({ schedules: [] }),
			"/v1/templates": reply({ templates: [{ id: "tpl_1", name: "deploy" }] }),
		},
	});
}

// sent returns the decoded body of the first request the page made with method.
function sent(page, method) {
	const call = page.net.calls.find((c) => c.method === method);
	assert.ok(call, "no " + method + " was sent");
	return JSON.parse(call.body);
}

test("an edit shows the stored setting and saves it back unchanged", async () => {
	const page = schedulePage();
	page.app.wireScheduleForm();
	await page.clock.flush();
	page.app.openScheduleEdit({
		id: "sch_1", name: "nightly", cron: "30 2 * * *", template_id: "tpl_1",
		timezone: "America/New_York", spring_forward: "skip",
	});
	const select = page.document.getElementById("schedule-spring-forward");
	assert.ok(select, "the dialog has no control for what happens to a time that does not exist");
	assert.equal(select.value, "skip", "the edit did not show the stored setting");

	fire(page.document.getElementById("schedule-form"), "submit");
	await page.clock.flush();
	assert.equal(sent(page, "PUT").spring_forward, "skip",
		"saving the dialog reset the setting it was showing");
});

test("a new schedule sends the chosen setting, and the default as empty", async () => {
	const page = schedulePage();
	page.app.wireScheduleForm();
	await page.clock.flush();
	page.document.getElementById("schedule-name").value = "nightly";
	page.document.getElementById("schedule-cron").value = "30 2 * * *";
	page.document.getElementById("schedule-template").value = "tpl_1";
	page.document.getElementById("schedule-spring-forward").value = "later";
	fire(page.document.getElementById("schedule-form"), "submit");
	await page.clock.flush();
	assert.equal(sent(page, "POST").spring_forward, "later", "the chosen setting was not sent");
});

test("the default choice names the default of the cadence in use", () => {
	const page = schedulePage();
	page.app.wireScheduleForm();
	const first = page.document.getElementById("schedule-spring-forward").options[0];
	page.app.setScheduleKind("cron");
	assert.match(first.textContent, /run when the clock jumps/,
		"a cron schedule's default is not named as the jump");
	page.app.setScheduleKind("rrule");
	assert.match(first.textContent, /run after the clock change/,
		"a recurrence rule's default is not named as after the clock change");
});

test("the preview asks with the setting and re-asks when it changes", async () => {
	const page = schedulePage();
	page.app.wireCronPreview();
	page.document.getElementById("schedule-cron").value = "30 2 * * *";
	fire(page.document.getElementById("schedule-cron"), "input");
	await page.clock.tick(400);
	await page.clock.flush();
	const before = page.net.calledWith("/schedules/preview").length;
	assert.ok(before > 0, "the preview never asked the server anything");
	assert.doesNotMatch(page.net.calledWith("/schedules/preview")[before - 1].url, /spring_forward/,
		"the default was sent as a setting rather than left to the server");

	const select = page.document.getElementById("schedule-spring-forward");
	select.value = "skip";
	fire(select, "change");
	await page.clock.flush();
	const after = page.net.calledWith("/schedules/preview");
	assert.ok(after.length > before, "changing the setting left the previous preview on screen");
	assert.match(after[after.length - 1].url, /spring_forward=skip/,
		"the preview did not ask with the setting, so its times are not the real ones");
});

test("the preview says what happens on the night the clocks go forward", async () => {
	const page = schedulePage(gap);
	page.app.wireCronPreview();
	page.document.getElementById("schedule-cron").value = "30 2 * * *";
	fire(page.document.getElementById("schedule-cron"), "input");
	await page.clock.tick(400);
	await page.clock.flush();
	const note = page.document.getElementById("cron-preview-gap");
	assert.equal(note.hidden, false, "the night the setting matters is not shown");
	assert.match(note.textContent, /02:00 to 03:00/, "the note does not say which times vanish");
	assert.match(note.textContent, /America\/New_York/, "the note does not name the zone");
	assert.match(note.textContent, /run after the clock change/, "the note does not name the setting");
	assert.match(note.textContent, /03:30/, "the note does not say when the schedule fires");
});

test("a skipped night is said to be skipped, and no note shows when there is none", () => {
	const page = schedulePage();
	const text = page.app.springGapText(Object.assign({}, gap, { setting: "skip", fires: [] }));
	assert.match(text, /skipped/, "a skipped night is not said to be skipped");
	assert.doesNotMatch(text, /fires at/, "a skipped night claims a fire");

	const note = page.document.getElementById("cron-preview-gap");
	page.app.renderSpringGap(note, gap);
	assert.equal(note.hidden, false);
	page.app.renderSpringGap(note, null);
	assert.equal(note.hidden, true,
		"a note stayed on screen after the preview stopped describing one");
});
