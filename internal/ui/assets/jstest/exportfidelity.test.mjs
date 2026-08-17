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
