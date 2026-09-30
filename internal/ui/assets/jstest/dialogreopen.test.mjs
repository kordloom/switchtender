// Tests for a dialog reopened after a failed attempt. The launch dialog kept the error from the
// last launch that failed, so opening it again for a fresh launch showed "Launch failed" beside a
// form that had not been submitted yet. Every other create dialog clears its status line when it
// opens.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// ROUTES answers the launch form's reads with empty lists and refuses the launch itself.
const ROUTES = {
	"/v1/runs": (req) => (req.method === "POST"
		? reply({ error: "command is required for the bash tool" }, { status: 400 })
		: reply({ runs: [] })),
	"/v1/credentials": reply({ credentials: [] }),
	"/v1/projects": reply({ projects: [] }),
	"/v1/inventories": reply({ inventories: [] }),
};

// mountLaunch loads the runs page with its launch dialog wired the way the page's startup wires it.
function mountLaunch(vars) {
	const page = loadPage("runs", { routes: ROUTES, vars: Object.assign({ ExtraTools: [] }, vars) });
	page.app.wireModal("launch");
	page.app.wireLaunchForm();
	return page;
}

test("reopening the launch dialog does not show the last launch's failure", async () => {
	const { document, clock, net } = mountLaunch({});
	const status = document.getElementById("launch-status");
	fire(document.getElementById("launch-open"), "click");
	document.getElementById("launch-tool").value = "bash";
	fire(document.getElementById("launch-form"), "submit");
	await clock.flush();
	assert.match(status.textContent, /^Launch failed: command is required/);

	fire(document.getElementById("launch-close"), "click");
	fire(document.getElementById("launch-open"), "click");
	assert.equal(status.textContent, "", "the reopened dialog still reports the failed launch");
	net.assertClean();
});

test("a read-only install still says why it launches nothing when the dialog reopens", async () => {
	const { document, clock } = mountLaunch({ ReadOnly: true });
	await clock.flush();
	const status = document.getElementById("launch-status");
	const reason = status.textContent;
	assert.match(reason, /read-only/);
	fire(document.getElementById("launch-open"), "click");
	fire(document.getElementById("launch-close"), "click");
	fire(document.getElementById("launch-open"), "click");
	assert.equal(status.textContent, reason, "reopening the dialog wiped the read-only explanation");
});

test("the token dialog still says why it cannot issue a token once it is open", async () => {
	// With no account to bind a token to, the dialog disables Save and says why in its status line,
	// which is inside the dialog. Opening the dialog reset that line, so the reason was never seen:
	// the reader met a dead Save button and nothing else.
	const { app, document, clock } = loadPage("users", {
		routes: { "/v1/users": reply({ users: [] }), "/v1/tokens": reply({ tokens: [] }) },
	});
	app.wireModal("token");
	app.wireTokenForm();
	await clock.flush();
	fire(document.getElementById("token-open"), "click");
	assert.equal(document.querySelector('#token-form button[type="submit"]').disabled, true);
	assert.match(document.getElementById("token-status").textContent,
		/has none yet\. Add a user first/, "the open dialog does not say why Save is disabled");
});
