// Tests for the launch panel on a read-only install, which is the demo every stranger meets first.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// ROUTES answers the reads the launch form makes, with one record in each list.
const ROUTES = {
	"/v1/projects": reply({ projects: [{ id: "proj_1", name: "web-platform" }] }),
	"/v1/inventories": reply({ inventories: [{ id: "inv_1", name: "production" }] }),
	"/v1/credentials": reply({ credentials: [{ id: "cred_1", name: "prod-ssh", kind: "ssh_key" }] }),
	"/v1/runs": reply({ runs: [], count: 0 }),
	"/v1/templates": reply({ templates: [] }),
	"/v1/schedules": reply({ schedules: [] }),
	"/v1/auth/whoami": reply({ error: "unauthorized" }, { status: 401 }),
};

// mountReadOnlyRuns loads the runs page as a read-only demo serves it and wires the launch panel.
function mountReadOnlyRuns() {
	return loadPage("runs", {
		parts: ALL_PARTS,
		routes: ROUTES,
		vars: { ReadOnly: true, ExtraTools: [] },
	});
}

// TestReadOnlyLaunchFormOffersTheStoredRecords pins that the demo's launch dialog is not empty.
//
// Read-only skipped wiring the launch form outright, so the dialog a visitor is invited to open
// showed a project select holding only "None, local paths", an empty inventory select, and an empty
// credential picker, on an install that seeds two projects, two inventories and four credentials.
// The demo exists to show what using the product looks like, and the one form that shows it asked
// for nothing.
test("a read-only demo fills the launch form from the records it holds", async () => {
	const { app, document, clock } = mountReadOnlyRuns();
	assert.equal(app.isReadOnly(), true, "the page did not mount as a read-only demo");
	fire(document, "DOMContentLoaded");
	await clock.flush();
	const names = (id) => Array.from(document.getElementById(id).options).map((o) => o.textContent);
	assert.ok(names("launch-project").includes("web-platform"),
		"the project select offered nothing the install holds");
	assert.ok(names("launch-inventory-id").includes("production"),
		"the inventory select offered nothing the install holds");
	assert.ok(names("launch-credentials").includes("prod-ssh (ssh_key)"),
		"the credential picker offered nothing the install holds");
});

// TestReadOnlyLaunchRefusesWithAReason pins that the filled form still cannot start a run, and says
// why before the request rather than after the server refuses it.
test("a read-only demo refuses the launch and says why", async () => {
	const { app, document, clock } = mountReadOnlyRuns();
	fire(document, "DOMContentLoaded");
	await clock.flush();
	const go = document.querySelector('#launch-form button[type="submit"]');
	assert.equal(go.disabled, true, "a read-only demo left the launch button live");
	assert.match(document.getElementById("launch-status").textContent, /read-only/i,
		"the panel did not say why launching is unavailable");
});
