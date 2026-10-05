// Integration tests for the run detail page, driven through the real detail template with the
// whole script loaded, because that is the only way this page is ever run. loadDetail wires the
// downloads, the actions, and the log filter, then dispatches to the single, split, or pipeline
// path; a helper tested on its own says nothing about whether any of that still executes.
import { test } from "node:test";
import assert from "node:assert/strict";

import { ALL_PARTS, sandboxOf } from "./loader.mjs";
import { reply, textReply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// EVENTS is one ordinary run's history: two tasks on one host, the second of which failed.
const EVENTS = [
	{ seq: 1, type: "task_start", task: "Gather facts", time: "2026-01-01T00:00:01Z" },
	{ seq: 2, type: "runner_ok", host: "web01", task: "Gather facts", time: "2026-01-01T00:00:02Z" },
	{ seq: 3, type: "task_start", task: "Deploy", time: "2026-01-01T00:00:03Z" },
	{ seq: 4, type: "runner_failed", host: "web01", task: "Deploy", rc: 2, time: "2026-01-01T00:00:04Z" },
];

// runOf builds a run record complete enough for the header to render every field it reaches for.
function runOf(id, extra) {
	return Object.assign({
		id,
		status: "failed",
		tool: "ansible",
		playbook: "plays/site.yml",
		inventory: "/srv/hosts.ini",
		source: "manual",
		actor: "operator",
		exit_code: 2,
		started_at: "2026-01-01T00:00:00Z",
		ended_at: "2026-01-01T00:00:05Z",
	}, extra);
}

// openDetail mounts the detail page for a run and returns the driven page.
function openDetail(runID, routes) {
	return loadPage("detail", {
		parts: ALL_PARTS,
		vars: { RunID: runID, MatrixCap: 20000 },
		routes,
	});
}

// statusOf returns the page's status line, which is empty and hidden once a run has loaded and
// carries the failure otherwise. It is the first thing to check, because loadDetail catches
// everything it throws and reports it here rather than to the test runner.
function statusOf(document) {
	const el = document.getElementById("status");
	return el.hidden ? "" : el.textContent;
}

test("an ordinary run loads its header, matrix, and downloads", async () => {
	const { app, document, net, clock } = openDetail("run_1", [
		[/^\/v1\/runs\/run_1\/events\?/, reply({ events: EVENTS })],
		[/^\/v1\/runs\/run_1\/logs(\?|$)/, textReply("ok: web01\nfailed: web02\n")],
		[/^\/v1\/runs\/run_1$/, reply(runOf("run_1"))],
	]);

	await app.loadDetail("run_1");
	await clock.flush();

	assert.equal(statusOf(document), "", "the page reported a failure instead of loading");
	assert.equal(document.getElementById("run-header").hidden, false, "the header never rendered");
	assert.equal(document.getElementById("matrix-panel").hidden, false, "the matrix never rendered");
	assert.deepEqual(
		document.querySelectorAll("#matrix .cell").map((c) => c.dataset.outcome),
		["ok", "failed"],
		"the grid does not show what the events say happened",
	);

	// A single run owns its log and its events, so both links stay offered.
	assert.equal(document.getElementById("full-log").hidden, false,
		"the full log link was hidden on a run that has one");
	assert.equal(document.getElementById("export-events").hidden, false,
		"the event export was hidden on a run that has events");
	assert.equal(document.getElementById("audit-link").getAttribute("href"), "/ui/audit?q=run_1");
	// A finished run has nothing to stream, but it does read back the output it produced, so the
	// page shows what the run did rather than only that it ran.
	assert.equal(net.calls.length, 3);
	assert.ok(net.calls.some((c) => String(c.url || c).includes("/runs/run_1/logs")),
		"a finished run should read its stored output");
	net.assertClean();
});

test("a split parent loads its shards and hides the links it has no bytes for", async () => {
	const shards = [
		{ id: "s0", status: "succeeded", shard_index: 0, limit: "web01,web02" },
		{ id: "s1", status: "failed", shard_index: 1, limit: "web03" },
	];
	const { app, document, net, clock } = openDetail("p1", [
		[/^\/v1\/runs\/(s0|s1)\/events\?/, (req) => reply({
			events: req.url.includes("/s0/") ? EVENTS : [],
		})],
		[/^\/v1\/runs\/p1\/shards$/, reply({ shards })],
		[/^\/v1\/runs\/p1\/logs(\?|$)/, textReply("")],
		[/^\/v1\/runs\/p1$/, reply(runOf("p1", { kind: "split", shard_count: 2, playbook: "plays/site.yml" }))],
	]);

	await app.loadDetail("p1");
	await clock.flush();

	assert.equal(statusOf(document), "", "the page reported a failure instead of loading");
	assert.equal(document.getElementById("run-header").hidden, false, "the header never rendered");
	assert.equal(document.getElementById("shards-panel").hidden, false, "the shard list never rendered");
	assert.equal(document.querySelectorAll("#shards .shard-row").length, 2);
	// The merged matrix is folded from every shard's events, so the parent shows the whole run.
	assert.deepEqual(
		document.querySelectorAll("#matrix .cell").map((c) => c.dataset.outcome),
		["ok", "failed"],
	);

	// A parent has no output of its own: each shard carries the log and the events, so offering the
	// links here would serve blanks.
	assert.equal(document.getElementById("full-log").hidden, true,
		"the full log link was offered on a parent that has no log");
	assert.equal(document.getElementById("export-events").hidden, true,
		"the event export was offered on a parent that has no events of its own");
	net.assertClean();
});

test("a live run opens its stream with a cursor, zero on a page with no history", async () => {
	const { app, document, net, streams, clock } = openDetail("run_1", [
		[/^\/v1\/runs\/run_1\/events\?/, reply({ events: [] })],
		[/^\/v1\/runs\/run_1\/logs(\?|$)/, textReply("")],
		[/^\/v1\/runs\/run_1$/, reply(runOf("run_1", { status: "running", ended_at: "" }))],
	]);

	await app.loadDetail("run_1");
	await clock.flush();

	// The cursors are read off the stream the page opened, not off the helper that built the path.
	// The server treats a missing parameter as "start from the current end", so a stream opened
	// without one loses whatever was written between the page's reads and the connection.
	assert.equal(streams.sources.length, 1, "a running run did not open a live stream");
	assert.equal(streams.last().url, "/v1/runs/run_1/stream?after=0&logafter=0");
	assert.equal(document.getElementById("live-indicator").hidden, false);

	// The stream is really wired to the page: what the server sends lands in the log pane.
	await streams.last().emit("log", "TASK [Gather facts]\n");
	assert.match(document.getElementById("log").textContent, /TASK \[Gather facts\]/);
	assert.equal(document.getElementById("log-panel").hidden, false);
	net.assertClean();
});

test("a live run resumes after the last sequence it already has", async () => {
	const { app, document, net, streams, clock } = openDetail("run_1", [
		[/^\/v1\/runs\/run_1\/events\?/, reply({ events: EVENTS })],
		[/^\/v1\/runs\/run_1\/logs(\?|$)/,
			textReply("TASK [Gather facts]\n", { headers: { "Switchtender-Log-Seq": "17" } })],
		[/^\/v1\/runs\/run_1$/, reply(runOf("run_1", { status: "running", ended_at: "" }))],
	]);

	await app.loadDetail("run_1");
	await clock.flush();
	assert.equal(streams.last().url, "/v1/runs/run_1/stream?after=4&logafter=17",
		"the stream replayed history the page already had, or skipped what it did not");

	// A live event lands in the grid, which is the whole reason the stream is open.
	await streams.last().emit("event", {
		seq: 5, type: "runner_ok", host: "web02", task: "Deploy", time: "2026-01-01T00:00:06Z",
	});
	await clock.tick(200);
	assert.deepEqual(
		document.querySelectorAll("#matrix tbody th").map((th) => th.textContent),
		["web01", "web02"],
		"a host that appeared live never reached the grid",
	);
	net.assertClean();
});

test("a run that cannot be read says so instead of rendering a blank page", async () => {
	const { app, document, net, clock } = openDetail("run_1", [
		[/^\/v1\/runs\/run_1$/, reply({ error: "no such run" }, { status: 404 })],
	]);

	await app.loadDetail("run_1");
	await clock.flush();

	// The server's own sentence, not the internal path and a status code. Readers were being
	// shown developer strings like "/runs/run_1 returned 404" on every failing surface.
	assert.equal(statusOf(document), "Failed to load run: no such run");
	assert.equal(document.getElementById("run-header").hidden, true);
	net.assertClean();
});

// TestStoredInventoryRunDraws pins the run page for a run launched against a stored inventory.
//
// Every template imported from AWX targets a stored inventory, and the demo's runs all use a path,
// so the one branch that drew a stored inventory went unexercised. It asked the field for a value
// span the field never built, the throw was reported as the run not existing, and a held run drew
// no Approve and no Reject, so the page could not do the one thing an approver opened it for.
test("a run against a stored inventory draws its header and offers its decision", async () => {
	const held = runOf("run_inv", {
		status: "pending_approval", held_by_policy: "production", inventory: undefined,
		inventory_id: "inv_1", exit_code: undefined, started_at: undefined, ended_at: undefined,
	});
	const { app, document, net, clock } = openDetail("run_inv", [
		[/^\/v1\/runs\/run_inv\/events\?/, reply({ events: [] })],
		[/^\/v1\/runs\/run_inv\/logs(\?|$)/, textReply("")],
		[/^\/v1\/inventories$/, reply({ inventories: [{ id: "inv_1", name: "prod-hosts" }] })],
		[/^\/v1\/runs\/run_inv\/decisions$/, reply({ decisions: [], count: 0 })],
		[/^\/v1\/runs\/run_inv$/, reply(held)],
	]);

	await app.loadDetail("run_inv");
	await clock.flush();

	assert.ok(!document.getElementById("run-deadend"), "an existing run was called missing");
	assert.doesNotMatch(statusOf(document), /Failed|could not be drawn/,
		"the page reported a failure: " + statusOf(document));
	const header = document.getElementById("run-header");
	assert.equal(header.hidden, false, "the header stayed hidden");
	assert.match(header.textContent, /prod-hosts/, "the stored inventory is not named");
	assert.ok(header.querySelector(".field .value button"),
		"the inventory id has no copy control");
	assert.equal(document.getElementById("approve-run").hidden, false, "a held run offers no Approve");
	assert.equal(document.getElementById("reject-run").hidden, false, "a held run offers no Reject");
	net.assertClean();
});

// TestDrawFaultKeepsTheDecision pins what a fault drawing the page does to a run that was read.
//
// A single failed field used to send the whole page down the path for a run that does not exist:
// the header and every action hidden, and a sentence saying there was no such run. Whatever the next
// fault is, the run was read, so the page must say that and keep the decision in reach.
test("a fault drawing a run does not call it missing or take its actions away", async () => {
	const held = runOf("run_held", {
		status: "pending_approval", exit_code: undefined, started_at: undefined, ended_at: undefined,
	});
	const { app, document, clock } = openDetail("run_held", [
		[/^\/v1\/runs\/run_held\/events\?/, reply({ events: [] })],
		[/^\/v1\/runs\/run_held$/, reply(held)],
	]);
	sandboxOf(app).renderTimeline = () => { throw new Error("a drawing fault"); };
	sandboxOf(app).renderDetail = () => { throw new Error("a drawing fault"); };

	await app.loadDetail("run_held");
	await clock.flush();

	assert.ok(!document.getElementById("run-deadend"), "a run that was read was called missing");
	assert.match(statusOf(document), /could not be drawn: a drawing fault/,
		"the page did not say what failed: " + statusOf(document));
	assert.match(statusOf(document), /intact/, "the page did not say the run itself is fine");
	assert.equal(document.getElementById("approve-run").hidden, false,
		"a drawing fault took Approve away from a held run");
	assert.equal(document.querySelector("main.content .actions").hidden, false,
		"a drawing fault hid the action row");
});

// TestUnreadableIsNotMissing pins the difference between a run that does not exist and a server
// that could not answer. Both were told the run did not exist, so a reader on a flaky connection
// was sent looking for a deleted run that was sitting in the database.
test("a server error is not reported as a missing run", async () => {
	const { app, document } = openDetail("run_1", [
		[/^\/v1\/runs\/run_1$/, reply({ error: "the database is locked, try again" }, { status: 503 })],
	]);

	await app.loadDetail("run_1");

	const box = document.getElementById("run-deadend");
	assert.ok(box, "nothing explained the failure");
	assert.doesNotMatch(box.textContent, /There is no run/,
		"a server error was reported as the run not existing: " + box.textContent);
	assert.match(box.textContent, /could not be read/, "the box does not say what happened");
});

// TestWatchedRunEndsWithItsOutputAndRunAgain pins what a page shows when the run it is watching ends.
// A short script finished before its stream connected, so the pane stayed empty under a run that had
// printed something, and Run again was decided once at load, so it never appeared for the run the
// person had just watched.
test("a run that ends while its page is open shows its output and offers Run again", async () => {
	const { app, document, net, streams, clock } = openDetail("run_1", [
		[/^\/v1\/runs\/run_1\/events\?/, reply({ events: [] })],
		[/^\/v1\/runs\/run_1\/logs(\?|$)/, textReply("")],
		[/^\/v1\/runs\/run_1$/, reply(runOf("run_1", { status: "running", ended_at: "", tool: "python" }))],
	]);
	await app.loadDetail("run_1");
	await clock.flush();
	assert.equal(document.getElementById("rerun-run").hidden, true,
		"Run again was offered while the run was still going");

	// The run finishes: its record and its log now say so.
	net.only([
		[/^\/v1\/runs\/run_1\/logs(\?|$)/, textReply("x\n", { headers: { "Switchtender-Log-Seq": "1" } })],
		[/^\/v1\/runs\/run_1$/, reply(runOf("run_1", { status: "succeeded", tool: "python" }))],
	]);
	await streams.last().emit("end", "");
	// The end handler reads the run, then its log, each an await or two deep.
	for (let i = 0; i < 10; i++) await clock.flush();

	assert.equal(document.getElementById("log-panel").hidden, false,
		"the run printed something and the pane stayed empty");
	assert.match(document.getElementById("log").textContent, /^x/);
	assert.equal(document.getElementById("rerun-run").hidden, false,
		"Run again never appeared for the run that just finished");
	net.assertClean();
});
