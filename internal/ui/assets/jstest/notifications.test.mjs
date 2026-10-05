// Tests for the notification targets page, 28-notifications.js: the helpers that decide what the
// target dialog shows and sends, the page itself driven against a stubbed API, and the section the
// run page draws of what its targets were told. The rule under test above all is that a stored
// address or key never travels back through the form: an edit sends a secret only when one was
// typed, and a target an import left waiting for its secret asks only for the part it is missing.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { ALL_PARTS, loadParts, sandboxOf } from "./loader.mjs";
import { loadPage } from "./pages.mjs";
import { settle } from "./failures.mjs";

const app = loadParts(["01-boot.js", "28-notifications.js"]);

// REQUIRED_FIELDS is the server's rule for each kind: which of an address, a key, and a recipient it
// takes, from notification.kindNeedsURL, kindNeedsKey, and run.ValidateNotifyTarget.
const REQUIRED_FIELDS = {
	webhook: { url: true, key: false, to: false },
	slack: { url: true, key: false, to: false },
	mattermost: { url: true, key: false, to: false },
	rocketchat: { url: true, key: false, to: false },
	discord: { url: true, key: false, to: false },
	teams: { url: true, key: false, to: false },
	ntfy: { url: true, key: false, to: false },
	pagerduty: { url: false, key: true, to: false },
	grafana: { url: true, key: true, to: false },
	twilio: { url: false, key: false, to: true },
	email: { url: false, key: false, to: true },
};

// plain turns a value built in the sandbox into one deepEqual compares across realms.
function plain(v) {
	return JSON.parse(JSON.stringify(v));
}

// target builds one target as GET /v1/notifications answers it.
function target(extra) {
	return Object.assign({
		id: "ntf_chat", name: "ops chat", description: "on-call channel", kind: "slack",
		url: "https://hooks.slack.com/…", key_set: false, needs_secret: false,
		created_at: "2026-09-01T12:00:00Z",
		delivery: { state: "healthy", last_delivered_at: "2026-09-30T12:00:00Z", failed: 0, pending: 0 },
	}, extra || {});
}

// grafanaShell is a Grafana target an import left waiting for its token, its address kept.
function grafanaShell() {
	return target({
		id: "ntf_dash", name: "dashboards", kind: "grafana", url: "https://grafana.example.com/…",
		needs_secret: true, delivery: { state: "needs_secret", missing: ["key"], failed: 0, pending: 0 },
	});
}

test("ntfFields asks for exactly what the server requires of each kind", () => {
	assert.deepEqual(Object.keys(plain(app.NTF_KIND_LABELS)).sort(), Object.keys(REQUIRED_FIELDS).sort());
	for (const [kind, want] of Object.entries(REQUIRED_FIELDS)) {
		assert.deepEqual(plain(app.ntfFields(kind)), want, kind);
	}
	assert.deepEqual(plain(app.ntfFields("carrier-pigeon")), { url: false, key: false, to: false });
});

test("a target waiting for its secret is told what it is missing in the other system's words", () => {
	const tests = [
		// Test 0: A chat channel's secret is its incoming webhook address.
		{ Kind: "slack", Missing: ["url"], Want: "Waiting for the incoming webhook address." },
		// Test 1: PagerDuty's is the routing key.
		{ Kind: "pagerduty", Missing: ["key"], Want: "Waiting for the PagerDuty Events API v2 routing key." },
		// Test 2: Grafana with its address kept waits for the token alone.
		{ Kind: "grafana", Missing: ["key"], Want: "Waiting for the Grafana API token." },
		// Test 3: Grafana with nothing kept waits for both.
		{ Kind: "grafana", Missing: ["url", "key"],
			Want: "Waiting for the Grafana instance address and the Grafana API token." },
		// Test 4: A webhook waits for its address.
		{ Kind: "webhook", Missing: ["url"], Want: "Waiting for the webhook address." },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.ntfMissingSentence(tc.Kind, tc.Missing), tc.Want, "test " + i);
	}
});

