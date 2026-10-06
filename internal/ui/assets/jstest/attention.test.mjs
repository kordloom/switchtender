// Tests for the overview's Needs attention panel in 28-attention.js: four counts answering what is
// stopping work, each a filter over the items beneath them, and each item saying its main blocker,
// its other conditions, how long it has been in its current blocker, who can act, and what happens
// next. There is no requeue control, by design: the lease sweep's reclaim is the safe version.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { failWith, reply } from "./net.mjs";
import { sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";

// minutesAgo returns the ISO time n minutes before the sandbox clock's now.
function minutesAgo(n) {
	return new Date(Date.now() - n * 60000).toISOString();
}

// answer is what GET /v1/attention returns for a workflow whose worker was lost while another
// branch waits at an approval step, a run no worker serves, and a held run.
function answer() {
	return {
		counts: { worker_lost: 1, no_worker: 1, approval_needed: 1, blocked: 0 },
		total: 3, count: 3,
		items: [{
			key: "run_wf", kind: "workflow", run_id: "run_wf", name: "release",
			blocker: "worker_lost", since: minutesAgo(1), waiting_seconds: 60,
			alert_after_seconds: 60, alerting: true,
			interaction: "The approval can be decided now.",
			main: {
				blocker: "worker_lost", run_id: "run_migrate", step: "migrate",
				reason: "Worker relay-7 stopped reporting on step \"migrate\" 1m ago.",
				who_can_act: "An admin. Every server runs the lease sweep.",
				next: "The reclaim is overdue.", worker: "relay-7", last_seen: minutesAgo(1),
				reclaim_at: minutesAgo(0.5),
			},
			badges: [{ blocker: "approval_needed", run_id: "run_gate", step: "gate",
				reason: "The workflow is waiting at approval step \"gate\"." }],
		}, {
			key: "run_q", kind: "run", run_id: "run_q", name: "site.yml",
			blocker: "no_worker", since: minutesAgo(17), waiting_seconds: 1020,
			alert_after_seconds: 900, alerting: true,
			main: {
				blocker: "no_worker", run_id: "run_q", queue: "prod", eligible_workers: 0,
				reason: "No connected worker serves queue \"prod\", so nothing can take this run.",
				who_can_act: "An admin: connect a worker that serves queue \"prod\".",
				next: "It starts automatically when a worker that serves queue \"prod\" appears.",
			},
		}, {
			key: "run_held", kind: "run", run_id: "run_held", name: "deploy.yml",
			blocker: "approval_needed", since: minutesAgo(4), waiting_seconds: 240,
			alert_after_seconds: 0, alerting: false,
			main: {
				blocker: "approval_needed", run_id: "run_held",
				reason: "It is held for approval by rule \"prod changes\".",
				who_can_act: "An admin other than dev-lead can approve it.",
				next: "Approve: it joins the default queue. Deny: it ends rejected.",
				approval: { scope: "run", decision_id: "run_held",
					approvers: { role: "admin", excluded: "dev-lead", agents: false },
					on_approve: "It joins the default queue and starts on a free worker.",
					on_deny: "It ends rejected and never runs." },
			},
		}],
	};
}

// overviewWith loads the overview with the panel answering body, optionally at a search string.
async function overviewWith(body, search) {
	const page = loadPage("overview", {
		search: search || "",
		routes: {
			"/v1/attention": body,
			"/v1/runs": reply({ runs: [], summary: { total: 0, succeeded: 0, failed: 0 } }),
			"/v1/fleet": reply({ hosts: [] }),
			"/v1/audit/verify": reply({ ok: true, count: 3, anchored: 0 }),
		},
	});
	sandboxOf(page.app).localStorage.setItem("st_role", "admin");
	await page.app.loadOverview();
	for (let i = 0; i < 6; i++) await page.clock.flush();
	return page;
}

// counts reads the four cards back as label -> value.
function counts(document) {
	const out = {};
	for (const card of document.querySelectorAll("#attn-counts .stat-card")) {
		const label = card.querySelector(".stat-label").textContent;
		out[label] = card.querySelector(".stat-value").textContent;
	}
	return out;
}

// listed returns the keys of the items the list shows.
function listed(document) {
	return Array.from(document.querySelectorAll("#attn-list .attn-item")).map((el) => el.dataset.key);
}

test("the panel shows four counts and every item with its main blocker", async () => {
	const { document } = await overviewWith(reply(answer()));
	assert.equal(document.getElementById("attention-panel").hidden, false);
	assert.deepEqual(counts(document), {
		"Worker lost": "1", "No worker available": "1", "Approval needed": "1", "Blocked": "0",
	});
	assert.deepEqual(listed(document), ["run_wf", "run_q", "run_held"]);
	const first = document.querySelector('#attn-list .attn-item[data-key="run_wf"]');
	assert.equal(first.querySelector(".attn-main").textContent, "Worker lost");
	assert.equal(first.querySelector(".attn-name").getAttribute("href"), "/ui/runs/run_wf");
	assert.match(first.querySelector(".attn-since").textContent, /^In this state for /);
	assert.equal(first.querySelector(".attn-alerting").textContent, "Alerting");
	const badge = first.querySelector('.attn-badge[data-blocker="approval_needed"]');
	assert.ok(badge, "the other condition is not shown as a badge");
	assert.equal(badge.textContent, "Approval needed: gate");
	assert.equal(first.querySelector(".attn-interaction").textContent,
		"The approval can be decided now.");
	assert.match(first.textContent, /Due now, at the next lease sweep/,
		"the reclaim countdown is missing for a lost worker");
});

test("a run named by a playbook path shows the file, and its tooltip keeps the path", async () => {
	const body = answer();
	body.items[2].name = "/tmp/switchtender-demo-assets-1/restart-app.yml";
	body.items[0].name = "deploy/web";
	const { document } = await overviewWith(reply(body));
	const held = document.querySelector('#attn-list .attn-item[data-key="run_held"] .attn-name');
	assert.equal(held.textContent, "restart-app.yml");
	assert.equal(held.title, "/tmp/switchtender-demo-assets-1/restart-app.yml");
	// A workflow is named, not pathed, so a slash in its name is kept.
	const wf = document.querySelector('#attn-list .attn-item[data-key="run_wf"] .attn-name');
	assert.equal(wf.textContent, "deploy/web");
	assert.equal(wf.title, "");
});

test("each item says who can act and what happens next", async () => {
	const { document } = await overviewWith(reply(answer()));
	const queued = document.querySelector('#attn-list .attn-item[data-key="run_q"]').textContent;
	assert.match(queued, /Queue/);
	assert.match(queued, /prod, 0 eligible workers connected/);
	assert.match(queued, /It starts automatically when a worker that serves queue "prod" appears\./);
	const held = document.querySelector('#attn-list .attn-item[data-key="run_held"]');
	assert.equal(held.querySelector('.attn-badge.scope').textContent, "Run");
	assert.match(held.textContent, /An admin other than dev-lead can approve it\./);
	assert.match(held.textContent, /On approve/);
	assert.match(held.textContent, /It ends rejected and never runs\./);
});

test("a count narrows the list, and pressing it again shows everything", async () => {
	const { document } = await overviewWith(reply(answer()));
	const card = document.querySelector('#attn-counts [data-blocker="no_worker"]');
	fire(card, "click");
	assert.deepEqual(listed(document), ["run_q"]);
	assert.equal(document.querySelector('#attn-counts [data-blocker="no_worker"]')
		.getAttribute("aria-pressed"), "true");
	assert.equal(document.getElementById("attn-filter").hidden, false);
	fire(document.querySelector('#attn-counts [data-blocker="no_worker"]'), "click");
	assert.deepEqual(listed(document), ["run_wf", "run_q", "run_held"]);
	assert.equal(document.getElementById("attn-filter").hidden, true);
});

test("an alert's link opens the list it names", async () => {
	const { document } = await overviewWith(reply(answer()), "?attention=approval_needed");
	assert.deepEqual(listed(document), ["run_held"]);
});

test("there is no requeue control anywhere in the panel", async () => {
	const { document } = await overviewWith(reply(answer()));
	const panel = document.getElementById("attention-panel");
	assert.equal(panel.querySelectorAll("button").length, 0,
		"the panel offers a button; nothing here may move a run back to the queue by hand");
	assert.doesNotMatch(panel.textContent, /requeue/i);
});

test("nothing waiting says so", async () => {
	const { document } = await overviewWith(reply({ counts: {}, items: [], total: 0, count: 0 }));
	assert.deepEqual(listed(document), []);
	assert.match(document.getElementById("attn-list").textContent, /Nothing is waiting/);
});

test("a failed read says so in the panel and leaves the overview loaded", async () => {
	const { document } = await overviewWith(failWith("connection refused"));
	assert.match(document.getElementById("attn-list").textContent,
		/Could not read what needs attention/);
	const labels = Array.from(document.querySelectorAll("#ov-metrics .stat-label"))
		.map((l) => l.textContent);
	assert.ok(labels.includes("Total runs"), "the overview's own metrics did not load");
});

test("the headline strip no longer carries a separate awaiting approval card", async () => {
	const { document } = await overviewWith(reply(answer()));
	const labels = Array.from(document.querySelectorAll("#ov-metrics .stat-label"))
		.map((l) => l.textContent);
	assert.ok(!labels.includes("Awaiting approval"),
		"the old card would count held runs a second time, beside the panel's own count");
});
