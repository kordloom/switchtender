// Tests for the approval story's visibility. The release binds a decision to a spec digest and
// records who asked, and the pages have to show it: a held run names the rule that held it, a
// decided run shows the digest the approver bound to, an agent's request reads as an agent's, and
// the audit table renders the chain's own entry kinds as sentences.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

test("the header names the requester, the agent, the rule, and the bound digest", () => {
	const page = loadPage("detail");
	page.app.renderHeader({
		id: "run_a", playbook: "site.yml", status: "pending_approval",
		actor: "prod-remediator", actor_type: "agent",
		held_by_policy: "agent tf approval", risk: { level: "high", reasons: ["destroys"] },
		approved_spec_digest: "sha256:abcdef0123456789",
	});
	const text = page.document.getElementById("run-header").textContent;
	assert.ok(text.includes("prod-remediator (agent)"), "the agent attribution is missing");
	assert.ok(text.includes("Approved spec"), "the bound digest field is missing");
	const callout = page.document.getElementById("risk-callout").textContent;
	assert.ok(callout.includes('agent tf approval'), "the holding rule is not named: " + callout);
});

test("a rejected run is terminal: no cancel, no live stream", () => {
	const page = loadPage("detail");
	assert.equal(page.app.isTerminal("rejected"), true);
	page.app.renderHeader({ id: "run_r", playbook: "site.yml", status: "rejected" });
	assert.equal(page.document.getElementById("cancel-run").hidden, true);
});

test("decision, schedule, and outcome entries read as sentences", () => {
	const page = loadPage("audit");
	const change = (method, path) => page.app.auditChange(method, path);
	assert.equal(change("DECISION", "/runs/run_x/decision/approved"),
		"Approved run run_x, binding its spec digest");
	assert.equal(change("DECISION", "/runs/run_x/decision/rejected"), "Rejected run run_x");
	assert.equal(change("SCHEDULE", "/schedules/sch_1/fired"), "Schedule sch_1 fired");
	assert.equal(change("RUN", "/runs/run_x/outcome/succeeded"), "Run run_x finished succeeded");
});

// The signed receipt is the artifact the product's claim rests on, and it was reachable only from a
// shell on the server. The run page now offers it, for the role that may read the trail it is drawn
// from, and tells the reader the fingerprint to pin when verifying.
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";
import { sandboxOf } from "./loader.mjs";

test("the run page downloads a signed receipt and names the key to pin", async () => {
	const page = loadPage("detail", {
		vars: { RunID: "run_1" },
		routes: {
			"/v1/runs/run_1/receipt": reply("{\"loomseal\":\"0.1\"}", {
				headers: { "Switchtender-Key-Id": "sha256:deadbeef" },
			}),
		},
	});
	sandboxOf(page.app).localStorage.setItem("st_role", "admin");
	const downloads = [];
	sandboxOf(page.app).downloadBlob = (name, type, content) => downloads.push({ name, type, content });
	page.app.loadDetail("run_1");
	await page.clock.flush();

	const btn = page.document.getElementById("download-receipt");
	assert.ok(btn, "the run page offers a receipt control");
	assert.equal(btn.hidden, false, "the receipt control is hidden for an admin session");
	fire(btn, "click");
	await page.clock.flush();
	assert.equal(downloads.length, 1, "clicking the control produced no file");
	assert.match(downloads[0].name, /run_1\.receipt$/);
	assert.match(page.document.getElementById("status").textContent, /sha256:deadbeef/,
		"the reader is not told which fingerprint to pin");
});
