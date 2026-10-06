// Tests for adding steps on the workflow canvas in 05-workflow-editor.js: the canvas's own Add step,
// the add control on every card that adds a step after it already linked, the empty canvas and the
// control that clears the sample into it, and the id and pointer defects that made the sample graph
// ignore the gestures its own help described.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { ALL_PARTS, sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";

// mountEditor opens the workflow page on its seeded sample, or on vars when given, and mounts the
// editor.
function mountEditor(options) {
	const page = loadPage("workflows", Object.assign({ parts: ALL_PARTS, routes: {} }, options || {}));
	page.app.mountWorkflow();
	return page;
}

// card returns the rendered card for the step with the given name.
function card(document, name) {
	return document.querySelectorAll(".wf-node")
		.find((el) => el.querySelector(".wf-node-name").textContent === name);
}

// saveDialog fills the open step dialog as a bash step and submits it.
function saveDialog(document, name, command) {
	document.getElementById("wf-step-name").value = name;
	document.getElementById("wf-step-tool").value = "bash";
	fire(document.getElementById("wf-step-tool"), "change");
	document.getElementById("wf-step-command").value = command;
	fire(document.getElementById("wf-step-form"), "submit");
}

// plain copies a value built inside the page's sandbox into this realm, so deepEqual compares its
// contents rather than the sandbox's own Array and Object prototypes.
function plain(value) {
	return JSON.parse(JSON.stringify(value));
}

// overlapping returns the pairs of cards whose boxes overlap, at the card size the canvas draws.
function overlapping(app) {
	const nodes = app.wfState.nodes;
	const pairs = [];
	for (let i = 0; i < nodes.length; i++) {
		for (let j = i + 1; j < nodes.length; j++) {
			const a = nodes[i];
			const b = nodes[j];
			if (Math.abs(a.x - b.x) < app.WF_CARD_W && Math.abs(a.y - b.y) < app.WF_NODE_H) {
				pairs.push(a.name + " and " + b.name);
			}
		}
	}
	return pairs;
}

test("the canvas carries its own Add step, and it opens the dialog for a new step", () => {
	const { app, document } = mountEditor();
	const add = document.getElementById("wf-canvas-add");
	assert.ok(add, "the canvas has no Add step of its own");
	assert.ok(document.getElementById("wf-canvas").contains(add), "Add step is not inside the canvas");
	assert.equal(add.tagName, "BUTTON", "the canvas Add step is not a focusable button");
	assert.match(add.textContent, /Add step/);

	fire(add, "click");
	assert.equal(document.getElementById("wf-step-modal").hidden, false, "the dialog did not open");
	assert.equal(document.getElementById("wf-step-title").textContent, "Add a step");
	assert.equal(document.getElementById("wf-step-after").hidden, true,
		"a step added from the corner claimed to run after another");
	saveDialog(document, "lint", "make lint");
	assert.equal(app.wfState.nodes.length, 5, "saving did not add the step");
	assert.ok(card(document, "lint"), "the new step has no card");
});

test("every card carries an add control named for its step", () => {
	const { app, document } = mountEditor();
	const cards = document.querySelectorAll(".wf-node");
	assert.equal(cards.length, app.wfState.nodes.length);
	for (const el of cards) {
		const name = el.querySelector(".wf-node-name").textContent;
		const add = el.querySelector(".wf-node-add");
		assert.ok(add, name + " has no add control");
		assert.equal(add.tagName, "BUTTON", name + "'s add control is not a focusable button");
		assert.equal(add.getAttribute("aria-label"), "Add a step after " + name);
	}
});

