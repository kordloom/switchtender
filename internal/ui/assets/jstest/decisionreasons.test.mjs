// Tests for approver reasons in the interface: the field says the reason is audit evidence, a
// reason the server masked is shown and confirmed before anything is recorded, a rule's requirement
// is enforced before the request, and a run's decisions read as a thread with corrections and
// redactions an admin can add but never an edit.
import { test } from "node:test";
import assert from "node:assert/strict";

import { sandboxOf } from "./loader.mjs";
import { fire } from "./dom.mjs";
import { reply, sequence } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// heldRun is a run waiting for a decision, with whatever the case changes.
function heldRun(extra) {
	return Object.assign({
		id: "run_1", status: "pending_approval", actor: "deploy-bot", actor_type: "agent",
		held_by_policy: "production changes", created_at: "2026-10-01T12:00:00Z",
		tool: "bash", command: "deploy",
	}, extra || {});
}

// openHeld opens the detail page for a held run as an admin, with the approve route answering from
// approveReply and the decisions route from decisions.
async function openHeld(run, approveReply, decisions) {
	const page = loadPage("detail", {
		vars: { RunID: run.id },
		routes: [
			[/^\/v1\/runs\/run_1\/events/, reply({ events: [] })],
			[/^\/v1\/runs\/run_1\/logs/, reply("")],
			[/^\/v1\/runs\/run_1\/decisions$/, reply({ decisions: decisions || [], count: 0 })],
			[/^\/v1\/runs\/run_1\/(approve|reject)$/, approveReply || reply(run)],
			[/^\/v1\/auth\/me$/, reply({ name: "ops-admin", role: "admin", actor_type: "session" })],
			[/^\/v1\/runs\/run_1$/, reply(run)],
		],
	});
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_role", "admin");
	win.localStorage.setItem("st_user", "ops-admin");
	await page.app.loadDetail(run.id);
	await page.clock.flush();
	return page;
}

test("the reason field says the reason is permanent audit evidence", async () => {
	const page = await openHeld(heldRun());
	fire(page.document.getElementById("approve-run"), "click");
	await page.clock.flush();
	const modal = page.document.getElementById("reason-modal");
	assert.ok(modal && !modal.hidden, "approving opens no reason dialog");
	assert.equal(page.document.getElementById("reason-label").textContent,
		"Stored as audit evidence. Don't include secrets or personal data.");
	assert.equal(page.document.getElementById("reason-note").textContent,
		"Masked for known secrets before storage. Treat it as permanent audit data.");
	assert.equal(page.document.getElementById("reason-text").getAttribute("maxlength"), "1000");
});

test("a masked reason is shown and confirmed before anything is recorded", async () => {
	const posts = sequence(
		reply({ error: "masked", masked_reason: "approved, used token=***" }, { status: 409 }),
		reply(heldRun({ status: "pending" })),
	);
	const page = await openHeld(heldRun(), posts);
	fire(page.document.getElementById("approve-run"), "click");
	await page.clock.flush();
	page.document.getElementById("reason-text").value = "approved, used token=abc123";
	fire(page.document.getElementById("reason-go"), "click");
	await page.clock.flush();
	const box = page.document.getElementById("reason-masked-box");
	assert.equal(box.hidden, false, "the masked text was not shown for confirmation");
	assert.equal(page.document.getElementById("reason-masked").textContent, "approved, used token=***");
	assert.match(page.document.getElementById("reason-masked-intro").textContent,
		/Nothing has been recorded yet/);
	fire(page.document.getElementById("reason-go"), "click");
	await page.clock.flush();
	const sent = page.net.calls.filter((c) => c.method === "POST");
	assert.equal(sent.length, 2, "the confirmation was not sent");
	assert.deepEqual(JSON.parse(sent[0].body), { reason: "approved, used token=abc123" });
	assert.deepEqual(JSON.parse(sent[1].body),
		{ reason: "approved, used token=abc123", masked_reason: "approved, used token=***" });
	assert.ok(sandboxOf(page.app).location.navigations.includes("reload"),
		"the page did not reload after the decision");
});

test("going back from the masked text records nothing", async () => {
	const page = await openHeld(heldRun(),
		reply({ error: "masked", masked_reason: "token=***" }, { status: 409 }));
	fire(page.document.getElementById("approve-run"), "click");
	await page.clock.flush();
	page.document.getElementById("reason-text").value = "token=abc";
	fire(page.document.getElementById("reason-go"), "click");
	await page.clock.flush();
	fire(page.document.getElementById("reason-cancel"), "click");
	await page.clock.flush();
	const sent = page.net.calls.filter((c) => c.method === "POST");
	assert.equal(sent.length, 1, "declining the masked text still sent a decision");
	assert.equal(page.document.getElementById("approve-run").disabled, false,
		"the Approve button stays disabled after the approver went back");
});

test("a rule's reason requirement keeps the decision from being sent bare", async () => {
	const tests = [
		// Test 0: Always: an approval needs a reason.
		{ Requirement: "always", Button: "approve-run", WantDisabled: true },
		// Test 1: Denials: an approval does not.
		{ Requirement: "denials", Button: "approve-run", WantDisabled: false },
		// Test 2: Denials: a rejection does.
		{ Requirement: "denials", Button: "reject-run", WantDisabled: true },
		// Test 3: No requirement: nothing is required.
		{ Requirement: "", Button: "reject-run", WantDisabled: false },
	];
	for (const [i, tc] of tests.entries()) {
		const page = await openHeld(heldRun({ require_reason: tc.Requirement }));
		fire(page.document.getElementById(tc.Button), "click");
		await page.clock.flush();
		const go = page.document.getElementById("reason-go");
		assert.equal(go.disabled, tc.WantDisabled, "test " + i + " before typing");
		assert.equal(page.document.getElementById("reason-required").hidden, !tc.WantDisabled,
			"test " + i + " requirement note");
		const field = page.document.getElementById("reason-text");
		field.value = "ticket OPS-12";
		fire(field, "input");
		assert.equal(go.disabled, false, "test " + i + " after typing");
	}
});

