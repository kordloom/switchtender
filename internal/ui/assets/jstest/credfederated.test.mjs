// Tests for the federated credential kinds in the credential dialog and table. A federated credential
// stores no secret, so the dialog must not ask for one or send one, and the table must not call it
// unfinished.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// mount opens the credentials page, recording the body of any create.
function mount(list) {
	const posted = [];
	const routes = {
		"/v1/credentials": (req) => {
			if (req.method === "POST") {
				posted.push(JSON.parse(req.body));
				return reply({ id: "cred-new" });
			}
			return reply({ credentials: list || [], count: (list || []).length, sealing: true });
		},
		"/v1/templates": reply({ templates: [] }),
	};
	return { ...loadPage("credentials", { parts: ALL_PARTS, routes }), posted };
}

test("a federated kind hides the secret and source and asks for settings", async () => {
	const { app, document } = mount();
	app.wireCredentialForm();
	const kind = document.getElementById("cred-kind");
	kind.value = "aws_oidc";
	fire(kind, "change");
	assert.equal(document.getElementById("cred-secret-field").hidden, true,
		"the dialog asked for a secret a federated credential never stores");
	assert.equal(document.getElementById("cred-source-field").hidden, true,
		"the dialog offered a source for a credential that reads from none");
	assert.equal(document.getElementById("cred-secret").disabled, true,
		"the hidden secret box still takes part in the form");
	assert.match(document.getElementById("cred-settings").placeholder, /role_arn=/);

	kind.value = "ssh_key";
	fire(kind, "change");
	assert.equal(document.getElementById("cred-secret-field").hidden, false,
		"leaving a federated kind left the secret box hidden");
	assert.equal(document.getElementById("cred-secret").disabled, false);
});

test("a federated credential is saved with settings and no secret or source", async () => {
	const { app, document, clock, posted } = mount();
	app.wireCredentialForm();
	document.getElementById("cred-name").value = "aws-prod";
	// Typed into the secret box before switching kind, which is how a stray value would leak.
	document.getElementById("cred-secret").value = "left over";
	const kind = document.getElementById("cred-kind");
	kind.value = "aws_oidc";
	fire(kind, "change");
	document.getElementById("cred-settings").value =
		"role_arn=arn:aws:iam::123456789012:role/deploy\nenvironment=prod";
	fire(document.getElementById("cred-form"), "submit");
	await clock.flush();
	assert.equal(posted.length, 1);
	assert.equal(posted[0].kind, "aws_oidc");
	assert.equal(posted[0].secret, undefined, "the leftover secret was sent with a federated credential");
	assert.equal(posted[0].source, undefined, "a source was sent with a federated credential");
	assert.deepEqual(posted[0].settings,
		{ role_arn: "arn:aws:iam::123456789012:role/deploy", environment: "prod" });
});

test("the table shows a federated credential as minted per run, not as unfinished", async () => {
	const { app, document, clock } = mount([{ id: "c1", name: "aws-prod", kind: "aws_oidc",
		source: "local", needs_secret: false, created_at: "2026-10-01T10:00:00Z",
		settings: { role_arn: "arn:aws:iam::123456789012:role/deploy" } }]);
	await app.loadCredentials();
	await clock.flush();
	const text = document.getElementById("credentials").textContent;
	assert.match(text, /minted per run/);
	assert.doesNotMatch(text, /needs a secret/);
});