test("adding after a step places it to the right, links it, and undoes as one", () => {
	const { app, document } = mountEditor();
	const before = { nodes: app.wfState.nodes.length, edges: app.wfState.edges.length };
	const source = app.wfState.nodes.find((n) => n.name === "smoke-test");

	fire(card(document, "smoke-test").querySelector(".wf-node-add"), "click");
	assert.equal(document.getElementById("wf-step-modal").hidden, false, "the dialog did not open");
	const after = document.getElementById("wf-step-after");
	assert.equal(after.hidden, false, "the dialog does not say where the new step runs");
	assert.match(after.textContent, /^Runs after smoke-test\./);
	saveDialog(document, "notify", "echo shipped");

	const added = app.wfState.nodes.find((n) => n.name === "notify");
	assert.ok(added, "saving did not add the step");
	assert.equal(added.x, source.x + app.WF_PATTERN_COL, "the step did not land one column right");
	assert.equal(added.y, source.y, "the step left its source's row with the row free");
	assert.deepEqual(plain(app.wfState.edges.filter((e) => e.to === added.id)), [{ from: source.id, to: added.id }],
		"the new step is not linked from the step it was added after");
	assert.equal(document.getElementById("status").textContent, "notify now waits for smoke-test.");
	assert.equal(app.workflowSteps().find((s) => s.name === "notify").depends_on[0], "smoke-test");

	app.wfUndo();
	assert.equal(app.wfState.nodes.length, before.nodes, "one undo did not take back the step");
	assert.equal(app.wfState.edges.length, before.edges, "one undo did not take back its link");
});

test("steps added after a crowded step land on free rows, never on a card", () => {
	const { app, document } = mountEditor();
	// provision fans out to configure above and migrate-db below, so one column right of it the
	// first new step fits between them and the next ones have to find rows of their own.
	const tests = [
		// Test 0: The row between configure and migrate-db is free.
		{ Name: "lint", WantY: 150 },
		// Test 1: The source row is taken now, and the rows next to it crowd a card, so it goes on.
		{ Name: "audit", WantY: 410 },
		// Test 2: Every nearer row is taken, so it goes further still.
		{ Name: "scan", WantY: 540 },
	];
	for (const [i, tc] of tests.entries()) {
		fire(card(document, "provision").querySelector(".wf-node-add"), "click");
		saveDialog(document, tc.Name, "echo " + tc.Name);
		const added = app.wfState.nodes.find((n) => n.name === tc.Name);
		assert.ok(added, "test " + i + ": the step was not added");
		assert.equal(added.y, tc.WantY, "test " + i + ": the step landed on the wrong row");
		assert.deepEqual(overlapping(app), [], "test " + i + ": a new step landed on a card");
	}
});

test("a step from the toolbar never lands overlapping a card", () => {
	const { app, document } = mountEditor();
	app.wfState.nodes = [];
	app.wfState.edges = [];
	app.resetView();
	// The first free slot sits at 0, 0 at this pan, so a card off it by less than a card's width
	// overlaps it. The old check skipped only a slot within thirty pixels of a card's corner.
	app.wfState.nodes.push({ id: "n9", name: "existing", tool: "bash", command: "true", x: 60, y: 30 });
	fire(document.getElementById("wf-add"), "click");
	saveDialog(document, "next", "echo next");
	assert.deepEqual(overlapping(app), [], "a step added from the toolbar overlaps a card");
});

test("A on a focused card adds after it, and Enter on its add control is not an edit", () => {
	const { app, document } = mountEditor();
	const modal = document.getElementById("wf-step-modal");
	const provision = card(document, "provision");

	fire(provision, "keydown", { key: "a" });
	assert.equal(modal.hidden, false, "A on a focused card did not open the dialog");
	assert.equal(app.wfState.editing, null, "A opened the card for editing instead");
	assert.match(document.getElementById("wf-step-after").textContent, /^Runs after provision\./);
	app.closeStepModal();

	// With a modifier held, A belongs to the browser, select all above everything.
	fire(provision, "keydown", { key: "a", metaKey: true });
	assert.equal(modal.hidden, true, "Cmd-A on a card was taken for adding a step");

	// Enter on the add control is that control's own: the card's keys must not turn it into an edit.
	const event = fire(provision.querySelector(".wf-node-add"), "keydown", { key: "Enter" });
	assert.equal(event.defaultPrevented, false, "the card swallowed Enter on its add control");
	assert.equal(modal.hidden, true, "Enter on the add control opened the card for editing");
	assert.equal(app.wfState.editing, null);
});

test("a dialog opened from a card hands focus to the saved step", () => {
	const { app, document } = mountEditor();
	const add = card(document, "configure").querySelector(".wf-node-add");
	add.focus();
	fire(add, "click");
	saveDialog(document, "report", "echo report");
	const added = app.wfState.nodes.find((n) => n.name === "report");
	assert.equal(document.activeElement && document.activeElement.dataset.id, added.id,
		"focus did not land on the step just added");
	assert.ok(document.activeElement.isConnected, "focus was left on a card the render removed");
});