test("ntfFormPlan never puts a stored secret back in an input and asks a shell only for its gap", () => {
	const tests = [
		// Test 0: A new Slack target asks for the address and nothing is configured.
		{ Kind: "slack", Editing: null,
			Want: { url: { show: true, configured: "" }, key: { show: false, configured: "" },
				to: { show: false }, missing: [] } },
		// Test 1: Editing it describes the stored address by its hint and keeps the input empty.
		{ Kind: "slack", Editing: target(),
			Want: { url: { show: true,
				configured: "Address: configured (https://hooks.slack.com/…). Leave empty to keep it." },
			key: { show: false, configured: "" }, to: { show: false }, missing: [] } },
		// Test 2: A Grafana shell with its address kept asks only for the token.
		{ Kind: "grafana", Editing: grafanaShell(),
			Want: { url: { show: false,
				configured: "Address: configured (https://grafana.example.com/…). Leave empty to keep it." },
			key: { show: true, configured: "" }, to: { show: false }, missing: ["key"] } },
		// Test 3: Changing the kind of what is being edited starts the new kind over.
		{ Kind: "pagerduty", Editing: target(),
			Want: { url: { show: false, configured: "" }, key: { show: true, configured: "" },
				to: { show: false }, missing: [] } },
		// Test 4: A PagerDuty target holding its key says so.
		{ Kind: "pagerduty", Editing: target({ kind: "pagerduty", url: "", key_set: true }),
			Want: { url: { show: false, configured: "" },
				key: { show: true, configured: "Routing key: configured. Leave empty to keep it." },
				to: { show: false }, missing: [] } },
	];
	for (const [i, tc] of tests.entries()) {
		assert.deepEqual(plain(app.ntfFormPlan(tc.Kind, tc.Editing)), tc.Want, "test " + i);
	}
});

test("ntfPayload never sends a blank or masked secret and refuses an incomplete new target", () => {
	const base = { name: "ops chat", description: "on-call channel", kind: "slack", url: "", key: "", to: "" };
	const tests = [
		// Test 0: A new target carries its address.
		{ In: Object.assign({}, base, { url: "https://hooks.slack.com/services/T/B/x" }), Editing: null,
			Want: { payload: { name: "ops chat", description: "on-call channel", kind: "slack",
				url: "https://hooks.slack.com/services/T/B/x" } } },
		// Test 1: A new target with no address is refused before anything is sent.
		{ In: base, Editing: null,
			Want: { error: "Slack targets need the incoming webhook address." } },
		// Test 2: An edit that types nothing secret sends no secret, and no kind it already has.
		{ In: base, Editing: target(), Want: { payload: { name: "ops chat", description: "on-call channel" } } },
		// Test 3: The masked hint pasted back is not a new address.
		{ In: Object.assign({}, base, { url: "https://hooks.slack.com/…" }), Editing: target(),
			Want: { payload: { name: "ops chat", description: "on-call channel" } } },
		// Test 4: Finishing a Grafana shell sends the token alone and keeps the stored address.
		{ In: Object.assign({}, base, { name: "dashboards", kind: "grafana", key: "glsa_token" }),
			Editing: grafanaShell(),
			Want: { payload: { name: "dashboards", description: "on-call channel", key: "glsa_token" } } },
		// Test 5: A shell may be renamed and stay waiting.
		{ In: Object.assign({}, base, { name: "dashboards", kind: "grafana" }), Editing: grafanaShell(),
			Want: { payload: { name: "dashboards", description: "on-call channel" } } },
		// Test 6: Changing a configured target's kind needs the new kind's secret.
		{ In: Object.assign({}, base, { kind: "pagerduty" }), Editing: target(),
			Want: { error: "PagerDuty targets need the PagerDuty Events API v2 routing key." } },
		// Test 7: An email target carries its recipients and nothing secret.
		{ In: Object.assign({}, base, { kind: "email", url: "https://x.example.com/y", to: "a@example.com" }),
			Editing: null, Want: { payload: { name: "ops chat", description: "on-call channel",
				kind: "email", to: "a@example.com" } } },
		// Test 8: An email target with no recipient is refused.
		{ In: Object.assign({}, base, { kind: "email" }), Editing: null,
			Want: { error: "Email targets need a recipient." } },
		// Test 9: A target needs a name.
		{ In: Object.assign({}, base, { name: "  " }), Editing: null, Want: { error: "Give the target a name." } },
	];
	for (const [i, tc] of tests.entries()) {
		assert.deepEqual(plain(app.ntfPayload(tc.In, tc.Editing)), tc.Want, "test " + i);
	}
});

