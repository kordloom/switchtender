// Tests for the run comparison page in 24-run-compare.js. Host and task names come from
// inventories and playbooks this server did not author, so the rendering itself is part of the
// security surface: a hostile name must land as text, never as markup.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";

// PARTS covers the compare page and the helpers it calls into.
const PARTS = [
	"01-boot.js", "02-page-data.js", "07-nav-theme.js", "08-auth-status.js",
	"18-host-page.js", "24-run-compare.js",
];

// comparison returns a minimal but complete document the way the server answers.
function comparison() {
	return {
		a: { id: "run_new", status: "failed", created_at: "2026-08-04T12:10:00Z" },
		b: { id: "run_old", status: "succeeded", created_at: "2026-08-04T12:00:00Z" },
		same_source: true,
		duration_delta_seconds: 60,
		totals: { ok: 1, broke: 1, recovered: 0, still_failing: 0, added: 0, removed: 0 },
		hosts: [
			{
				host: "<img src=x onerror=alert(1)>.example",
				verdict: "broke",
				a: { worst: "failed", failures: 2, changed: 0 },
				b: { worst: "ok", failures: 0, changed: 1 },
			},
			{ host: "web02", verdict: "ok", a: { worst: "ok", failures: 0, changed: 0 },
				b: { worst: "ok", failures: 0, changed: 0 } },
		],
		tasks: [
			{ task: "deploy <script>alert(2)</script>", a_seconds: 35, b_seconds: 10, delta_seconds: 25 },
			{ task: "only new", a_seconds: 3, b_seconds: -1, delta_seconds: 0 },
		],
	};
}

test("the comparison renders verdicts and a hostile name stays text", () => {
	const { app, document } = loadPage("compare", { parts: PARTS, vars: { RunID: "run_new" } });
	app.renderCompare(comparison());

	const rows = document.getElementById("compare-hosts").children;
	assert.equal(rows.length, 2);
	// The broken host leads and its badge names the break.
	assert.equal(rows[0].children[1].textContent, "Broke");
	// The hostile host name landed as text: no img element exists anywhere in the row.
	assert.equal(rows[0].querySelectorAll("img").length, 0);
	assert.ok(rows[0].textContent.includes("<img src=x onerror=alert(1)>.example"));
	// The host cell links to the host page with the name escaped into the URL.
	const link = rows[0].children[0].querySelector("a");
	assert.ok(link.getAttribute("href").startsWith("/ui/hosts/%3Cimg"));

	// Task rows carry the swing and the dash for a one-sided task, and the hostile task name
	// stays text.
	const taskRows = document.getElementById("compare-tasks").children;
	assert.equal(taskRows[0].children[3].textContent, "+25.00s");
	assert.equal(taskRows[1].children[2].textContent, "-");
	assert.equal(taskRows[0].querySelectorAll("script").length, 0);

	// The summary counts the verdicts and the duration swing.
	const summary = document.getElementById("compare-summary");
	assert.equal(summary.hidden, false);
	assert.ok(summary.textContent.includes("Broke"));
	assert.ok(summary.textContent.includes("+60.0s"));
});

test("runs from different sources carry the apples-to-apples warning", () => {
	const { app, document } = loadPage("compare", { parts: PARTS, vars: { RunID: "run_new" } });
	const c = comparison();
	c.same_source = false;
	app.renderCompare(c);
	assert.ok(document.getElementById("compare-header").textContent.includes("different sources"));
});
