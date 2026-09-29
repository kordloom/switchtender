// Tests for the run matrix data model in 23-run-matrix.js: buildModel's full pass over an event
// stream, applyEvent's incremental fold, and the worstOutcome rollup.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "23-run-matrix.js"]);

// plain deep-copies a value produced inside the vm so it compares cleanly against host literals.
const plain = (v) => JSON.parse(JSON.stringify(v));

// Timestamps for a small synthetic run, one second apart.
const t = (n) => "2026-03-01T12:00:0" + n + "Z";
const ms = (n) => Date.parse(t(n));

// runEvents is a realistic little stream: two tasks announced, per-host runner results covering
// every outcome, one host that only ever appears in the final stats, and hosts arriving out of
// alphabetical order.
const runEvents = [
	{ type: "playbook_start", time: t(0) },
	{ type: "task_start", task: "Gather facts", time: t(1) },
	{ type: "runner_ok", host: "web2", task: "Gather facts", changed: false, time: t(2) },
	{ type: "runner_ok", host: "web1", task: "Gather facts", changed: true, time: t(2) },
	{ type: "task_start", task: "Install nginx", time: t(3) },
	{
		type: "runner_failed", host: "web1", task: "Install nginx", rc: 2,
		message: "apt broke", stdout: "out", stderr: "err", truncated: true, time: t(4),
	},
	{ type: "runner_skipped", host: "web2", task: "Install nginx", time: t(4) },
	{ type: "task_start", task: "Restart", time: t(5) },
	{ type: "runner_unreachable", host: "web2", task: "Restart", message: "ssh timeout", time: t(6) },
	{ type: "stats", time: t(7), stats: { web1: {}, web2: {}, db1: {} } },
];

test("buildModel groups events into tasks, hosts, and cells", () => {
	const m = app.buildModel(runEvents);

	// Tasks keep first-seen order; hosts sort alphabetically and include stats-only hosts.
	assert.deepEqual(plain(m.tasks), ["Gather facts", "Install nginx", "Restart"]);
	assert.deepEqual(plain(m.hosts), ["db1", "web1", "web2"]);
	assert.deepEqual([...m.hostSet].sort(), ["db1", "web1", "web2"]);
	assert.deepEqual([...m.taskSeen].sort(), ["Gather facts", "Install nginx", "Restart"]);

	// Outcomes: a changed ok is "changed", a plain ok is "ok", and the runner_ prefix strips.
	assert.equal(m.cells.web1["Gather facts"].outcome, "changed");
	assert.equal(m.cells.web2["Gather facts"].outcome, "ok");
	assert.equal(m.cells.web1["Install nginx"].outcome, "failed");
	assert.equal(m.cells.web2["Install nginx"].outcome, "skipped");
	assert.equal(m.cells.web2.Restart.outcome, "unreachable");

	// The failure cell carries the drill fields through.
	assert.deepEqual(plain(m.cells.web1["Install nginx"]), {
		outcome: "failed", message: "apt broke", stdout: "out", stderr: "err",
		rc: 2, truncated: true,
	});

	// A stats-only host has no cells, and no task exists for the unannounced host either.
	assert.equal(m.cells.db1, undefined);

	// Task start times, the newest event time, and the stats time all land in milliseconds.
	assert.deepEqual(plain(m.taskStart), {
		"Gather facts": ms(1), "Install nginx": ms(3), Restart: ms(5),
	});
	assert.equal(m.lastTime, ms(7));
	assert.equal(m.statsTime, ms(7));
	assert.equal(m.end, ms(7));
});

test("buildModel without stats ends at the newest event", () => {
	const m = app.buildModel(runEvents.slice(0, -1));
	assert.equal(m.statsTime, null);
	assert.equal(m.end, ms(6));
	assert.deepEqual(plain(m.hosts), ["web1", "web2"]);
});

test("buildModel on an empty stream is an empty model", () => {
	const m = app.buildModel([]);
	assert.deepEqual(plain(m.tasks), []);
	assert.deepEqual(plain(m.hosts), []);
	assert.equal(m.lastTime, null);
	assert.equal(m.statsTime, null);
	assert.equal(m.end, null);
});

test("buildModel keeps the first task_start time and the last cell result", () => {
	const m = app.buildModel([
		{ type: "task_start", task: "Deploy", time: t(1) },
		{ type: "task_start", task: "Deploy", time: t(3) },
		{ type: "runner_failed", host: "web1", task: "Deploy", time: t(4) },
		// A retry of the same task on the same host replaces the earlier cell.
		{ type: "runner_ok", host: "web1", task: "Deploy", changed: true, time: t(5) },
	]);
	assert.deepEqual(plain(m.tasks), ["Deploy"]);
	assert.equal(m.taskStart.Deploy, ms(1));
	assert.equal(m.cells.web1.Deploy.outcome, "changed");
});

