// Tests for the cron helpers in 17-cron.js: the one-line cadence sentence, the per-field
// breakdown, and the hover tip that combines them.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "17-cron.js"]);

test("describeCron names the common shapes", () => {
	const tests = [
		// Test 0: Every-minute wildcard pair.
		{ In: "* * * * *", Want: "Every minute" },
		// Test 1: Minute step under a wildcard hour.
		{ In: "*/5 * * * *", Want: "Every 5 minutes" },
		// Test 2: Top of every hour.
		{ In: "0 * * * *", Want: "Hourly, on the hour" },
		// Test 3: A fixed minute past every hour.
		{ In: "30 * * * *", Want: "Hourly at :30" },
		// Test 4: Minute 5 pads to two digits.
		{ In: "5 * * * *", Want: "Hourly at :05" },
		// Test 5: Hour step.
		{ In: "15 */4 * * *", Want: "Every 4 hours" },
		// Test 6: Daily, morning.
		{ In: "0 9 * * *", Want: "Daily at 9:00am" },
		// Test 7: Daily, evening with minutes.
		{ In: "30 18 * * *", Want: "Daily at 6:30pm" },
		// Test 8: Midnight is 12am, not 0am.
		{ In: "0 0 * * *", Want: "Daily at 12:00am" },
		// Test 9: Noon is 12pm.
		{ In: "0 12 * * *", Want: "Daily at 12:00pm" },
		// Test 10: A single-digit minute pads.
		{ In: "5 9 * * *", Want: "Daily at 9:05am" },
		// Test 11: Weekly on a named day.
		{ In: "0 9 * * 1", Want: "Mondays at 9:00am" },
		// Test 12: Day-of-week 7 is Sunday, as cron itself reads it.
		{ In: "0 9 * * 7", Want: "Sundays at 9:00am" },
		// Test 13: Monthly on a day of month.
		{ In: "0 4 1 * *", Want: "Monthly on day 1 at 4:00am" },
		// Test 14: The weekday range shorthand.
		{ In: "30 8 * * 1-5", Want: "Weekdays at 8:30am" },
		// Test 15: The weekend list, in either order.
		{ In: "0 10 * * 6,0", Want: "Weekends at 10:00am" },
		// Test 16: The weekend list, reversed.
		{ In: "0 10 * * 0,6", Want: "Weekends at 10:00am" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.describeCron(tc.In), tc.Want, "test " + i + ": " + tc.In);
	}
});

test("describeCron reports unrecognized shapes as custom", () => {
	const tests = [
		// Test 0: Empty input.
		{ In: "" },
		// Test 1: Undefined input.
		{ In: undefined },
		// Test 2: Four fields is not a five-field expression.
		{ In: "0 0 * *" },
		// Test 3: Six fields is not either.
		{ In: "0 0 * * * *" },
		// Test 4: Nonsense tokens.
		{ In: "a b c d e" },
		// Test 5: Day-of-month plus day-of-week is valid cron, DOM OR DOW, and 0 0 30 2 1 fires
		// on February 30th-or-Mondays; there is no simple sentence for it, so it reads as custom
		// rather than being mislabeled monthly or weekly.
		{ In: "0 0 30 2 1" },
		// Test 6: A weekend list with a wildcard hour has no clock time to print.
		{ In: "0 * * * 6,0" },
		// Test 7: A minute step during one hour has no clock time either.
		{ In: "*/5 9 * * *" },
		// Test 8: The same for a named day.
		{ In: "*/10 9 * * 1" },
		// Test 9: And for the monthly shape.
		{ In: "*/5 8 1 * *" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.describeCron(tc.In), "Custom schedule", "test " + i + ": " + tc.In);
	}
});

test("cronValue names values in named fields and wraps day-of-week 7 to Sunday", () => {
	const fields = app.CRON_FIELDS;
	const [minute, , , month, dow] = fields;
	const tests = [
		// Test 0: A plain number in an unnamed field stays a number.
		{ Token: "30", Field: minute, Want: "30" },
		// Test 1: Month 1 is January; the field is one-based.
		{ Token: "1", Field: month, Want: "January" },
		// Test 2: Month 12 is December.
		{ Token: "12", Field: month, Want: "December" },
		// Test 3: Day 0 is Sunday.
		{ Token: "0", Field: dow, Want: "Sunday" },
		// Test 4: Day 7 wraps to Sunday, matching cron.
		{ Token: "7", Field: dow, Want: "Sunday" },
		// Test 5: Day 6 is Saturday.
		{ Token: "6", Field: dow, Want: "Saturday" },
		// Test 6: A non-numeric token passes through untouched.
		{ Token: "MON", Field: dow, Want: "MON" },
		// Test 7: An out-of-range negative index passes through untouched.
		{ Token: "-1", Field: dow, Want: "-1" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.cronValue(tc.Token, tc.Field), tc.Want, "test " + i + ": " + tc.Token);
	}
});