test("ntfState reads each delivery status the way a person scanning the list needs it", () => {
	const tests = [
		{ In: target(), Want: ["healthy", "ok"] }, // Test 0.
		{ In: grafanaShell(), Want: ["needs secret", "flaky"] }, // Test 1.
		{ In: target({ delivery: { state: "failing", last_error: "the target answered 404" } }),
			Want: ["failing", "failed"] }, // Test 2.
		{ In: target({ delivery: { state: "retrying" } }), Want: ["retrying", "warn"] }, // Test 3.
		{ In: target({ delivery: { state: "configured" } }), Want: ["configured", "none"] }, // Test 4.
		{ In: target({ delivery: undefined }), Want: ["configured", "none"] }, // Test 5.
	];
	for (const [i, tc] of tests.entries()) {
		const state = app.ntfState(tc.In);
		assert.deepEqual([state.label, state.cls], tc.Want, "test " + i);
	}
	assert.match(app.ntfState(grafanaShell()).tip, /Grafana API token/);
	assert.match(app.ntfState(target({ delivery: { state: "failing", last_error: "the target answered 404" } })).tip,
		/the target answered 404/);
});

// mountTargets opens the notifications page as role, answering the list with targets and recording
// every write.
function mountTargets(targets, options) {
	const opts = options || {};
	const writes = [];
	const routes = [
		[(req) => req.method !== "GET", (req) => {
			writes.push({ method: req.method, path: req.path, body: req.body ? JSON.parse(req.body) : null });
			return reply({ id: "ntf_new" });
		}],
		[/^\/v1\/notifications$/, reply({ notifications: targets, count: targets.length,
			total: targets.length, sealing: opts.sealing !== false })],
	].concat(opts.routes || []);
	const page = loadPage("notifications", { parts: ALL_PARTS, routes, quiet: true });
	if (opts.role) sandboxOf(page.app).localStorage.setItem("st_role", opts.role);
	return Object.assign({ writes }, page);
}

test("the list shows each target's state and gathers the ones waiting for a secret", async () => {
	const page = mountTargets([target(), grafanaShell(),
		target({ id: "ntf_fail", name: "pager", kind: "pagerduty", url: "", key_set: true,
			delivery: { state: "failing", last_error: "the target answered 404", failed: 2 } })]);
	await page.app.loadNotifications();
	await settle(page.clock);
	const rows = page.document.querySelectorAll("#notifications tr");
	assert.equal(rows.length, 3);
	const states = Array.from(rows).map((r) => r.querySelector(".chip").textContent);
	assert.deepEqual(states, ["healthy", "needs secret", "failing"]);
	assert.equal(page.document.getElementById("notify-table").hidden, false);
	const needs = page.document.getElementById("notify-needs");
	assert.equal(needs.hidden, false, "the target waiting for its secret is not gathered above the list");
	assert.match(needs.textContent, /1 target is waiting for its secret/);
	assert.match(needs.textContent, /Waiting for the Grafana API token\./);
	assert.doesNotMatch(page.document.body.textContent, /hooks\.slack\.com\/services/,
		"a stored address reached the page");
	page.net.assertClean();
});

test("editing a target keeps its stored address out of the form and out of the save", async () => {
	const page = mountTargets([target()]);
	page.app.wireNotifyForm();
	await page.app.loadNotifications();
	await settle(page.clock);
	const edit = Array.from(page.document.querySelectorAll("#notifications button"))
		.find((b) => b.textContent === "Edit");
	fire(edit, "click");
	const url = page.document.getElementById("notify-url");
	assert.equal(url.value, "", "the address input was filled with what the server holds");
	assert.equal(url.getAttribute("type"), "password");
	const state = page.document.getElementById("notify-url-state");
	assert.equal(state.hidden, false);
	assert.match(state.textContent, /^Address: configured \(https:\/\/hooks\.slack\.com\/…\)/);
	page.document.getElementById("notify-name").value = "ops channel";
	fire(page.document.getElementById("notify-form"), "submit");
	await settle(page.clock);
	const put = page.writes.find((w) => w.method === "PUT");
	assert.ok(put, "the edit was not saved");
	assert.equal(put.path, "/v1/notifications/ntf_chat");
	assert.deepEqual(put.body, { name: "ops channel", description: "on-call channel" },
		"an edit that typed no secret sent one anyway");
});

