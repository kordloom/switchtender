// Tests for workflow approval steps in the interface: the editor places one and its deny path, and
// the approvals panel lists a waiting step with what comes next and decides it.
import { test } from "node:test";
import assert from "node:assert/strict";

import { ALL_PARTS, sandboxOf } from "./loader.mjs";
import { fire } from "./dom.mjs";
import { failWith, reply, sequence } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// mountEditor opens the workflow page with an empty graph.
function mountEditor() {
	const page = loadPage("workflows", { parts: ALL_PARTS, routes: {} });
	page.app.mountWorkflow();
	page.app.wfState.nodes = [];
	page.app.wfState.edges = [];
	return page;
}

// waitingStep is one step as GET /v1/approvals describes it.
function waitingStep(extra) {
	return Object.assign({
		id: "run_gate", run_id: "run_wf", workflow: "release", step: "gate",
		description: "Check the canary", requested_at: "2026-03-01T12:00:00Z",
		requested_by: "deploy-bot", requested_by_type: "agent",
		upstream: [{ name: "build", status: "succeeded", run_id: "run_build" }],
		on_approve: ["deploy"], on_deny: ["notify"], state_digest: "sha256:shown",
	}, extra || {});
}

// mountRuns opens the runs page answering the approvals queue with steps, signed in with role.
function mountRuns(steps, role, user) {
	const page = loadPage("runs", {
		parts: ALL_PARTS,
		routes: [
			[/^\/v1\/approvals$/, reply({ approvals: steps, count: steps.length, total: steps.length })],
			[/^\/v1\/runs\/run_gate\/(approve|reject)$/, reply({ id: "run_gate", status: "succeeded" })],
		],
	});
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_role", role);
	if (user) win.localStorage.setItem("st_user", user);
	return page;
}

test("an approval step and its deny path serialize to the pipeline the API takes", () => {
	const { app } = mountEditor();
	const tests = [
		// Test 0: Build, an approval with a description and timeout, ship on approval, notify on
		// denial. The approval runs no tool, so it carries none of a tool step's fields.
		{
			Nodes: [
				{ id: "n0", name: "build", tool: "bash", command: "make" },
				{ id: "n1", name: "gate", kind: "approval", tool: "approval", description: "Ship it?", timeout: 600 },
				{ id: "n2", name: "ship", tool: "bash", command: "deploy" },
				{ id: "n3", name: "notify", tool: "bash", command: "page" },
			],
			Edges: [{ from: "n0", to: "n1" }, { from: "n1", to: "n2" }, { from: "n1", to: "n3", deny: true }],
			Want: [
				{ name: "build", tool: "bash", command: "make" },
				{ name: "gate", type: "approval", description: "Ship it?", approval_timeout: 600, depends_on: ["build"] },
				{ name: "ship", tool: "bash", command: "deploy", depends_on: ["gate"] },
				{ name: "notify", tool: "bash", command: "page", if_denied: ["gate"] },
			],
		},
		// Test 1: An approval with no timeout leaves the field out, so it waits until a decision.
		{
			Nodes: [{ id: "n0", name: "gate", kind: "approval", tool: "approval", description: "", timeout: 0 }],
			Edges: [],
			Want: [{ name: "gate", type: "approval" }],
		},
	];
	for (const [i, tc] of tests.entries()) {
		app.wfState.nodes = tc.Nodes;
		app.wfState.edges = tc.Edges;
		assert.deepEqual(JSON.parse(JSON.stringify(app.workflowSteps())), tc.Want, "test " + i);
	}
});

test("a deny path leaves only an approval step and never doubles an approve path", () => {
	const { app, document } = mountEditor();
	app.wfState.nodes = [
		{ id: "n0", name: "build", tool: "bash", command: "make", x: 0, y: 0 },
		{ id: "n1", name: "gate", kind: "approval", tool: "approval", x: 200, y: 0 },
		{ id: "n2", name: "ship", tool: "bash", command: "deploy", x: 400, y: 0 },
	];
	app.wfState.edges = [];
	const tests = [
		// Test 0: A tool step has no deny path.
		{ From: "n0", To: "n2", Deny: true, WantEdges: 0, WantStatus: /Only a step that waits for approval/ },
		// Test 1: An approval step's deny path is drawn.
		{ From: "n1", To: "n2", Deny: true, WantEdges: 1, WantStatus: /runs if gate is denied/ },
		// Test 2: The same pair cannot also be an approve path, since that step could never run.
		{ From: "n1", To: "n2", Deny: false, WantEdges: 1, WantStatus: /cannot both wait/ },
	];
	for (const [i, tc] of tests.entries()) {
		app.linkTo(tc.From, tc.To, tc.Deny);
		assert.equal(app.wfState.edges.length, tc.WantEdges, "test " + i + " edges");
		assert.match(document.getElementById("status").textContent, tc.WantStatus, "test " + i + " status");
	}
});

