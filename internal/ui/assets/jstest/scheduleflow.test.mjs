// Integration tests for the schedule create dialog, which carried the same duplicate-submit defect
// the credential dialog did: a create writes a new schedule, so a fast double submit stored it twice.
// The submit is now guarded so a second submit while the first is still posting is dropped, and the
// button comes back once the request settles either way, since a modal save stays on the page. Each
// form here is submitted the way a reader double clicks Save, and the assertion is how many schedules
// the server was asked to create.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

test("the schedule form creates one schedule however fast Save is clicked", async () => {
	const gate = deferred();
	let creates = 0;
	const routes = {
		// The template picker loads its options when the form is wired, and again on the reload.
		"/v1/templates": reply({ templates: [{ id: "tpl-1", name: "deploy" }] }),
		// The create POST is held open so a second submit lands while the first is in flight; the same
		// path answers the table reload GET the save fires on success.
		"/v1/schedules": (req) => {
			if (req.method === "POST") { creates++; return gate.promise; }
			return reply({ schedules: [] });
		},
	};
	const { app, document, net, clock } = loadPage("schedules", { parts: ALL_PARTS, routes });
	app.wireScheduleForm();
	// Let the wire-time template lookup settle so the picker holds its option.
	await clock.flush();

	const form = document.getElementById("schedule-form");
	const button = form.querySelector('button[type="submit"]');
	document.getElementById("schedule-name").value = "nightly deploy";
	document.getElementById("schedule-cron").value = "0 2 * * *";
	document.getElementById("schedule-template").value = "tpl-1";

	// Three submits in the time it takes to double click, before the first has come back.
	const events = [fire(form, "submit"), fire(form, "submit"), fire(form, "submit")];

	assert.equal(creates, 1, "a repeat submit created the schedule a second time");
	assert.equal(button.disabled, true, "the Save button stayed live while the schedule was saving");
	// Every submit is canceled, the dropped ones included, or a dropped submit posts the form the old
	// fashioned way and the browser navigates.
	assert.deepEqual(events.map((e) => e.defaultPrevented), [true, true, true]);

	gate.resolve(reply({ id: "sch-1" }));
	await clock.flush();
	assert.equal(creates, 1);
	// A modal save stays on the page, so the button comes back for the next schedule.
	assert.equal(button.disabled, false, "a saved schedule left the Save button dead");
	assert.equal(document.getElementById("schedule-status").textContent, "Saved.");
	net.assertClean();
});

test("a rejected schedule save hands the dialog back to the operator", async () => {
	let attempts = 0;
	const routes = {
		"/v1/templates": reply({ templates: [{ id: "tpl-1", name: "deploy" }] }),
		"/v1/schedules": (req) => {
			if (req.method !== "POST") return reply({ schedules: [] });
			attempts++;
			return attempts === 1 ? reply({ error: "cron is invalid" }, { status: 400 }) : reply({ id: "sch-9" });
		},
	};
	const { app, document, net, clock } = loadPage("schedules", { parts: ALL_PARTS, routes });
	app.wireScheduleForm();
	await clock.flush();

	const form = document.getElementById("schedule-form");
	const button = form.querySelector('button[type="submit"]');
	document.getElementById("schedule-name").value = "nightly deploy";
	document.getElementById("schedule-cron").value = "0 2 * * *";
	document.getElementById("schedule-template").value = "tpl-1";

	fire(form, "submit");
	await clock.flush();
	assert.equal(attempts, 1);
	assert.equal(button.disabled, false, "a failed save left the button dead, so nobody can retry");
	assert.equal(document.getElementById("schedule-status").textContent, "Save failed: cron is invalid");

	// Corrected, the same button saves, which is the whole point of re-enabling it.
	fire(form, "submit");
	await clock.flush();
	assert.equal(attempts, 2);
	assert.equal(document.getElementById("schedule-status").textContent, "Saved.");
	net.assertClean();
});
