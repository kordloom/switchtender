// What a page does while the server says nothing, and what it does when the server answers in a
// way that never lets the page finish. Nothing in the shipped script sets a request deadline, so
// the first group here pins the behavior a reader gets on a stalled connection rather than
// claiming it is fine. The second group is the case that does not resolve on its own: a paging
// loop whose cursor the server left out spins forever, hammering the endpoint and never drawing.
import { test } from "node:test";
import assert from "node:assert/strict";

import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { PARTS, PLACEHOLDER_PAGES, settle, statusLine } from "./failures.mjs";

// stalled returns a response that never settles, the way a connection held open by a wedged proxy
// behaves before any TCP timeout fires.
function stalled() {
	return deferred().promise;
}

for (const entry of PLACEHOLDER_PAGES) {
	test(`${entry.page}: a stalled request leaves the placeholder up and nothing else`, async () => {
		// No fetch in the script carries an AbortSignal, so there is no client deadline: the page
		// waits on the browser's own. This pins that the wait is a stated placeholder rather than a
		// blank page or a half-drawn table, which is the part a reader can act on.
		const routes = [[() => true, stalled()]];
		const { app, document, clock } = loadPage(entry.page, {
			parts: PARTS, routes, vars: entry.vars || {}, quiet: true,
		});
		entry.load(app);
		await settle(clock);
		// Ten minutes of virtual time. Nothing is scheduled to rescue the page.
		await clock.tick(600000);
		await settle(clock);

		// A wait reads either as the page's own placeholder or as skeleton rows in the table the
		// data is going to fill. What it must never be is a blank middle of the page.
		const status = statusLine(document);
		const skeletons = document.querySelectorAll(".skeleton-row").length;
		assert.ok(status.visible.length > 0 || skeletons > 0,
			`${entry.page} shows nothing at all while its request hangs`);
		if (!skeletons) {
			assert.equal(status.visible, entry.text,
				`${entry.page} says "${status.visible}" while its request hangs, which is not its wait`);
		}
		assert.deepEqual(app.location.navigations, [],
			`${entry.page} navigated somewhere on its own while a request was merely slow`);
	});
}

test("a stalled runs load leaves skeleton rows, not a table of real-looking blanks", async () => {
	const routes = [[() => true, stalled()]];
	const { app, document, clock } = loadPage("runs", { parts: PARTS, routes, quiet: true });
	app.loadRuns();
	await settle(clock);
	const rows = document.getElementById("runs").children;
	assert.ok(rows.length > 0, "the runs table is empty while loading, so the page reads as broken");
	for (const row of rows) {
		assert.ok(row.className.includes("skeleton-row"),
			"a waiting runs table drew a row that is not marked as a placeholder");
	}
});

test("the event pager stops when the server omits its cursor instead of asking forever", async () => {
	// loadAllEvents follows data.next_after. A full page with the cursor missing sets `after` to
	// undefined, so the very same URL is requested again, forever: the run page never renders and
	// the server is asked for the same 5000 events without end. A truncating proxy, an older
	// server, or one field dropped from the response is enough to reach it.
	const BATCH = 5000;
	const page = { events: [] };
	for (let i = 0; i < BATCH; i++) page.events.push({ seq: i + 1, kind: "task", host: "web01" });

	let calls = 0;
	const routes = [[(req) => req.url.includes("/events"), () => {
		calls++;
		// The loop must stop on its own. Answer the same full page each time, minus the cursor.
		if (calls > 20) return reply({ events: [] });
		return reply(page);
	}]];
	const { app, clock } = loadPage("detail", {
		parts: PARTS, routes, quiet: true, vars: { RunID: "run_1", MatrixCap: "2000" },
	});

	const done = app.loadAllEvents("run_1");
	await settle(clock, 60);
	assert.ok(calls <= 20,
		`loadAllEvents asked for the same page ${calls} times with no cursor and never stopped`);
	await done;
});

test("the event pager does not re-request the page it just read", async () => {
	// The weaker guarantee that would also close the loop above: whatever the server sends back,
	// the next request must be for a different window.
	const BATCH = 5000;
	const page = { events: [], next_after: undefined };
	for (let i = 0; i < BATCH; i++) page.events.push({ seq: i + 1, kind: "task", host: "web01" });

	const asked = [];
	let calls = 0;
	const routes = [[(req) => req.url.includes("/events"), (req) => {
		asked.push(req.url);
		calls++;
		return calls > 5 ? reply({ events: [] }) : reply(page);
	}]];
	const { app, clock } = loadPage("detail", {
		parts: PARTS, routes, quiet: true, vars: { RunID: "run_1", MatrixCap: "2000" },
	});
	const done = app.loadAllEvents("run_1");
	await settle(clock, 30);
	await done;

	const unique = new Set(asked);
	assert.equal(unique.size, asked.length,
		`the event pager asked for the same window twice: ${JSON.stringify(asked.slice(0, 4))}`);
});
