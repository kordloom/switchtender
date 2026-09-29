// Tests for the seeded example on the workflow canvas, which is what a fresh install opens on.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// mountWorkflows opens the workflow page with nothing saved, so the editor seeds its example.
function mountWorkflows() {
	return loadPage("workflows", {
		parts: ALL_PARTS,
		routes: { "/v1/pipelines": reply({ id: "run_1" }) },
	});
}

// TestSampleIsLabeled pins that the seeded graph says it is a sample.
//
// A fresh install opened this page on a four-node release pipeline nobody wrote, with nothing
// marking it as an example, under the largest and brightest control on the page.
test("the seeded workflow says it is a sample", () => {
	const { app, document } = mountWorkflows();
	app.mountWorkflow();
	assert.equal(document.getElementById("wf-sample-note").hidden, false,
		"the seeded example did not say it was one");
	assert.match(document.getElementById("wf-sample-note").textContent, /example, not yours/);
});

// TestRunningTheSampleIsRefused pins that Run workflow does not execute a graph the reader never
// wrote. Unedited, it runs terraform against infra/network, two playbooks, and a curl at a host
// that does not exist, on the reader's own machine, and writes the failure into their first runs.
test("running the untouched sample is refused, and posts nothing", async () => {
	const { app, document, net } = mountWorkflows();
	app.mountWorkflow();
	await app.runWorkflow();
	assert.match(document.getElementById("status").textContent, /sample pipeline, not yours/i,
		"running the sample was not refused");
	net.assertClean();
});

// TestEditingClearsTheSample pins that the notice and the guard both let go once the graph is the
// reader's own work.
test("changing a step clears the sample notice and allows the run", async () => {
	const { app, document } = mountWorkflows();
	app.mountWorkflow();
	document.getElementById("wf-name").value = "deploy api";
	app.renderWorkflow();
	assert.equal(document.getElementById("wf-sample-note").hidden, true,
		"the sample notice outlived the edit that replaced the sample");
	await app.runWorkflow();
	assert.doesNotMatch(document.getElementById("status").textContent, /sample pipeline/i,
		"an edited graph was still refused as the sample");
});
