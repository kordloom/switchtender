// activitycheck holds the properties the overview activity chart's buckets must satisfy in any
// timezone. Two test files exercise them, one west of UTC and one east, because an offset error in
// the bucketing only shows up on one side or the other and never under UTC.
import assert from "node:assert/strict";

// checkBuckets asserts that each column's key is the key of its own start, that the columns tile
// the range without gap or overlap, and that every run a column counted falls inside the window
// that column drills into. It returns how many runs the model counted.
export function checkBuckets(app, model, runs) {
	let counted = 0;
	for (const [i, day] of model.days.entries()) {
		assert.equal(app.activityKey(day.start, model.hourly), day.key,
			"column " + i + " key round trip");
		assert.ok(day.end.getTime() > day.start.getTime(), "column " + i + " window runs forward");
		if (i + 1 < model.days.length) {
			assert.equal(day.end.getTime(), model.days[i + 1].start.getTime(),
				"column " + i + " ends where the next begins");
		}
		counted += day.succeeded + day.failed + day.other;
	}
	for (const r of runs) {
		const at = new Date(r.created_at);
		const day = model.days.find((d) => d.key === app.activityKey(at, model.hourly));
		if (!day) continue;
		assert.ok(at.getTime() >= day.start.getTime() && at.getTime() < day.end.getTime(),
			r.created_at + " must fall inside the window its column drills into, " +
			day.start.toISOString() + " to " + day.end.toISOString());
	}
	return counted;
}
