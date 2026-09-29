// Tests for the runs page's empty states. A no-match search must leave the toolbar, and the
// search box inside it, on screen: hiding them turned a typo into a page only a reload could
// leave. A truly empty instance still hides the noise, and a list that gains rows gets its
// controls back.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";

// runRow is one renderable run for the non-empty case.
const runRow = {
	id: "run_1", playbook: "site.yml", status: "succeeded",
	created_at: "2026-08-16T12:00:00Z", source: "api",
};

// mountRuns mounts the runs page against a canned run list and loads it with the given search.
async function mountRuns(runs, query) {
	const page = loadPage("runs", {
		routes: { "/v1/runs": reply({ runs, count: runs.length, summary: {} }) },
	});
	if (query) page.document.getElementById("runs-search").value = query;
	await page.app.loadRuns();
	return page;
}

// toolbarHidden reports whether the runs toolbar is hidden on the page.
function toolbarHidden(page) {
	return page.document.querySelector(".runs-toolbar").hidden;
}

test("a search that matches nothing keeps the toolbar and says so", async () => {
	const page = await mountRuns([], "no-such-run");
	assert.equal(toolbarHidden(page), false, "the toolbar hid, taking the search box with it");
	const status = page.document.getElementById("status");
	assert.equal(status.hidden, false);
	assert.ok(status.textContent.includes("No runs match"), status.textContent);
});

test("a truly empty instance hides the controls with the empty state", async () => {
	const page = await mountRuns([], "");
	assert.equal(toolbarHidden(page), true);
	const status = page.document.getElementById("status");
	assert.ok(status.textContent.includes("No runs yet"), status.textContent);
});

test("a list that gains rows gets its controls back", async () => {
	let empty = true;
	const page = loadPage("runs", {
		routes: {
			"/v1/runs": () => reply(empty
				? { runs: [], count: 0, summary: {} }
				: { runs: [runRow], count: 1, summary: {} }),
		},
	});
	await page.app.loadRuns();
	assert.equal(toolbarHidden(page), true);
	empty = false;
	await page.app.loadRuns();
	assert.equal(toolbarHidden(page), false, "the toolbar stayed hidden after rows arrived");
});
