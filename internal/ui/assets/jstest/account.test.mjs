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
	win.localStorage.setItem("st_token", "swt_abc");
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
	win.localStorage.setItem("st_token", "swt_abc");
	win.localStorage.setItem("st_user", "casey");
	page.app.offerStoredSession();

	const back = page.document.getElementById("signed-in-return");
	assert.ok(back, "sign in is a dead end for a browser that already holds a session");
	assert.equal(back.hidden, false, "the way back is hidden from a signed-in reader");
	assert.match(back.textContent, /casey/, "the way back does not say whose session is stored");
});

test("a single sign-on fragment replaces every cached detail of the previous account", () => {
	const tests = [
		{ // Test 0: A fragment naming a role and a user leaves nothing of the previous account.
			Fragment: "#access_token=tok_new&role=viewer&user=newperson",
			WantRole: "viewer", WantUser: "newperson", WantAccount: null, WantName: "newperson",
		},
		{ // Test 1: A fragment carrying only a token leaves the role and the names unknown.
			Fragment: "#access_token=tok_new",
			WantRole: null, WantUser: null, WantAccount: null, WantName: "",
		},
	];
	for (const [i, tc] of tests.entries()) {
		const page = loadPage("overview");
		const win = sandboxOf(page.app);
		win.localStorage.setItem("st_token", "tok_old");
		win.localStorage.setItem("st_role", "admin");
		win.localStorage.setItem("st_user", "oldperson");
		win.localStorage.setItem("st_account", "oldperson-renamed");
		win.sessionStorage.setItem("st_sso_pending", String(Date.now()));
		win.location.hash = tc.Fragment;
		page.app.consumeSSOFragment();

		assert.equal(win.localStorage.getItem("st_token"), "tok_new", "test " + i + ": token");
		assert.equal(win.localStorage.getItem("st_role"), tc.WantRole, "test " + i + ": role");
		assert.equal(win.localStorage.getItem("st_user"), tc.WantUser, "test " + i + ": user");
		assert.equal(win.localStorage.getItem("st_account"), tc.WantAccount,
			"test " + i + ": the previous account's name survived the new sign-in");
		assert.equal(page.app.accountName(), tc.WantName, "test " + i + ": badge name");
	}
});
