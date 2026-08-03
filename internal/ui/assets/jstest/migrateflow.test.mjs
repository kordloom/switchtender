// Integration tests for the migrate Import button on the projects migration page. Import writes the
// whole plan, so a double click used to import it twice. The button is now routed through the same
// guard the launch form uses, so a repeat click while the first request is in flight is dropped, and
// a rejected import hands the button back for a retry. Each button here is clicked the way a reader
// double clicks one, and the assertion is how many times the server was asked to import.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// EXPORT is a minimal non-empty body, enough to get past the paste check and reach the endpoint.
const EXPORT = '{"projects":[]}';

test("the migrate import writes the plan once however fast Import is clicked", async () => {
	const gate = deferred();
	let count = 0;
	const routes = {
		"/v1/import/awx": () => { count++; return gate.promise; },
	};
	const { app, document, net, clock } = loadPage("migrate", { parts: ALL_PARTS, routes });
	app.wireMigrate();

	document.getElementById("migrate-format").value = "awx";
	document.getElementById("migrate-export").value = EXPORT;
	const button = document.getElementById("migrate-apply");

	// Three clicks in the time it takes to double click, before the first import has come back.
	fire(button, "click");
	fire(button, "click");
	fire(button, "click");

	assert.equal(count, 1, "a repeat Import click imported the whole plan a second time");
	assert.equal(button.disabled, true, "the Import button stayed live while the import was running");

	gate.resolve(reply({ applied: true, created: 2, projects: ["web"] }));
	await clock.flush();
	assert.equal(count, 1);
	// A finished import leaves the plan in, so the button stays down behind it, as the launch form's does.
	assert.equal(button.disabled, true);
	assert.equal(document.getElementById("migrate-status").textContent, "Imported 2 objects.");
	net.assertClean();
});

test("a rejected migrate import hands Import back to the operator", async () => {
	let attempts = 0;
	const routes = {
		"/v1/import/awx": () => {
			attempts++;
			return attempts === 1
				? reply({ error: "invalid export" }, { status: 400 })
				: reply({ applied: true, created: 1, projects: ["web"] });
		},
	};
	const { app, document, net, clock } = loadPage("migrate", { parts: ALL_PARTS, routes });
	app.wireMigrate();

	document.getElementById("migrate-format").value = "awx";
	document.getElementById("migrate-export").value = EXPORT;
	const button = document.getElementById("migrate-apply");

	fire(button, "click");
	await clock.flush();
	assert.equal(attempts, 1);
	assert.equal(button.disabled, false, "a failed import left the button dead, so nobody can retry");
	assert.equal(document.getElementById("migrate-status").textContent, "Import failed: invalid export");

	// Corrected, the same button imports, which is the whole point of re-enabling it.
	fire(button, "click");
	await clock.flush();
	assert.equal(attempts, 2);
	assert.equal(document.getElementById("migrate-status").textContent, "Imported 1 objects.");
	net.assertClean();
});
