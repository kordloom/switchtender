// Tests for getAllPages, which reads every page of a configuration list. The lists page at 1000
// rows, and every table, picker and lookup drew the first page as if it were the whole set, so
// row 1001 existed on the server and nowhere on screen, in the filter, or in an export.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply, failWith } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS, sandboxOf } from "./loader.mjs";
import { fire } from "./dom.mjs";

// pagedList answers a configuration list the way the server does: limit and offset over total
// rows, with has_more and next_offset. A failing offset answers 500 instead.
function pagedList(key, total, opts) {
	const options = opts || {};
	return (req) => {
		const offset = Number(req.params.get("offset") || 0);
		if (options.failAt === offset) return reply({ error: "store unavailable" }, { status: 500 });
		const limit = Math.min(Number(req.params.get("limit") || 1000), 1000);
		const end = Math.min(offset + limit, total);
		const rows = [];
		for (let i = offset; i < end; i++) {
			rows.push({ id: "tpl_" + String(i + 1).padStart(5, "0"), name: "template " + (i + 1),
				playbook: "site.yml" });
		}
		return reply({ [key]: rows, count: rows.length, total, has_more: end < total,
			next_offset: end });
	};
}

// mountTemplates opens the templates page over a list of total templates.
function mountTemplates(total, opts) {
	return loadPage("jobtemplates", {
		parts: ALL_PARTS,
		quiet: true,
		routes: [[/^\/v1\/templates(\?|$)/, pagedList("templates", total, opts)]],
	});
}

test("three pages come back as every row once, in order", async () => {
	const page = mountTemplates(2500);
	const { rows, total } = await page.app.getAllPages("/templates", "templates");
	assert.equal(total, 2500);
	assert.equal(rows.length, 2500, "a page was dropped or read twice");
	assert.equal(new Set(rows.map((r) => r.id)).size, 2500, "a row came back twice");
	assert.equal(rows[2499].id, "tpl_02500");
	assert.deepEqual(page.net.calledWith("/v1/templates").map((c) => c.params.get("offset")),
		["0", "1000", "2000"]);
});

test("the ceiling stops reading and keeps the total", async () => {
	const page = mountTemplates(25000);
	const { rows, total } = await page.app.getAllPages("/templates", "templates");
	assert.equal(rows.length, 20000, "the ceiling did not hold");
	assert.equal(total, 25000, "the total past the ceiling was lost, so nothing can say what is missing");
	assert.equal(page.net.calledWith("/v1/templates").length, 20,
		"reading went past the ceiling, so one page load grows with the install");
});

test("one failed page rejects the whole read", async () => {
	const page = mountTemplates(2500, { failAt: 1000 });
	await assert.rejects(page.app.getAllPages("/templates", "templates"),
		"a partial set would be drawn as the whole");
});

test("a dropped connection on a later page rejects the whole read", async () => {
	const page = loadPage("jobtemplates", {
		parts: ALL_PARTS,
		quiet: true,
		routes: [[(req) => req.params.get("offset") === "1000", failWith("connection reset")],
			[/^\/v1\/templates(\?|$)/, pagedList("templates", 2500)]],
	});
	await assert.rejects(page.app.getAllPages("/templates", "templates"));
});

test("a server too old to page is read once and its total kept", async () => {
	const page = loadPage("jobtemplates", {
		parts: ALL_PARTS,
		quiet: true,
		routes: [[/^\/v1\/templates(\?|$)/, reply({
			templates: [{ id: "tpl_1", name: "one" }, { id: "tpl_2", name: "two" }],
			count: 2, total: 1500,
		})]],
	});
	const { rows, total } = await page.app.getAllPages("/templates", "templates");
	assert.equal(page.net.calledWith("/v1/templates").length, 1,
		"an answer with no next_offset was asked again, which an old server answers forever");
	assert.equal(rows.length, 2);
	assert.equal(total, 1500, "the old server's own total was dropped");
});

test("the templates table draws row 1001 and the filter finds it", async () => {
	const page = mountTemplates(1001);
	await page.app.loadTemplates();
	await page.clock.flush();
	const tbody = page.document.getElementById("templates");
	assert.equal(tbody.rows.length, 1001, "the table drew only the first page");
	assert.equal(tbody.closest("table").dataset.listTotal, undefined,
		"a list read whole was marked as partial");
	page.app.mountListFilter();
	const box = page.document.querySelector(".list-filter input");
	box.value = "template 1001";
	fire(box, "input");
	await page.clock.tick(200);
	const shown = [...tbody.rows].filter((tr) => tr.dataset.fhide !== "1");
	assert.equal(shown.length, 1, "the filter could not find the 1001st template");
	assert.match(shown[0].textContent, /template 1001/);
});

test("a table holding part of its list says so in the count and the export name", async () => {
	const page = mountTemplates(3);
	const sandbox = sandboxOf(page.app);
	const downloads = [];
	sandbox.downloadBlob = (name) => { downloads.push(name); };
	await page.app.loadTemplates();
	const table = page.document.getElementById("templates").closest("table");
	page.app.noteListTotal(table, 3, 25000);
	page.app.mountTableExport();
	page.app.mountTablePager();
	const count = page.document.querySelector(".table-foot span");
	assert.ok(!page.document.querySelector(".table-foot").hidden, "the count line was hidden");
	assert.match(count.textContent, /Only the first 3 of 25,000 were read/,
		"the count passed a partial list off as the whole: " + count.textContent);
	fire(page.document.querySelectorAll("button.table-export")[0], "click");
	await page.clock.flush();
	assert.match(downloads[0], /-first-3\.csv$/, "the export file does not say it holds a part");
});
