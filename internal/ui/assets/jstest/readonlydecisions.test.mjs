// Tests for the gate's own controls on a read-only server. A read-only server refuses every
// decision, so the approval panel's Approve and Deny are drawn disabled with the reason, the way
// every other mutating control is, and pressing one opens nothing, posts nothing, and reports no
// failure. A held run's page keeps Approve and Reject, disabled, beside a note naming who decides
// the run on a writable install.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { sandboxOf } from "./loader.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// REASONS is what a disabled control says, by whether the read-only server is the demo.
const REASONS = { true: "Disabled in this read-only demo", false: "Disabled on this read-only server" };

// waitingStep is a person's release waiting at its approval step, as GET /v1/approvals describes it.
function waitingStep() {
	return {
		id: "run_gate", run_id: "run_wf", workflow: "Release 4.3", step: "approve-release",
		description: "Release 4.3 built cleanly on every host. Ship it to production?",
		requested_at: "2026-03-01T12:00:00Z", requested_by: "admin", requested_by_type: "session",
		upstream: [{ name: "build", status: "succeeded", run_id: "run_build" }],
		on_approve: ["deploy"], on_deny: [], state_digest: "sha256:shown",
	};
}

// heldRun is a run the built-in agent hold is keeping waiting, as GET /v1/runs/{id} returns it.
function heldRun(extra) {
	return Object.assign({
		id: "run_1", status: "pending_approval", actor: "remediation-agent", actor_type: "agent",
		held_by_policy: "requested by an agent, held by default", require_distinct_approver: false,
		hold_note: "An agent asked for this apply, so it waits for a person before anything plans.",
		created_at: "2026-03-01T12:00:00Z", tool: "terraform", command: "infra/network",
	}, extra || {});
}

// readOnlyRuns mounts the runs page read-only with one waiting step, as the demo or as an install.
function readOnlyRuns(demo) {
	return loadPage("runs", {
		vars: { ReadOnly: true, Demo: demo },
		quiet: true,
		routes: [
			[/^\/v1\/approvals$/, reply({ approvals: [waitingStep()], count: 1, total: 1 })],
			[/^\/v1\/runs\/run_gate\/(approve|reject)$/,
				reply({ error: "this server is read-only" }, { status: 403 })],
		],
	});
}

// readOnlyDetail mounts a run's page read-only, as the demo or as an install, and loads the run.
async function readOnlyDetail(run, demo) {
	const page = loadPage("detail", {
		vars: { RunID: run.id, ReadOnly: true, Demo: demo },
		quiet: true,
		routes: [
			[new RegExp("^/v1/runs/" + run.id + "$"), reply(run)],
			[new RegExp("^/v1/runs/" + run.id + "/events(\\?|$)"), reply({ events: [] })],
			[new RegExp("^/v1/runs/" + run.id + "/steps(\\?|$)"), reply({ steps: [] })],
			[/^\/v1\/approvals$/, reply({ approvals: [], count: 0, total: 0 })],
		],
	});
	page.app.loadDetail(run.id);
	await page.clock.flush();
	return page;
}

test("the approval panel draws Approve and Deny disabled with the reason on a read-only server", async () => {
	for (const demo of [true, false]) {
		const page = readOnlyRuns(demo);
		await page.app.loadApprovalSteps("");
		const entry = page.document.querySelector("#approval-steps .approval-step");
		assert.ok(entry, "demo=" + demo + ": the waiting step was not drawn");
		const buttons = [...entry.querySelectorAll("button")];
		const approve = buttons.find((b) => b.textContent === "Approve");
		const deny = buttons.find((b) => b.textContent === "Deny");
		assert.ok(approve && deny, "demo=" + demo + ": the step offers no Approve and Deny to look at");
		for (const btn of [approve, deny]) {
			assert.equal(btn.hidden, false, "demo=" + demo + ": " + btn.textContent + " is hidden");
			assert.equal(btn.disabled, true, "demo=" + demo + ": " + btn.textContent + " is live, and " +
				"its only future is the server's refusal");
			assert.equal(btn.title, REASONS[demo] + ".", "demo=" + demo + ": " + btn.textContent +
				" does not say why it is disabled");
		}
		assert.match(entry.textContent, /An admin decides this step here/,
			"demo=" + demo + ": nothing says who decides the step on a writable install");
		assert.match(entry.textContent, new RegExp(REASONS[demo]),
			"demo=" + demo + ": the entry does not say the server is read-only");
	}
});

test("pressing a disabled Approve in the read-only demo posts nothing and reports no failure", async () => {
	const page = readOnlyRuns(true);
	await page.app.loadApprovalSteps("");
	const approve = [...page.document.querySelectorAll("#approval-steps button")]
		.find((b) => b.textContent === "Approve");
	fire(approve, "click");
	// A live Approve builds the reason dialog in the same turn as the click, so it is checked before
	// anything is awaited. The check compares a boolean: a failure that carried the element itself
	// would spend minutes printing the whole document it belongs to.
	assert.equal(page.document.getElementById("reason-go") !== null, false,
		"the read-only demo opened the reason dialog for a decision it refuses");
	await page.clock.flush();
	assert.equal(page.net.calls.some((c) => c.method === "POST"), false,
		"the read-only demo posted a decision: " + page.net.urls.join(", "));
	assert.doesNotMatch(page.document.getElementById("status").textContent, /failed/,
		"the first panel of the demo reported a failure");
});

