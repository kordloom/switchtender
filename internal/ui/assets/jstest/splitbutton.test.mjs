// Tests for the template launch split button in 12-templates-notify.js: its structure, and the
// menu's open/close discipline (toggle, outside click, Escape, single-open). The advanced item
// delegates to openPromptLaunch, whose own flow is covered by launchflow.test.mjs.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts, sandboxOf } from "./loader.mjs";

// The split button references svgIcon (07), openSurvey/openPromptLaunch (12/13); load enough parts
// that its construction resolves. The menu logic under test lives in 12-templates-notify.js.
function fresh() {
	return loadParts([
		"01-boot.js", "02-page-data.js", "07-nav-theme.js", "08-auth-status.js",
		"10-modals-credentials.js", "12-templates-notify.js", "13-fileviewer-inventory.js",
	]);
}

test("the split button renders a primary Launch joined to a caret", () => {
	const app = fresh();
	const doc = sandboxOf(app).document;
	const wrap = app.launchSplitButton({ id: "t1", name: "Deploy web", survey: [] });
	doc.body.appendChild(wrap);

	assert.ok(wrap.classList.contains("split-btn"));
	const main = wrap.querySelector(".split-main");
	const caret = wrap.querySelector(".split-caret");
	assert.equal(main.textContent, "Launch");
	assert.equal(caret.getAttribute("aria-haspopup"), "true");
	assert.equal(caret.getAttribute("aria-expanded"), "false");
	assert.equal(doc.querySelector(".launch-menu"), null, "the menu is closed until the caret is clicked");
});

test("clicking the caret opens the one advanced launch option, and toggles it shut", () => {
	const app = fresh();
	const doc = sandboxOf(app).document;
	const wrap = app.launchSplitButton({ id: "t1", name: "Deploy web", survey: [] });
	doc.body.appendChild(wrap);
	const caret = wrap.querySelector(".split-caret");

	caret.click();
	const menu = doc.querySelector(".launch-menu");
	assert.ok(menu, "the caret opens the menu");
	assert.equal(doc.querySelector(".launch-menu-title").textContent, "Launch with overrides…");
	assert.equal(caret.getAttribute("aria-expanded"), "true");

	caret.click();
	assert.equal(doc.querySelector(".launch-menu"), null, "a second caret click closes the menu");
	assert.equal(caret.getAttribute("aria-expanded"), "false");
});

test("a click outside the split button closes an open menu", () => {
	const app = fresh();
	const doc = sandboxOf(app).document;
	const wrap = app.launchSplitButton({ id: "t1", name: "x", survey: [] });
	doc.body.appendChild(wrap);
	wrap.querySelector(".split-caret").click();
	assert.ok(doc.querySelector(".launch-menu"));

	const elsewhere = doc.createElement("div");
	doc.body.appendChild(elsewhere);
	app.launchMenuOutside({ target: elsewhere });
	assert.equal(doc.querySelector(".launch-menu"), null, "an outside click closes the menu");
});

test("Escape closes an open menu", () => {
	const app = fresh();
	const doc = sandboxOf(app).document;
	const wrap = app.launchSplitButton({ id: "t1", name: "x", survey: [] });
	doc.body.appendChild(wrap);
	wrap.querySelector(".split-caret").click();
	assert.ok(doc.querySelector(".launch-menu"));

	app.launchMenuKey({ key: "Escape" });
	assert.equal(doc.querySelector(".launch-menu"), null, "Escape closes the menu");
});

test("only one launch menu is open at a time", () => {
	const app = fresh();
	const doc = sandboxOf(app).document;
	const a = app.launchSplitButton({ id: "a", name: "A", survey: [] });
	const b = app.launchSplitButton({ id: "b", name: "B", survey: [] });
	doc.body.appendChild(a);
	doc.body.appendChild(b);

	const aCaret = a.querySelector(".split-caret");
	const bCaret = b.querySelector(".split-caret");
	aCaret.click();
	bCaret.click();
	assert.equal(doc.querySelectorAll(".launch-menu").length, 1, "opening one menu closes the other");
	assert.equal(aCaret.getAttribute("aria-expanded"), "false", "the first caret is marked closed");
	assert.equal(bCaret.getAttribute("aria-expanded"), "true", "the most recently opened caret owns the menu");
});
