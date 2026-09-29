// Tests for the Users page on an install with no accounts yet. The quickstart leaves exactly that
// state, a token and no accounts, and the page is where somebody goes next.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// usersPage mounts the Users page with no accounts, and tokens or not as asked.
function usersPage(tokens) {
	return loadPage("users", {
		vars: { TokensExist: tokens },
		routes: [
			[/^\/v1\/users(\?|$)/, reply({ users: [] })],
			[/^\/v1\/tokens(\?|$)/, reply({ tokens: [] })],
			[/^\/v1\/runs(\?|$)/, reply({ runs: [] })],
		],
		quiet: true,
	});
}

// TestATokenInstallIsNotCalledOpen pins the empty state to what secures the install. One with a
// token and no accounts authenticates every request, and the page told it that it ran open.
test("an install with a token and no accounts is not told it runs open", async () => {
	for (const [tokens, want, deny] of [[true, /API token/, /runs open/], [false, /runs open/, /API token/]]) {
		const page = usersPage(tokens);
		await page.app.loadUsers();
		await page.clock.flush();
		const text = page.document.getElementById("status").textContent;
		assert.match(text, want, "tokens=" + tokens + ": the empty state says the wrong thing");
		assert.doesNotMatch(text, deny, "tokens=" + tokens + ": the empty state says the wrong thing");
	}
});

// TestIssueTokenExplainsItNeedsAnAccount pins the token dialog on an install with no accounts. A
// token acts as an account, and the dialog opened with an empty required picker whose only answer
// to a submit was the browser's "Please select an item in the list".
test("the token dialog says a token needs an account when there is none", async () => {
	const page = usersPage(true);
	page.app.wireTokenForm();
	for (let i = 0; i < 5; i++) await page.clock.flush();
	const submit = page.document.querySelector('#token-form button[type="submit"]');
	assert.ok(submit && submit.disabled, "the dialog still offers a submit that cannot work");
	assert.match(page.document.getElementById("token-status").textContent, /Add a user first/,
		"nothing says what to do instead");
});