test("describeCronField reads wildcards, steps, ranges, and lists", () => {
	const [minute, hour, dom, , dow] = app.CRON_FIELDS;
	const tests = [
		// Test 0: A wildcard is the field's own any-phrase.
		{ Spec: "*", Field: minute, Want: "every minute" },
		// Test 1: A question mark reads the same as a wildcard.
		{ Spec: "?", Field: dow, Want: "every day" },
		// Test 2: A step over the wildcard.
		{ Spec: "*/15", Field: minute, Want: "every 15 minutes" },
		// Test 3: A plain range in an unnamed field.
		{ Spec: "9-17", Field: hour, Want: "9 to 17" },
		// Test 4: A range in a named field uses the names.
		{ Spec: "1-5", Field: dow, Want: "Monday to Friday" },
		// Test 5: A step over a range.
		{ Spec: "1-5/2", Field: dow, Want: "every 2 days from Monday to Friday" },
		// Test 6: A step from a single origin.
		{ Spec: "3/2", Field: dow, Want: "every 2 days from Wednesday" },
		// Test 7: A list reads each element.
		{ Spec: "1,15", Field: dom, Want: "1, 15" },
		// Test 8: A list of named values.
		{ Spec: "1,3,5", Field: dow, Want: "Monday, Wednesday, Friday" },
		// Test 9: An unreadable token comes back verbatim.
		{ Spec: "L", Field: dom, Want: "L" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.describeCronField(tc.Spec, tc.Field), tc.Want, "test " + i + ": " + tc.Spec);
	}
});

test("cronBreakdown reads the DOM-OR-DOW expression field by field", () => {
	assert.equal(
		app.cronBreakdown("0 0 30 2 1"),
		"Minute: 0\nHour: 0\nDay of month: 30\nMonth: February\nDay of week: Monday",
	);
});

test("cronBreakdown is empty for anything but five fields", () => {
	assert.equal(app.cronBreakdown("0 0 * *"), "");
	assert.equal(app.cronBreakdown(""), "");
	assert.equal(app.cronBreakdown(undefined), "");
});

test("cronTip combines the sentence and the breakdown, or teaches the format", () => {
	// A recognized cadence leads with its sentence.
	assert.equal(
		app.cronTip("0 9 * * *"),
		"Daily at 9:00am\nMinute: 0\nHour: 9\nDay of month: every day\nMonth: every month\n" +
			"Day of week: every day",
	);
	// The DOM-OR-DOW case still gets a full breakdown, not the fallback: it is a valid expression.
	assert.equal(
		app.cronTip("0 0 30 2 1"),
		"Custom schedule\nMinute: 0\nHour: 0\nDay of month: 30\nMonth: February\nDay of week: Monday",
	);
	// A malformed expression falls back to naming the five fields.
	assert.equal(app.cronTip("not cron"), "Five fields: minute, hour, day of month, month, day of week");
});

// cronInputStub returns a stand-in for the schedules form's cron field that counts the listeners
// subscribed to it, which is the thing being asserted.
function cronInputStub(value) {
	return {
		value,
		dataset: {},
		listeners: [],
		addEventListener(name, fn) { this.listeners.push([name, fn]); },
	};
}

test("wireCronTips subscribes an input once however often it runs", () => {
	const input = cronInputStub("0 2 * * *");
	const root = { querySelectorAll: (sel) => (sel.startsWith("input") ? [input] : []) };

	// The schedules table rewires after every save, but the form field outlives the table, so each
	// pass used to leave another listener on it: after ten saves a keystroke did the work ten times.
	app.wireCronTips(root);
	app.wireCronTips(root);
	app.wireCronTips(root);
	assert.equal(input.listeners.length, 1);
	assert.equal(input.listeners[0][0], "input");
	assert.equal(input.dataset.tip, app.cronTip("0 2 * * *"));

	// The tip still follows the field's current value on a later pass.
	input.value = " 0 9 * * 1 ";
	app.wireCronTips(root);
	assert.equal(input.listeners.length, 1);
	assert.equal(input.dataset.tip, app.cronTip("0 9 * * 1"));
});
