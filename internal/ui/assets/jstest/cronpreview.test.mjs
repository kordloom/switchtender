// Tests for the schedule dialog's cron preview, which is the only place an operator can check a
// cadence before saving it.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";
import { fire } from "./dom.mjs";

// mountSchedules opens the schedules page and records every preview URL the page asks for.
function mountSchedules() {
	return loadPage("schedules", {
		parts: ALL_PARTS,
		routes: {
			// The preview matcher has to come first: "/v1/schedules" also matches this path, and
			// the first matching route wins.
			"/v1/schedules/preview": reply({ next: ["2026-09-16T06:00:00Z"] }),
			"/v1/schedules": reply({ schedules: [] }),
			"/v1/templates": reply({ templates: [] }),
		},
	});
}

// TestPreviewCarriesTheZone pins that the preview asks about the schedule the operator is writing.
//
// The zone input sat right beside the cron box and was never sent, so an operator who picked
// America/New_York and typed 0 2 * * * was shown firing times computed in UTC. The preview exists
// precisely so a cadence is verifiable before saving, and it was answering a different question.
test("the cron preview sends the timezone the operator chose", async () => {
	const { app, document, clock, net } = mountSchedules();
	app.wireCronPreview();
	document.getElementById("schedule-timezone").value = "America/New_York";
	document.getElementById("schedule-cron").value = "0 2 * * *";
	fire(document.getElementById("schedule-cron"), "input");
	await clock.tick(400);
	await clock.flush();
	const previews = net.calledWith("/schedules/preview");
	assert.ok(previews.length > 0, "the preview never asked the server anything");
	const last = previews[previews.length - 1].url;
	assert.match(last, /timezone=America%2FNew_York/,
		"the preview asked without the zone, so its times are computed in the wrong one: " + last);
});

// TestPreviewLabelsTheZone pins that the reader is told which clock the times are on.
test("the cron preview says which zone its times are in", async () => {
	const { app, document, clock } = mountSchedules();
	app.wireCronPreview();
	document.getElementById("schedule-timezone").value = "America/New_York";
	document.getElementById("schedule-cron").value = "0 2 * * *";
	fire(document.getElementById("schedule-cron"), "input");
	await clock.tick(400);
	await clock.flush();
	assert.match(document.getElementById("cron-preview").textContent, /America\/New_York/,
		"the preview showed times without saying which zone they are on");
});

// TestChangingTheZoneReasks pins that the preview follows the zone control.
test("changing the zone re-asks rather than leaving stale times", async () => {
	const { app, document, clock, net } = mountSchedules();
	app.wireCronPreview();
	document.getElementById("schedule-cron").value = "0 2 * * *";
	fire(document.getElementById("schedule-cron"), "input");
	await clock.tick(400);
	await clock.flush();
	const before = net.calledWith("/schedules/preview").length;
	document.getElementById("schedule-timezone").value = "Europe/Berlin";
	fire(document.getElementById("schedule-timezone"), "change");
	await clock.flush();
	const after = net.calledWith("/schedules/preview");
	assert.ok(after.length > before, "changing the zone left the previous zone's times on screen");
	assert.match(after[after.length - 1].url, /timezone=Europe%2FBerlin/);
});
