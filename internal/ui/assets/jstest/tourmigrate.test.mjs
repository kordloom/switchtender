// Tests for what the guided tours say about migrating. The pitch tour's step on the Migrate tile
// read "Import from AWX, Semaphore, Rundeck, Jenkins, or a crontab in a single pass." The page that
// tile opens takes six formats, Chef and Puppet among them, and has no way to take a crontab: the
// import endpoint refuses one and says it imports from the command line only. The Migrate page's
// own tour opened on AWX, Semaphore, Rundeck, and Jenkins alone.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";

// MIGRATE_TILE is the overview tile the tour spotlights when it talks about switching.
const MIGRATE_TILE = "#tiles a[href='/ui/migrate']";

// SOURCES matches any tool a migration can come from, so a step that names none is left alone.
const SOURCES = /\b(AWX|Semaphore|Chef|Puppet|Rundeck|Jenkins|crontab|cron)\b/i;

test("the tour steps about migrating name what the page takes, and send a crontab to the command line", () => {
	// The formats are read from the page itself, the select the tile leads to, rather than listed
	// here, so the step is held to what a reader who follows it can actually choose.
	const { app, document } = loadPage("migrate", { quiet: true });
	const formats = Array.from(document.querySelectorAll("#migrate-format option"))
		.map((o) => o.textContent.trim());
	assert.ok(formats.length > 0, "the Migrate page offers no formats, so this checks nothing");
	assert.ok(!formats.some((f) => /cron/i.test(f)), "the Migrate page offers a crontab after all");

	// A step about migrating is one on the Migrate tile or one in the Migrate page's own tour.
	// Other steps may mention AWX in passing without offering to import from it.
	const steps = app.TOURS.flatMap((t) => t.steps.map((s) => Object.assign({ tour: t.id }, s)))
		.filter((s) => (s.sel === MIGRATE_TILE || s.tour === "migrate") && SOURCES.test(s.body));
	assert.ok(steps.some((s) => s.sel === MIGRATE_TILE), "no Migrate tile step names a source");
	assert.ok(steps.some((s) => s.tour === "migrate"), "no Migrate tour step names a source");
	for (const step of steps) {
		for (const format of formats) {
			assert.match(step.body, new RegExp("\\b" + format + "\\b"),
				"the step leaves out " + format + ", which the page takes: " + step.body);
		}
		// A crontab can be mentioned, but only as the command-line import it is.
		if (/cron/i.test(step.body)) {
			assert.match(step.body, /command line/,
				"the step offers a crontab import the page cannot do: " + step.body);
		}
	}
});
