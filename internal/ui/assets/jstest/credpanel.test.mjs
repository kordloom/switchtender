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

// cred builds a stored local credential as the list endpoint returns it.
function cred(id, name, kind, needsSecret) {
	return { id, name, kind, source: "local", needs_secret: needsSecret };
}

// panelNames reads the credentials the panel still lists, in order.
function panelNames(document) {
	return Array.from(document.querySelectorAll(".cred-needs-row .cred-needs-name"))
		.map((el) => el.textContent);
}

// deleteFromTable presses Delete on the credentials table row for the named credential.
function deleteFromTable(document, name) {
	const row = Array.from(document.querySelectorAll("#credentials tr"))
		.find((tr) => tr.cells[0].textContent === name);
	assert.ok(row, "the table has no row for " + name);
	fire(row.querySelector("button.danger"), "click");
}

test("a credential deleted from the table leaves the panel without a reload", async () => {
	// The reverse of the case above. Delete took the row out of the table and left the credential
	// in the panel, still asking for a secret for something that no longer exists, until a reload.
	const live = new Map([
		cred("cred_1", "prod-ssh", "ssh_key", true),
		cred("cred_2", "aws-prod", "aws", true),
		cred("cred_3", "vault-pass", "vault_password", true),
		cred("cred_4", "deploy-token", "token", false),
	].map((c) => [c.id, c]));
	const routes = [
		[/^\/v1\/credentials\/cred_\d$/, (req) => {
			const id = req.path.split("/").pop();
			if (req.method === "DELETE") {
				live.delete(id);
				return reply({}, { status: 204 });
			}
			assert.equal(req.method, "PUT");
			live.get(id).needs_secret = false;
			return reply({ id });
		}],
		["/v1/credentials", () => reply({ credentials: Array.from(live.values()) })],
	];
	const { app, document, net, clock } = loadPage("credentials", { routes });
	await app.loadCredentials();
	await clock.flush();
	assert.deepEqual(panelNames(document), ["prod-ssh", "aws-prod", "vault-pass"]);
	const title = () => document.querySelector(".cred-needs-head strong").textContent;
	const panel = document.getElementById("cred-needs");

	// A secret is half pasted into another row of the panel when the delete lands.
	const pasting = document.querySelectorAll(".cred-needs-row textarea")[1];
	pasting.value = "access_key=AKIAEXAMPLE";
	deleteFromTable(document, "prod-ssh");
	await clock.flush();
	assert.deepEqual(panelNames(document), ["aws-prod", "vault-pass"],
		"the panel still asks for a secret for the deleted credential");
	assert.match(title(), /^2 credentials need a secret/);
	assert.equal(pasting.value, "access_key=AKIAEXAMPLE", "the delete threw away a pasted secret");

	// Deleting one that never needed a secret leaves the panel as it is.
	deleteFromTable(document, "deploy-token");
	await clock.flush();
	assert.deepEqual(panelNames(document), ["aws-prod", "vault-pass"]);
	assert.equal(pasting.value, "access_key=AKIAEXAMPLE");

	// The panel counts what it still lists, so a save there after a delete empties it exactly.
	deleteFromTable(document, "vault-pass");
	await clock.flush();
	assert.deepEqual(panelNames(document), ["aws-prod"]);
	assert.match(title(), /^1 credential needs a secret/);
	fire(document.querySelector(".cred-needs-row button"), "click");
	await clock.flush();
	assert.deepEqual(panelNames(document), []);
	assert.equal(panel.hidden, true, "the panel stayed up with nothing left in it");
	net.assertClean();
});

test("deleting the last credential that needs a secret takes the panel down", async () => {
	const live = [
		cred("cred_1", "prod-ssh", "ssh_key", true),
		cred("cred_2", "ci-token", "token", false),
	];
	const routes = [
		[/^\/v1\/credentials\/cred_\d$/, (req) => {
			assert.equal(req.method, "DELETE");
			live.splice(live.findIndex((c) => req.path.endsWith("/" + c.id)), 1);
			return reply({}, { status: 204 });
		}],
		["/v1/credentials", () => reply({ credentials: live.slice() })],
	];
	const { app, document, net, clock } = loadPage("credentials", { routes });
	await app.loadCredentials();
	await clock.flush();
	const panel = document.getElementById("cred-needs");
	assert.equal(panel.hidden, false);
	deleteFromTable(document, "prod-ssh");
	await clock.flush();
	assert.equal(panel.hidden, true, "the panel still asks for a secret for a deleted credential");
	assert.deepEqual(panelNames(document), []);
	net.assertClean();
});
