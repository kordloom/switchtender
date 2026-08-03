// Integration tests for the template create dialog, which carried the same duplicate-submit defect
// the credential dialog did: a create writes a new template, so a fast double submit stored it twice.
// The submit is now guarded so a second submit while the first is still posting is dropped, and the
// button comes back once the request settles either way, since a modal save stays on the page. Each
// form here is submitted the way a reader double clicks Save, and the assertion is how many templates
// the server was asked to create.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

test("the template form creates one template however fast Save is clicked", async () => {
	const gate = deferred();
	let creates = 0;
	const routes = {
		// The project and credential pickers load their options when the form is wired.
		"/v1/projects": reply({ projects: [] }),
		"/v1/credentials": reply({ credentials: [] }),
		// The create POST is held open so a second submit lands while the first is in flight; the same
		// path answers the table reload GET the save fires on success.
		"/v1/templates": (req) => {
			if (req.method === "POST") { creates++; return gate.promise; }
			return reply({ templates: [] });
		},
	};
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: ALL_PARTS, routes });
	app.wireTemplateForm();
	// Let the wire-time picker lookups settle before the form is used.
	await clock.flush();

	const form = document.getElementById("template-form");
	// The harness stubs no form.reset, a browser API the success path calls to clear the dialog. A
	// real browser resets the fields; here a no-op stands in so the save can run to completion.
	form.reset = () => {};
	const button = form.querySelector('button[type="submit"]');
	document.getElementById("tpl-name").value = "deploy production";
	document.getElementById("tpl-playbook").value = "plays/site.yml";

	// Three submits in the time it takes to double click, before the first has come back.
	const events = [fire(form, "submit"), fire(form, "submit"), fire(form, "submit")];

	assert.equal(creates, 1, "a repeat submit created the template a second time");
	assert.equal(button.disabled, true, "the Save button stayed live while the template was saving");
	// Every submit is canceled, the dropped ones included, or a dropped submit posts the form the old
	// fashioned way and the browser navigates.
	assert.deepEqual(events.map((e) => e.defaultPrevented), [true, true, true]);

	gate.resolve(reply({ id: "tpl-1" }));
	await clock.flush();
	assert.equal(creates, 1);
	// A modal save stays on the page, so the button comes back for the next template.
	assert.equal(button.disabled, false, "a saved template left the Save button dead");
	assert.equal(document.getElementById("tpl-status").textContent, "Saved.");
	net.assertClean();
});

test("a rejected template save hands the dialog back to the operator", async () => {
	let attempts = 0;
	const routes = {
		"/v1/projects": reply({ projects: [] }),
		"/v1/credentials": reply({ credentials: [] }),
		"/v1/templates": (req) => {
			if (req.method !== "POST") return reply({ templates: [] });
			attempts++;
			return attempts === 1 ? reply({ error: "name is required" }, { status: 400 }) : reply({ id: "tpl-9" });
		},
	};
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: ALL_PARTS, routes });
	app.wireTemplateForm();
	await clock.flush();

	const form = document.getElementById("template-form");
	form.reset = () => {};
	const button = form.querySelector('button[type="submit"]');
	document.getElementById("tpl-name").value = "deploy production";
	document.getElementById("tpl-playbook").value = "plays/site.yml";

	fire(form, "submit");
	await clock.flush();
	assert.equal(attempts, 1);
	assert.equal(button.disabled, false, "a failed save left the button dead, so nobody can retry");
	assert.equal(document.getElementById("tpl-status").textContent, "Save failed: name is required");

	// Corrected, the same button saves, which is the whole point of re-enabling it.
	fire(form, "submit");
	await clock.flush();
	assert.equal(attempts, 2);
	assert.equal(document.getElementById("tpl-status").textContent, "Saved.");
	net.assertClean();
});
