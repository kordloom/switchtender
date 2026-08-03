// Integration tests for the three controls that start a run: the launch form on the runs page, the
// survey dialog, and the launch-with-overrides dialog. All three share the guard in
// 10-modals-credentials.js, and all three are wired separately, so the guard existing proves
// nothing about any of them. Each button here is clicked the way a reader double clicks one, and
// the assertion is how many runs the server was asked to start.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// PARTS covers the launch form, both template dialogs, and what they call into.
const PARTS = [
	"01-boot.js", "07-nav-theme.js", "08-auth-status.js", "09-audit.js", "10-modals-credentials.js",
	"12-templates-notify.js", "13-fileviewer-inventory.js", "16-runs-list.js", "18-host-page.js",
	"19-cron-preview.js", "20-held-copy-stream.js",
];

// TEMPLATE is the saved template both dialogs launch, with no survey questions to answer.
const TEMPLATE = { id: "tpl-1", name: "Deploy", survey: [] };

// LOOKUPS answers the reference lists the dialogs load before they can be used.
const LOOKUPS = {
	"/v1/credentials": reply({ credentials: [] }),
	"/v1/projects": reply({ projects: [] }),
	"/v1/inventories": reply({ inventories: [] }),
};

// launchRoutes returns the reference lists plus a launch endpoint held open until the test releases
// it, so a second click lands while the first request is still in flight.
function launchRoutes(path) {
	const gate = deferred();
	const state = { count: 0, release: () => gate.resolve(reply({ id: "run-1" })) };
	return [state, Object.assign({}, LOOKUPS, {
		[path]: () => {
			state.count++;
			return gate.promise;
		},
	})];
}

test("the launch form starts one run however fast the operator submits", async () => {
	const [launch, routes] = launchRoutes("/v1/runs");
	const { app, document, net, clock } = loadPage("runs", { parts: PARTS, routes });
	app.wireLaunchForm();
	await clock.flush();

	const form = document.getElementById("launch-form");
	const button = form.querySelector('button[type="submit"]');
	document.getElementById("launch-playbook").value = "plays/site.yml";
	document.getElementById("launch-inventory").value = "/srv/hosts.ini";

	// Three submits in the time it takes to double click, before anything has come back.
	const events = [fire(form, "submit"), fire(form, "submit"), fire(form, "submit")];

	assert.equal(launch.count, 1, "a repeat submit started a second run at the same hosts");
	assert.equal(button.disabled, true, "the launch button stayed live while the run was starting");
	// Every submit is canceled, the dropped ones included, or a dropped submit posts the form the
	// old fashioned way and the browser navigates.
	assert.deepEqual(events.map((e) => e.defaultPrevented), [true, true, true]);

	launch.release();
	await clock.flush();
	assert.equal(launch.count, 1);
	// A launch that succeeded navigates to the new run, so the button stays down behind it.
	assert.equal(button.disabled, true);
	assert.deepEqual(app.location.navigations, ["/ui/runs/run-1"]);
	net.assertClean();
});

test("a launch the server rejects hands the form back to the operator", async () => {
	let attempts = 0;
	const routes = Object.assign({}, LOOKUPS, {
		"/v1/runs": () => {
			attempts++;
			return attempts === 1 ? reply({ error: "no such playbook" }, { status: 400 }) : reply({ id: "run-9" });
		},
	});
	const { app, document, net, clock } = loadPage("runs", { parts: PARTS, routes });
	app.wireLaunchForm();
	await clock.flush();

	const form = document.getElementById("launch-form");
	const button = form.querySelector('button[type="submit"]');
	fire(form, "submit");
	await clock.flush();

	assert.equal(attempts, 1);
	assert.equal(button.disabled, false, "a failed launch left the button dead, so nobody can retry");
	assert.equal(document.getElementById("launch-status").textContent,
		"Launch failed: no such playbook");

	// Corrected, the same button launches, which is the whole point of re-enabling it.
	fire(form, "submit");
	await clock.flush();
	assert.equal(attempts, 2);
	assert.deepEqual(app.location.navigations, ["/ui/runs/run-9"]);
	net.assertClean();
});

