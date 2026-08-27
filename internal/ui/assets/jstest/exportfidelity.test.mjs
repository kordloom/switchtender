// Tests for export fidelity. A cell that draws a truncation or a graphic carries its exportable
// value in data-export, so a file gets the fact rather than the abbreviation: the audit hash
// exports whole, a sparkline exports its outcomes, and every table on a page gets export buttons,
// including the second one the compare page used to leave out.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";
import { sandboxOf } from "./loader.mjs";

const fullHash = "abcdef0123456789abcdef0123456789abcdef0123456789abcdef0123456789";

test("the audit export carries whole hashes while the table shows prefixes", async () => {
	const page = loadPage("audit", {
		routes: { "/v1/audit": reply({ entries: [{
			seq: 1, at: "2026-08-17T12:00:00Z", actor: "root", actor_type: "session",
			method: "POST", path: "/v1/runs", hash: fullHash,
		}], count: 1 }) },
	});
	const sandbox = sandboxOf(page.app);
	page.downloads = [];
	sandbox.downloadBlob = (name, type, content) => { page.downloads.push({ name, type, content }); };
	page.app.mountTableExport();
	await page.app.loadAudit();
	const shown = page.document.querySelectorAll("#audit td");
	assert.ok([...shown].some((c) => c.textContent === fullHash.slice(0, 12)), "the cell shows a prefix");
	fire(page.document.querySelectorAll("button.table-export")[0], "click");
	await page.clock.flush();
	assert.ok(page.downloads[0].content.includes(fullHash), "the export lost the whole hash");
});

test("every table on the compare page gets its own export buttons", () => {
	const page = loadPage("compare");
	page.app.mountTableExport();
	const tables = page.document.querySelectorAll("main.content table").length;
	const buttons = page.document.querySelectorAll("button.table-export").length;
	assert.ok(tables >= 2, "the compare page holds two tables");
	assert.equal(buttons, tables * 3, "each table carries CSV, JSON, and YAML exports");
});

test("an export carries the rows the facet kept, not the ones it hid", async () => {
	// Twelve entries, the last four DELETE, so ticking DELETE in the Method facet leaves four rows
	// on screen out of twelve and the selection also sits past the default page size.
	const entries = [];
	for (let i = 0; i < 12; i++) {
		entries.push({
			seq: 12 - i, at: "2026-08-17T12:00:00Z", actor: "root", actor_type: "session",
			method: i >= 8 ? "DELETE" : "POST",
			path: i >= 8 ? "/v1/credentials/cred_" + i : "/v1/runs",
			hash: fullHash,
		});
	}
	const page = loadPage("audit", {
		routes: { "/v1/audit": reply({ entries, count: entries.length }) },
	});
	const sandbox = sandboxOf(page.app);
	page.downloads = [];
	sandbox.downloadBlob = (name, type, content) => { page.downloads.push({ name, type, content }); };
	await page.app.loadAudit();
	// The page mounts these in this order, and the export joins the row the filter builds, so a
	// test that mounts them the other way round is not exercising the page anyone loads.
	page.app.mountListFilter();
	page.app.mountFacetFilters();
	page.app.mountTableExport();

	// Tick DELETE in the Method facet, through the control a reader would use.
	const items = [...page.document.querySelectorAll(".facet-item")];
	const row = items.find((el) => el.querySelector(".facet-item-label").textContent === "DELETE");
	assert.ok(row, "the Method facet offers DELETE");
	const del = row.querySelector("input");
	del.checked = true;
	fire(del, "change");

	const rows = [...page.document.getElementById("audit").querySelectorAll("tr")];
	assert.equal(rows.filter((tr) => !tr.hidden).length, 4, "the facet leaves the four DELETE rows");

	fire(page.document.querySelectorAll("button.table-export")[0], "click");
	await page.clock.flush();
	const lines = page.downloads[0].content.trim().split("\n");
	assert.equal(lines.length, 5, "the export holds a header and the four rows the facet kept, and "
		+ "not the eight it hid: a file that answers a question nobody asked reads as the answer to "
		+ "the one they did");
	assert.ok(!page.downloads[0].content.includes("POST"),
		"the export shipped rows the reader had ticked away");
});

test("an export runs past the pager but stops at the text filter", async () => {
	// Forty entries against a default page of twenty-five, so the pager holds rows back and the two
	// limits can be told apart: the pager is a limit on what fits the screen, the filter box is the
	// reader saying which rows they mean, and only the second one belongs in the file.
	const entries = [];
	for (let i = 0; i < 40; i++) {
		entries.push({
			seq: 40 - i, at: "2026-08-17T12:00:00Z", actor: "root", actor_type: "session",
			method: "POST", path: i < 6 ? "/v1/credentials" : "/v1/runs", hash: fullHash,
		});
	}
	const page = loadPage("audit", {
		routes: { "/v1/audit": reply({ entries, count: entries.length }) },
	});
	const sandbox = sandboxOf(page.app);
	page.downloads = [];
	sandbox.downloadBlob = (name, type, content) => { page.downloads.push({ name, type, content }); };
	await page.app.loadAudit();
	page.app.mountListFilter();
	page.app.mountTableExport();
	page.app.mountTablePager();

	const table = page.document.querySelector("main.content table");
	const shown = () => [...table.tBodies[0].rows].filter((tr) => !tr.hidden).length;
	assert.equal(shown(), 25, "the pager holds the table to its first page");

	const csvRows = async () => {
		page.downloads.length = 0;
		fire(page.document.querySelectorAll("button.table-export")[0], "click");
		await page.clock.flush();
		return page.downloads[0].content.trim().split("\n").length - 1;
	};
	assert.equal(await csvRows(), 40, "the export stopped at the page boundary: a file that holds "
		+ "the first page while looking like the whole table is the audit artifact this export "
		+ "exists to avoid");

	const box = page.document.querySelector(".list-filter input");
	assert.ok(box, "the audit page carries a filter box");
	box.value = "credentials";
	fire(box, "input");
	// Typing is debounced, so the filter has not run until the clock passes the debounce.
	await page.clock.tick(200);
	assert.equal(await csvRows(), 6,
		"the export carried rows the reader had filtered away by typing");
});