test("a held run's page keeps Approve and Reject, disabled, and says who decides it", async () => {
	const tests = [
		// Test 0: The agent's held run in the demo shows the decision an admin would make.
		{ Run: heldRun(), Demo: true, WantShown: true, WantWho: /^An admin decides this run here/ },
		// Test 1: An agent's run under a second-person rule excludes the account the agent acts for
		// from approving, and leaves the reject to any admin.
		{ Run: heldRun({ account: "ops-lead", require_distinct_approver: true }), Demo: true,
			WantShown: true, WantWho: new RegExp("^An admin other than ops-lead, the account agent " +
				"remediation-agent acts for, approves this run here, and any admin can reject it\\. ") },
		// Test 2: A read-only install that is not the demo says so in its own words.
		{ Run: heldRun(), Demo: false, WantShown: true, WantWho: /^An admin decides this run here/ },
		// Test 3: A run nobody is deciding draws no decision and no note.
		{ Run: heldRun({ status: "succeeded", held_by_policy: "", hold_note: "" }), Demo: true,
			WantShown: false },
		// Test 4: A workflow paused at a step is decided in the approval panel, which draws its own.
		{ Run: heldRun({ id: "run_wf", kind: "pipeline", started_at: "2026-03-01T12:00:00Z",
			tool: "", command: "" }), Demo: true, WantShown: false },
		// Test 5: A person's run under a second-person rule excludes the account behind the credential.
		{ Run: heldRun({ actor: "casey-cli", actor_type: "token", account: "casey",
			require_distinct_approver: true }), Demo: true, WantShown: true,
			WantWho: /^An admin other than casey approves this run here, and any admin can reject it\. / },
		// Test 6: A run bound to no account excludes the credential that asked.
		{ Run: heldRun({ actor: "deploy-bot", actor_type: "token", require_distinct_approver: true }),
			Demo: true, WantShown: true,
			WantWho: /^An admin other than deploy-bot approves this run here, and any admin can reject it\. / },
	];
	for (const [i, tc] of tests.entries()) {
		const page = await readOnlyDetail(tc.Run, tc.Demo);
		const approve = page.document.getElementById("approve-run");
		const reject = page.document.getElementById("reject-run");
		const note = page.document.getElementById("decide-note");
		assert.equal(approve.hidden, !tc.WantShown, "test " + i + ": Approve hidden");
		assert.equal(reject.hidden, !tc.WantShown, "test " + i + ": Reject hidden");
		if (!tc.WantShown) {
			assert.equal(note !== null, false, "test " + i + ": a note explains a decision nobody is making");
			continue;
		}
		for (const btn of [approve, reject]) {
			assert.equal(btn.disabled, true, "test " + i + ": " + btn.textContent + " is live");
			assert.equal(btn.title, REASONS[tc.Demo] + ".", "test " + i + ": " + btn.textContent +
				" does not say why it is disabled");
		}
		assert.ok(note, "test " + i + ": nothing beside the buttons says who decides the run");
		assert.match(note.textContent, tc.WantWho, "test " + i + ": who decides");
		assert.match(note.textContent, new RegExp(REASONS[tc.Demo] + "\\.$"),
			"test " + i + ": the note does not say the server is read-only");
		assert.equal(page.document.querySelector("main.content .actions").contains(note), true,
			"test " + i + ": the note is not beside the buttons");
	}
});

test("a held run's note survives a header refresh without doubling", async () => {
	const page = await readOnlyDetail(heldRun(), true);
	page.app.renderHeader(heldRun());
	page.app.renderHeader(heldRun());
	const notes = page.document.querySelectorAll("#decide-note");
	assert.equal(notes.length, 1, "every header refresh added another note");
});

// requesterDetail mounts a writable run page signed in as the person who asked for a held run.
async function requesterDetail(role) {
	const run = heldRun({ actor: "casey", actor_type: "session", held_by_policy: "production apply",
		require_distinct_approver: true, tool: "bash", command: "deploy" });
	const page = loadPage("detail", {
		vars: { RunID: run.id },
		quiet: true,
		routes: [
			[/^\/v1\/runs\/run_1$/, reply(run)],
			[/^\/v1\/runs\/run_1\/events(\?|$)/, reply({ events: [] })],
			[/^\/v1\/approvals$/, reply({ approvals: [], count: 0, total: 0 })],
		],
	});
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_role", role);
	win.localStorage.setItem("st_user", "casey");
	page.app.loadDetail(run.id);
	await page.clock.flush();
	return page;
}

test("the requester is told about the way out they are drawn, reject for an admin and cancel below", async () => {
	const tests = [
		// Test 0: An admin requester keeps Reject, since withdrawing a request needs nobody else.
		{ Role: "admin", WantReject: true, WantStatus: /You can still reject it to withdraw the request\./ },
		// Test 1: An operator requester is refused a reject by the server, and is drawn Cancel.
		{ Role: "operator", WantReject: false, WantStatus: /You can still cancel it to withdraw the request\./ },
	];
	for (const [i, tc] of tests.entries()) {
		const page = await requesterDetail(tc.Role);
		assert.equal(page.document.getElementById("approve-run").hidden, true, "test " + i + ": Approve");
		assert.equal(page.document.getElementById("reject-run").hidden, !tc.WantReject, "test " + i + ": Reject");
		assert.equal(page.document.getElementById("cancel-run").hidden, false, "test " + i + ": Cancel");
		assert.match(page.document.getElementById("status").textContent, tc.WantStatus,
			"test " + i + ": the status names a control the requester is not drawn");
	}
});
