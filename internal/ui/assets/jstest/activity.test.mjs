// Tests for the overview activity chart's bucketing in 15-overview-doctor.js. The columns are cut
// on local calendar hours and days, and clicking one drills into a time window, so the window a
// bar links to has to be the window that bar counted. The whole class of bug here only shows up
// away from UTC, so the process is pinned to America/Chicago (UTC-5 in August) before the parts
// are loaded. Under UTC every offset error is zero and the tests would pass either way.
process.env.TZ = "America/Chicago";

import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";
import { checkBuckets } from "./activitycheck.mjs";

const app = loadParts(["01-boot.js", "15-overview-doctor.js"]);

test("hourly columns drill into the hour they counted", () => {
	// Noon and a half, local time, on a day with a five hour offset from UTC.
	const now = new Date(2026, 7, 3, 12, 30);
	const runs = [
		// 09:30 local.
		{ created_at: "2026-08-03T14:30:00Z", status: "succeeded" },
		{ created_at: "2026-08-03T14:45:00Z", status: "failed" },
		// 12:59 local, the newest column.
		{ created_at: "2026-08-03T17:59:00Z", status: "running" },
		// 01:15 local, the oldest column.
		{ created_at: "2026-08-03T06:15:00Z", status: "succeeded" },
		// 00:59 local, one minute older than the chart reaches.
		{ created_at: "2026-08-03T05:59:00Z", status: "succeeded" },
	];
	const model = app.activityBuckets(runs, now);
	assert.equal(model.hourly, true);
	assert.equal(model.days.length, 12);
	assert.equal(checkBuckets(app, model, runs), 4);

	// The 09:00 local column is 14:00Z to 15:00Z, not the 19:00Z the local reading of its key gives.
	const nine = model.days.find((d) => d.key === "2026-08-03T09");
	assert.equal(nine.start.toISOString(), "2026-08-03T14:00:00.000Z");
	assert.equal(nine.end.toISOString(), "2026-08-03T15:00:00.000Z");
	assert.equal(nine.succeeded, 1);
	assert.equal(nine.failed, 1);
	assert.equal(nine.other, 0);

	// The run an hour older than the oldest column is left out rather than folded into it.
	assert.equal(model.days[0].key, "2026-08-03T01");
	assert.equal(model.days[0].succeeded, 1);
	assert.equal(model.max, 2);
});

test("daily columns drill into the day they counted", () => {
	const now = new Date(2026, 7, 3, 12, 30);
	const runs = [
		// 23:30 local on July 31, which is already August 1 in UTC.
		{ created_at: "2026-08-01T04:30:00Z", status: "succeeded" },
		// 19:00 local on August 2.
		{ created_at: "2026-08-03T00:00:00Z", status: "failed" },
		// Today.
		{ created_at: "2026-08-03T15:00:00Z", status: "succeeded" },
		// Older than the fourteen day window, and old enough to force daily buckets.
		{ created_at: "2026-07-01T12:00:00Z", status: "succeeded" },
	];
	const model = app.activityBuckets(runs, now);
	assert.equal(model.hourly, false);
	assert.equal(model.days.length, 14);
	assert.equal(checkBuckets(app, model, runs), 3);

	// A run at 23:30 local belongs to that local day, and the window is local midnight to local
	// midnight, which is 05:00Z to 05:00Z here.
	const july31 = model.days.find((d) => d.key === "2026-07-31");
	assert.equal(july31.start.toISOString(), "2026-07-31T05:00:00.000Z");
	assert.equal(july31.end.toISOString(), "2026-08-01T05:00:00.000Z");
	assert.equal(july31.succeeded, 1);

	// The run at midnight UTC is the evening before in local time, so it counts on August 2.
	assert.equal(model.days.find((d) => d.key === "2026-08-02").failed, 1);
	assert.equal(model.days.find((d) => d.key === "2026-08-03").succeeded, 1);
});

test("activityKey and the bucket window agree at the boundaries", () => {
	const now = new Date(2026, 7, 3, 12, 30);
	const hourStart = new Date(2026, 7, 3, 9);
	const tests = [
		// Test 0: The first instant of the hour is in it.
		{ At: hourStart, Want: "2026-08-03T09" },
		// Test 1: The last instant of the hour is still in it.
		{ At: new Date(hourStart.getTime() + 3599999), Want: "2026-08-03T09" },
		// Test 2: The first instant of the next hour has moved on.
		{ At: new Date(hourStart.getTime() + 3600000), Want: "2026-08-03T10" },
		// Test 3: Local midnight belongs to the day starting, not the day ending.
		{ At: new Date(2026, 7, 3, 0, 0, 0), Want: "2026-08-03T00" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.activityKey(tc.At, true), tc.Want, "test " + i);
	}
	assert.equal(app.activityKey(hourStart, false), "2026-08-03");
	assert.equal(app.activityBucketEnd(hourStart, true).toISOString(), "2026-08-03T15:00:00.000Z");
	assert.equal(app.activityBucketEnd(hourStart, false).toISOString(), "2026-08-04T05:00:00.000Z");
	assert.equal(app.activityBuckets([], now), null);
	// Runs that all fall outside the window leave nothing to draw.
	assert.equal(app.activityBuckets([{ created_at: "2020-01-01T00:00:00Z" }], now), null);
});
