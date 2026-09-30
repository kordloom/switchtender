// Tests for the activity columns on the Users page. Runs fired and Last seen were read off the
// newest runs by the name each was fired under, which is the username only for a browser session,
// so a run fired with a token named ci counted for nobody. And seen meant only "fired a run", so an
// account that signed in and worked without launching anything read "never", including the one
// signed in and reading the page.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// ago returns the ISO time the given number of milliseconds before now.
function ago(ms) {
	return new Date(Date.now() - ms).toISOString();
}

test("an account is last seen when one of its credentials was used, and has its runs", async () => {
	const signedIn = ago(30 * 1000);
	const tokenUsed = ago(2 * 60 * 1000);
	const routes = {
		"/v1/users": reply({ users: [
			{ id: "user_admin", username: "admin", role: "admin", created_at: ago(86400000) },
			{ id: "user_opal", username: "opal", role: "operator", created_at: ago(86400000) },
			{ id: "user_idle", username: "idle", role: "viewer", created_at: ago(86400000) },
		] }),
		"/v1/runs": reply({ runs: [
			// Fired with opal's token named ci, so the run is named for the token, not the person.
			{ id: "run_2", actor: "ci", actor_user_id: "user_opal", created_at: ago(10 * 60 * 1000) },
			// Recorded before runs carried the account, in a browser session named for the username.
			{ id: "run_1", actor: "admin", created_at: ago(3 * 3600 * 1000) },
		] }),
		"/v1/tokens": reply({ tokens: [
			{ id: "tok_1", name: "admin", user_id: "user_admin", kind: "session", last_used_at: signedIn },
			{ id: "tok_2", name: "ci", user_id: "user_opal", last_used_at: tokenUsed },
		] }),
	};
	const { app, document, clock, net } = loadPage("users", { routes });
	await app.loadUsers();
	await clock.flush();

	const rows = new Map(Array.from(document.querySelectorAll("#users tr")).map((tr) => [
		tr.cells[0].textContent,
		{ fired: tr.cells[4].textContent, seen: tr.cells[5].dataset.time || tr.cells[5].textContent },
	]));
	assert.deepEqual(rows.get("admin"), { fired: "1", seen: signedIn },
		"the signed-in admin was not seen at sign-in, or lost the run fired in a session");
	assert.deepEqual(rows.get("opal"), { fired: "1", seen: tokenUsed },
		"the run fired with opal's token, or the token's use, did not count for opal");
	assert.deepEqual(rows.get("idle"), { fired: "0", seen: "never" });
	net.assertClean();
});