test("buildModel records a runner result for a task never announced", () => {
	const m = app.buildModel([
		{ type: "runner_ok", host: "web1", task: "Adhoc", changed: false, time: t(1) },
	]);
	assert.deepEqual(plain(m.tasks), ["Adhoc"]);
	assert.equal(m.cells.web1.Adhoc.outcome, "ok");
	// The task was never announced, so it has no start time.
	assert.equal(m.taskStart.Adhoc, undefined);
});

test("buildModel ignores event types outside the model", () => {
	const m = app.buildModel([
		{ type: "playbook_start", time: t(1) },
		{ type: "verbose", host: "web1", task: "Noise", time: t(2) },
		{ type: "warning", time: t(3) },
	]);
	assert.deepEqual(plain(m.tasks), []);
	assert.deepEqual(plain(m.hosts), []);
	// Their times still advance the clock.
	assert.equal(m.lastTime, ms(3));
});

test("applyEvent folds one event and reports structural changes", () => {
	const m = app.buildModel(runEvents);

	// Test 0: A result for a known host and task is not structural and targets its cell.
	let change = app.applyEvent(m, {
		type: "runner_ok", host: "web1", task: "Restart", changed: false, time: t(8),
	});
	assert.deepEqual(plain(change), { structural: false, host: "web1", task: "Restart" });
	assert.equal(m.cells.web1.Restart.outcome, "ok");
	assert.equal(m.lastTime, ms(8));
	// The stats time still bounds the run end.
	assert.equal(m.end, ms(7));

	// Test 1: A brand new task is structural.
	change = app.applyEvent(m, { type: "task_start", task: "Verify", time: t(9) });
	assert.equal(change.structural, true);
	assert.deepEqual(plain(m.tasks), ["Gather facts", "Install nginx", "Restart", "Verify"]);
	assert.equal(m.taskStart.Verify, ms(9));

	// Test 2: A repeat task_start is not structural and keeps the first time.
	change = app.applyEvent(m, { type: "task_start", task: "Verify", time: t(9) });
	assert.equal(change.structural, false);
	assert.equal(m.taskStart.Verify, ms(9));

	// Test 3: A brand new host is structural and re-sorts the host list.
	change = app.applyEvent(m, {
		type: "runner_ok", host: "lb1", task: "Verify", changed: false, time: t(9),
	});
	assert.equal(change.structural, true);
	assert.deepEqual(plain(m.hosts), ["db1", "lb1", "web1", "web2"]);
	assert.equal(m.cells.lb1.Verify.outcome, "ok");

	// Test 4: Stats naming only known hosts is not structural.
	change = app.applyEvent(m, { type: "stats", time: t(9), stats: { web1: {}, lb1: {} } });
	assert.equal(change.structural, false);
	assert.equal(m.statsTime, ms(9));
	assert.equal(m.end, ms(9));

	// Test 5: Stats naming a new host is structural.
	change = app.applyEvent(m, { type: "stats", time: t(9), stats: { cache1: {} } });
	assert.equal(change.structural, true);
	assert.deepEqual(plain(m.hosts), ["cache1", "db1", "lb1", "web1", "web2"]);
});

test("folding events one at a time matches one full pass", () => {
	const incremental = app.buildModel([]);
	for (const e of runEvents) app.applyEvent(incremental, e);
	const full = app.buildModel(runEvents);
	// applyEvent sets end on every fold; buildModel computes it once at the close. Everything
	// else must agree exactly.
	assert.deepEqual(incremental, full);
});

