// Tests for the overview's chain check. The verdict is read by admins only, and the overview asked
// for it on every session, so each operator's and viewer's overview left a 403 in the console for a
// tile it then drew nothing in.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { sandboxOf } from "./loader.mjs";

// overviewAs loads the overview as the given role and returns what it asked the server for.
async function overviewAs(role) {
	const page = loadPage("overview", {
		routes: {
			"/v1/runs": reply({ runs: [] }),
			"/v1/fleet": reply({ hosts: [] }),
			"/v1/audit/verify": reply({ ok: true, count: 3, anchored: 1 }),
		},
	});
	if (role) sandboxOf(page.app).localStorage.setItem("st_role", role);
	await page.app.loadOverview();
	await page.clock.flush();
	return page.net.urls;
}

test("an operator's overview does not ask for the chain verdict", async () => {
	for (const role of ["operator", "viewer"]) {
		const urls = await overviewAs(role);
		assert.ok(!urls.some((u) => u.includes("/audit/verify")),
			role + " asked for the admin-only chain verdict: " + urls);
		assert.ok(urls.some((u) => u.includes("/v1/runs")), role + " no longer loads runs");
	}
});

test("an admin's overview and an open install still ask for it", async () => {
	for (const role of ["admin", ""]) {
		const urls = await overviewAs(role);
		assert.ok(urls.some((u) => u.includes("/audit/verify")),
			(role || "an open install") + " stopped asking for the chain verdict: " + urls);
	}
});
