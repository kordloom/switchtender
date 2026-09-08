// A fresh install answers every list endpoint with nothing in it, which is the first thing a
// visitor sees and the one state the happy-path suite never drives. The failure it hides is not an
// error: it is a page whose placeholder is hidden, whose table is hidden, and whose middle is
// therefore blank, with nothing saying the list is empty rather than broken.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { drive, PLACEHOLDER_PAGES, statusLine } from "./failures.mjs";

// NOTHING answers every list endpoint at once with a well-formed empty result, so one response
// serves whichever page is being driven.
const NOTHING = {
	runs: [], hosts: [], entries: [], policies: [], users: [], templates: [], projects: [],
	inventories: [], sources: [], workers: [], credentials: [], schedules: [], tasks: [],
	findings: [], events: [], shards: [], steps: [], tokens: [],
	count: 0, next_after: 0, has_more: false,
	checked_templates: 0, checked_schedules: 0, checked_credentials: 0,
	summary: { total: 0, succeeded: 0, failed: 0 },
};

// LIST_PAGES are the pages whose whole content is one list, so an empty answer must produce a
// stated empty state. The run detail and comparison pages are about one object rather than a list
// and are checked separately below.
const LIST_PAGES = PLACEHOLDER_PAGES.filter((e) => !["detail", "compare", "overview"].includes(e.page));

for (const entry of LIST_PAGES) {
	test(`${entry.page}: an empty list says so instead of leaving a blank page`, async () => {
		const { document } = await drive(entry, reply(NOTHING));
		const status = statusLine(document);
		assert.notEqual(status.visible, entry.text,
			`${entry.page} still reads "${entry.text}" when the server said the list is empty`);
		assert.ok(status.visible.length > 0,
			`${entry.page} went blank on an empty list: no placeholder, no empty state, nothing`);
		assert.ok(!/fail|error|unavailable/i.test(status.visible),
			`${entry.page} reported an empty list as a failure: "${status.visible}"`);
		// showEmpty draws a card rather than a bare line, which is what separates a designed empty
		// state from a status message nobody styled.
		assert.equal(status.el.className, "empty-state",
			`${entry.page} answered an empty list with a plain status line, not an empty state`);
		assert.ok(status.el.querySelector("p"),
			`${entry.page} drew an empty state with no sentence in it`);
	});

	test(`${entry.page}: an empty list hides the table rather than showing bare headers`, async () => {
		const { document } = await drive(entry, reply(NOTHING));
		for (const table of document.querySelectorAll("table.runs")) {
			assert.equal(table.hidden, true,
				`${entry.page} left an empty table with only its column headers on show`);
		}
	});
}

test("an empty runs list keeps the search box when the emptiness is the query's fault", async () => {
	// Hiding the toolbar on a no-match search takes away the box the person is typing in, which
	// turns a typo into a dead end only a reload can leave.
	const entry = PLACEHOLDER_PAGES.find((e) => e.page === "runs");
	const { document } = await drive(entry, reply(NOTHING), {
		search: "?q=nothingmatchesthis",
		before: ({ document: doc }) => { doc.getElementById("runs-search").value = "nothingmatchesthis"; },
	});
	const status = statusLine(document);
	assert.match(status.visible, /match/i,
		`a filtered empty runs list said "${status.visible}" rather than naming the search`);
	for (const bar of document.querySelectorAll(".runs-toolbar")) {
		assert.notEqual(bar.hidden, true, "the runs toolbar hid itself on a no-match search");
	}
});

test("an unfiltered empty runs list says the install is new, not that the search missed", async () => {
	const entry = PLACEHOLDER_PAGES.find((e) => e.page === "runs");
	const { document } = await drive(entry, reply(NOTHING));
	const status = statusLine(document);
	assert.ok(!/match/i.test(status.visible),
		`an unfiltered empty runs list blamed the search: "${status.visible}"`);
});

test("an overview with nothing in it still draws its cards rather than going blank", async () => {
	const entry = PLACEHOLDER_PAGES.find((e) => e.page === "overview");
	const { document } = await drive(entry, reply(NOTHING));
	const status = statusLine(document);
	assert.equal(status.visible, "",
		`the overview reported an empty install as a problem: "${status.visible}"`);
	const metrics = document.getElementById("ov-metrics");
	assert.ok(metrics && (metrics.textContent || "").trim().length > 0,
		"a fresh install's overview has no metric cards at all, so the page reads as broken");
});

test("a comparison with no hosts and no tasks says so rather than emptying the page", async () => {
	const entry = PLACEHOLDER_PAGES.find((e) => e.page === "compare");
	const empty = {
		a: { id: "run_new", status: "succeeded" }, b: { id: "run_old", status: "succeeded" },
		same_source: true, duration_delta_seconds: 0,
		totals: { ok: 0, broke: 0, recovered: 0, still_failing: 0, added: 0, removed: 0 },
		hosts: [], tasks: [],
	};
	const { document } = await drive(entry, reply(empty));
	// Both sections hide themselves when their list is empty and the status line hides on success,
	// so the only thing left has to carry the answer.
	const header = (document.getElementById("compare-header").textContent || "").trim();
	const summary = document.getElementById("compare-summary");
	assert.ok(header.length > 0 || (summary && !summary.hidden),
		"a comparison with nothing in it left the page with no content at all");
});
