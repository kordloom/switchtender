// Tests for the panel that lists credentials still waiting for a secret. Setting one there stores
// it, and the table under the panel kept calling that credential "needs a secret" until the page
// was reloaded, while the API already answered needs_secret false for it.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// secretCells reads the Secret column of the credentials table, one entry per row.
function secretCells(document) {
	return Array.from(document.querySelectorAll("#credentials tr"))
		.map((tr) => tr.cells[3].textContent);
}

test("a secret set in the panel reads as set in the table without a reload", async () => {
	let stored = false;
	const routes = {
		// The PUT stores the secret, and every list read after it reports the credential as usable.
		"/v1/credentials/cred_1": (req) => {
			assert.equal(req.method, "PUT");
			stored = true;
			return reply({ id: "cred_1" });
		},
		"/v1/credentials": () => reply({ credentials: [
			{ id: "cred_1", name: "prod-ssh", kind: "ssh_key", source: "local", needs_secret: !stored },
			{ id: "cred_2", name: "aws-prod", kind: "aws", source: "local", needs_secret: true },
		] }),
	};
	const { app, document, net, clock } = loadPage("credentials", { routes });
	await app.loadCredentials();
	await clock.flush();
	assert.deepEqual(secretCells(document), ["needs a secret", "needs a secret"]);

	const [first, second] = document.querySelectorAll(".cred-needs-row");
	first.querySelector("textarea").value = "-----BEGIN OPENSSH PRIVATE KEY-----";
	// A second secret is half pasted when the first is saved. Refreshing the table must not take it.
	second.querySelector("textarea").value = "access_key=AKIAEXAMPLE";
	fire(first.querySelector("button"), "click");
	await clock.flush();

	assert.deepEqual(secretCells(document), ["set", "needs a secret"],
		"the table still says the saved credential needs a secret");
	assert.equal(document.querySelectorAll("#credentials tr").length, 2,
		"the refresh duplicated rows");
	assert.equal(second.querySelector("textarea").value, "access_key=AKIAEXAMPLE",
		"refreshing the table threw away a secret pasted into the panel");
	assert.match(document.querySelector(".cred-needs-head strong").textContent, /^1 credential needs/);
	net.assertClean();
});
