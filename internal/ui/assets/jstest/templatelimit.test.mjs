// Tests for what the template dialog does with the fields it did not used to carry. A save writes
// the template whole, so anything the form leaves out is written back empty and the answer is 200.
// A template pinned to a canary host, or restricted to a handful of tags, silently became one that
// reaches the whole inventory on every later schedule, webhook, and launch. The assertion here is
// on the body the dialog sends, since the reply looks the same either way.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// mountTemplateForm wires the template dialog against a recording server and returns the page plus
// the array every create and update body lands in, newest last.
function mountTemplateForm() {
	const sent = [];
	const routes = {
		"/v1/projects": reply({ projects: [] }),
		"/v1/credentials": reply({ credentials: [] }),
		"/v1/templates/tpl-1": (req) => {
			sent.push({ method: req.method, body: JSON.parse(req.body) });
			return reply({ id: "tpl-1" });
		},
		"/v1/templates": (req) => {
			if (req.method === "POST") {
				sent.push({ method: req.method, body: JSON.parse(req.body) });
				return reply({ id: "tpl-1" });
			}
			return reply({ templates: [] });
		},
	};
	const page = loadPage("jobtemplates", { parts: ALL_PARTS, routes });
	page.sent = sent;
	page.app.wireTemplateForm();
	// The harness has no form.reset, a browser API the success path and the New button both call.
	// This stands in for what a browser does, clearing the fields, because a no-op would let a
	// value left over from an edit pass for one the dialog carried forward on purpose.
	const form = page.document.getElementById("template-form");
	form.reset = () => {
		for (const el of form.querySelectorAll("input, textarea")) {
			if (el.type === "checkbox") el.checked = false;
			else el.value = "";
		}
	};
	return page;
}

test("a template created with a host limit sends it", async () => {
	const page = mountTemplateForm();
	await page.clock.flush();

	page.document.getElementById("tpl-name").value = "deploy production";
	page.document.getElementById("tpl-playbook").value = "plays/site.yml";
	page.document.getElementById("tpl-limit").value = "canary01";
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();

	assert.equal(page.sent.length, 1);
	assert.equal(page.sent[0].method, "POST");
	assert.equal(page.sent[0].body.limit, "canary01",
		"the dialog dropped the limit, so the template targets the whole inventory");
	page.net.assertClean();
});

test("editing a template keeps the limit and the controls the dialog does not show", async () => {
	const page = mountTemplateForm();
	await page.clock.flush();

	// The record as the server returns it: an imported template pinned to one host and narrowed to
	// two tags, with controls this dialog has no field for.
	page.app.openTemplateEdit({
		id: "tpl-1", name: "deploy production", playbook: "plays/site.yml",
		limit: "canary01", tags: ["web"], skip_tags: ["reboot"], verbosity: 2, forks: 25,
		diff_mode: true, selectable_credential_ids: ["cred-7"],
	});
	assert.equal(page.document.getElementById("tpl-limit").value, "canary01",
		"the edit dialog did not prefill the limit");

	// A rename, the smallest edit there is, and the one that used to erase everything else.
	page.document.getElementById("tpl-name").value = "deploy production v2";
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();

	assert.equal(page.sent.length, 1);
	assert.equal(page.sent[0].method, "PUT");
	const body = page.sent[0].body;
	assert.equal(body.name, "deploy production v2");
	assert.equal(body.limit, "canary01", "the rename dropped the host limit");
	assert.deepEqual(body.tags, ["web"], "the rename dropped the tags");
	assert.deepEqual(body.skip_tags, ["reboot"], "the rename dropped the skip tags");
	assert.equal(body.verbosity, 2, "the rename dropped the verbosity");
	assert.equal(body.forks, 25, "the rename dropped the fork count");
	assert.equal(body.diff_mode, true, "the rename dropped diff mode");
	assert.deepEqual(body.selectable_credential_ids, ["cred-7"],
		"the rename dropped the selectable credentials");
	page.net.assertClean();
});

test("a new template after an edit carries none of the edited template's controls", async () => {
	const page = mountTemplateForm();
	await page.clock.flush();

	page.app.openTemplateEdit({
		id: "tpl-1", name: "deploy production", playbook: "plays/site.yml",
		limit: "canary01", tags: ["web"], verbosity: 2,
	});
	// The New button resets the dialog to add mode. Nothing the edited record carried may leak into
	// the next template, or a create silently inherits another template's blast radius.
	fire(page.document.getElementById("template-open"), "click");
	page.document.getElementById("tpl-name").value = "fresh";
	page.document.getElementById("tpl-playbook").value = "plays/other.yml";
	fire(page.document.getElementById("template-form"), "submit");
	await page.clock.flush();

	assert.equal(page.sent.length, 1);
	assert.equal(page.sent[0].method, "POST");
	assert.equal(page.sent[0].body.limit, undefined, "the create inherited the edited limit");
	assert.equal(page.sent[0].body.tags, undefined, "the create inherited the edited tags");
	assert.equal(page.sent[0].body.verbosity, undefined,
		"the create inherited the edited verbosity");
	page.net.assertClean();
});
