// Tests for the inventory source dialog's refresh cadence. The table has always shown two cadence
// columns, "Keeps updated" and "Refresh", and the dialog that creates and edits a source had a field
// for neither: a source made in the interface could never refresh, and the only way to set a cadence
// was the API. The table then reported what the source would do if it had one.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// sourcePage mounts the sources page with the pickers the dialog fills.
function sourcePage() {
	return loadPage("sources", {
		routes: {
			"/v1/credentials": reply({ credentials: [] }),
			"/v1/projects": reply({ projects: [] }),
			"/v1/inventory-sources": reply({ sources: [] }),
		},
	});
}

test("a new source can be given a refresh cadence", async () => {
	const page = sourcePage();
	page.app.wireSourceForm();
	await page.clock.flush();

	const every = page.document.getElementById("src-interval");
	const onLaunch = page.document.getElementById("src-update-on-launch");
	assert.ok(every, "the dialog cannot set the refresh interval the table reports");
	assert.ok(onLaunch, "the dialog cannot ask for a refresh before each launch");

	page.document.getElementById("src-name").value = "aws production";
	page.document.getElementById("src-source").value = "aws_ec2.yml";
	every.value = "15m";
	onLaunch.checked = true;
	fire(page.document.getElementById("source-form"), "submit");
	await page.clock.flush();

	const post = page.net.calls.find((c) => c.method === "POST");
	assert.ok(post, "the source was never sent");
	const body = JSON.parse(post.body);
	assert.equal(body.sync_interval_seconds, 900, "15m did not reach the server as 900 seconds");
	assert.equal(body.update_on_launch, true, "the before-launch refresh was dropped");
});

test("editing a source keeps the cadence it already has", async () => {
	const page = sourcePage();
	page.app.wireSourceForm();
	await page.clock.flush();

	page.app.openSourceEdit({
		id: "src_1", name: "aws production", source: "aws_ec2.yml",
		sync_interval_seconds: 3600, update_on_launch: true,
	});
	assert.equal(page.document.getElementById("src-interval").value, "1h",
		"the edit does not show the cadence the source refreshes on");
	assert.equal(page.document.getElementById("src-update-on-launch").checked, true,
		"the edit dropped the before-launch refresh");

	fire(page.document.getElementById("source-form"), "submit");
	await page.clock.flush();
	const put = page.net.calls.find((c) => c.method === "PUT");
	assert.ok(put, "the edit was never sent");
	const body = JSON.parse(put.body);
	assert.equal(body.sync_interval_seconds, 3600, "saving turned the refresh off");
	assert.equal(body.update_on_launch, true, "saving dropped the before-launch refresh");
});

test("an unreadable interval is refused rather than silently turning refresh off", async () => {
	const page = sourcePage();
	page.app.wireSourceForm();
	await page.clock.flush();
	page.document.getElementById("src-name").value = "aws production";
	page.document.getElementById("src-source").value = "aws_ec2.yml";
	page.document.getElementById("src-interval").value = "soonish";

	fire(page.document.getElementById("source-form"), "submit");
	await page.clock.flush();
	assert.equal(page.net.calls.filter((c) => c.method === "POST").length, 0,
		"an unreadable cadence was sent to the server");
	assert.match(page.document.getElementById("src-status").textContent, /15m|interval|cadence/i,
		"the refusal does not say what the field accepts");
});