test("finishing an imported target asks only for its missing token and keeps its address", async () => {
	const page = mountTargets([grafanaShell()]);
	page.app.wireNotifyForm();
	await page.app.loadNotifications();
	await settle(page.clock);
	const finish = Array.from(page.document.querySelectorAll("#notify-needs button"))
		.find((b) => b.textContent === "Finish");
	assert.ok(finish, "the waiting target offers no way to finish it");
	fire(finish, "click");
	const doc = page.document;
	assert.equal(doc.getElementById("notify-modal").hidden, false);
	assert.equal(doc.getElementById("notify-url").hidden, true, "the kept address is asked for again");
	assert.equal(doc.getElementById("notify-key").hidden, false, "the missing token is not asked for");
	assert.equal(doc.getElementById("notify-missing").hidden, false);
	assert.match(doc.getElementById("notify-missing").textContent, /Waiting for the Grafana API token\./);
	assert.match(doc.getElementById("notify-url-state").textContent, /^Address: configured/);
	doc.getElementById("notify-key").value = "glsa_new_token";
	fire(doc.getElementById("notify-form"), "submit");
	await settle(page.clock);
	const put = page.writes.find((w) => w.method === "PUT");
	assert.deepEqual(put && put.body, { name: "dashboards", description: "on-call channel",
		key: "glsa_new_token" }, "finishing sent more than the missing token");
	assert.equal(doc.getElementById("notify-key").value, "", "the token stayed in the input after the save");
});

test("a server with no encryption key says so above the list and in the dialog", async () => {
	const page = mountTargets([target({ kind: "email", url: "", to: "ops@example.com" })], { sealing: false });
	page.app.wireNotifyForm();
	await page.app.loadNotifications();
	await settle(page.clock);
	const notice = page.document.querySelector(".seal-notice");
	assert.ok(notice, "nothing says this server cannot seal an address");
	assert.match(notice.textContent, /SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT/);
	fire(page.document.getElementById("notify-open"), "click");
	assert.match(page.document.getElementById("notify-secret-hint").textContent,
		/no encryption key, so an address or key cannot be saved/,
		"the dialog promises to seal an address this server cannot seal");
});

test("a viewer is told who reads targets and the page asks the server for nothing", async () => {
	const page = mountTargets([target()], { role: "viewer" });
	await page.app.loadNotifications();
	await settle(page.clock);
	assert.equal(page.net.calls.length, 0, "the page asked for a list the server refuses this role");
	assert.match(page.document.getElementById("status").textContent, /readable by operators and admins/);
});

test("the detail panel lists attachments and deliveries, a failed delivery stands out, and attach posts", async () => {
	const page = mountTargets([target()], {
		routes: [
			[/^\/v1\/notifications\/ntf_chat\/attachments$/, reply({ attachments: [
				{ id: "nta_1", notification_id: "ntf_chat", object_kind: "template", object_id: "tpl_deploy",
					event: "failure" },
				{ id: "nta_2", notification_id: "ntf_chat", object_kind: "schedule", object_id: "sch_gone",
					event: "success" },
			], count: 2 })],
			[/^\/v1\/notifications\/ntf_chat\/deliveries$/, reply({ deliveries: [
				{ notification_id: "ntf_chat", run_id: "run_b", seq: 2, event: "failure", status: "delivered",
					attempts: 1, note: "An earlier notification about this run to this target could not be delivered.",
					created_at: "2026-09-30T12:01:00Z" },
				{ notification_id: "ntf_chat", run_id: "run_b", seq: 1, event: "started", status: "failed",
					attempts: 5, last_error: "the target answered 503", created_at: "2026-09-30T12:00:00Z" },
			], count: 2 })],
			[/^\/v1\/templates$/, reply({ templates: [{ id: "tpl_deploy", name: "deploy" }] })],
			[/^\/v1\/schedules$/, reply({ schedules: [] })],
		],
	});
	page.app.wireNotifyDetail();
	await page.app.loadNotifications();
	await settle(page.clock);
	page.document.getElementById("notify-attach-kind").value = "template";
	const open = Array.from(page.document.querySelectorAll("#notifications button"))
		.find((b) => b.textContent === "Open");
	fire(open, "click");
	await settle(page.clock);
	const doc = page.document;
	assert.equal(doc.getElementById("notify-detail").hidden, false);
	const attached = Array.from(doc.querySelectorAll("#notify-attachments tr")).map((r) =>
		Array.from(r.querySelectorAll("td")).slice(0, 3).map((c) => c.textContent));
	assert.deepEqual(attached, [["Template", "deploy", "A run fails"],
		["Schedule", "sch_gone", "A run succeeds"]]);
	const rows = doc.querySelectorAll("#notify-deliveries tr");
	assert.equal(rows.length, 2);
	assert.equal(rows[1].className, "notify-failed", "the failed delivery does not stand out");
	assert.match(rows[1].textContent, /the target answered 503/);
	assert.match(rows[0].textContent, /An earlier notification about this run/);
	assert.equal(rows[0].querySelector("a").getAttribute("href"), "/ui/runs/run_b");
	assert.equal(doc.getElementById("notify-attach-object").value, "tpl_deploy");
	doc.getElementById("notify-attach-event").value = "approval";
	fire(doc.getElementById("notify-attach-form"), "submit");
	await settle(page.clock);
	const post = page.writes.find((w) => w.method === "POST");
	assert.deepEqual(post && post.body, { object_kind: "template", object_id: "tpl_deploy",
		event: "approval" });
	assert.equal(post.path, "/v1/notifications/ntf_chat/attachments");
});

