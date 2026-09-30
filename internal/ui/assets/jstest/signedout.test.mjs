// Tests for a signed-out visit to an install that requires sign-in. Every page asked the server for
// its data first and was refused, so the first page a stranger opened logged a 401 in the console
// for each request, three of them on the overview, before the redirect to the sign-in page landed.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";

// routesFor answers the overview's reads the way the install does: one that requires sign-in
// refuses a request carrying no token, and one that runs open answers everybody.
function routesFor(signIn) {
	return [[(req) => req.url.startsWith("/v1/"), (req) => (req.headers.Authorization || !signIn
		? reply({ runs: [], hosts: [], ok: true, count: 0, anchored: 0 })
		: reply({ error: "authentication required" }, { status: 401 }))]];
}

// bootOverview mounts the overview as the server renders it, optionally holding a session, and runs
// the page's startup.
async function bootOverview(signIn, token) {
	const page = loadPage("overview",
		{ vars: { SignIn: signIn }, routes: routesFor(signIn), quiet: true });
	if (token) sandboxOf(page.app).localStorage.setItem("st_token", token);
	fire(page.document, "DOMContentLoaded");
	await page.clock.flush();
	return { page, sandbox: sandboxOf(page.app) };
}

test("a signed-out visitor is sent to sign in before the page asks for anything", async () => {
	const { page, sandbox } = await bootOverview(true, "");
	assert.deepEqual(page.net.urls, [],
		"the page asked for data the server was always going to refuse");
	assert.deepEqual(sandbox.location.navigations, ["/ui/login"]);
	assert.equal(sandbox.sessionStorage.getItem("st_return"), "/ui/",
		"signing in would not come back to the page the visitor asked for");
});

test("a visitor holding a session loads the page as before", async () => {
	const { page, sandbox } = await bootOverview(true, "swt_session");
	assert.ok(page.net.urls.includes("/v1/runs"), "a signed-in overview did not load its runs");
	assert.deepEqual(sandbox.location.navigations, []);
});

test("an install that runs open loads the page with no session at all", async () => {
	const { page, sandbox } = await bootOverview(false, "");
	assert.ok(page.net.urls.includes("/v1/runs"), "an open install's overview did not load its runs");
	assert.equal(sandbox.location.navigations.includes("/ui/login"), false,
		"an open install sent its visitor to a sign-in page it does not need");
});
