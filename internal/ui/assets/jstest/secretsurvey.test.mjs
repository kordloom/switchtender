// Tests for a secret survey field in the launch dialog. Its answer is typed into a password field
// that is never echoed or autofilled, and its default, which the server shows only as set, is never
// put into the field: leaving it empty is what uses the default, and anything else would launch with
// the mask itself as the answer.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// PARTS covers the survey dialog and what it calls into.
const PARTS = [
	"01-boot.js", "07-nav-theme.js", "08-auth-status.js", "09-audit.js", "10-modals-credentials.js",
	"12-templates-notify.js", "13-fileviewer-inventory.js", "16-runs-list.js", "18-host-page.js",
	"19-cron-preview.js", "20-held-copy-stream.js",
];

// TEMPLATE asks one secret question with a sealed default and one plain question.
const TEMPLATE = {
	id: "tpl-1", name: "Rotate", survey: [
		{ var: "db_password", label: "DB password", type: "secret", default: "[redacted]" },
		{ var: "env", label: "Env", type: "text", default: "prod" },
	],
};

// launchCapture returns the routes for a launch and the list its request bodies are recorded in.
function launchCapture() {
	const bodies = [];
	const routes = {
		"/v1/templates/tpl-1/launch": (request) => {
			bodies.push(JSON.parse(request.body));
			return reply({ id: "run-1" });
		},
	};
	return [bodies, routes];
}

test("a secret question is a password field that never shows its default", async () => {
	const [, routes] = launchCapture();
	const { app, document } = loadPage("jobtemplates", { parts: PARTS, routes });
	app.openSurvey(TEMPLATE);

	const secret = document.querySelector('#survey-form [data-var="db_password"]');
	assert.equal(secret.type, "password", "a secret answer would be echoed on screen");
	assert.equal(secret.getAttribute("autocomplete"), "new-password",
		"the browser would offer a remembered password as the answer");
	assert.equal(secret.value, "", "the mask for a set default was put in the field as an answer");
	assert.match(secret.placeholder, /default is set/i, "nothing says a default stands behind the field");

	const plain = document.querySelector('#survey-form [data-var="env"]');
	assert.equal(plain.type, "text");
	assert.equal(plain.value, "prod", "a plain default stopped being offered");
});

test("an empty secret answer is left out so the sealed default applies", async () => {
	const [bodies, routes] = launchCapture();
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: PARTS, routes });
	app.openSurvey(TEMPLATE);
	fire(document.getElementById("survey-go"), "click");
	await clock.flush();

	assert.deepEqual(bodies, [{ answers: { env: "prod" } }],
		"the launch sent a value for the secret question the operator left empty");
	net.assertClean();
});

test("a typed secret answer is sent under its variable", async () => {
	const [bodies, routes] = launchCapture();
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: PARTS, routes });
	app.openSurvey(TEMPLATE);
	document.querySelector('#survey-form [data-var="db_password"]').value = "typed-secret";
	fire(document.getElementById("survey-go"), "click");
	await clock.flush();

	assert.deepEqual(bodies, [{ answers: { db_password: "typed-secret", env: "prod" } }]);
	net.assertClean();
});
