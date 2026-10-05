// Tests for the fact cache and provisioning callback controls. The dialog writes a template whole,
// so a switch the form loses is a switch the next save turns off, and a callback key is shown once,
// so the page that turns callbacks on is the one that has to show it.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// mountForm wires the template dialog against a recording server. saved is what a save answers
// with, and minted is what minting a key answers with.
function mountForm(saved, minted) {
	const sent = [];
	const record = (req, answer) => {
		sent.push({ method: req.method, path: req.url, body: req.body ? JSON.parse(req.body) : null });
		return reply(answer);
	};
	const routes = {
		"/v1/projects": reply({ projects: [] }),
		"/v1/credentials": reply({ credentials: [] }),
		"/v1/templates/tpl-1/callback-key": (req) => record(req, minted || {}),
		"/v1/templates/tpl-1": (req) => record(req, saved || { id: "tpl-1" }),
		"/v1/templates": (req) => {
			if (req.method === "POST") return record(req, saved || { id: "tpl-1" });
			return reply({ templates: [] });
		},
	};
	const page = loadPage("jobtemplates", { parts: ALL_PARTS, routes });
	page.sent = sent;
	page.app.wireTemplateForm();
	const form = page.document.getElementById("template-form");
	form.reset = () => {
		for (const el of form.querySelectorAll("input, textarea")) {
			if (el.type === "checkbox") el.checked = false;
			else el.value = "";
		}
	};
	return page;
}

test("a new template with the fact cache on sends it with its timeout", async () => {
	const page = mountForm();
	await page.clock.flush();
	page.document.getElementById("tpl-name").value = "patch";
	page.document.getElementById("tpl-playbook").value = "plays/patch.yml";
	page.document.getElementById("tpl-fact-cache").checked = true;
	page.document.getElementById("tpl-fact-cache-timeout").value = "3600";
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();

	assert.equal(page.sent.length, 1);
	const body = page.sent[0].body;
	assert.equal(body.use_fact_cache, true, "the dialog dropped the fact cache switch");
	assert.equal(body.fact_cache_timeout, 3600, "the dialog dropped the fact cache timeout");
	assert.equal(body.allow_callbacks, undefined, "a create stated a switch nobody touched");
	page.net.assertClean();
});

test("editing keeps both switches and turning one off says so", async () => {
	const page = mountForm();
	await page.clock.flush();
	page.app.openTemplateEdit({
		id: "tpl-1", name: "boot", playbook: "boot.yml", inventory_id: "inv-1",
		use_fact_cache: true, fact_cache_timeout: 600, allow_callbacks: true, host_config_key_set: true,
	});
	assert.equal(page.document.getElementById("tpl-fact-cache").checked, true,
		"the edit dialog did not show the fact cache as on");
	assert.equal(page.document.getElementById("tpl-allow-callbacks").checked, true,
		"the edit dialog did not show callbacks as on");
	page.document.getElementById("tpl-allow-callbacks").checked = false;
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();

	const body = page.sent[0].body;
	assert.equal(page.sent[0].method, "PUT");
	assert.equal(body.use_fact_cache, true, "the edit turned the fact cache off");
	assert.equal(body.fact_cache_timeout, 600, "the edit dropped the timeout");
	assert.equal(body.allow_callbacks, false, "turning callbacks off was not sent");
	assert.equal(body.inventory_id, "inv-1");
	page.net.assertClean();
});

test("turning callbacks on mints the key and shows it once with the host's command", async () => {
	const saved = { id: "tpl-1", name: "boot", allow_callbacks: true, host_config_key_set: false };
	const minted = {
		template: "tpl-1", host_config_key: "hck_shown_once",
		callback_path: "/v1/templates/tpl-1/callback",
	};
	const page = mountForm(saved, minted);
	await page.clock.flush();
	page.app.openTemplateEdit({
		id: "tpl-1", name: "boot", playbook: "boot.yml", inventory_id: "inv-1",
	});
	page.document.getElementById("tpl-allow-callbacks").checked = true;
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();

	assert.deepEqual(page.sent.map((s) => s.method + " " + s.path),
		["PUT /v1/templates/tpl-1", "POST /v1/templates/tpl-1/callback-key"]);
	const modal = page.document.getElementById("callback-key-modal");
	assert.ok(modal && !modal.hidden, "the minted key was not shown");
	const code = page.document.getElementById("callback-key-code").textContent;
	assert.ok(code.includes("hck_shown_once"), "the shown command lacks the key");
	assert.ok(code.includes("/v1/templates/tpl-1/callback"),
		"the shown command lacks the callback URL");
	page.net.assertClean();
});

test("an inventory's cached facts list per host and open one document", async () => {
	// The longer path is listed first, since a route claims every path it prefixes.
	const routes = {
		"/v1/inventories/inv-1/facts/web01": reply({
			host: "web01", facts: { ansible_distribution: "Debian" },
		}),
		"/v1/inventories/inv-1/facts": reply({ inventory_id: "inv-1", count: 1, hosts: [{
			inventory_id: "inv-1", host: "web01", bytes: 42, run_id: "run-9",
			modified_at: "2026-10-01T09:00:00Z",
		}] }),
	};
	const page = loadPage("inventories", { parts: ALL_PARTS, routes });
	await page.app.openInventoryFacts({ id: "inv-1", name: "fleet" });
	const rows = page.document.getElementById("facts-rows");
	assert.equal(rows.children.length, 1, "the cached host was not listed");
	fire(rows.querySelector("button"), "click");
	await page.clock.flush();
	const doc = page.document.getElementById("facts-doc");
	assert.ok(!doc.hidden && doc.textContent.includes("Debian"), "the host's facts were not shown");
	page.net.assertClean();
});