test("the survey dialog starts one run however fast the operator clicks", async () => {
	const [launch, routes] = launchRoutes("/v1/templates/tpl-1/launch");
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: PARTS, routes });
	app.openSurvey(TEMPLATE);
	assert.equal(document.getElementById("survey-modal").hidden, false, "the dialog never opened");

	const go = document.getElementById("survey-go");
	fire(go, "click");
	fire(go, "click");
	fire(go, "click");

	assert.equal(launch.count, 1, "a repeat click started a second run from the same template");
	assert.equal(go.disabled, true, "the launch button stayed live while the run was starting");

	launch.release();
	await clock.flush();
	assert.equal(launch.count, 1);
	assert.deepEqual(app.location.navigations, ["/ui/runs/run-1"]);
	net.assertClean();
});

test("a survey launch the server rejects hands the dialog back", async () => {
	let attempts = 0;
	const routes = Object.assign({}, LOOKUPS, {
		"/v1/templates/tpl-1/launch": () => {
			attempts++;
			return attempts === 1 ? reply({ error: "queue is full" }, { status: 503 }) : reply({ id: "run-9" });
		},
	});
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: PARTS, routes });
	app.openSurvey(TEMPLATE);

	const go = document.getElementById("survey-go");
	fire(go, "click");
	await clock.flush();
	assert.equal(attempts, 1);
	assert.equal(go.disabled, false, "a failed launch left the dialog locked");
	assert.equal(document.getElementById("survey-status").textContent, "Launch failed: queue is full");

	fire(go, "click");
	await clock.flush();
	assert.equal(attempts, 2);
	assert.deepEqual(app.location.navigations, ["/ui/runs/run-9"]);
	net.assertClean();
});

test("the overrides dialog starts one run however fast the operator clicks", async () => {
	const [launch, routes] = launchRoutes("/v1/templates/tpl-1/launch");
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: PARTS, routes });
	// The dialog loads the stored inventories before its button is live, so the open is awaited.
	await app.openPromptLaunch(TEMPLATE);
	assert.equal(document.getElementById("prompt-modal").hidden, false, "the dialog never opened");

	const go = document.getElementById("prompt-go");
	document.getElementById("prompt-limit").value = "web*";
	fire(go, "click");
	fire(go, "click");
	fire(go, "click");

	assert.equal(launch.count, 1, "a repeat click started a second run from the same template");
	assert.equal(go.disabled, true, "the launch button stayed live while the run was starting");
	// The overrides the reader typed ride along, so the one request that survived is the right one.
	const posted = JSON.parse(net.calledWith("/launch")[0].body);
	assert.equal(posted.limit, "web*");

	launch.release();
	await clock.flush();
	assert.equal(launch.count, 1);
	assert.deepEqual(app.location.navigations, ["/ui/runs/run-1"]);
	net.assertClean();
});

test("an overrides launch the server rejects hands the dialog back", async () => {
	let attempts = 0;
	const routes = Object.assign({}, LOOKUPS, {
		"/v1/templates/tpl-1/launch": () => {
			attempts++;
			return attempts === 1 ? reply({ error: "inventory is empty" }, { status: 422 }) : reply({ id: "run-9" });
		},
	});
	const { app, document, net, clock } = loadPage("jobtemplates", { parts: PARTS, routes });
	await app.openPromptLaunch(TEMPLATE);

	const go = document.getElementById("prompt-go");
	fire(go, "click");
	await clock.flush();
	assert.equal(attempts, 1);
	assert.equal(go.disabled, false, "a failed launch left the dialog locked");
	assert.equal(document.getElementById("prompt-status").textContent,
		"Launch failed: inventory is empty");

	fire(go, "click");
	await clock.flush();
	assert.equal(attempts, 2);
	assert.deepEqual(app.location.navigations, ["/ui/runs/run-9"]);
	net.assertClean();
});
