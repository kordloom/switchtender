// Tests for issuing and revoking API tokens from the interface. Until now every token, including the
// agent tokens the product's machine-principal story depends on, could only be minted with a shell on
// the server against the database file. An install without shell access could not give an agent a
// credential, could not see what credentials existed, and could not take one back.
import { test } from "node:test";
import assert from "node:assert/strict";

import { sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";
import { reply, sequence } from "./net.mjs";
import { fire } from "./dom.mjs";

// tokensPage mounts the users page with an account list and a token list.
function tokensPage(tokens, extra) {
	return loadPage("users", {
		routes: Object.assign({
			"/v1/users": reply({ users: [{ id: "usr_1", username: "casey", role: "operator" }] }),
			"/v1/tokens": reply({ tokens: tokens || [], count: (tokens || []).length }),
		}, extra || {}),
	});
}

test("the token list shows every credential and never a secret", async () => {
	const page = tokensPage([
		{
			id: "tok_1", name: "deploy-bot", kind: "agent", user_id: "usr_1",
			created_at: "2026-03-01T12:00:00Z", last_used_at: "2026-03-02T09:00:00Z",
		},
		{ id: "tok_2", name: "casey", kind: "session", user_id: "usr_1", created_at: "2026-03-02T08:00:00Z" },
	]);
	page.app.loadTokens();
	await page.clock.flush();

	const rows = [...page.document.querySelectorAll("#tokens tr")];
	assert.equal(rows.length, 2, "the token list did not render");
	assert.match(rows[0].textContent, /deploy-bot/);
	assert.match(rows[0].textContent, /agent/i, "an agent token does not say it is one");
	assert.match(rows[0].textContent, /casey/, "the token does not say which account it acts as");
	// A browser session is a credential too, and being able to see and end one is the point.
	assert.match(rows[1].textContent, /session/i, "a browser session is not identified as one");
	assert.equal(page.document.getElementById("tokens-table").hidden, false);
});

test("issuing a token shows the secret once and reloads the list", async () => {
	const page = tokensPage([], {
		"/v1/tokens": sequence(
			reply({ tokens: [], count: 0 }),
			reply({ id: "tok_9", name: "deploy-bot", token: "ymt_secret_value", kind: "agent" },
				{ status: 201 }),
			reply({ tokens: [{ id: "tok_9", name: "deploy-bot", kind: "agent", user_id: "usr_1" }], count: 1 }),
		),
	});
	page.app.wireTokenForm();
	page.app.loadTokens();
	await page.clock.flush();

	page.document.getElementById("token-name").value = "deploy-bot";
	page.document.getElementById("token-user").value = "casey";
	page.document.getElementById("token-agent").checked = true;
	page.document.getElementById("token-ttl").value = "720h";
	fire(page.document.getElementById("token-form"), "submit");
	await page.clock.flush();

	const post = page.net.calls.find((c) => c.method === "POST");
	assert.ok(post, "the token was never requested");
	const body = JSON.parse(post.body);
	assert.equal(body.username, "casey", "the token was minted without an account behind it");
	assert.equal(body.kind, "agent", "the agent cap was not requested");
	assert.equal(body.ttl_hours, 720, "the lifetime did not reach the server");

	const reveal = page.document.getElementById("token-secret");
	assert.equal(reveal.hidden, false, "the minted token was never shown, so it is lost");
	assert.equal(page.document.getElementById("token-value").textContent, "ymt_secret_value");

	fire(page.document.getElementById("token-copy"), "click");
	await page.clock.flush();
	assert.deepEqual(sandboxOf(page.app).navigator.clipboard.copied, ["ymt_secret_value"]);
});

test("an unreadable lifetime is refused before anything is minted", async () => {
	const page = tokensPage([]);
	page.app.wireTokenForm();
	await page.clock.flush();
	page.document.getElementById("token-name").value = "bot";
	page.document.getElementById("token-user").value = "casey";
	page.document.getElementById("token-ttl").value = "a while";

	fire(page.document.getElementById("token-form"), "submit");
	await page.clock.flush();
	assert.equal(page.net.calls.filter((c) => c.method === "POST").length, 0,
		"an unreadable lifetime was sent, where it would have become a token that never expires");
	assert.match(page.document.getElementById("token-status").textContent, /24h|720h|lifetime/i,
		"the refusal does not say what the field accepts");
});

test("revoking a token asks first and drops the row", async () => {
	const page = tokensPage([{ id: "tok_1", name: "deploy-bot", kind: "agent", user_id: "usr_1" }], {
		"/v1/tokens/tok_1": reply({ revoked: "tok_1" }),
	});
	page.app.loadTokens();
	await page.clock.flush();

	const del = page.document.querySelector("#tokens tr button.danger, #tokens tr .icon-danger");
	assert.ok(del, "there is no way to revoke a token from the list");
	fire(del, "click");
	await page.clock.flush();
	const sent = page.net.calls.find((c) => c.method === "DELETE");
	assert.ok(sent, "the revocation was never sent");
	assert.match(sent.path, /tok_1$/);
});
