// Tests for the federation signing key section on the credentials page: the listing, the normal
// rotation that goes straight to the server, the emergency rotation that states its cost before it
// sends anything, and the audit sentences for the entries both leave on the chain.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS, loadParts, sandboxOf } from "./loader.mjs";

const HOUR = 3600 * 1000;

// iso returns the time offset from now by ms, as the API writes it.
function iso(ms) {
	return new Date(Date.now() + ms).toISOString();
}

// signingKey is a key that has signed since an hour ago with nothing scheduled.
function signingKey(id) {
	return { id, algorithm: "RS256", state: "signing", created_at: iso(-HOUR),
		activated_at: iso(-HOUR), signing: true, published: true };
}

// pendingKey is a key a normal rotation published, starting to sign in 23 hours.
function pendingKey(id) {
	return { id, algorithm: "RS256", state: "pending", created_at: iso(-HOUR),
		activated_at: iso(23 * HOUR), signing: false, published: true };
}

// mount opens the credentials page signed in with role, answering the key listing with keys or with
// the status given, and recording every rotation request.
function mount(role, listing, rotations) {
	const posted = [];
	const routes = [
		[(req) => req.path === "/v1/federation/keys", listing],
		[(req) => req.path.startsWith("/v1/federation/keys/rotate"), (req) => {
			posted.push(req.method + " " + req.path);
			return rotations[req.path];
		}],
		[(req) => req.path.startsWith("/v1/credential"), reply({ credentials: [], types: [], count: 0 })],
		[(req) => req.path === "/v1/templates", reply({ templates: [] })],
	];
	const page = loadPage("credentials", { parts: ALL_PARTS, routes });
	sandboxOf(page.app).localStorage.setItem("st_role", role);
	return { ...page, posted };
}

test("the section stays hidden on an install with federation off", async () => {
	const { app, document, clock } = mount("admin",
		reply({ error: "workload identity federation is not configured" }, { status: 404 }), {});
	await app.loadFederationKeys();
	await clock.flush();
	assert.equal(document.getElementById("fedkey-section").hidden, true,
		"a 404 from the listing drew a section about keys that do not exist");
});

test("the listing draws each key's state and times, and holds a second rotation back", async () => {
	const keys = [
		{ id: "kid_removed_0000000000000000000000000000000", algorithm: "RS256", state: "removed",
			created_at: iso(-72 * HOUR), activated_at: iso(-72 * HOUR), retired_at: iso(-48 * HOUR),
			removed_at: iso(-24 * HOUR), signing: false, published: false },
		signingKey("kid_signing"),
		pendingKey("kid_pending"),
	];
	const { app, document, clock } = mount("admin",
		reply({ issuer: "https://st.example.com", keys }), {});
	await app.loadFederationKeys();
	await clock.flush();
	assert.equal(document.getElementById("fedkey-section").hidden, false);
	const rows = document.querySelectorAll("#fedkeys tr");
	assert.equal(rows.length, 3);
	const states = Array.from(rows).map((tr) => tr.cells[1].textContent);
	assert.deepEqual(states, ["Removed", "Signing", "Pending"]);
	assert.equal(rows[0].cells[0].dataset.tip, keys[0].id, "the full key id is not kept with its row");
	assert.match(rows[2].cells[3].textContent, /^in 23h/,
		"the pending key does not say when it signs");
	assert.equal(rows[1].cells[4].textContent, "Not scheduled");
	const rotate = document.getElementById("fedkey-rotate");
	assert.equal(rotate.disabled, true, "a second rotation was offered while one is under way");
	assert.match(rotate.dataset.tip, /under way/);
	assert.match(document.getElementById("fedkey-status").textContent, /starts signing in 23h/);
	assert.match(document.getElementById("fedkey-intro").textContent,
		/https:\/\/st\.example\.com\/\.well-known\/jwks\.json/);
	assert.equal(document.getElementById("fedkey-emergency-open").disabled, false,
		"an emergency rotation must stay available while a normal one is under way");
});

test("a normal rotation posts once and redraws with the key it published", async () => {
	const pending = pendingKey("kid_next");
	const { app, document, clock, posted } = mount("admin",
		reply({ issuer: "https://st.example.com", keys: [signingKey("kid_now")] }), {
			"/v1/federation/keys/rotate": reply({ key: pending, emergency: false,
				keys: [signingKey("kid_now"), pending] }),
		});
	app.wireFederationKeys();
	await app.loadFederationKeys();
	await clock.flush();
	const rotate = document.getElementById("fedkey-rotate");
	assert.equal(rotate.disabled, false);
	fire(rotate, "click");
	fire(rotate, "click");
	await clock.flush();
	assert.deepEqual(posted, ["POST /v1/federation/keys/rotate"], "a double click rotated twice");
	assert.equal(document.querySelectorAll("#fedkeys tr").length, 2);
	assert.match(document.getElementById("fedkey-status").textContent,
		/Published key kid_next\. It starts signing in 23h/);
	assert.equal(rotate.disabled, true, "the redraw still offers a rotation while one is under way");
});

