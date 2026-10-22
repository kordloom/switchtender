// Tests for the undo point Start over takes in 05-workflow-editor.js. Start over clears the
// workflow's name and inventory along with the graph, so undoing it has to bring those back, and an
// ordinary graph edit has to leave a name typed afterward alone.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { ALL_PARTS, sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";

// mountEditor opens the workflow page on its seeded sample and mounts the editor.
function mountEditor() {
	const page = loadPage("workflows", { parts: ALL_PARTS, routes: {} });
	page.app.mountWorkflow();
	return page;
}

// typeFields types a name and an inventory into the toolbar the way a visitor does, so the draft
// saves with them.
function typeFields(document, name, inventory) {
	document.getElementById("wf-name").value = name;
	fire(document.getElementById("wf-name"), "input");
	document.getElementById("wf-inventory").value = inventory;
	fire(document.getElementById("wf-inventory"), "input");
}

// makeOwnGraph replaces the sample with a graph that is plainly the visitor's own: one step
// retargeted, one renamed, and one link removed.
function makeOwnGraph(app) {
	app.wfState.nodes[0].command = "infra/my-own-network";
	app.wfState.nodes[1].name = "mine";
	app.wfState.edges = app.wfState.edges.filter((e) => !(e.from === "1" && e.to === "3"));
	app.renderWorkflow();
}

// shape reduces the editor to primitives built in this realm: the steps with their commands, the
// links, and the two toolbar fields.
function shape(app, document) {
	return {
		steps: app.wfState.nodes.map((n) => n.name + ":" + (n.command || n.playbook)).join(","),
		links: app.wfState.edges.map((e) => e.from + ">" + e.to).sort().join(","),
		name: document.getElementById("wf-name").value,
		inventory: document.getElementById("wf-inventory").value,
	};
}

// cmdZ presses Cmd Z on the page body, the undo shortcut the status line names.
function cmdZ(document) {
	fire(document.body, "keydown", { key: "z", metaKey: true });
}

// cmdShiftZ presses Cmd Shift Z on the page body, the redo shortcut.
function cmdShiftZ(document) {
	fire(document.body, "keydown", { key: "z", metaKey: true, shiftKey: true });
}

// startOver presses the Start over button, which asks first and is answered yes by the harness.
function startOver(document) {
	fire(document.getElementById("wf-start-over"), "click");
}

// savedDraft returns the draft the editor last wrote to storage, copied into this realm.
function savedDraft(app) {
	return JSON.parse(sandboxOf(app).localStorage.getItem("st_wf_draft"));
}

// FIELD_CASES are the name and inventory values a visitor may have typed before starting over,
// including empty ones, which a restore must put back as empty rather than skip.
const FIELD_CASES = [
	// Test 0: Both fields filled.
	{ Name: "prod-deploy", Inventory: "inv_abc" },
	// Test 1: Only the name filled.
	{ Name: "only a name", Inventory: "" },
	// Test 2: Only the inventory filled, so the example's name replaces an empty one.
	{ Name: "", Inventory: "inv_only" },
	// Test 3: Neither filled.
	{ Name: "", Inventory: "" },
];

test("case 1: undoing Start over brings back the steps, links, name, and inventory", () => {
	for (const [i, tc] of FIELD_CASES.entries()) {
		const { app, document } = mountEditor();
		makeOwnGraph(app);
		typeFields(document, tc.Name, tc.Inventory);
		const before = shape(app, document);
		const beforeNodes = JSON.stringify(app.wfState.nodes);
		const beforeEdges = JSON.stringify(app.wfState.edges);

		startOver(document);
		assert.equal(document.getElementById("wf-name").value, "Release pipeline",
			"test " + i + ": Start over did not seed the example's name");
		assert.equal(document.getElementById("wf-inventory").value, "",
			"test " + i + ": Start over left an inventory behind");

		cmdZ(document);
		assert.deepEqual(shape(app, document), before, "test " + i + ": undo did not restore the editor");
		assert.equal(JSON.stringify(app.wfState.nodes), beforeNodes, "test " + i + ": steps differ");
		assert.equal(JSON.stringify(app.wfState.edges), beforeEdges, "test " + i + ": links differ");
		const draft = savedDraft(app);
		assert.equal(draft.name, tc.Name, "test " + i + ": the saved draft kept the example's name");
		assert.equal(draft.inventory, tc.Inventory, "test " + i + ": the saved draft lost the inventory");
	}
});

