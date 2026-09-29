// A session expires mid-read far more often than it fails to start: a token times out, an admin
// revokes it, or the server restarts, and the next thing the page asks for comes back 401. Every
// list page has to leave for sign-in cleanly and remember where it was, rather than parking on a
// permanent placeholder or an error that reads as breakage. Nothing exercised that until here.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply, sequence } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { drive, PARTS, PLACEHOLDER_PAGES, settle, statusLine } from "./failures.mjs";

// UNAUTHORIZED is what the API answers a session it no longer knows.
const UNAUTHORIZED = () => reply({ error: "unauthorized" }, { status: 401 });

for (const entry of PLACEHOLDER_PAGES) {
	test(`${entry.page}: a 401 mid-session routes to sign-in and remembers the page`, async () => {
		const { app, document } = await drive(entry, UNAUTHORIZED());
		const nav = app.location.navigations;
		assert.ok(nav.includes("/ui/login"),
			`${entry.page} did not leave for sign-in on a 401; it went to ${JSON.stringify(nav)}`);
		assert.equal(app.sessionStorage.getItem("st_return"), app.location.pathname,
			`${entry.page} left for sign-in without recording where to send the reader back`);
		// A page on its way to sign-in must not also stamp the placeholder with an error that
		// reads as a server fault; whatever it says, it must not still read as loading.
		const status = statusLine(document);
		assert.notEqual(status.visible, entry.text,
			`${entry.page} still shows "${entry.text}" while walking to sign-in`);
	});
}

test("the redirect flag is raised so timers do not fire into the navigation", async () => {
	const entry = PLACEHOLDER_PAGES.find((e) => e.page === "runs");
	const { app } = await drive(entry, UNAUTHORIZED());
	assert.equal(app.ymRedirecting, true,
		"ymRedirecting was not raised, so the tour and the poll fire into a page that is leaving");
});

test("a page that fans out keeps the return path pointing at itself, not at sign-in", async () => {
	// The overview asks three endpoints at once, so a dead session refuses three times and
	// requireLogin runs once per refusal. Each run rewrites st_return from location.pathname.
	// If any of them ran after the navigation had landed, the return path would read /ui/login
	// and sign-in would send the reader in a circle.
	const entry = PLACEHOLDER_PAGES.find((e) => e.page === "overview");
	const { app } = await drive(entry, UNAUTHORIZED());
	const logins = app.location.navigations.filter((u) => u === "/ui/login");
	assert.ok(logins.length >= 1, "the overview never left for sign-in on a 401");
	assert.equal(app.sessionStorage.getItem("st_return"), "/ui/",
		"the overview asked sign-in to return it somewhere other than the overview");
});

test("a 401 on the sign-in page itself does not loop back to sign-in", async () => {
	const { app } = loadPage("login", { parts: PARTS, routes: [[() => true, UNAUTHORIZED()]], quiet: true });
	app.requireLogin();
	assert.deepEqual(app.location.navigations, [],
		"the sign-in page redirected to itself on a 401, which is an unbreakable loop");
});

test("a 403 explains the role instead of walking to sign-in", async () => {
	// A viewer opening an admin page is not a broken session. Sending them to sign-in would loop:
	// they sign in again, land back, and get the same 403.
	const entry = PLACEHOLDER_PAGES.find((e) => e.page === "audit");
	const { app, document } = await drive(entry, reply({ error: "forbidden" }, { status: 403 }));
	assert.deepEqual(app.location.navigations, [],
		"a 403 walked to sign-in, which loops straight back to the same refusal");
	const status = statusLine(document);
	assert.match(status.visible, /role/i,
		`a 403 said "${status.visible}" rather than explaining it in role terms`);
});

test("a session that expires between the list and a delete walks to sign-in", async () => {
	// The list loads on a live session and the token expires while the reader is looking at it.
	// The refusal arrives on the action, not on the load, which is the path no test covered.
	const routes = [
		["/v1/policies", sequence(
			reply({ policies: [{ id: "pol_1", name: "prod", effect: "hold", created_at: "2026-08-04T12:00:00Z" }] }),
		)],
		["/v1/runs", reply({ runs: [] })],
		["/v1/inventories", reply({ inventories: [] })],
	];
	const { app, document, clock } = loadPage("policies", { parts: PARTS, routes, quiet: true });
	await app.loadPolicies();
	await settle(clock);

	const rows = document.getElementById("policies").children;
	assert.equal(rows.length, 1, "the policy list did not draw its one row");
	const del = rows[0].querySelector("button.danger");
	assert.ok(del, "the policy row has no delete button to drive");

	// The DELETE is what expires.
	app.fetch = async (url, init) => {
		if ((init || {}).method === "DELETE") {
			return { url, status: 401, ok: false, statusText: "401", headers: { get: () => null },
				text: async () => "", json: async () => ({ error: "unauthorized" }) };
		}
		return { url, status: 200, ok: true, statusText: "200", headers: { get: () => null },
			text: async () => "{}", json: async () => ({}) };
	};
	fire(del, "click");
	await settle(clock);

	assert.ok(app.location.navigations.includes("/ui/login"),
		"a 401 on delete left the row in place and never went to sign-in");
});
