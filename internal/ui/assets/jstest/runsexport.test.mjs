// Tests for the runs export's completeness. The export reads the rendered table, and the table
// pages from the server, so an export used to carry only the rows already scrolled into view: a
// file headed like the record that quietly was not. An export now pulls the rest of the current
// query first, and one the bound cuts short says so in its name.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";
import { sandboxOf } from "./loader.mjs";

// runRows builds n renderable runs with distinct ids.
function runRows(start, n) {
	const out = [];
	for (let i = 0; i < n; i++) {
		out.push({
			id: "run_" + (start + i), playbook: "site.yml", status: "succeeded",
			created_at: "2026-08-16T12:00:00Z", source: "api",
		});
	}
	return out;
}

// mountPagedRuns mounts the runs page against a server holding `total` runs served in pages, wires
// the exports, and returns the page with a download recorder.
async function mountPagedRuns(total) {
	const page = loadPage("runs", {
		routes: {
			"/v1/runs": (req) => {
				const url = new URL("http://x" + req.url);
				const offset = parseInt(url.searchParams.get("offset") || "0", 10);
				const limit = parseInt(url.searchParams.get("limit") || "20", 10);
				const runs = runRows(offset, Math.max(0, Math.min(limit, total - offset)));
				return reply({ runs, count: total, summary: {}, has_more: offset + runs.length < total });
			},
		},
	});
	const sandbox = sandboxOf(page.app);
	page.downloads = [];
	sandbox.downloadBlob = (name, type, content) => { page.downloads.push({ name, type, content }); };
	page.app.mountTableExport();
	await page.app.loadRuns();
	return page;
}

// csvBodyRows counts the data rows in a downloaded CSV.
function csvBodyRows(download) {
	return download.content.trim().split("\n").length - 1;
}

test("an export pulls the pages the table had not loaded yet", async () => {
	const page = await mountPagedRuns(45);
	assert.ok(page.document.querySelectorAll("#runs tr").length < 45, "the first page loaded partially");
	const csvBtn = page.document.querySelectorAll("button.table-export")[0];
	fire(csvBtn, "click");
	await page.clock.flush();
	assert.equal(page.downloads.length, 1, "the export produced one file");
	assert.equal(csvBodyRows(page.downloads[0]), 45, "the file carries the whole query");
	assert.ok(!page.downloads[0].name.includes("partial"), page.downloads[0].name);
});

test("a complete table exports as it stands without refetching", async () => {
	const page = await mountPagedRuns(5);
	const before = page.net.calls.length;
	fire(page.document.querySelectorAll("button.table-export")[0], "click");
	await page.clock.flush();
	assert.equal(page.net.calls.length, before, "a whole table still refetched for export");
	assert.equal(csvBodyRows(page.downloads[0]), 5);
});
