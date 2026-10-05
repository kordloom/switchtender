// Tests that the smart inventory editor says, where the filter is written and where its hosts are
// previewed, that a smart inventory carries hosts and their variables but none of their groups, the
// same as AWX, and that a constructed inventory is the kind that keeps or builds groups. A play
// written for a group reaches nothing a smart inventory holds, and nothing on the page said so.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// INVENTORIES are the stored inventories the page lists: one with hosts of its own.
const INVENTORIES = [
	{ id: "inv_web", name: "web fleet", content: "[web]\nweb1\n", created_at: "2026-09-01T00:00:00Z" },
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

test("the smart inventory filter says its hosts arrive without their groups", async () => {
	const page = inventoryPage(() => ({}));
	page.app.wireInventoryForm();
	await page.clock.flush();

	const doc = page.document;
	const kind = doc.getElementById("inv-kind");
	kind.value = "smart";
	fire(kind, "change");
	const hint = doc.getElementById("inv-smart-groups");
	assert.ok(hint, "the smart inventory fields say nothing about groups");
	assert.equal(doc.getElementById("inv-kind-smart").hidden, false);
	assert.match(hint.textContent, /none of their groups/);
	assert.match(hint.textContent, /hosts: all reaches them, and a play for a group does not/);
	assert.match(hint.textContent, /constructed inventory/);
});

test("a smart preview says its hosts arrive without groups and a constructed one does not",
	async () => {
		for (const [kindName, wantSaid] of [["smart", true], ["constructed", false]]) {
			const page = inventoryPage(() => ({
				kind: kindName, hosts: ["web1"], count: 1, inputs: [{ id: "inv_web", name: "web fleet" }],
			}));
			page.app.wireInventoryForm();
			await page.clock.flush();

			const doc = page.document;
			const kind = doc.getElementById("inv-kind");
			kind.value = kindName;
			fire(kind, "change");
			if (kindName === "smart") {
				doc.getElementById("inv-host-filter").value = "name__startswith=web";
			} else {
				for (const o of doc.getElementById("inv-inputs").options) o.selected = true;
			}
			fire(doc.getElementById("inv-preview"), "click");
			await page.clock.flush();

			const status = doc.getElementById("inv-preview-status").textContent;
			assert.match(status, /1 host right now, from web fleet\./);
			assert.equal(/without their groups/.test(status), wantSaid,
				kindName + " preview said the wrong thing about groups: " + status);
		}
	});