test("a run's decisions read as a thread, and only an admin may correct or redact", async () => {
	const records = [
		{ id: "aud_d", kind: "decision", decision_id: "aud_d", run_id: "run_1", verdict: "approved",
			at: "2026-10-01T12:05:00Z", actor: "ops-admin", actor_type: "session",
			reason: { text: "change window confirmed", random: "ab", commitment: "sha256:c", masked: true },
			separation_of_duties: { required: true, requester: "dev-lead", decider: "ops-admin",
				independent: true, result: "satisfied" } },
		{ id: "aud_c", kind: "correction", decision_id: "aud_d", run_id: "run_1",
			at: "2026-10-01T12:10:00Z", actor: "ops-admin",
			reason: { commitment: "sha256:e", redacted: { actor: "privacy-admin", category: "personal_data",
				at: "2026-10-02T09:00:00Z" } } },
	];
	for (const [i, tc] of [{ Role: "admin", WantButtons: true }, { Role: "operator", WantButtons: false }]
		.entries()) {
		const page = loadPage("detail", { vars: { RunID: "run_1" }, routes: {} });
		sandboxOf(page.app).localStorage.setItem("st_role", tc.Role);
		page.app.renderDecisions("run_1", records);
		const panel = page.document.getElementById("decisions-panel");
		assert.equal(panel.hidden, false, "test " + i + " the decisions are not shown");
		const text = panel.textContent;
		for (const want of [/Approved by ops-admin/, /change window confirmed/, /masked for known secrets/,
			/Correction by ops-admin/, /Reason redacted \(personal data\) by privacy-admin/,
			/bound account dev-lead counts as the requester/]) {
			assert.match(text, want, "test " + i);
		}
		const buttons = [...panel.querySelectorAll("button")].map((b) => b.textContent);
		assert.equal(buttons.includes("Add correction"), tc.WantButtons, "test " + i + " correction");
		assert.equal(buttons.includes("Redact reason"), tc.WantButtons, "test " + i + " redact");
		assert.equal(buttons.includes("Redact correction"), false,
			"test " + i + " an already redacted correction is offered for redaction again");
		assert.equal(buttons.some((b) => /edit/i.test(b)), false, "test " + i + " a reason is editable");
	}
});

test("a correction and a redaction post what the server takes", async () => {
	const record = { id: "aud_d", kind: "decision", decision_id: "aud_d", run_id: "run_1",
		verdict: "rejected", at: "2026-10-01T12:05:00Z", actor: "ops-admin",
		reason: { text: "not during the freeze", random: "ab", commitment: "sha256:c" } };
	const page = loadPage("detail", {
		vars: { RunID: "run_1" },
		routes: [
			[/^\/v1\/runs\/run_1\/decisions\/aud_d\/corrections$/, reply({ id: "aud_x" }, { status: 201 })],
			[/^\/v1\/runs\/run_1\/decisions\/aud_d\/redact$/, reply(record)],
			[/^\/v1\/runs\/run_1\/decisions$/, reply({ decisions: [record], count: 1 })],
		],
	});
	sandboxOf(page.app).localStorage.setItem("st_role", "admin");
	page.app.renderDecisions("run_1", [record]);
	const button = (label) => [...page.document.querySelectorAll("#decisions button")]
		.find((b) => b.textContent === label);
	fire(button("Add correction"), "click");
	await page.clock.flush();
	assert.equal(page.document.getElementById("reason-go").disabled, true,
		"an empty correction can be sent");
	const field = page.document.getElementById("reason-text");
	field.value = "the freeze ended on Friday";
	fire(field, "input");
	fire(page.document.getElementById("reason-go"), "click");
	await page.clock.flush();
	const correction = page.net.calls.find((c) => c.url.endsWith("/corrections"));
	assert.ok(correction, "the correction was not sent");
	assert.deepEqual(JSON.parse(correction.body), { text: "the freeze ended on Friday" });

	fire(button("Redact reason"), "click");
	page.document.getElementById("redact-category").value = "secret";
	fire(page.document.getElementById("redact-go"), "click");
	await page.clock.flush();
	const redaction = page.net.calls.find((c) => c.url.endsWith("/redact"));
	assert.ok(redaction, "the redaction was not sent");
	assert.deepEqual(JSON.parse(redaction.body), { category: "secret" });
});

test("the audit page reads what happened to a reason as a sentence", () => {
	const page = loadPage("audit", { routes: {} });
	const tests = [
		// Test 0: A correction.
		{ Path: "/runs/run_1/decisions/aud_d/corrections/aud_c", Want: "Added a correction to a decision on run run_1" },
		// Test 1: A decision's reason redacted.
		{ Path: "/runs/run_1/decisions/aud_d/reason_redacted/personal_data", Want: "Redacted a reason on run run_1 (personal data)" },
		// Test 2: A correction's text redacted.
		{ Path: "/runs/run_1/decisions/aud_d/corrections/aud_c/reason_redacted/secret", Want: "Redacted a reason on run run_1 (secret)" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(page.app.auditChange("REASON", tc.Path), tc.Want, "test " + i);
	}
});
