// Tests for what the audit page does with a cut trail. The server answers has_more when the trail
// runs past the page it served. The page has to say so and refuse the table exports: a CSV headed
// "audit" that quietly drops everything older than the newest few hundred changes is the artifact a
// reviewer treats as the record, and nothing in the file says it is not.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { sandboxOf } from "./loader.mjs";

// entries builds n audit entries the table can render.
function entries(n) {
	const out = [];
	for (let i = 0; i < n; i++) {
		out.push({
			seq: n - i, at: "2026-08-11T09:00:00Z", actor: "root",
			method: "POST", path: "/v1/runs/run_" + (n - i), hash: "abcdef0123456789",
		});
	}
	return out;
}

// mountAudit loads the audit page against a trail answer, mounts the table exports the way the
// boot sequence does, and returns the page with a recorder standing in for the download.
async function mountAudit(hasMore) {
	const page = loadPage("audit", {
		routes: { "/v1/audit": reply({ entries: entries(3), count: 3, has_more: hasMore }) },
	});
	const sandbox = sandboxOf(page.app);
	page.downloads = [];
	sandbox.downloadBlob = (name, type, content) => { page.downloads.push({ name, type, content }); };
	page.app.mountTableExport();
	await page.app.loadAudit();
	page.buttons = page.document.querySelectorAll("button.table-export");
	return page;
}

test("a cut audit trail is announced and its table exports are refused", async () => {
	const page = await mountAudit(true);

	const notice = page.document.getElementById("audit-truncated");
	assert.ok(notice, "the page shows nothing about the trail being cut");
	assert.match(notice.textContent, /Showing the 3 most recent entries/);
	assert.match(notice.textContent, /Export signed/);
	// The notice belongs above the table it is describing, not appended anywhere in the document. The
	// table sits inside a .list-scroll wrapper that carries its horizontal overflow, so the notice
	// lands directly above that wrapper rather than inside it.
	const table = page.document.querySelector("main.content table.runs");
	const wrap = table.closest(".list-scroll") || table;
	assert.equal(notice.nextSibling, wrap, "the notice is not sitting above the table");

	assert.equal(page.buttons.length, 3, "the audit page should carry CSV, JSON, and YAML exports");
	for (const btn of page.buttons) {
		assert.equal(btn.disabled, true, "export button " + btn.textContent + " is still live");
		assert.match(btn.dataset.tip, /would leave the rest out/);
	}
	// Pressing one anyway must produce no file. A button that only looks disabled still exports.
	for (const btn of page.buttons) btn.click();
	assert.deepEqual(page.downloads, [], "a disabled export still wrote a file");
});

test("a whole audit trail exports normally and says nothing about truncation", async () => {
	const page = await mountAudit(false);

	assert.equal(page.document.getElementById("audit-truncated"), null,
		"a whole trail is being announced as cut");
	for (const btn of page.buttons) {
		assert.equal(btn.disabled, false, "export button " + btn.textContent + " was turned off");
	}
	page.buttons[0].click();
	// The export handlers are async so the runs page can complete its table first; a settled
	// microtask queue is part of the contract now.
	await page.clock.flush();
	assert.equal(page.downloads.length, 1, "the CSV export did not produce a file");
	assert.match(page.downloads[0].name, /^switchtender-audit-\d{4}-\d{2}-\d{2}\.csv$/);
	// The file has to hold the rows the table shows, or the export is broken in a quieter way.
	assert.match(page.downloads[0].content, /run_3/);
	assert.match(page.downloads[0].content, /run_1/);
});

test("the audit page asks for the page size it reports back to the reader", async () => {
	const page = await mountAudit(true);
	assert.deepEqual(page.net.urls, ["/v1/audit?limit=" + page.app.AUDIT_PAGE]);
	page.net.assertClean();
});
