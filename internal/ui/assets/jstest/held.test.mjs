// Tests for the held page in 32-held.js: every change waiting for a person, or with ?who=agent only
// the runs an AI agent asked for, drawn from GET /v1/attention with why each is held, who can decide
// it, and what each answer runs. The hosted demo lands here, and a visitor sent to the runs list
// instead met install-wide counts, a person's workflow, and two rows with no explanation.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";

// AGENT_HOLD is the name the server records on a run the built-in agent hold keeps waiting.
const AGENT_HOLD = "requested by an agent, held by default";

// heldItem builds one approval_needed item as GET /v1/attention answers it.
function heldItem(key, kind, name, heldBy, reason) {
	const approval = heldBy
		? { scope: "run", decision_id: key, held_by: heldBy,
			approvers: { role: "admin", excluded: "ops-lead", agents: false },
			on_approve: "It joins the default queue and starts on a free worker.",
			on_deny: "It ends rejected and never runs." }
		: undefined;
	return {
		key, kind, run_id: key, name, blocker: "approval_needed",
		since: new Date(Date.now() - 3600000).toISOString(), waiting_seconds: 3600,
		alert_after_seconds: 0, alerting: false,
		main: { blocker: "approval_needed", run_id: key, reason,
			who_can_act: "An admin other than ops-lead can approve it. No agent can approve it.",
			next: "Approve: it runs. Deny: it never runs.", approval },
	};
}

// liveAnswer is the hosted demo's attention answer: a bot's apply held by a rule, the agent's two
// held runs, and a person's workflow waiting at its approval step.
function liveAnswer() {
	const items = [
		heldItem("run_rule", "run", "terraform run_rule",
			"terraform destroys need a second approver (plan destroys 3, limit 0)",
			"The change proposed from run run_plan is held for approval by rule \"terraform destroys\"."),
		heldItem("run_restart", "run", "/tmp/switchtender-demo-assets-1/restart-app.yml", AGENT_HOLD,
			"It is held for approval: requested by an agent, held by default."),
		heldItem("run_apply", "run", "terraform run_apply", AGENT_HOLD,
			"It is held for approval: requested by an agent, held by default. An agent asked for this apply."),
		heldItem("run_release", "workflow", "Release 4.3", "",
			"The workflow is waiting at approval step \"approve-release\"."),
	];
	return { counts: { worker_lost: 0, no_worker: 0, approval_needed: 4, blocked: 0 },
		total: 4, count: 4, items };
}

// heldWith loads the held page at search with the attention endpoint answering body, as a read-only
// demo served at host when demoHost is set.
async function heldWith(body, search, demoHost) {
	const page = loadPage("held", {
		search: search || "",
		vars: demoHost ? { ReadOnly: true, Demo: true } : {},
		routes: { "/v1/attention": body },
	});
	if (demoHost) sandboxOf(page.app).location.hostname = demoHost;
	await page.app.loadHeld();
	for (let i = 0; i < 4; i++) await page.clock.flush();
	return page;
}

// listed returns the keys of the items the page shows.
function listed(document) {
	return Array.from(document.querySelectorAll("#held-list .attn-item")).map((el) => el.dataset.key);
}

// nextLinks returns the hrefs under Where to go next.
function nextLinks(document) {
	return Array.from(document.querySelectorAll("#held-next-links a")).map((a) => a.getAttribute("href"));
}

test("the agent's page shows only the runs an AI agent asked for, each with why it is held", async () => {
	const { document } = await heldWith(reply(liveAnswer()), "?who=agent");
	assert.deepEqual(listed(document), ["run_restart", "run_apply"]);
	assert.equal(document.getElementById("held-title").textContent, "An AI agent asked. A person decides.");
	assert.match(document.getElementById("held-lede").textContent, /an agent can never approve its own request/);
	assert.equal(document.getElementById("held-how").hidden, false);
	const apply = document.querySelector('#held-list .attn-item[data-key="run_apply"]');
	assert.match(apply.querySelector(".attn-reason").textContent, /requested by an agent/);
	assert.match(apply.textContent, /No agent can approve it/);
	assert.equal(document.getElementById("status").hidden, true);
});

test("the agent's page counts its runs and links the count of everything waiting", async () => {
	const { document } = await heldWith(reply(liveAnswer()), "?who=agent");
	const count = document.getElementById("held-count");
	assert.equal(count.hidden, false);
	assert.match(count.textContent, /^2 runs an AI agent asked for are waiting for a person\./);
	const all = count.querySelector("a");
	assert.equal(all.getAttribute("href"), "/ui/held");
	assert.equal(all.textContent, "See all 4 waiting for a person on this install");
});

test("one agent run reads in the singular and no all link shows when it is everything", async () => {
	const body = liveAnswer();
	body.items = [body.items[1]];
	body.counts.approval_needed = 1;
	const { document } = await heldWith(reply(body), "?who=agent");
	const count = document.getElementById("held-count");
	assert.equal(count.textContent, "1 run an AI agent asked for is waiting for a person.");
	assert.equal(count.querySelector("a"), null);
});

test("without who the page shows everything waiting for a person, workflows included", async () => {
	const { document } = await heldWith(reply(liveAnswer()), "");
	assert.deepEqual(listed(document), ["run_rule", "run_restart", "run_apply", "run_release"]);
	assert.equal(document.getElementById("held-title").textContent, "Waiting for a person");
	assert.equal(document.getElementById("held-count").textContent, "4 changes are waiting for a person.");
	assert.equal(document.getElementById("held-how").hidden, true);
});

test("the read-only demo says where a decision is made and draws no decision control", async () => {
	const { document } = await heldWith(reply(liveAnswer()), "?who=agent", "demo.switchtender.com");
	const decide = document.getElementById("held-decide");
	assert.equal(decide.hidden, false);
	assert.match(decide.textContent,
		/^Disabled in this read-only demo\. On a writable install, an admin opens the run and presses Approve or Deny there\./);
	assert.equal(document.querySelectorAll("#held-list button").length, 0);
});

test("where to go next opens the first held run, the register, and on the hosted demo the proof", async () => {
	const hosted = await heldWith(reply(liveAnswer()), "?who=agent", "demo.switchtender.com");
	assert.deepEqual(nextLinks(hosted.document),
		["/ui/runs/run_restart", "/ui/audit/register", "https://switchtender.com/#proof"]);
	const local = await heldWith(reply(liveAnswer()), "?who=agent", "127.0.0.1");
	assert.deepEqual(nextLinks(local.document), ["/ui/runs/run_restart", "/ui/audit/register"]);
});

test("an agent's page with nothing held says so", async () => {
	const body = liveAnswer();
	body.items = [body.items[0]];
	body.counts.approval_needed = 1;
	const { document } = await heldWith(reply(body), "?who=agent");
	assert.deepEqual(listed(document), []);
	assert.match(document.getElementById("status").textContent, /No run an AI agent asked for is waiting right now\./);
	assert.equal(document.getElementById("held-decide").hidden, true);
});