test("the step dialog saves an approval step without asking for a tool's input", () => {
	const { app, document } = mountEditor();
	app.openStepModal(null);
	document.getElementById("wf-step-name").value = "gate";
	document.getElementById("wf-step-kind").value = "approval";
	fire(document.getElementById("wf-step-kind"), "change");
	assert.equal(document.getElementById("wf-step-tool-fields").hidden, true,
		"an approval step still shows the tool fields");
	document.getElementById("wf-step-description").value = "Check the canary";
	document.getElementById("wf-step-timeout").value = "900";
	fire(document.getElementById("wf-step-form"), "submit");
	const node = app.wfState.nodes.find((n) => n.name === "gate");
	assert.ok(node, "the approval step was not saved: " + document.getElementById("wf-step-status").textContent);
	assert.equal(node.kind, "approval");
	assert.equal(node.description, "Check the canary");
	assert.equal(node.timeout, 900);
});

test("the approvals panel shows a waiting step with what each answer runs", async () => {
	const page = mountRuns([waitingStep({ expires_at: "2099-01-01T00:00:00Z" })], "admin", "approver");
	await page.app.loadApprovalSteps("");
	const host = page.document.getElementById("approval-steps");
	assert.equal(host.hidden, false, "a waiting step is not shown");
	const text = host.textContent;
	for (const want of [/release, gate/, /Check the canary/, /Already ran: build succeeded/,
		/Approving runs deploy/, /Denying runs notify/, /then takes the deny path/, /deploy-bot/]) {
		assert.match(text, want);
	}
});

test("approving from the panel sends the state digest it showed", async () => {
	const page = mountRuns([waitingStep()], "admin", "approver");
	await page.app.loadApprovalSteps("");
	const approve = [...page.document.querySelectorAll("#approval-steps button")]
		.find((b) => b.textContent === "Approve");
	assert.ok(approve, "no Approve button for an admin");
	fire(approve, "click");
	await page.clock.flush();
	// The reason dialog opens first, and an approval with no reason given is sent as it always was.
	fire(page.document.getElementById("reason-go"), "click");
	await page.clock.flush();
	const post = page.net.calls.find((c) => c.method === "POST");
	assert.ok(post, "approving sent nothing");
	assert.equal(post.url, "/v1/runs/run_gate/approve");
	assert.deepEqual(JSON.parse(post.body), { state_digest: "sha256:shown" });
});

test("denying from the panel asks for a reason and sends it with the digest", async () => {
	const page = mountRuns([waitingStep()], "admin", "approver");
	await page.app.loadApprovalSteps("");
	const deny = [...page.document.querySelectorAll("#approval-steps button")]
		.find((b) => b.textContent === "Deny");
	fire(deny, "click");
	await page.clock.flush();
	page.document.getElementById("reason-text").value = "canary is red";
	fire(page.document.getElementById("reason-go"), "click");
	await page.clock.flush();
	const post = page.net.calls.find((c) => c.method === "POST");
	assert.ok(post, "denying sent nothing");
	assert.equal(post.url, "/v1/runs/run_gate/reject");
	assert.deepEqual(JSON.parse(post.body), { state_digest: "sha256:shown", reason: "canary is red" });
});

test("who is offered the decision follows the role and separation of duties", async () => {
	const tests = [
		// Test 0: An admin who did not launch the workflow may approve and deny.
		{ Role: "admin", User: "approver", Distinct: true, WantApprove: true, WantDeny: true },
		// Test 1: The launcher under a rule requiring a second person may only deny.
		{ Role: "admin", User: "deploy-bot", Distinct: true, WantApprove: false, WantDeny: true },
		// Test 2: Without that rule the launcher may approve, as a held run allows.
		{ Role: "admin", User: "deploy-bot", Distinct: false, WantApprove: true, WantDeny: true },
		// Test 3: An operator, or an agent capped at operator, sees the step and decides nothing.
		{ Role: "operator", User: "deploy-bot", Distinct: false, WantApprove: false, WantDeny: false },
	];
	for (const [i, tc] of tests.entries()) {
		const page = mountRuns([waitingStep({ require_distinct_approver: tc.Distinct })], tc.Role, tc.User);
		await page.app.loadApprovalSteps("");
		const buttons = [...page.document.querySelectorAll("#approval-steps button")];
		const shown = (label) => buttons.some((b) => b.textContent === label && !b.hidden);
		assert.equal(page.document.getElementById("approval-steps").hidden, false, "test " + i + " panel");
		assert.equal(shown("Approve"), tc.WantApprove, "test " + i + " approve");
		assert.equal(shown("Deny"), tc.WantDeny, "test " + i + " deny");
	}
});

