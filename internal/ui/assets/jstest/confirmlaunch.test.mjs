// Tests for the confirm-on-launch template flag: the plain Launch button on a flagged template
// opens the overrides dialog instead of firing, and an unflagged template still launches on one
// click. The dialog itself is covered by launchflow.test.mjs; here the question is only which
// path a click takes.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { sandboxOf } from "./loader.mjs";

// PARTS covers the split button, both dialogs, and what they call into.
const PARTS = [
	"01-boot.js", "07-nav-theme.js", "08-auth-status.js", "09-audit.js", "10-modals-credentials.js",
	"12-templates-notify.js", "13-fileviewer-inventory.js", "16-runs-list.js", "18-host-page.js",
	"19-cron-preview.js", "20-held-copy-stream.js",
];

// LOOKUPS answers the reference lists the overrides dialog loads before it can be used.
const LOOKUPS = {
	"/v1/credentials": reply({ credentials: [] }),
	"/v1/projects": reply({ projects: [] }),
	"/v1/inventories": reply({ inventories: [] }),
};

test("a confirm-on-launch template opens the overrides dialog instead of firing", async () => {
	let launches = 0;
	const routes = Object.assign({}, LOOKUPS, {
		"/v1/templates/tpl-1/launch": () => {
			launches++;
			return reply({ id: "run-1" });
		},
	});
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: PARTS, routes });
	const wrap = app.launchSplitButton({ id: "tpl-1", name: "Destroy prod", survey: [], confirm_on_launch: true });
	document.body.appendChild(wrap);

	const main = wrap.querySelector(".split-main");
	fire(main, "click");
	await clock.flush();

	assert.equal(launches, 0, "one click on a confirm-on-launch template started a run");
	assert.equal(document.getElementById("prompt-modal").hidden, false,
		"the overrides dialog never opened, so there is no confirmation step");
	assert.equal(main.disabled, false, "the Launch button locked without launching anything");
	net.assertClean();
});

test("a template without the flag still launches on one click", async () => {
	const gate = deferred();
	let launches = 0;
	const routes = Object.assign({}, LOOKUPS, {
		"/v1/templates/tpl-1/launch": () => {
			launches++;
			gate.resolve(reply({ id: "run-1" }));
			return gate.promise;
		},
	});
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: PARTS, routes });
	const wrap = app.launchSplitButton({ id: "tpl-1", name: "Deploy", survey: [] });
	document.body.appendChild(wrap);

	fire(wrap.querySelector(".split-main"), "click");
	await clock.flush();

	assert.equal(launches, 1, "a plain template no longer launches from its Launch button");
	assert.deepEqual(app.location.navigations, ["/ui/runs/run-1"]);
	net.assertClean();
});

// The launch dialogs are the last thing between a click and a change on real hosts, and they opened
// without taking focus and without saying they were dialogs: a keyboard user pressed Launch and was
// still standing on the page behind, and a screen reader was told nothing had happened. Closing one
// dropped focus, so the next Tab restarted at the top of the document.
test("the launch dialogs announce themselves, take focus, and hand it back", async () => {
	const templates = [{
		id: "tpl_1", name: "deploy", playbook: "site.yml", confirm_on_launch: true,
		survey: [{ var: "env", label: "Environment", type: "text" }],
	}];
	const page = loadPage("jobtemplates", {
		routes: {
			"/v1/templates": reply({ templates }),
			"/v1/inventories": reply({ inventories: [] }),
			"/v1/credentials": reply({ credentials: [] }),
			"/v1/projects": reply({ projects: [] }),
		},
	});
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_role", "operator");

	for (const [name, open] of [
		["prompt", () => page.app.openPromptLaunch(templates[0])],
		["survey", () => page.app.openSurvey(templates[0])],
	]) {
		const opener = page.document.createElement("button");
		page.document.body.appendChild(opener);
		opener.focus();
		open();
		await page.clock.flush();

		const modal = page.document.getElementById(name + "-modal");
		assert.ok(modal, name + ": no dialog element");
		assert.equal(modal.hidden, false, name + ": the dialog did not open");
		const card = modal.querySelector(".modal-card") || modal;
		assert.equal(card.getAttribute("role"), "dialog",
			name + ": the dialog announces as nothing, so a screen reader is not told it opened");
		assert.equal(card.getAttribute("aria-modal"), "true", name + ": not marked modal");
		assert.ok(modal.contains(win.document.activeElement),
			name + ": focus stayed on the page behind the dialog");

		fire(page.document.getElementById(name === "prompt" ? "prompt-close" : "survey-cancel"), "click");
		await page.clock.flush();
		assert.equal(modal.hidden, true, name + ": the dialog did not close");
		assert.equal(win.document.activeElement, opener,
			name + ": closing dropped focus, so the next Tab starts at the top of the document");
	}
});
