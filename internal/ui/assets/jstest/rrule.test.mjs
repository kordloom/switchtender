// Tests for writing a schedule as an RFC 5545 recurrence rule: the dialog switches to it, previews
// the next fires the rule gives, opens an imported rule in that form, and saves only the form in use.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";
import { fire } from "./dom.mjs";

// QUARTER_CLOSE is the rule cron cannot say: the last Friday of each quarter.
const QUARTER_CLOSE = "DTSTART;TZID=America/New_York:20260102T170000\n" +
	"RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR";

// mountSchedules opens the schedules page, answering the preview with the given body.
function mountSchedules(preview) {
	return loadPage("schedules", {
		parts: ALL_PARTS,
		routes: {
			// The preview matcher comes first: "/v1/schedules" also matches its path.
			"/v1/schedules/preview": reply(preview),
			"/v1/schedules": reply({ schedules: [] }),
			"/v1/templates": reply({ templates: [{ id: "tpl_1", name: "close" }] }),
		},
	});
}

// typeRule switches the dialog to a recurrence rule and types one.
async function typeRule(page, rule) {
	const kind = page.document.getElementById("schedule-kind");
	kind.value = "rrule";
	fire(kind, "change");
	page.document.getElementById("schedule-rrule").value = rule;
	fire(page.document.getElementById("schedule-rrule"), "input");
	await page.clock.tick(400);
	await page.clock.flush();
}

test("a recurrence rule previews its next five fires in the zone its DTSTART names", async () => {
	const page = mountSchedules({
		next: ["2026-12-25T22:00:00Z", "2027-03-26T21:00:00Z", "2027-06-25T21:00:00Z",
			"2027-09-24T21:00:00Z", "2027-12-31T22:00:00Z"],
		timezone: "America/New_York",
	});
	page.app.wireScheduleForm();
	page.app.wireCronPreview();
	await typeRule(page, QUARTER_CLOSE);

	const previews = page.net.calledWith("/schedules/preview");
	assert.ok(previews.length > 0, "the rule was never previewed");
	const url = previews[previews.length - 1].url;
	assert.match(url, /rrule=DTSTART/, "the preview asked about a cron, not the rule: " + url);
	assert.doesNotMatch(url, /cron=/, "the preview sent both forms: " + url);
	const text = page.document.getElementById("cron-preview").textContent;
	assert.equal((text.match(/·/g) || []).length, 4, "the preview did not show five fires: " + text);
	assert.match(text, /America\/New_York/, "the preview did not say which clock it is on");
	assert.equal(page.document.getElementById("schedule-rrule-field").hidden, false,
		"the rule box stayed hidden");
	assert.equal(page.document.getElementById("schedule-cron-field").hidden, true,
		"the cron box stayed on screen beside the rule");
	assert.equal(page.document.getElementById("schedule-cron").required, false,
		"the cron box is still required, so a rule can never be saved");
});

test("a rule with fires running out says it stops", async () => {
	const page = mountSchedules({ next: ["2026-10-02T02:00:00Z", "2026-10-03T02:00:00Z"],
		finished: true });
	page.app.wireScheduleForm();
	page.app.wireCronPreview();
	await typeRule(page, "DTSTART:20261002T020000Z RRULE:FREQ=DAILY;COUNT=2");
	assert.match(page.document.getElementById("cron-preview").textContent, /then it stops/,
		"a rule that ends previewed as one that repeats");
});

test("a refused rule shows the server's reason", async () => {
	const page = loadPage("schedules", {
		parts: ALL_PARTS,
		routes: {
			"/v1/schedules/preview": reply({ error: "bad recurrence: FREQ=FORTNIGHTLY is not a frequency" },
				{ status: 400 }),
			"/v1/schedules": reply({ schedules: [] }),
			"/v1/templates": reply({ templates: [] }),
		},
	});
	page.app.wireScheduleForm();
	page.app.wireCronPreview();
	await typeRule(page, "DTSTART:20261002T020000Z RRULE:FREQ=FORTNIGHTLY");
	assert.match(page.document.getElementById("cron-preview").textContent, /FORTNIGHTLY/,
		"the reader is not told which part of the rule is wrong");
});

test("an imported rule opens as a rule and saves as one, with no cron", async () => {
	const page = mountSchedules({ next: [] });
	page.app.wireScheduleForm();
	await page.clock.flush();
	page.app.openScheduleEdit({
		id: "sch_q", name: "quarter close", rrule: QUARTER_CLOSE, template_id: "tpl_1",
		timezone: "America/New_York",
	});
	assert.equal(page.document.getElementById("schedule-kind").value, "rrule",
		"the dialog opened a rule schedule in cron form");
	assert.equal(page.document.getElementById("schedule-rrule").value, QUARTER_CLOSE);

	fire(page.document.getElementById("schedule-form"), "submit");
	await page.clock.flush();
	const put = page.net.calls.find((c) => c.method === "PUT");
	assert.ok(put, "the edit was never sent");
	const body = JSON.parse(put.body);
	assert.equal(body.rrule, QUARTER_CLOSE, "the save dropped the rule");
	assert.equal(body.cron, "", "the save sent a cron beside the rule");
});

test("switching an edit back to cron clears the rule", async () => {
	const page = mountSchedules({ next: [] });
	page.app.wireScheduleForm();
	await page.clock.flush();
	page.app.openScheduleEdit({ id: "sch_q", name: "q", rrule: QUARTER_CLOSE, template_id: "tpl_1" });
	const kind = page.document.getElementById("schedule-kind");
	kind.value = "cron";
	fire(kind, "change");
	page.document.getElementById("schedule-cron").value = "0 17 * * 5";
	fire(page.document.getElementById("schedule-form"), "submit");
	await page.clock.flush();
	const body = JSON.parse(page.net.calls.find((c) => c.method === "PUT").body);
	assert.equal(body.cron, "0 17 * * 5");
	assert.equal(body.rrule, "", "the old rule rode along with the new cron");
});

test("the list reads a rule in words", () => {
	const page = mountSchedules({ next: [] });
	assert.equal(page.app.describeRRule(QUARTER_CLOSE),
		"Every month on the last Friday in March, June, September, December");
	assert.equal(page.app.describeRRule(
		"DTSTART:20260101T180000Z RRULE:FREQ=MONTHLY;BYDAY=MO,TU,WE,TH,FR;BYSETPOS=-1"),
	"Every month on the last weekday");
	assert.equal(page.app.describeRRule(
		"DTSTART:20260106T020000Z RRULE:FREQ=WEEKLY;INTERVAL=2;BYDAY=TU;COUNT=6 " +
		"EXDATE:20260120T020000Z"),
	"Every 2 weeks on Tuesday, 6 times, with 1 more line");
	assert.equal(page.app.rruleLines(QUARTER_CLOSE),
		"RRULE:FREQ=MONTHLY;BYMONTH=3,6,9,12;BYDAY=-1FR");
	assert.equal(page.app.describeRRule("DTSTART:20260101T000000Z"), "Custom recurrence");
});
