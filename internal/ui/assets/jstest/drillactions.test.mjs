// Tests for the drill drawer's closing action row. The drawer used to end at whatever output
// the cell carried, which for a silent failure was a bare return code, and the reader's next
// step lived somewhere else on the page. Every cell drill now ends at a junction: the log,
// the clipboard, the host, and the host's history.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

// openDrill mounts the detail page and opens the drill for the given cell.
function openDrill(info) {
	const page = loadPage("detail");
	page.app.ensureDrill();
	page.app.showDrill(info);
	return page;
}

// actionLabels returns the text of every control in the drill's action row.
function actionLabels(page) {
	const row = page.document.querySelector(".drill-actions");
	if (!row) return [];
	return Array.from(row.querySelectorAll("a, button")).map((el) => el.textContent);
}

test("a failed cell with no output explains itself and still offers the exits", () => {
	const page = openDrill({
		host: "db01", task: "Apply configuration", outcome: "failed", rc: 1,
		message: "non-zero return code",
	});
	const note = page.document.querySelector(".drill-note");
	assert.ok(note, "the only-a-return-code note is present");
	const labels = actionLabels(page);
	assert.ok(labels.includes("Host page"), "the host page link is offered");
	assert.ok(labels.includes("Runs on this host"), "the host history link is offered");
});

test("a cell with captured output offers to copy it", () => {
	const page = openDrill({
		host: "db01", task: "Apply configuration", outcome: "failed", rc: 1,
		stdout: "applying revision", stderr: "error: rendered config failed validation",
	});
	assert.ok(actionLabels(page).includes("Copy output"), "the copy control is offered");
	assert.equal(page.document.querySelector(".drill-note"), null,
		"real output needs no only-a-return-code note");
});

test("the host links carry the host, encoded", () => {
	const page = openDrill({ host: "infra/network", task: "plan", outcome: "ok" });
	const row = page.document.querySelector(".drill-actions");
	const hrefs = Array.from(row.querySelectorAll("a")).map((a) => a.getAttribute("href"));
	assert.ok(hrefs.includes("/ui/hosts/infra%2Fnetwork"), "host page link encodes the host");
	assert.ok(hrefs.includes("/ui/runs?q=host%3Ainfra%2Fnetwork"),
		"history link lands on the filtered runs list");
});

test("a task bar drill without a host offers no host links", () => {
	const page = openDrill({ task: "Apply configuration", outcome: "failed" });
	const labels = actionLabels(page);
	assert.ok(!labels.includes("Host page"), "no host, no host link");
});
