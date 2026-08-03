// Integration test for the project create dialog, which carries the same duplicate-submit defect the
// credential dialog did: a create writes a new project, so a fast double submit stored it twice. The
// submit is now guarded so a second submit while the first is still posting is dropped, and the
// button comes back once the request settles, since a modal save stays on the page. The form is
// submitted the way a reader double clicks Save, and the assertion is how many projects were created.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

test("the project form creates one project however fast Save is clicked", async () => {
	const gate = deferred();
	let creates = 0;
	const routes = {
		// The credential pickers load their options when the form is wired.
		"/v1/credentials": reply({ credentials: [] }),
		"/v1/projects": (req) => {
			if (req.method === "POST") { creates++; return gate.promise; }
			return reply({ projects: [] });
		},
	};
	const { app, document, net, clock } = loadPage("projects", { parts: ALL_PARTS, routes });
	app.wireProjectForm();
	// Let the wire-time credential lookups settle before the form is used.
	await clock.flush();

	const form = document.getElementById("project-form");
	const button = form.querySelector('button[type="submit"]');
	document.getElementById("project-name").value = "web";
	document.getElementById("project-repo").value = "https://example.com/web.git";

	const events = [fire(form, "submit"), fire(form, "submit"), fire(form, "submit")];

	assert.equal(creates, 1, "a repeat submit created the project a second time");
	assert.equal(button.disabled, true, "the Save button stayed live while the project was saving");
	assert.deepEqual(events.map((e) => e.defaultPrevented), [true, true, true]);

	gate.resolve(reply({ id: "proj-1" }));
	await clock.flush();
	assert.equal(creates, 1);
	// A modal save stays on the page, so the button comes back for the next project.
	assert.equal(button.disabled, false, "a saved project left the Save button dead");
	assert.equal(document.getElementById("project-status").textContent, "Saved.");
	net.assertClean();
});
