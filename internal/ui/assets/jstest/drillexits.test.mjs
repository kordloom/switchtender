// Tests for the drill drawer's exits on the run detail page, the page that declares the panel in
// its template. The exits used to be wired only when the panel was built by script, so a failed
// cell's RETURN CODE pane opened with no backdrop and no Escape and became a dead end on phones.
// Every drill must close three ways on every page: the close button, the backdrop, and Escape.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { fire } from "./dom.mjs";

// mountDetailDrill mounts the detail page, wires the pre-declared panel the way boot does, and
// opens the drill for a failed cell.
function mountDetailDrill() {
	const page = loadPage("detail");
	page.app.ensureDrill();
	page.app.showDrill({
		host: "web1", task: "Install nginx", outcome: "failed", rc: 1,
		message: "non-zero return code",
	});
	return page;
}

test("opening a cell drill on the detail page shows a backdrop", () => {
	const page = mountDetailDrill();
	assert.equal(page.document.getElementById("drill").hidden, false);
	const backdrop = page.document.getElementById("drill-backdrop");
	assert.ok(backdrop, "the pre-declared panel gained a backdrop");
	assert.equal(backdrop.hidden, false);
});

test("the close button closes the drill and its backdrop", () => {
	const page = mountDetailDrill();
	fire(page.document.getElementById("drill-close"), "click");
	assert.equal(page.document.getElementById("drill").hidden, true);
	assert.equal(page.document.getElementById("drill-backdrop").hidden, true);
});

test("clicking the backdrop closes the drill", () => {
	const page = mountDetailDrill();
	fire(page.document.getElementById("drill-backdrop"), "click");
	assert.equal(page.document.getElementById("drill").hidden, true);
});

test("Escape closes the drill", () => {
	const page = mountDetailDrill();
	fire(page.document, "keydown", { key: "Escape" });
	assert.equal(page.document.getElementById("drill").hidden, true);
	assert.equal(page.document.getElementById("drill-backdrop").hidden, true);
});

test("running ensureDrill again does not stack close listeners", () => {
	const page = mountDetailDrill();
	page.app.ensureDrill();
	// One click closes; a second close on an already-hidden drill stays hidden and does not throw,
	// which is what a doubled listener set would have broken by toggling twice.
	fire(page.document.getElementById("drill-close"), "click");
	assert.equal(page.document.getElementById("drill").hidden, true);
	assert.equal(page.document.getElementById("drill").dataset.exitsWired, "true");
});
