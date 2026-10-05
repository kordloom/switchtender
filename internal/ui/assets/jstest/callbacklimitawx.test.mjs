// Tests for the template dialog's callback limit mode and AWX callback address switch, the
// template view's lines for them, and who the inventories page offers cached facts to.
//
// The dialog writes a template whole, so a setting the form loses is one the next save resets. The
// AWX address switch exists only for a template an import bound to its AWX job template id, so the
// dialog must not offer it, or send it, for any other template.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// mountForm wires the template dialog against a recording server.
function mountForm() {
	const sent = [];
	const record = (req) => {
		sent.push({ method: req.method, body: req.body ? JSON.parse(req.body) : null });
		return reply({ id: "tpl-1", host_config_key_set: true });
	};
	const routes = {
		"/v1/projects": reply({ projects: [] }),
		"/v1/credentials": reply({ credentials: [] }),
		"/v1/templates/tpl-1": record,
		"/v1/templates": (req) => (req.method === "POST" ? record(req) : reply({ templates: [] })),
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
		for (const el of form.querySelectorAll("select")) el.value = el.options[0] ? el.options[0].value : "";
	};
	return page;
}

// bound is a template an import bound to AWX job template 42, with callbacks on.
const bound = {
	id: "tpl-1", name: "provision", playbook: "mark.yml", inventory_id: "inv-1", limit: "web",
	allow_callbacks: true, host_config_key_set: true, awx_job_template_id: 42, awx_callback: true,
};

test("editing a bound template offers its AWX address and keeps both settings", async () => {
	const page = mountForm();
	await page.clock.flush();
	page.app.openTemplateEdit(bound);
	const field = page.document.getElementById("tpl-field-awx-callback");
	assert.ok(!field.hidden, "the AWX address switch is hidden for a bound template");
	assert.equal(page.document.getElementById("tpl-awx-callback").checked, true);
	assert.ok(page.document.getElementById("tpl-awx-callback-hint").textContent
		.includes("/api/v2/job_templates/42/callback/"), "the hint does not name the AWX address");
	page.document.getElementById("tpl-awx-callback").checked = false;
	page.document.getElementById("tpl-callback-limit").value = "replace";
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();

	const body = page.sent[0].body;
	assert.equal(page.sent[0].method, "PUT");
	assert.equal(body.awx_callback, false, "turning the AWX address off was not sent");
	assert.equal(body.callback_limit, "replace", "the callback limit mode was not sent");
	page.net.assertClean();
});

test("a template no import bound has no AWX address to offer or send", async () => {
	const page = mountForm();
	await page.clock.flush();
	page.app.openTemplateEdit(bound);
	page.app.openTemplateEdit({
		id: "tpl-1", name: "native", playbook: "mark.yml", inventory_id: "inv-1", allow_callbacks: true,
	});
	assert.ok(page.document.getElementById("tpl-field-awx-callback").hidden,
		"the AWX address switch is shown for a template no import bound");
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();
	const body = page.sent[0].body;
	assert.equal(body.awx_callback, undefined, "the dialog sent an AWX address switch");
	assert.equal(body.callback_limit, "intersect", "an edit did not state the callback limit");
	page.net.assertClean();
});

test("a new template states the callback limit only when it replaces", async () => {
	const page = mountForm();
	await page.clock.flush();
	for (const [mode, want] of [["intersect", undefined], ["replace", "replace"]]) {
		page.sent.length = 0;
		page.document.getElementById("tpl-name").value = "boot";
		page.document.getElementById("tpl-playbook").value = "boot.yml";
		page.document.getElementById("tpl-callback-limit").value = mode;
		fire(page.document.getElementById("template-form"), "submit");
		await page.clock.flush();
		assert.equal(page.sent[0].method, "POST");
		assert.equal(page.sent[0].body.callback_limit, want, "callback_limit for " + mode);
		assert.equal(page.sent[0].body.awx_callback, undefined, "a create sent an AWX switch");
	}
	page.net.assertClean();
});

test("the template view names the AWX address, when it was last called, and the limit mode", () => {
	const page = loadPage("jobtemplates", { parts: ALL_PARTS });
	page.app.openTemplateView(Object.assign({}, bound, {
		awx_callback_called_at: "2026-10-01T09:00:00Z",
	}));
	const rows = page.document.getElementById("view-rows").textContent;
	assert.ok(rows.includes("/api/v2/job_templates/42/callback/"), "no AWX address row: " + rows);
	assert.ok(rows.includes("Last called through AWX"), "no last called row: " + rows);
	assert.ok(!rows.includes("never since the import"), "a called address reads as never called");
	assert.ok(rows.includes("a calling host outside web is refused"), "no limit mode row: " + rows);

	page.app.openTemplateView(Object.assign({}, bound, { awx_callback: false }));
	const off = page.document.getElementById("view-rows").textContent;
	assert.ok(off.includes("(off)"), "an address turned off does not say so: " + off);
	assert.ok(off.includes("never since the import"), "an address never called does not say so");
});

test("the inventories page offers cached facts to admins only when the server says so", () => {
	const open = loadPage("inventories", { parts: ALL_PARTS });
	assert.equal(open.app.factCacheReaderRole(), "operator");
	const restricted = loadPage("inventories", { parts: ALL_PARTS, vars: { FactCacheAdminOnly: true } });
	assert.equal(restricted.app.factCacheReaderRole(), "admin");
});