test("case 2: redoing Start over puts the example, its name, and an empty inventory back", () => {
	for (const [i, tc] of FIELD_CASES.entries()) {
		const { app, document } = mountEditor();
		makeOwnGraph(app);
		typeFields(document, tc.Name, tc.Inventory);
		startOver(document);
		const example = shape(app, document);
		assert.equal(example.name, "Release pipeline", "test " + i + ": the example lost its name");
		assert.equal(example.inventory, "", "test " + i + ": the example kept an inventory");

		cmdZ(document);
		cmdShiftZ(document);
		assert.deepEqual(shape(app, document), example, "test " + i + ": redo did not restore the example");
		const draft = savedDraft(app);
		assert.equal(draft.name, "Release pipeline", "test " + i + ": the saved draft lost the name");
		assert.equal(draft.inventory, "", "test " + i + ": the saved draft kept the inventory");

		// A second undo after the redo goes back to the visitor's fields again.
		cmdZ(document);
		assert.equal(document.getElementById("wf-name").value, tc.Name, "test " + i + ": second undo name");
		assert.equal(document.getElementById("wf-inventory").value, tc.Inventory,
			"test " + i + ": second undo inventory");
	}
});

// ORDINARY_EDITS are graph edits that take an undo point the usual way, without the fields.
const ORDINARY_EDITS = [
	// Test 0: Clearing the canvas, which sits just above Start over and touches neither field.
	{ Run: (app) => app.wfClearCanvas(), WantSteps: 4 },
	// Test 1: Deleting a step the way the other editing paths do, with a plain snapshot first.
	{
		Run: (app) => {
			app.wfSnapshot();
			app.wfState.nodes = app.wfState.nodes.slice(1);
			app.wfState.edges = [];
			app.renderWorkflow();
		},
		WantSteps: 4,
	},
];

test("case 3: undoing an ordinary edit leaves a name typed after the edit alone", () => {
	for (const [i, tc] of ORDINARY_EDITS.entries()) {
		const { app, document } = mountEditor();
		typeFields(document, "named before", "inv_before");
		tc.Run(app);
		assert.equal(document.getElementById("wf-name").value, "named before",
			"test " + i + ": the edit itself rewrote the name");
		typeFields(document, "typed after the edit", "inv_after");

		cmdZ(document);
		assert.equal(app.wfState.nodes.length, tc.WantSteps, "test " + i + ": undo did not restore the steps");
		assert.equal(document.getElementById("wf-name").value, "typed after the edit",
			"test " + i + ": undo rewrote the name typed after the edit");
		assert.equal(document.getElementById("wf-inventory").value, "inv_after",
			"test " + i + ": undo rewrote the inventory typed after the edit");
	}
});

test("case 4: redoing an ordinary edit leaves the fields as they are", () => {
	const { app, document } = mountEditor();
	app.wfClearCanvas();
	typeFields(document, "first name", "inv_first");
	cmdZ(document);
	typeFields(document, "second name", "inv_second");

	cmdShiftZ(document);
	assert.equal(app.wfState.nodes.length, 0, "redo did not clear the canvas again");
	assert.equal(document.getElementById("wf-name").value, "second name", "redo rewrote the name");
	assert.equal(document.getElementById("wf-inventory").value, "inv_second", "redo rewrote the inventory");
});

test("case 5: only the Start over undo point carries the fields", () => {
	const { app, document } = mountEditor();
	typeFields(document, "prod-deploy", "inv_abc");
	app.wfSnapshot();
	assert.equal("fields" in JSON.parse(app.wfState.past[app.wfState.past.length - 1]), false,
		"an ordinary snapshot carried the fields");

	makeOwnGraph(app);
	startOver(document);
	const entry = JSON.parse(app.wfState.past[app.wfState.past.length - 1]);
	assert.deepEqual({ name: entry.fields.name, inventory: entry.fields.inventory },
		{ name: "prod-deploy", inventory: "inv_abc" }, "the Start over undo point lost the fields");

	cmdZ(document);
	assert.equal("fields" in JSON.parse(app.wfState.future[app.wfState.future.length - 1]), true,
		"the redo entry for Start over dropped the fields");
});
