// Integration tests for the credential create dialog. A create writes a new credential, so a fast
// double submit used to store it twice. The submit is now guarded so a second submit while the first
// is still posting is dropped, and the button comes back once the request settles either way, since a
// modal save stays on the page rather than navigating. Each form here is submitted the way a reader
// double clicks Save, and the assertion is how many credentials the server was asked to create.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

test("the credential form stores one credential however fast Save is clicked", async () => {
	const gate = deferred();
	let creates = 0;
	const routes = {
		// The create POST is held open so a second submit lands while the first is in flight; the same
		// path answers the table reload GET the save fires on success.
		"/v1/credentials": (req) => {
			if (req.method === "POST") { creates++; return gate.promise; }
			return reply({ credentials: [] });
		},
		"/v1/templates": reply({ templates: [] }),
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	app.wireCredentialForm();

	const form = document.getElementById("cred-form");
	const button = form.querySelector('button[type="submit"]');
	document.getElementById("cred-name").value = "prod-ssh";
	document.getElementById("cred-secret").value = "the secret";

	// Three submits in the time it takes to double click, before the first has come back.
	const events = [fire(form, "submit"), fire(form, "submit"), fire(form, "submit")];

	assert.equal(creates, 1, "a repeat submit created the credential a second time");
	assert.equal(button.disabled, true, "the Save button stayed live while the credential was saving");
	// Every submit is canceled, the dropped ones included, or a dropped submit posts the form the old
	// fashioned way and the browser navigates.
	assert.deepEqual(events.map((e) => e.defaultPrevented), [true, true, true]);

	gate.resolve(reply({ id: "cred-1" }));
	await clock.flush();
	assert.equal(creates, 1);
	// A modal save stays on the page, so the button comes back for the next credential.
	assert.equal(button.disabled, false, "a saved credential left the Save button dead");
	assert.equal(document.getElementById("cred-status").textContent, "Saved.");
	net.assertClean();
});

test("a rejected credential save hands the dialog back to the operator", async () => {
	let attempts = 0;
	const routes = {
		"/v1/credentials": (req) => {
			if (req.method !== "POST") return reply({ credentials: [] });
			attempts++;
			return attempts === 1 ? reply({ error: "kind is required" }, { status: 400 }) : reply({ id: "cred-9" });
		},
		"/v1/templates": reply({ templates: [] }),
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	app.wireCredentialForm();

	const form = document.getElementById("cred-form");
	const button = form.querySelector('button[type="submit"]');
	document.getElementById("cred-name").value = "prod-ssh";
	document.getElementById("cred-secret").value = "the secret";

	fire(form, "submit");
	await clock.flush();
	assert.equal(attempts, 1);
	assert.equal(button.disabled, false, "a failed save left the button dead, so nobody can retry");
	assert.equal(document.getElementById("cred-status").textContent, "Save failed: kind is required");

	// Corrected, the same button saves, which is the whole point of re-enabling it.
	fire(form, "submit");
	await clock.flush();
	assert.equal(attempts, 2);
	assert.equal(document.getElementById("cred-status").textContent, "Saved.");
	net.assertClean();
});
