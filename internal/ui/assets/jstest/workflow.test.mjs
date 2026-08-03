// Tests for the workflow editor draft restore in 05-workflow-editor.js: the counter the next node
// id is minted from.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const PARTS = ["01-boot.js", "04-tour.js", "05-workflow-editor.js"];

test("wfNextSeq reads the highest id in the graph, not the node count", () => {
	const app = loadParts(PARTS);
	const tests = [
		// Test 0: A draft written before the counter existed: ids outrun the count after deletions.
		{ Nodes: [{ id: "n0" }, { id: "n2" }, { id: "n4" }], Seq: undefined, Want: 5 },
		// Test 1: An empty graph starts at zero.
		{ Nodes: [], Seq: undefined, Want: 0 },
		// Test 2: A stored counter is honored when it is already past every id.
		{ Nodes: [{ id: "n0" }, { id: "n1" }], Seq: 7, Want: 7 },
		// Test 3: A stored counter behind the ids is raised rather than trusted.
		{ Nodes: [{ id: "n0" }, { id: "n9" }], Seq: 2, Want: 10 },
		// Test 4: Ids in some other shape are ignored, not parsed as numbers.
		{ Nodes: [{ id: "step-a" }, { id: "n3" }, {}], Seq: undefined, Want: 4 },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.wfNextSeq(tc.Nodes, tc.Seq), tc.Want, "test " + i);
	}
});

test("the ids minted after a restore collide with none of the draft's nodes", () => {
	const app = loadParts(PARTS);
	// A draft from before the counter was stored, whose ids outrun its length because steps were
	// deleted along the way. The count said three, so the editor minted n3, n4, n5 for the next
	// three steps and that last one already existed: an edge drawn to it attaches to the wrong node,
	// and deleting either takes both.
	const nodes = [{ id: "n0" }, { id: "n2" }, { id: "n5" }];
	const ids = new Set(nodes.map((n) => n.id));
	let seq = app.wfNextSeq(nodes, undefined);
	// The mint loop, which reads the counter and advances it for each new step.
	for (let i = 0; i < 5; i++) {
		const id = "n" + seq++;
		assert.ok(!ids.has(id), "a minted id collides with a restored node: " + id);
		ids.add(id);
	}
});
