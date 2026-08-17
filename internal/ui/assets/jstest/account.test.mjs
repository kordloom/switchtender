// Tests for the account control in the navigation. Every page could be reached while signed in and
// none of them said who you were signed in as or let you stop being that person: the sign-in page
// was reachable only by being thrown at it by a 401, and arriving there while already signed in left
// no way back. An install shared by an operator and an auditor had no way to hand the browser over.
import { test } from "node:test";
import assert from "node:assert/strict";

import { sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";
import { fire } from "./dom.mjs";

test("the navigation names the signed-in account and signs it out", async () => {
	const page = loadPage("overview");
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_token", "ymt_abc");
	win.localStorage.setItem("st_role", "operator");
	win.localStorage.setItem("st_user", "casey");
	page.app.buildNav();

	const group = page.document.querySelector(".account-group");
	assert.ok(group, "no account control anywhere in the navigation");
	assert.match(group.textContent, /casey/, "the account control does not name who is signed in");
	assert.match(group.textContent, /[Oo]perator/, "the account control does not show the role");

	const out = page.document.querySelector(".account-signout");
	assert.ok(out, "there is no way to sign out");
	fire(out, "click");
	await page.clock.flush();

	assert.equal(win.localStorage.getItem("st_token"), null,
		"signing out left the session token behind");
	assert.equal(win.localStorage.getItem("st_role"), null, "signing out left the cached role behind");
	assert.equal(win.localStorage.getItem("st_user"), null,
		"signing out left the cached username behind");
	assert.equal(win.location.navigations.at(-1), "/ui/login",
		"signing out did not land on sign in");
});

test("a signed-out session is offered sign-in rather than an account", () => {
	const page = loadPage("overview");
	page.app.buildNav();

	const group = page.document.querySelector(".account-group");
	assert.ok(group, "the navigation drops the account control when nobody is signed in");
	assert.equal(page.document.querySelector(".account-signout"), null,
		"a signed-out session is offered a sign out");
	assert.ok(group.querySelector("a[href='/ui/login']"),
		"a signed-out session has no way to reach sign in");
});

test("the sign-in page offers a way back when a session is already stored", () => {
	const page = loadPage("login");
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_token", "ymt_abc");
	win.localStorage.setItem("st_user", "casey");
	page.app.offerStoredSession();

	const back = page.document.getElementById("signed-in-return");
	assert.ok(back, "sign in is a dead end for a browser that already holds a session");
	assert.equal(back.hidden, false, "the way back is hidden from a signed-in reader");
	assert.match(back.textContent, /casey/, "the way back does not say whose session is stored");
});
