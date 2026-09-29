// Tests for the credentials page on an install that cannot seal a new secret.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// FOUR is what the public demo holds: real seeded credentials on a server with no encryption key.
const FOUR = [
	{ id: "c1", name: "prod-ssh", kind: "ssh_key", created_at: "2026-09-13T10:00:00Z" },
	{ id: "c2", name: "ansible-vault", kind: "vault_password", created_at: "2026-09-13T10:00:00Z" },
	{ id: "c3", name: "dockerhub", kind: "registry", created_at: "2026-09-13T10:00:00Z" },
	{ id: "c4", name: "openstack-prod", kind: "openstack", created_at: "2026-09-13T10:00:00Z" },
];

// mount opens the credentials page against a list with the given sealing state.
function mount(sealing, creds, vars) {
	return loadPage("credentials", {
		parts: ALL_PARTS,
		vars: vars || {},
		routes: {
			"/v1/credentials": reply({ credentials: creds, count: creds.length, sealing }),
			"/v1/templates": reply({ templates: [] }),
		},
	});
}

// TestSealingOffStillShowsTheRows pins the regression that blanked the flagship demo.
//
// Saying an install cannot store a new secret is worth doing, but it is a fact about writing. Gating
// the render on it discarded four real credentials on demo.switchtender.com while the launch dialog
// two clicks away listed the same four by name, so one install contradicted itself in one session.
test("an install that cannot seal still lists the credentials it holds", async () => {
	const { app, document, clock } = mount(false, FOUR);
	await app.loadCredentials();
	await clock.flush();
	const rows = document.getElementById("credentials").querySelectorAll("tr");
	assert.equal(rows.length, 4, "credentials the install holds were hidden because it cannot store new ones");
	assert.match(document.querySelector("main.content").textContent, /prod-ssh/);
	assert.ok(document.querySelector(".seal-notice"), "nothing said why New credential is disabled");
	const add = document.querySelector(".page-head .button.primary");
	assert.equal(add.disabled, true, "the control that would write was left enabled");
});

// TestSealingOffAndEmptySaysBoth pins that the empty case still explains itself rather than showing
// the ordinary "add one" invitation an install cannot honor.
test("an install that cannot seal and holds none says both things", async () => {
	const { app, document, clock } = mount(false, []);
	await app.loadCredentials();
	await clock.flush();
	const text = document.querySelector("main.content").textContent;
	assert.match(text, /could not seal one anyway/i, "the empty state invited an action the server refuses");
});

// TestSealingOnIsUnchanged pins that a normal install sees no notice.
test("an install that can seal shows its rows and no notice", async () => {
	const { app, document, clock } = mount(true, FOUR);
	await app.loadCredentials();
	await clock.flush();
	assert.equal(document.getElementById("credentials").querySelectorAll("tr").length, 4);
	assert.equal(document.querySelector(".seal-notice"), null,
		"a working install was told it cannot seal credentials");
});
