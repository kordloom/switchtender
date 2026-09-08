// What a page does when the answer is far larger than the one the demo returns. Every list here is
// built by appending a row per record with no windowing, so the size of the answer is the size of
// the DOM. These tests pin that a ten thousand row answer still finishes, still renders every row
// it claims to, and still lands the reader on a bounded first screen through the pager rather than
// on ten thousand rows at once.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { drive, pageEntry } from "./failures.mjs";

// HUGE is how many records the server is made to answer with.
const HUGE = 10000;

// runsOf builds a large well-formed run list.
function runsOf(n) {
	const runs = [];
	for (let i = 0; i < n; i++) {
		runs.push({
			id: "run_" + i, status: i % 7 === 0 ? "failed" : "succeeded", tool: "ansible",
			playbook: "playbooks/site" + i + ".yml", actor: "operator", actor_type: "human",
			created_at: "2026-08-04T12:00:00Z", started_at: "2026-08-04T12:00:00Z",
			finished_at: "2026-08-04T12:00:10Z", host_count: 3,
		});
	}
	return runs;
}

// hostsOf builds a large well-formed fleet list.
function hostsOf(n) {
	const hosts = [];
	for (let i = 0; i < n; i++) {
		hosts.push({
			host: "host" + i + ".example", failures: i % 5, total: 20, flaky: i % 5 > 0,
			recent: ["ok", "ok", "failed"], recent_runs: ["run_a", "run_b", "run_c"],
			last_outcome: "ok", last_run: "2026-08-04T12:00:00Z", drifted_tasks: i % 3,
			checked_at: "2026-08-04T12:00:00Z", tasks: 10,
		});
	}
	return hosts;
}

// entriesOf builds a large well-formed audit page.
function entriesOf(n) {
	const entries = [];
	for (let i = 0; i < n; i++) {
		entries.push({
			seq: i + 1, at: "2026-08-04T12:00:00Z", actor: "admin", actor_type: "human",
			method: "POST", path: "/v1/runs", hash: "abcdef0123456789".repeat(4),
			prev_hash: "0".repeat(64), content_digest: "sha256:" + i,
		});
	}
	return entries;
}

// BUDGET_MS is a ceiling loose enough for a simulated DOM on a loaded machine and tight enough to
// catch quadratic work: a per-row pass over every row already placed would blow straight through it.
const BUDGET_MS = 30000;

test("ten thousand runs render without hanging the page", async () => {
	const started = Date.now();
	const { document } = await drive(pageEntry("runs"),
		reply({ runs: runsOf(HUGE), summary: { total: HUGE }, has_more: false }));
	const took = Date.now() - started;
	const rows = document.getElementById("runs").children.length;
	assert.ok(rows >= HUGE, `the runs table drew ${rows} rows for ${HUGE} runs`);
	assert.ok(took < BUDGET_MS, `ten thousand runs took ${took}ms, which is a hung page`);
});

test("the runs page never asks for more than a page, whatever the history holds", async () => {
	// The runs table is the one list mountTablePager skips, because it pages against the server
	// instead. That makes the requested limit the only thing bounding the table, so it has to be
	// on the request rather than left to the server's default.
	const { seen } = await drive(pageEntry("runs"),
		reply({ runs: runsOf(50), summary: { total: HUGE }, has_more: true }));
	const first = seen.find((u) => u.includes("/runs?"));
	assert.ok(first, `the runs page asked for ${JSON.stringify(seen)}, none of it a runs page`);
	const limit = Number(new URLSearchParams(first.split("?")[1]).get("limit"));
	assert.ok(limit > 0 && limit <= 500,
		`the runs page asked for limit=${limit}, which is not a bounded first screen`);
});