test("a refused rotation shows the server's reason", async () => {
	const { app, document, clock } = mount("admin",
		reply({ issuer: "https://st.example.com", keys: [signingKey("kid_now")] }), {
			"/v1/federation/keys/rotate": reply({ error: "a key rotation is already under way: key " +
				"kid_next starts signing at 2026-10-02T09:00:00Z" }, { status: 409 }),
		});
	app.wireFederationKeys();
	await app.loadFederationKeys();
	await clock.flush();
	fire(document.getElementById("fedkey-rotate"), "click");
	await clock.flush();
	assert.match(document.getElementById("fedkey-status").textContent,
		/Rotation refused: a key rotation is already under way: key kid_next/);
});

test("an emergency rotation says what it costs and sends nothing until confirmed", async () => {
	const fresh = signingKey("kid_fresh");
	const { app, document, clock, posted } = mount("admin",
		reply({ issuer: "https://st.example.com", keys: [signingKey("kid_old")] }), {
			"/v1/federation/keys/rotate/emergency": reply({ key: fresh, emergency: true, keys: [
				{ id: "kid_old", state: "removed", created_at: iso(-HOUR), activated_at: iso(-HOUR),
					retired_at: iso(0), removed_at: iso(0), signing: false, published: false },
				fresh,
			] }),
		});
	app.wireFederationKeys();
	await app.loadFederationKeys();
	await clock.flush();
	fire(document.getElementById("fedkey-emergency-open"), "click");
	await clock.flush();
	const modal = document.getElementById("fedkey-emergency-modal");
	assert.equal(modal.hidden, false);
	assert.match(modal.textContent, /Runs fail for a while/);
	assert.match(modal.textContent, /cannot be undone/);
	assert.match(modal.textContent, /a key a normal rotation published/,
		"the dialog does not say it also removes a key a pending normal rotation published");
	assert.match(modal.textContent, /a suspected compromise of one is treated as a compromise of all/,
		"the dialog does not say why every stored key goes");
	assert.match(modal.textContent, /every other key leaves the published set at once, not only the one signing/,
		"the dialog does not say the rotation removes more than the key that signs");
	const help = document.querySelector(".fedkey-help").textContent;
	assert.match(help, /not only the one signing/,
		"the rotation help does not say an emergency rotation removes more than the key that signs");
	assert.match(help, /stored the same way, so a suspected compromise of one is treated as a compromise of all/,
		"the rotation help does not say why every stored key goes");
	assert.match(document.getElementById("fedkey-emergency-open").dataset.tip, /every key/,
		"the emergency button's tip reads as replacing one key");
	assert.deepEqual(posted, [], "opening the dialog rotated without a confirmation");
	fire(document.getElementById("fedkey-emergency-close"), "click");
	assert.equal(modal.hidden, true);
	assert.deepEqual(posted, [], "closing the dialog rotated anyway");

	fire(document.getElementById("fedkey-emergency-open"), "click");
	fire(document.getElementById("fedkey-emergency-confirm"), "click");
	await clock.flush();
	assert.deepEqual(posted, ["POST /v1/federation/keys/rotate/emergency"]);
	assert.equal(modal.hidden, true, "the dialog stayed open after the rotation went through");
	const states = Array.from(document.querySelectorAll("#fedkeys tr"))
		.map((tr) => tr.cells[1].textContent);
	assert.deepEqual(states, ["Removed", "Signing"]);
	assert.match(document.getElementById("fedkey-status").textContent, /Key kid_fresh signs now/);
});

test("a viewer reads the keys and is offered no rotation", async () => {
	const { app, document, clock } = mount("viewer",
		reply({ issuer: "https://st.example.com", keys: [signingKey("kid_now")] }), {});
	await app.loadFederationKeys();
	await clock.flush();
	assert.equal(document.getElementById("fedkey-section").hidden, false);
	assert.equal(document.querySelectorAll("#fedkeys tr").length, 1);
	assert.equal(document.getElementById("fedkey-rotate").hidden, true);
	assert.equal(document.getElementById("fedkey-emergency-open").hidden, true);
});

test("the audit page reads token issuances and rotations as sentences", () => {
	const app = loadParts(["01-boot.js", "09-audit.js"]);
	const tests = [
		// Test 0: A token issuance names the run, the credential, and the signing key.
		{
			Method: "TOKEN", Path: "/runs/run_abc/federation/cred_aws/kid/kid_1/jti/j1/exp/1790846100",
			Want: "Issued an identity token for run run_abc under credential cred_aws, signed by key kid_1",
		},
		// Test 1: A normal rotation says when the new key signs.
		{
			Method: "POST", Path: "/v1/federation/keys/rotate",
			Want: "Rotated the federation signing key, the new key signing in 24 hours",
		},
		// Test 2: An emergency rotation reads as one, and says it removed every other key.
		{
			Method: "POST", Path: "/v1/federation/keys/rotate/emergency",
			Want: "Emergency rotation, every other federation signing key removed",
		},
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.auditChange(tc.Method, tc.Path), tc.Want, "test " + i + ": " + tc.Path);
	}
});
