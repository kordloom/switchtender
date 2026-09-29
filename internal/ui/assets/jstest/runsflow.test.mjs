// Integration tests for the runs list in 16-runs-list.js, driven through the real runs template.
// Nothing here is stubbed above the network: the handlers run, the rows are the rows the page
// builds, and the assertions read the table a reader would be looking at.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire, press } from "./dom.mjs";
import { deferred, failWith, reply, sequence } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// PARTS is the slice of the script the runs list needs: the boot constants, the fetch and format
// helpers, the shared table cells, and the list itself.
const PARTS = [
	"01-boot.js", "06-workflow-canvas.js", "07-nav-theme.js", "08-auth-status.js", "09-audit.js",
	"13-fileviewer-inventory.js", "16-runs-list.js", "18-host-page.js",
];

// runOf builds one run record for the list, complete enough for every cell to render.
function runOf(id, extra) {
	return Object.assign({
		id,
		status: "failed",
		tool: "ansible",
		playbook: "plays/site.yml",
		source: "api",
		started_at: "2026-01-01T00:00:00Z",
		ended_at: "2026-01-01T00:00:05Z",
	}, extra);
}

// rowIDs returns the run id shown in each table row, which is what a reader identifies a row by.
function rowIDs(document) {
	return document.getElementById("runs").querySelectorAll("tr")
		.map((tr) => tr.querySelectorAll("td")[2].textContent);
}

// rowNumbers returns the position column, which has to keep counting across an appended page.
function rowNumbers(document) {
	return document.getElementById("runs").querySelectorAll("td.col-num").map((td) => td.textContent);
}

// queryOf splits a recorded URL into its offset and everything else, so the two pages can be
// compared on the part that has to be identical.
function queryOf(url) {
	const params = new URLSearchParams(url.slice(url.indexOf("?") + 1));
	const offset = params.get("offset");
	params.delete("offset");
	params.sort();
	return { offset, rest: params.toString() };
}

test("Load more asks for the next page of the same filtered set and appends it", async () => {
	const { app, document, net, clock } = loadPage("runs", {
		parts: PARTS,
		search: "?after=2026-01-01&before=2026-02-01",
		routes: {
			"/v1/runs": sequence(
				reply({ runs: [runOf("run_one"), runOf("run_two")], has_more: true, summary: { total: 4 } }),
				reply({ runs: [runOf("run_three"), runOf("run_four")], has_more: false, summary: { total: 4 } }),
			),
		},
	});

	// The reader has narrowed the list: a status, a tool, an order, a search, and a page size, on
	// top of the date window the URL carries.
	document.getElementById("runs-status").value = "failed";
	document.getElementById("runs-tool").value = "ansible";
	document.getElementById("runs-order").value = "oldest";
	document.getElementById("runs-search").value = "web";
	document.getElementById("runs-pagesize").value = "2";

	await app.loadRuns();
	await clock.flush();
	assert.deepEqual(rowIDs(document), ["run_one", "run_two"]);

	const more = document.getElementById("runs-more");
	assert.ok(more, "the Load more control was never placed");
	assert.equal(more.hidden, false, "Load more is hidden while there are more runs to load");

	await press(more);
	await clock.flush();

	// The whole point: the second page comes from the same filtered result set as the first, or its
	// offset counts within a different one and the rows appended are unrelated runs.
	assert.equal(net.calls.length, 2, "Load more did not fetch a page");
	const first = queryOf(net.urls[0]);
	const second = queryOf(net.urls[1]);
	assert.equal(second.rest, first.rest, "Load more dropped filters the first page carried");
	assert.equal(first.rest,
		"after=2026-01-01&before=2026-02-01&limit=2&order=oldest&q=web&status=failed&tool=ansible");
	assert.equal(first.offset, "0");
	assert.equal(second.offset, "2", "the next page has to start where the loaded rows end");

	// Appended, not replaced: the first page's rows are still there, and the numbering continues.
	assert.deepEqual(rowIDs(document), ["run_one", "run_two", "run_three", "run_four"]);
	assert.deepEqual(rowNumbers(document), ["1", "2", "3", "4"]);
	assert.equal(document.getElementById("runs-more").hidden, true,
		"Load more stayed offered after the last page");
	net.assertClean();
});

test("a Load more that fails says so and hands the control back", async () => {
	const { app, document, net, clock } = loadPage("runs", {
		parts: PARTS,
		routes: {
			"/v1/runs": sequence(
				reply({ runs: [runOf("run_one")], has_more: true, summary: {} }),
				failWith(new Error("network gone")),
			),
		},
	});

	await app.loadRuns();
	await clock.flush();
	const more = document.getElementById("runs-more");
	await press(more);
	await clock.flush();

	assert.equal(document.getElementById("status").textContent, "Failed to load more runs: network gone");
	// The rows already on the page are what the reader was reading, so a failed next page leaves
	// them alone and lets the reader try again.
	assert.deepEqual(rowIDs(document), ["run_one"]);
	assert.equal(more.disabled, false, "a failed page left Load more dead");
	net.assertClean();
});

test("a search superseded by a newer one does not overwrite the newer rows", async () => {
	const stale = deferred();
	let call = 0;
	const { app, document, net, clock } = loadPage("runs", {
		parts: PARTS,
		routes: {
			"/v1/runs": () => {
				call++;
				// The first search answers last, which is the race the generation guard exists for.
				return call === 1 ? stale.promise : reply({ runs: [runOf("run_fresh")], has_more: false, summary: {} });
			},
		},
	});
	app.wireRunsSearch();
	const box = document.getElementById("runs-search");

	box.value = "web";
	fire(box, "input");
	await clock.tick(250);
	assert.equal(net.calls.length, 1, "the first search never reached the server");

	box.value = "database";
	fire(box, "input");
	await clock.tick(250);
	assert.equal(net.calls.length, 2);
	assert.deepEqual(rowIDs(document), ["run_fresh"], "the newer search never rendered");

	// The older, slower search now answers with rows for a query the reader has moved off.
	stale.resolve(reply({ runs: [runOf("run_stale")], has_more: false, summary: {} }));
	await clock.flush();

	assert.deepEqual(rowIDs(document), ["run_fresh"],
		"a superseded search overwrote the rows a newer one had already drawn");
	net.assertClean();
});

test("a search superseded by a newer one leaves the table alone when it fails", async () => {
	const stale = deferred();
	let call = 0;
	const { app, document, net, clock } = loadPage("runs", {
		parts: PARTS,
		routes: {
			"/v1/runs": () => {
				call++;
				return call === 1 ? stale.promise : reply({ runs: [runOf("run_fresh")], has_more: false, summary: {} });
			},
		},
	});
	app.wireRunsSearch();
	const box = document.getElementById("runs-search");
	const table = document.querySelector("table.runs");
	const status = document.getElementById("status");

	box.value = "web";
	fire(box, "input");
	await clock.tick(250);
	box.value = "database";
	fire(box, "input");
	await clock.tick(250);
	assert.deepEqual(rowIDs(document), ["run_fresh"]);
	assert.equal(table.hidden, false);

	// The stale request now fails. It says nothing about the table a newer load already drew, so
	// clearing the rows or showing an error over them would be reporting the wrong request.
	stale.reject(new Error("network gone"));
	await clock.flush();

	assert.deepEqual(rowIDs(document), ["run_fresh"], "the stale failure wiped rows a newer load drew");
	assert.equal(table.hidden, false, "the stale failure hid a table a newer load drew");
	assert.equal(status.hidden, true,
		"the stale failure showed a message over good data: " + JSON.stringify(status.textContent));
	net.assertClean();
});
