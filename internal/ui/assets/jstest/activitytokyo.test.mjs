// The same activity chart properties as activity.test.mjs, run east of UTC. Asia/Tokyo is UTC+9
// with no daylight saving, and the daily columns only go wrong on this side: a local midnight read
// as an ISO date lands on the previous day, so both the column's label and the window it drills
// into slip by a day. Each test file runs in its own process, which is what makes a second
// timezone possible.
process.env.TZ = "Asia/Tokyo";

import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";
import { checkBuckets } from "./activitycheck.mjs";

const app = loadParts(["01-boot.js", "15-overview-doctor.js"]);

test("daily columns east of UTC drill into the day they counted", () => {
	const now = new Date(2026, 7, 3, 12, 30);
	const runs = [
		// 11:00 local on August 3.
		{ created_at: "2026-08-03T02:00:00Z", status: "succeeded" },
		// 00:30 local on August 3, which is still August 2 in UTC.
		{ created_at: "2026-08-02T15:30:00Z", status: "failed" },
		// 23:30 local on August 2.
		{ created_at: "2026-08-02T14:30:00Z", status: "succeeded" },
		// Old enough to force daily buckets, and outside the fourteen day window.
		{ created_at: "2026-07-01T12:00:00Z", status: "succeeded" },
	];
	const model = app.activityBuckets(runs, now);
	assert.equal(model.hourly, false);
	assert.equal(checkBuckets(app, model, runs), 3);

	// The August 3 column runs from local midnight to local midnight, 2026-08-02T15:00Z onward,
	// and holds the two runs that happened on August 3 local time.
	const aug3 = model.days[model.days.length - 1];
	assert.equal(aug3.key, "2026-08-03");
	assert.equal(aug3.start.toISOString(), "2026-08-02T15:00:00.000Z");
	assert.equal(aug3.end.toISOString(), "2026-08-03T15:00:00.000Z");
	assert.equal(aug3.succeeded, 1);
	assert.equal(aug3.failed, 1);

	const aug2 = model.days.find((d) => d.key === "2026-08-02");
	assert.equal(aug2.start.toISOString(), "2026-08-01T15:00:00.000Z");
	assert.equal(aug2.succeeded, 1);
});

test("hourly columns east of UTC drill into the hour they counted", () => {
	const now = new Date(2026, 7, 3, 12, 30);
	const runs = [
		{ created_at: "2026-08-03T03:15:00Z", status: "succeeded" },
		{ created_at: "2026-08-03T02:45:00Z", status: "failed" },
	];
	const model = app.activityBuckets(runs, now);
	assert.equal(model.hourly, true);
	assert.equal(checkBuckets(app, model, runs), 2);

	// 12:00 local on August 3 is 03:00Z, not the 03:00 local a reparsed key would give.
	const noon = model.days[model.days.length - 1];
	assert.equal(noon.key, "2026-08-03T12");
	assert.equal(noon.start.toISOString(), "2026-08-03T03:00:00.000Z");
	assert.equal(noon.end.toISOString(), "2026-08-03T04:00:00.000Z");
	assert.equal(noon.succeeded, 1);
});
