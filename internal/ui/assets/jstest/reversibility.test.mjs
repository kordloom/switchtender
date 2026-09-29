// Tests for whether a held run says it cannot be taken back, in the place the decision is made.
//
// Risk and reversibility answer different halves of one question. Risk says how bad the outcome is;
// reversibility says whether there is a second chance. A fleet restart grades high risk and undoes
// itself. Deleting a backup set reads quietly and is permanent. An approver shown only the first
// releases the second.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

test("a held run that cannot be undone says so beside the risk grade", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_x", playbook: "wipe.yml", status: "pending_approval",
		held_by_policy: "hold the permanent",
		risk: { level: "high", reasons: ["destroys infrastructure"] },
		reversibility: {
			class: "irreversible",
			reasons: ["a task removes a file with state absent, which destroys what it held"],
		},
	});
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.textContent.includes("irreversible"),
		"the callout does not say the change is permanent: " + callout.textContent);
	// Both gradings' reasons, because the approver decides once.
	assert.ok(callout.textContent.includes("state absent"),
		"the reversibility reason is missing, so the badge has nothing behind it");
	assert.ok(callout.textContent.includes("destroys infrastructure"),
		"the risk reason was dropped when reversibility was added beside it");
	assert.ok(callout.querySelector(".undo-irreversible"),
		"the permanent grade is not styled as the one an approver must not miss");
});

test("a recoverable run is not dressed up as a warning", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_y", playbook: "restart.yml", status: "pending_approval",
		risk: { level: "medium", reasons: ["changes state"] },
		reversibility: { class: "costly", reasons: ["changes state, so undoing it means running something else"] },
	});
	const callout = page.document.getElementById("risk-callout");
	assert.ok(callout.querySelector(".undo-costly"),
		"a recoverable run is not styled neutrally; coloring most work as a warning makes the " +
		"signal meaningless");
	assert.ok(!callout.querySelector(".undo-irreversible"), "a restart is marked permanent");
});

test("a run with no grade renders without one rather than breaking", () => {
	const page = loadPage("detail");
	page.app.renderHeader({ id: "run_z", playbook: "site.yml", status: "succeeded" });
	assert.ok(page.document.getElementById("run-header"), "the header failed to render");
});
