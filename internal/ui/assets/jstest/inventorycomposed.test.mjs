// Tests for creating, editing, and previewing smart and constructed inventories on the Inventories
// page. Before these kinds existed the dialog could only store content, so an inventory composed from
// the others could be made through the API alone and its hosts could not be seen before a launch.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// INVENTORIES are the stored inventories the page lists: two with hosts of their own and one smart.
const INVENTORIES = [
	{ id: "inv_web", name: "web fleet", content: "[web]\nweb1\n", created_at: "2026-09-01T00:00:00Z" },
	{ id: "inv_db", name: "db fleet", content: "[db]\ndb1\n", created_at: "2026-09-01T00:00:00Z" },
	{
		id: "inv_smart", name: "all web", kind: "smart", host_filter: "groups__name=web",
		created_at: "2026-09-02T00:00:00Z",
	},
];

// inventoryPage mounts the inventories page with the dialog wired, answering previews with preview.
function inventoryPage(preview) {
	return loadPage("inventories", {
		routes: [
			["/v1/inventories/preview", (req) => reply(preview(req))],
			["/v1/inventories", reply({ inventories: INVENTORIES })],
			["/v1/credentials", reply({ credentials: [] })],
		],
	});
}

test("a smart inventory is saved as a host filter, with no content source", async () => {
	const page = inventoryPage(() => ({}));
	page.app.wireInventoryForm();
	await page.clock.flush();

	const doc = page.document;
	doc.getElementById("inv-name").value = "canaries";
	const kind = doc.getElementById("inv-kind");
	kind.value = "smart";
	fire(kind, "change");
	assert.equal(doc.getElementById("inv-kind-smart").hidden, false, "the filter field stayed hidden");
	assert.equal(doc.getElementById("inv-kind-static").hidden, true,
		"the content fields still show for an inventory that holds no content");
	doc.getElementById("inv-host-filter").value = "name__startswith=canary";
	fire(doc.getElementById("inventory-form"), "submit");
	await page.clock.flush();

	const post = page.net.calls.find((c) => c.method === "POST" && c.path === "/v1/inventories");
	assert.ok(post, "the inventory was never sent");
	const body = JSON.parse(post.body);
	assert.deepEqual(body, { name: "canaries", kind: "smart", host_filter: "name__startswith=canary" });
});

test("a constructed inventory offers only inventories with hosts as inputs and previews its hosts",
	async () => {
		let previewed;
		const page = inventoryPage((req) => {
			previewed = req;
			return { kind: "constructed", hosts: ["db1", "web1"], count: 2,
				inputs: [{ id: "inv_web", name: "web fleet" }, { id: "inv_db", name: "db fleet" }] };
		});
		page.app.wireInventoryForm();
		await page.clock.flush();

		const doc = page.document;
		const kind = doc.getElementById("inv-kind");
		kind.value = "constructed";
		fire(kind, "change");
		const inputs = doc.getElementById("inv-inputs");
		assert.deepEqual(Array.from(inputs.options).map((o) => o.value), ["inv_web", "inv_db"],
			"a smart inventory was offered as an input, which the API refuses");
		for (const o of inputs.options) o.selected = true;
		doc.getElementById("inv-source-vars").value = "groups:\n  off: state == 'shutdown'\n";
		doc.getElementById("inv-limit").value = "off";

		fire(doc.getElementById("inv-preview"), "click");
		await page.clock.flush();
		assert.ok(previewed, "the preview was never asked for");
		assert.equal(previewed.method, "POST");
		assert.deepEqual(JSON.parse(previewed.body), {
			name: "", kind: "constructed", input_inventory_ids: ["inv_web", "inv_db"],
			source_vars: "groups:\n  off: state == 'shutdown'\n", limit: "off",
		});
		const shown = Array.from(doc.getElementById("inv-preview-hosts").children).map((li) => li.textContent);
		assert.deepEqual(shown, ["db1", "web1"], "the preview did not list the hosts it resolved to");
		assert.match(doc.getElementById("inv-preview-status").textContent, /2 hosts.*web fleet, db fleet/);
	});

test("a constructed inventory with no input is refused before anything is sent", async () => {
	const page = inventoryPage(() => ({}));
	page.app.wireInventoryForm();
	await page.clock.flush();

	const doc = page.document;
	doc.getElementById("inv-name").value = "empty";
	const kind = doc.getElementById("inv-kind");
	kind.value = "constructed";
	fire(kind, "change");
	fire(doc.getElementById("inventory-form"), "submit");
	await page.clock.flush();
	assert.equal(page.net.calls.filter((c) => c.method === "POST").length, 0,
		"a constructed inventory with no input was sent");
	assert.match(doc.getElementById("inv-status").textContent, /input inventory/i);
});

test("editing a smart inventory keeps its kind and filter", async () => {
	const page = inventoryPage(() => ({}));
	page.app.wireInventoryForm();
	await page.clock.flush();

	page.app.openInventoryEdit(INVENTORIES[2]);
	const doc = page.document;
	assert.equal(doc.getElementById("inv-kind").value, "smart");
	assert.equal(doc.getElementById("inv-host-filter").value, "groups__name=web");
	assert.equal(doc.getElementById("inv-kind-smart").hidden, false);
	fire(doc.getElementById("inventory-form"), "submit");
	await page.clock.flush();

	const put = page.net.calls.find((c) => c.method === "PUT");
	assert.ok(put, "the edit was never sent");
	assert.equal(put.path, "/v1/inventories/inv_smart");
	const body = JSON.parse(put.body);
	assert.equal(body.kind, "smart", "saving turned the smart inventory into a static one");
	assert.equal(body.host_filter, "groups__name=web");
	assert.equal(body.content, undefined, "a smart inventory was sent content of its own");
});

test("the list says a composed inventory's hosts are resolved at launch", async () => {
	const page = inventoryPage(() => ({}));
	await page.app.loadInventories();
	await page.clock.flush();

	const rows = {};
	for (const row of page.document.querySelectorAll("tbody tr")) {
		rows[row.children[0].textContent] = [row.children[1].textContent, row.children[2].textContent];
	}
	assert.deepEqual(rows["all web"], ["smart", "at launch"]);
	assert.deepEqual(rows["web fleet"], ["ini", "1"]);
});
