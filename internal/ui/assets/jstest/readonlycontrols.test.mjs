// The read-only demo disables what would change something and leaves everything else alone. The
// rule that marks a page header's primary action as mutating used to mark every one of them, on the
// reasoning that a header's primary action creates something. Two of them do not, and both are the
// first thing a visitor touches: the overview's primary action is a link to the runs page, and the
// audit page's bundle download is a GET the server serves in read-only mode, sitting one line under
// copy promising the reader can download it. Both were swallowed, silently, with their tooltips
// still promising the action.
//
// These tests drive the same clicks a visitor makes, so the exemption cannot be removed without a
// failure here.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

test("the overview's Launch run link navigates in the read-only demo", () => {
	const { app, document } = loadPage("overview", { parts: ALL_PARTS, vars: { ReadOnly: true } });
	app.applyReadOnly();

	const link = document.querySelector('.page-head a.button.primary[href="/ui/runs"]');
	assert.ok(link, "the overview page header has no primary link to the runs page");
	assert.equal(link.dataset.mutates, undefined,
		"a link to another page was marked as mutating, so the demo's landing page does nothing");

	const event = fire(link, "click");
	assert.equal(event.defaultPrevented, false,
		"the read-only pass swallowed a navigation, so Launch run leads nowhere");
});

test("the audit page's Download bundle stays live in the read-only demo", () => {
	const { app, document } = loadPage("audit", { parts: ALL_PARTS, vars: { ReadOnly: true } });
	app.applyReadOnly();

	const bundle = document.getElementById("audit-bundle");
	assert.ok(bundle, "the audit page has no bundle download button");
	assert.equal(bundle.dataset.mutates, undefined,
		"the bundle download is a GET and must not be marked as mutating");
	assert.equal(bundle.disabled, false, "the bundle download was disabled in the demo");

	const event = fire(bundle, "click");
	assert.equal(event.defaultPrevented, false,
		"the read-only pass swallowed the download the page copy promises");
});

test("a page header action that really does create something is still refused", () => {
	const { app, document } = loadPage("workflows", { parts: ALL_PARTS, vars: { ReadOnly: true } });
	app.applyReadOnly();

	// The exemption is narrow on purpose: it covers navigation and controls marked demo-safe, and
	// nothing else. The workflow toolbar's Run is neither, and it starts a run, so it must still be
	// refused. Without this, the change that freed two read-only controls would have opened every
	// write on every page header.
	const run = document.getElementById("wf-run");
	assert.ok(run, "the workflow page has no Run control");
	assert.equal(run.dataset.mutates, "true",
		"a control that starts a run was left live in the read-only demo");

	const event = fire(run, "click");
	assert.equal(event.defaultPrevented, true,
		"the read-only pass let a run start from the demo");
});

test("a dialog's committing control is sealed even when it is not a form submit", () => {
	const { app, document } = loadPage("jobtemplates", { parts: ALL_PARTS, vars: { ReadOnly: true } });
	app.applyReadOnly();

	// The launch prompt and the survey prompt both commit with a type="button" control outside any
	// form. Sealing only form submits let a visitor fill in a whole launch and learn it was refused
	// only after pressing the button, which is the demo behavior this pass exists to prevent.
	for (const id of ["prompt-go", "survey-go"]) {
		const btn = document.getElementById(id);
		assert.ok(btn, `the templates page has no ${id} control`);
		assert.equal(btn.dataset.mutates, "true",
			`${id} commits a launch and was left unsealed in the read-only demo`);

		// The launch prompt re-enables its own control when the dialog opens, so the marker rather
		// than the disabled flag is what has to hold.
		btn.disabled = false;
		const event = fire(btn, "click");
		assert.equal(event.defaultPrevented, true,
			`${id} started a launch from the read-only demo after the dialog re-enabled it`);
	}
});

test("the workflow editor builds a graph in the demo but never leaves the browser", () => {
	const { app, document } = loadPage("workflows", { parts: ALL_PARTS, vars: { ReadOnly: true } });
	app.applyReadOnly();

	// The editor's own comment promises a visitor can build a graph. Editing one rewrites local
	// state and nothing else, so sealing it made the demo hide the feature it exists to show.
	for (const sel of ['#wf-step-form button[type="submit"]', "#wf-step-delete"]) {
		const btn = document.querySelector(sel);
		assert.ok(btn, `the step modal has no ${sel}`);
		assert.equal(btn.dataset.mutates, undefined,
			`${sel} only rewrites the local graph and must stay usable in the demo`);
	}

	// Everything that leaves the browser stays refused. Saving the graph as a template posts it to
	// /v1/templates, which is a real write, and drafting a step posts the prompt to /v1/ai/draft.
	for (const id of ["wf-run", "wf-save-template"]) {
		const btn = document.getElementById(id);
		assert.ok(btn, `the workflow toolbar has no ${id}`);
		assert.equal(btn.dataset.mutates, "true",
			`${id} reaches the server and was left live in the read-only demo`);
	}
});
