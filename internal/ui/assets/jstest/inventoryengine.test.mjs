// Tests that the page names the engine that resolved an inventory, the way the run's evidence will:
// the inventory editor's preview says whether the native engine or Ansible resolved it, and the
// doctor page shows the server's ansible-core beside the releases the native engine is tested on.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// previewOf mounts the inventories page, opens a smart inventory in the dialog, and previews it
// with the given answer, returning the status line under the preview.
async function previewOf(answer) {
	const page = loadPage("inventories", {
		routes: [
			["/v1/inventories/preview", reply(answer)],
			["/v1/inventories", reply({ inventories: [] })],
			["/v1/credentials", reply({ credentials: [] })],
		],
	});
	page.app.wireInventoryForm();
	await page.clock.flush();
	const doc = page.document;
	const kind = doc.getElementById("inv-kind");
	kind.value = "smart";
	fire(kind, "change");
	doc.getElementById("inv-host-filter").value = "name__startswith=web";
	fire(doc.getElementById("inv-preview"), "click");
	await page.clock.flush();
	return doc.getElementById("inv-preview-status").textContent;
}

test("a preview the native engine resolved says it needed no Ansible", async () => {
	const status = await previewOf({ kind: "smart", hosts: ["web1"], count: 1,
		inputs: [{ id: "inv_a", name: "fleet" }], engine: "native" });
	assert.match(status, /Resolved by the native engine, without Ansible\./);
});

test("a preview Ansible resolved names its ansible-core", async () => {
	const status = await previewOf({ kind: "smart", hosts: ["ec2-1"], count: 1,
		inputs: [{ id: "inv_c", name: "cloud" }], engine: "ansible", ansible_core: "2.18.1" });
	assert.match(status, /Resolved by Ansible, ansible-core 2\.18\.1\./);
	assert.doesNotMatch(status, /native/);
});

test("the doctor page shows ansible-core and whether it is a tested release", async () => {
	for (const [ansible, wantValue, wantTone] of [
		[{ installed: true, version: "2.18.1", in_range: true, tested: ["2.16", "2.21"] }, "2.18.1", "ok"],
		[{ installed: true, version: "2.14.3", in_range: false, tested: ["2.16", "2.21"] }, "2.14.3", "flaky"],
		[{ installed: false, in_range: false, tested: ["2.16", "2.21"] }, "not installed", ""],
	]) {
		const page = loadPage("doctor", {
			routes: [["/v1/doctor", reply({ findings: [], checked_templates: 0,
				checked_schedules: 0, checked_credentials: 0, ansible })]],
		});
		await page.app.loadDoctor();
		await page.clock.flush();
		const cards = page.document.getElementById("doctor-summary").querySelectorAll(".stat-card");
		const card = cards.find((c) => /ansible-core/.test(c.textContent));
		assert.ok(card, "the doctor summary has no ansible-core card");
		const value = card.querySelector(".stat-value");
		assert.match(value.textContent, new RegExp(wantValue.replace(/\./g, "\\.")));
		assert.equal(value.className.includes("flaky"), wantTone === "flaky",
			"the card for " + wantValue + " has tone " + value.className);
		assert.match(card.dataset.tip, /2\.16, 2\.21/);
	}
});