test("applyEvent stops flagging structural for events it cannot place", () => {
	// A runner that reports no task, or no host, has no cell in a host by task grid. Such an event
	// must not keep marking the model structural, or the whole run repaints on every event instead
	// of patching one cell.
	const m = app.buildModel(runEvents);
	const before = {
		tasks: plain(m.tasks), hosts: plain(m.hosts), cells: plain(m.cells),
	};
	const unplaceable = [
		// Test 0: A result with no task field at all.
		{ type: "runner_ok", host: "web1", changed: false, time: t(8) },
		// Test 1: A result with an empty task name.
		{ type: "runner_ok", host: "web1", task: "", changed: false, time: t(8) },
		// Test 2: A result with no host field.
		{ type: "runner_failed", task: "Install nginx", time: t(8) },
		// Test 3: A task_start with no task name.
		{ type: "task_start", time: t(8) },
		// Test 4: Stats naming an empty host.
		{ type: "stats", time: t(8), stats: { "": {} } },
	];
	for (const [i, e] of unplaceable.entries()) {
		// Ten in a row is what a chatty runner sends; not one of them may rebuild the grid.
		for (let n = 0; n < 10; n++) {
			const change = app.applyEvent(m, e);
			assert.equal(change.structural, false, "test " + i + " repeat " + n);
		}
	}
	// The grid itself is untouched: no phantom row, column, or cell was created.
	assert.deepEqual(plain(m.tasks), before.tasks);
	assert.deepEqual(plain(m.hosts), before.hosts);
	assert.deepEqual(plain(m.cells), before.cells);
	assert.equal(m.taskStart.undefined, undefined);

	// A placeable event still rebuilds once and only once.
	assert.equal(app.applyEvent(m, {
		type: "runner_ok", host: "lb9", task: "Verify", changed: false, time: t(9),
	}).structural, true);
	assert.equal(app.applyEvent(m, {
		type: "runner_ok", host: "lb9", task: "Verify", changed: true, time: t(9),
	}).structural, false);
});

test("buildModel leaves out results with no host or no task", () => {
	const m = app.buildModel([
		{ type: "task_start", task: "Deploy", time: t(1) },
		{ type: "runner_ok", host: "web1", task: "Deploy", changed: false, time: t(2) },
		{ type: "runner_ok", host: "web1", changed: false, time: t(3) },
		{ type: "runner_failed", task: "Deploy", time: t(4) },
		{ type: "stats", time: t(5), stats: { web1: {}, "": {} } },
	]);
	assert.deepEqual(plain(m.tasks), ["Deploy"]);
	assert.deepEqual(plain(m.hosts), ["web1"]);
	assert.deepEqual(plain(m.cells), { web1: { Deploy: { outcome: "ok" } } });
	// The unplaceable events still advance the run clock.
	assert.equal(m.lastTime, ms(5));
});

test("folding unplaceable events one at a time matches one full pass", () => {
	// The fold equivalence property has to survive the events both folds now decline to place.
	const noisy = [
		{ type: "task_start", task: "Gather facts", time: t(0) },
		{ type: "runner_ok", host: "web1", changed: false, time: t(1) },
		{ type: "runner_ok", host: "web1", task: "Gather facts", changed: true, time: t(2) },
		{ type: "task_start", time: t(3) },
		{ type: "runner_failed", task: "Gather facts", time: t(4) },
		{ type: "runner_ok", host: "web2", task: "", changed: false, time: t(5) },
		{ type: "runner_skipped", host: "web2", task: "Gather facts", time: t(6) },
		{ type: "stats", time: t(7), stats: { web1: {}, web2: {}, "": {} } },
	];
	const incremental = app.buildModel([]);
	for (const e of noisy) app.applyEvent(incremental, e);
	assert.deepEqual(incremental, app.buildModel(noisy));
});

test("worstOutcome rolls a task up to its most severe host outcome", () => {
	const hosts = ["h1", "h2", "h3"];
	const cellsFor = (...outcomes) => {
		const cells = {};
		outcomes.forEach((o, i) => {
			if (o) cells["h" + (i + 1)] = { task: { outcome: o } };
		});
		return cells;
	};
	const tests = [
		// Test 0: All ok stays ok.
		{ Cells: cellsFor("ok", "ok", "ok"), Want: "ok" },
		// Test 1: One changed outranks ok.
		{ Cells: cellsFor("ok", "changed", "ok"), Want: "changed" },
		// Test 2: A failure outranks changed.
		{ Cells: cellsFor("changed", "failed", "ok"), Want: "failed" },
		// Test 3: Unreachable is the worst but wears the failed class.
		{ Cells: cellsFor("ok", "ok", "unreachable"), Want: "failed" },
		// Test 4: All skipped stays skipped.
		{ Cells: cellsFor("skipped", "skipped", "skipped"), Want: "skipped" },
		// Test 5: Skipped loses to ok.
		{ Cells: cellsFor("skipped", "ok"), Want: "ok" },
		// Test 6: No host ran the task at all.
		{ Cells: {}, Want: "skipped" },
		// Test 7: Hosts with no cell for the task are passed over.
		{ Cells: cellsFor(null, "failed", null), Want: "failed" },
		// Test 8: An unknown outcome ranks lowest and comes back verbatim when it is all there is.
		{ Cells: cellsFor("mystery"), Want: "mystery" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.worstOutcome("task", tc.Cells, hosts), tc.Want, "test " + i);
	}
});