test("the pager leaves a bounded first screen out of ten thousand audit entries", async () => {
	// Every row is in the DOM, so the pager is the only thing standing between the reader and ten
	// thousand rows of scroll. Without it the first screen is unusable however fast it rendered.
	const { app, document } = await drive(pageEntry("audit"), reply({ entries: entriesOf(HUGE) }));
	app.mountTablePager();
	const rows = [...document.getElementById("audit").children];
	const shown = rows.filter((r) => r.dataset.phide !== "1").length;
	assert.ok(shown < HUGE,
		`the pager left all ${HUGE} rows on screen at once, so the page is one endless scroll`);
	assert.ok(shown > 0, "the pager hid every row, leaving an empty table");
});

test("ten thousand hosts render on the fleet page without hanging", async () => {
	const started = Date.now();
	const { document } = await drive(pageEntry("fleet"), reply({ hosts: hostsOf(HUGE) }));
	const took = Date.now() - started;
	const rows = document.getElementById("fleet").children.length;
	assert.equal(rows, HUGE, `the fleet table drew ${rows} rows for ${HUGE} hosts`);
	assert.ok(took < BUDGET_MS, `ten thousand hosts took ${took}ms, which is a hung page`);
});

test("ten thousand audit entries render without hanging", async () => {
	const started = Date.now();
	const { document } = await drive(pageEntry("audit"), reply({ entries: entriesOf(HUGE) }));
	const took = Date.now() - started;
	const rows = document.getElementById("audit").children.length;
	assert.equal(rows, HUGE, `the audit table drew ${rows} rows for ${HUGE} entries`);
	assert.ok(took < BUDGET_MS, `ten thousand audit entries took ${took}ms, which is a hung page`);
});

test("a run matrix past its cap refuses to draw rather than building a million cells", async () => {
	// The detail page carries its own cap on the body. A grid of every host against every task is
	// the one place where a large but ordinary run turns into a cell count nothing can paint.
	const events = [];
	for (let h = 0; h < 400; h++) {
		for (let t = 0; t < 40; t++) {
			events.push({
				seq: events.length + 1, kind: "task", host: "host" + h + ".example",
				task: "task " + t, outcome: t % 9 === 0 ? "failed" : "ok",
				at: "2026-08-04T12:00:00Z",
			});
		}
	}
	const model = { hosts: [], tasks: [], cells: {} };
	const hostSet = new Set(), taskSet = new Set();
	for (const e of events) {
		hostSet.add(e.host);
		taskSet.add(e.task);
		if (!model.cells[e.host]) model.cells[e.host] = {};
		model.cells[e.host][e.task] = { outcome: e.outcome };
	}
	model.hosts = [...hostSet];
	model.tasks = [...taskSet];

	const started = Date.now();
	const { app, document } = await drive(pageEntry("detail"), reply({ events: [] }));
	document.body.dataset.matrixCap = "2000";
	app.renderMatrix(model);
	const took = Date.now() - started;
	assert.ok(took < BUDGET_MS, `a 16000 cell matrix took ${took}ms`);
	const table = document.getElementById("matrix");
	const drawn = table ? table.querySelectorAll("td").length : 0;
	assert.ok(drawn < 16000,
		`the matrix drew ${drawn} cells past its cap of 2000 instead of offering the summary`);
});

test("a run with a hundred thousand log lines does not render one node per line", async () => {
	// The stored log arrives as one string. Splitting it into an element per line is the shape that
	// turns a long-running playbook's page into a browser that stops responding.
	const lines = [];
	for (let i = 0; i < 100000; i++) lines.push("TASK [step " + i + "] ok: [web01]");
	const started = Date.now();
	const { document } = await drive(pageEntry("detail"), reply({ events: [], log: lines.join("\n") }));
	const took = Date.now() - started;
	assert.ok(took < BUDGET_MS, `a hundred thousand log lines took ${took}ms to reach the page`);
	const log = document.getElementById("log");
	if (log) {
		assert.ok(log.children.length < 100000,
			`the log built ${log.children.length} elements, one per line`);
	}
});