test("a press on a button in the canvas is not taken for a pan", () => {
	const { app, document } = mountEditor();
	const canvas = document.getElementById("wf-canvas");
	const captured = [];
	// The simulated DOM has no pointer capture, so the canvas records what it was asked to capture.
	canvas.setPointerCapture = (id) => captured.push(id);
	app.wfClearCanvas();
	for (const id of ["wf-canvas-add", "wf-hint-add", "wf-hint-wizard"]) {
		const btn = document.getElementById(id);
		fire(btn, "pointerdown", { pointerId: 7, button: 0, isPrimary: true, clientX: 5, clientY: 5 });
		assert.equal(app.wfState.pan, null, id + " started a pan, so its click goes to the canvas");
		assert.deepEqual(captured, [], id + " was captured by the canvas");
		fire(btn, "pointerup", { pointerId: 7, button: 0, isPrimary: true, clientX: 5, clientY: 5 });
	}
	// Empty canvas still pans, so the check above can see a pan when there is one.
	fire(canvas, "pointerdown", { pointerId: 8, button: 0, isPrimary: true, clientX: 5, clientY: 5 });
	assert.ok(app.wfState.pan, "a press on empty canvas no longer pans");
	assert.deepEqual(captured, [8]);
});

test("the sample links and unlinks by the ids the canvas reads back from its cards", () => {
	const { app, document } = mountEditor();
	// A dropped link names its target by the card under the pointer, read from the card's data
	// attribute, so that is the id handed over here.
	const configure = app.wfState.nodes.find((n) => n.name === "configure");
	app.linkTo(configure.id, card(document, "migrate-db").dataset.id);
	assert.equal(app.wfState.edges.length, 5, "a link dropped on a sample step was ignored");
	assert.equal(document.getElementById("status").textContent, "migrate-db now waits for configure.");

	app.renderWorkflow();
	const hit = document.querySelectorAll(".wf-edge-hit")
		.find((el) => el.getAttribute("aria-label").startsWith("Dependency link, provision into configure."));
	assert.ok(hit, "the sample link has no hit path");
	fire(hit, "click");
	fire(hit, "keydown", { key: "Delete" });
	assert.equal(app.wfState.edges.length, 4, "deleting a sample link left it in the graph");
	assert.equal(app.workflowSteps().find((s) => s.name === "configure").depends_on, undefined,
		"the removed link still orders the pipeline");
});

test("a draft saved with number ids restores with string ids that link", () => {
	// Every visit saved the old sample with number ids, so this is what a returning browser holds.
	const node = (id, name, tool, x, y, extra) => Object.assign({
		id, name, tool, x, y, playbook: "", command: "", inventory: "",
		dryRun: false, continueOnFailure: false, retries: 0,
	}, extra);
	const draft = {
		nodes: [
			node(1, "provision", "terraform", 60, 150, { command: "infra/network", dryRun: true }),
			node(2, "configure", "ansible", 330, 60, { playbook: "site.yml" }),
			node(3, "migrate-db", "ansible", 330, 250, { playbook: "migrate.yml" }),
			node(4, "smoke-test", "bash", 600, 150,
				{ command: "curl -fsS https://example.internal/healthz", retries: 2 }),
		],
		edges: [{ from: 1, to: 2 }, { from: 1, to: 3 }, { from: 2, to: 4 }, { from: 3, to: 4 }],
		seq: 4, name: "Release pipeline", inventory: "",
	};
	const page = loadPage("workflows", { parts: ALL_PARTS, routes: {} });
	sandboxOf(page.app).localStorage.setItem("st_wf_draft", JSON.stringify(draft));
	page.app.mountWorkflow();
	const { app, document } = page;
	assert.deepEqual(plain(app.wfState.nodes.map((n) => n.id)), ["1", "2", "3", "4"]);
	assert.deepEqual(plain(app.wfState.edges.map((e) => [e.from, e.to])),
		[["1", "2"], ["1", "3"], ["2", "4"], ["3", "4"]]);
	// It is still the untouched sample, so the guard on Run still holds for it.
	assert.equal(document.getElementById("wf-sample-note").hidden, false);
	app.linkTo("2", card(document, "migrate-db").dataset.id);
	assert.equal(app.wfState.edges.length, 5, "a link onto a restored step was ignored");
});

