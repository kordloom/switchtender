// Tests for the overview activity chart's window control, filter, and empty-window behavior, added
// with the Grafana-style controls in 15-overview-doctor.js. Pinned off UTC, like the sibling
// activity tests, so a bucketing offset error shows up rather than cancelling to zero.
process.env.TZ = "America/Chicago";

import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";
import { checkBuckets } from "./activitycheck.mjs";

const app = loadParts(["01-boot.js", "15-overview-doctor.js"]);

test("the window pins the column count and the granularity", () => {
	const now = new Date(2026, 7, 3, 12, 30);
	const runs = [{ created_at: "2026-08-03T17:00:00Z", status: "succeeded" }];

	const six = app.activityBuckets(runs, now, { windowH: 6 });
	assert.equal(six.hourly, true);
	assert.equal(six.days.length, 6);

	const day = app.activityBuckets(runs, now, { windowH: 24 });
	assert.equal(day.hourly, true);
	assert.equal(day.days.length, 24);

	const week = app.activityBuckets(runs, now, { windowH: 168 });
	assert.equal(week.hourly, false);
	assert.equal(week.days.length, 7);

	const month = app.activityBuckets(runs, now, { windowH: 720 });
	assert.equal(month.hourly, false);
	assert.equal(month.days.length, 30);

	// The counted run still lands in the window that its column drills into, in every window.
	for (const m of [six, day, week, month]) assert.equal(checkBuckets(app, m, runs), 1);
});

test("the filter narrows the counted runs without dropping columns", () => {
	const now = new Date(2026, 7, 3, 12, 30);
	const runs = [
		{ created_at: "2026-08-03T17:00:00Z", status: "succeeded", playbook: "deploy-web.yml", hosts: ["web01"] },
		{ created_at: "2026-08-03T17:10:00Z", status: "failed", playbook: "deploy-db.yml", hosts: ["db01"] },
		{ created_at: "2026-08-03T17:20:00Z", status: "succeeded", playbook: "deploy-web.yml", hosts: ["web02"] },
	];
	const all = app.activityBuckets(runs, now, { windowH: 12 });
	assert.equal(all.matched, 3);

	const web = app.activityBuckets(runs, now, { windowH: 12, filter: "web" });
	assert.equal(web.matched, 2);
	assert.equal(web.days.length, 12, "filtering hides no columns, only counts");

	const failed = app.activityBuckets(runs, now, { windowH: 12, filter: "failed" });
	assert.equal(failed.matched, 1);

	const db = app.activityBuckets(runs, now, { windowH: 12, filter: "db01" });
	assert.equal(db.matched, 1);
});

test("a window with nothing in it still draws its axis instead of blanking", () => {
	const now = new Date(2026, 7, 3, 12, 30);
	// A filter that matches nothing, and an empty run set, both keep the frame under the window path.
	const empty = app.activityBuckets([], now, { windowH: 12 });
	assert.notEqual(empty, null);
	assert.equal(empty.days.length, 12);
	assert.equal(empty.matched, 0);
	assert.equal(empty.max, 1, "an empty window still has a positive scale, so no divide by zero");

	const runs = [{ created_at: "2026-08-03T17:00:00Z", status: "succeeded" }];
	const noMatch = app.activityBuckets(runs, now, { windowH: 12, filter: "no-such-run" });
	assert.equal(noMatch.matched, 0);
	assert.equal(noMatch.days.length, 12);

	// The auto path, with no window, still returns null on nothing, the property its own tests pin.
	assert.equal(app.activityBuckets([], now), null);
});

test("the filter matches the fields a run actually carries", () => {
	const now = new Date(2026, 7, 3, 12, 30);
	const runs = [
		{ created_at: "2026-08-03T17:00:00Z", status: "succeeded", playbook: "site.yml",
			inventory: "prod.ini", actor: "deploy-bot", source: "schedule", labels: { env: "prod" } },
		{ created_at: "2026-08-03T17:05:00Z", status: "failed", playbook: "db.yml",
			inventory: "staging.ini", actor: "admin", source: "template", labels: { env: "staging" } },
	];
	const only = (q, n) => assert.equal(app.activityBuckets(runs, now, { windowH: 12, filter: q }).matched, n,
		"filter " + q);
	only("deploy-bot", 1); // actor
	only("schedule", 1);   // source
	only("prod", 1);       // label value (and prod.ini inventory on the same run)
	only("staging", 1);    // inventory / label on the other run
	only("site", 1);       // playbook / name
	only("failed", 1);     // status
	only("nomatch-xyz", 0);
})