test("a run page shows only its own workflow's waiting step", async () => {
	const page = loadPage("detail", {
		parts: ALL_PARTS,
		vars: { RunID: "run_wf" },
		routes: [[/^\/v1\/approvals$/, reply({ approvals: [waitingStep(), waitingStep({ id: "run_other",
			run_id: "run_other_wf", step: "other" })] })]],
	});
	await page.app.loadApprovalSteps("run_wf");
	const entries = page.document.querySelectorAll("#approval-steps .approval-step");
	assert.equal(entries.length, 1, "the run page lists another workflow's step");
	assert.equal(entries[0].dataset.stepId, "run_gate");
});

test("a workflow's step list names its approval step by where it stands", () => {
	const page = loadPage("detail", { parts: ALL_PARTS, vars: { RunID: "run_wf" }, routes: {} });
	page.app.renderSteps([
		{ id: "run_build", step_name: "build", step_index: 0, status: "succeeded", tool: "bash", command: "make" },
		{ id: "run_gate", step_name: "gate", step_index: 1, status: "pending_approval", kind: "approval" },
		{ id: "run_gate2", step_name: "late", step_index: 2, status: "failed", kind: "approval",
			error: "timed out: nobody decided within 60s, so the deny path was taken" },
	]);
	const text = page.document.getElementById("steps").textContent;
	assert.match(text, /2\. gate {2}· {2}approval, waiting for a decision/);
	assert.match(text, /3\. late {2}· {2}approval, timed out/);
});

// REFRESH is the period the approvals panel polls on, in virtual milliseconds.
const REFRESH = 15000;

// pollingPanel loads the approvals panel on the runs page answering the queue with answers in order,
// and starts the refresh the page starts when it mounts.
async function pollingPanel(...answers) {
	const page = loadPage("runs", {
		parts: ALL_PARTS,
		routes: [[/^\/v1\/approvals$/, sequence(...answers)]],
	});
	await page.app.loadApprovalSteps("");
	page.app.wireApprovalsAutoRefresh("");
	return { page, host: page.document.getElementById("approval-steps") };
}

test("a step that arrives while the page is open shows on the next refresh", async () => {
	// The panel used to be read once, when the page loaded. A workflow that reached its approval step
	// afterward never offered Approve until the approver reloaded.
	const { page, host } = await pollingPanel(reply({ approvals: [] }),
		reply({ approvals: [waitingStep()] }));
	assert.equal(host.hidden, true, "an empty queue showed the panel");
	await page.clock.tick(REFRESH);
	assert.equal(host.hidden, false, "a step that arrived after the page loaded never appeared");
	assert.match(host.textContent, /release, gate/);
});

test("a refresh that finds the same queue leaves the panel's controls alone", async () => {
	// Redrawing every poll would drop keyboard focus from the Approve button the reader is on.
	const same = reply({ approvals: [waitingStep()] });
	const { page, host } = await pollingPanel(same, same, same);
	const approve = [...host.querySelectorAll("button")].find((b) => b.textContent === "Approve");
	await page.clock.tick(REFRESH);
	await page.clock.tick(REFRESH);
	const after = [...host.querySelectorAll("button")].find((b) => b.textContent === "Approve");
	// Compared with ok rather than equal: a failing equal prints both nodes, and a node drags in the
	// whole document with it.
	assert.ok(after === approve, "an unchanged queue was redrawn, which discards the reader's focus");
});

test("a refresh that finds a changed queue redraws it", async () => {
	const { page, host } = await pollingPanel(reply({ approvals: [waitingStep()] }),
		reply({ approvals: [waitingStep(), waitingStep({ id: "run_gate2", run_id: "run_wf2",
			workflow: "nightly", step: "gate" })] }));
	assert.equal(host.querySelectorAll(".approval-step").length, 1);
	await page.clock.tick(REFRESH);
	assert.equal(host.querySelectorAll(".approval-step").length, 2, "a second waiting step was not added");
});

test("a failed refresh hides the panel and the next good one brings it back", async () => {
	// The fetch that failed must not leave the queue it last drew looking current, or the same queue
	// coming back would be skipped as unchanged and the panel would stay hidden.
	const same = reply({ approvals: [waitingStep()] });
	const { page, host } = await pollingPanel(same, failWith(new Error("server went away")), same);
	assert.equal(host.hidden, false);
	await page.clock.tick(REFRESH);
	assert.equal(host.hidden, true, "a failed read left a stale panel on screen");
	await page.clock.tick(REFRESH);
	assert.equal(host.hidden, false, "the panel stayed hidden after the server answered again");
	assert.match(host.textContent, /release, gate/);
});

test("a decision still redraws the panel when the queue it finds is unchanged", async () => {
	const page = mountRuns([waitingStep()], "admin", "approver");
	await page.app.loadApprovalSteps("");
	const before = page.document.querySelector("#approval-steps .approval-step");
	await page.app.loadApprovalSteps("");
	assert.ok(page.document.querySelector("#approval-steps .approval-step") !== before,
		"a non-quiet reload skipped the redraw, so a failed decision could not put its buttons back");
});