test("Clear canvas empties the sample as one undo point and opens the empty canvas", () => {
	const { app, document } = mountEditor();
	const clear = document.getElementById("wf-clear");
	assert.ok(clear, "the sample note offers no way to clear the canvas");
	assert.ok(document.getElementById("wf-sample-note").contains(clear));

	fire(clear, "click");
	assert.equal(app.wfState.nodes.length, 0, "clearing left steps on the canvas");
	assert.equal(app.wfState.edges.length, 0, "clearing left links on the canvas");
	assert.equal(document.getElementById("wf-sample-note").hidden, true,
		"an empty canvas still says it holds the sample");
	assert.equal(document.querySelector(".wf-hint").hidden, false, "the empty canvas shows no way in");
	assert.equal(document.activeElement, document.getElementById("wf-hint-add"),
		"focus was left on the control the clear removed");
	assert.match(document.getElementById("status").textContent, /^Cleared the canvas\./);

	app.wfUndo();
	assert.equal(app.wfState.nodes.length, 4, "undo did not bring the sample back");
	assert.equal(document.getElementById("wf-sample-note").hidden, false);
});

test("the empty canvas offers Add your first step and Start from a pattern", () => {
	const { app, document } = mountEditor();
	app.wfClearCanvas();
	const add = document.getElementById("wf-hint-add");
	const pattern = document.getElementById("wf-hint-wizard");
	assert.equal(add.textContent.trim(), "Add your first step");
	assert.equal(pattern.textContent.trim(), "Start from a pattern");
	// The canvas's own Add step stays where it always is.
	assert.equal(document.getElementById("wf-canvas-add").hidden, false);

	fire(add, "click");
	assert.equal(document.getElementById("wf-step-modal").hidden, false, "Add your first step did nothing");
	saveDialog(document, "first", "echo first");
	assert.equal(document.querySelector(".wf-hint").hidden, true, "the empty state outlived the first step");

	app.wfClearCanvas();
	fire(pattern, "click");
	assert.equal(document.getElementById("wf-wizard-modal").hidden, false, "Start from a pattern did nothing");
});

test("the demo labels a form it disables, and not the step dialog that still saves", () => {
	const workflow = loadPage("workflows", { parts: ALL_PARTS, vars: { ReadOnly: true, Demo: true } });
	workflow.app.applyReadOnly();
	const form = workflow.document.getElementById("wf-step-form");
	assert.equal(form.querySelectorAll(".ro-note").length, 0,
		"the step dialog says it is disabled beside a Save step that works");
	assert.equal(form.querySelector('button[type="submit"]').disabled, false);
	assert.equal(workflow.document.getElementById("wf-step-draft-go").disabled, true,
		"drafting posts to the server and must stay off in the demo");

	const projects = loadPage("projects", { parts: ALL_PARTS, vars: { ReadOnly: true, Demo: true } });
	projects.app.applyReadOnly();
	const note = projects.document.querySelector("#project-form .ro-note");
	assert.ok(note, "a form the demo disables lost its note");
	assert.equal(note.textContent, "Disabled in this read-only demo");
});

test("typing the workflow's name or inventory saves the draft with it", () => {
	// The mount handed the name and inventory inputs a button as their input handler, because a local
	// variable for the Save as template button shared its name with the function that saves the draft.
	// A name typed over the sample, a reload, and the name was gone, since only a change to the graph
	// saved it.
	const { app, document } = mountEditor();
	const saved = () => plain(JSON.parse(sandboxOf(app).localStorage.getItem("st_wf_draft")));
	document.getElementById("wf-name").value = "Release 4.3";
	fire(document.getElementById("wf-name"), "input");
	assert.equal(saved().name, "Release 4.3", "typing the name did not save the draft");
	document.getElementById("wf-inventory").value = "production";
	fire(document.getElementById("wf-inventory"), "input");
	assert.equal(saved().inventory, "production", "typing the inventory did not save the draft");
	assert.equal(saved().name, "Release 4.3", "saving the inventory lost the name");
});