// mountRun opens a run's page answering its notification record with answer.
function mountRun(answer) {
	const page = loadPage("detail", {
		parts: ALL_PARTS, vars: { RunID: "run_x", MatrixCap: "2000" }, quiet: true,
		routes: [[/^\/v1\/runs\/run_x\/notifications$/, answer]],
	});
	return page;
}

test("a run's page lists what its targets were told, in order, with failures marked", async () => {
	const page = mountRun(reply({ deliveries: [
		{ notification_id: "ntf_chat", target_name: "ops chat", target_kind: "slack", run_id: "run_x", seq: 1,
			event: "started", status: "failed", attempts: 5, last_error: "the target answered 503" },
		{ notification_id: "ntf_chat", target_name: "ops chat", target_kind: "slack", run_id: "run_x", seq: 2,
			event: "failure", status: "delivered", attempts: 1,
			note: "An earlier notification about this run to this target could not be delivered." },
		{ notification_id: "ntf_shell", target_name: "imported slack", target_kind: "slack", run_id: "run_x",
			seq: 2, event: "failure", status: "skipped", attempts: 0,
			last_error: "not sent: the target is waiting for its secret to be entered" },
	], count: 3 }));
	await page.app.loadRunNotifications("run_x");
	await settle(page.clock);
	const host = page.document.getElementById("run-notifications");
	assert.equal(host.hidden, false);
	assert.match(host.querySelector(".panel-head").textContent, /Notifications/);
	assert.equal(host.querySelector(".chip.failed").textContent, "1 failed");
	const rows = host.querySelectorAll("tbody tr");
	assert.deepEqual(Array.from(rows).map((r) => r.querySelector("td").textContent),
		["ops chat", "ops chat", "imported slack"]);
	assert.equal(rows[0].className, "notify-failed");
	assert.match(rows[2].textContent, /waiting for its secret/);
	assert.match(rows[1].textContent, /An earlier notification/);
});

test("a run no target heard about, or one the session cannot read, draws no notification section", async () => {
	for (const answer of [reply({ deliveries: [], count: 0 }), reply({ error: "forbidden" }, { status: 403 })]) {
		const page = mountRun(answer);
		await page.app.loadRunNotifications("run_x");
		await settle(page.clock);
		assert.equal(page.document.getElementById("run-notifications").hidden, true);
	}
});

test("the page is in the navigation for operators, named in its link tips, and searchable", () => {
	const nav = loadParts(ALL_PARTS);
	const item = nav.NAV_GROUPS.flatMap((g) => g.items).find((it) => it.key === "notifications");
	assert.ok(item, "the navigation has no notifications entry");
	assert.equal(item.href, "/ui/notifications");
	assert.equal(item.operator, true, "the entry is not offered to operators, who may read targets");
	assert.equal(Boolean(item.admin), false, "the entry is hidden from operators, who may read targets");
	assert.equal(nav.PAGE_NAV.notifications, "notifications");
	assert.ok(nav.NAV_ICONS.notifications, "the entry has no icon");
	assert.equal(nav.describeRoute("/ui/notifications"), "Click to open notification targets");
	assert.ok(nav.LIST_PAGES.includes("notifications"), "the target list has no search box");
	assert.ok(nav.PAGE_DOCS.notifications, "the page has no guide chip");
});

test("booting the page loads the list and hides the create button from an operator", async () => {
	const page = mountTargets([target()], { role: "operator" });
	fire(page.document, "DOMContentLoaded");
	await settle(page.clock);
	assert.ok(page.net.calledWith("/v1/notifications").length > 0, "the boot never asked for the list");
	assert.equal(page.document.querySelectorAll("#notifications tr").length, 1);
	assert.equal(page.document.getElementById("notify-open").hidden, true,
		"an operator is offered a create the server refuses");
	const actions = Array.from(page.document.querySelectorAll("#notifications button.button"))
		.map((b) => b.textContent);
	assert.deepEqual(actions, ["Open"], "an operator is offered edits the server refuses");
});
