// Tests for the advisory-AI-off state: every AI surface reads the body marker the server set and
// goes plainly dark instead of looking usable and failing on first use. The propose box stands in
// for the pattern; the ask panel, the draft row, and run triage share the same helpers.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";

// PARTS covers the propose box and what its wiring calls into.
const PARTS = [
	"01-boot.js", "02-page-data.js", "07-nav-theme.js", "08-auth-status.js",
	"15-overview-doctor.js", "16-runs-list.js",
];

test("the propose box is plainly off when the server has no AI provider", () => {
	const { app, document } = loadPage("runs", { parts: PARTS, vars: { AIOff: true } });
	app.wirePropose();

	const input = document.getElementById("propose-input");
	const go = document.getElementById("propose-go");
	assert.equal(document.body.dataset.aiOff, "true", "the server's marker never reached the page");
	assert.equal(input.disabled, true, "the input still looks usable with AI off");
	assert.equal(input.placeholder, "Advisory AI is off on this server");
	assert.equal(go.disabled, true, "the Propose button still looks clickable with AI off");
	const off = document.querySelector("#propose-panel .ask-off");
	assert.ok(off, "the standard off notice is missing");
	assert.equal(off.querySelector("a").getAttribute("href"), "/ui/docs/ai",
		"the notice does not link to how to enable AI");
});

test("the propose box stays live when AI is on", () => {
	const { app, document } = loadPage("runs", { parts: PARTS });
	app.wirePropose();

	assert.equal(document.getElementById("propose-input").disabled, false);
	assert.equal(document.getElementById("propose-go").disabled, false);
	assert.equal(document.querySelector("#propose-panel .ask-off"), null,
		"the off notice shows even though AI is on");
});
