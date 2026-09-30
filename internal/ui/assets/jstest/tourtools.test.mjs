// Tests for the tours' lists of what SwitchTender runs. The welcome tour opened on "One binary runs
// Ansible, Terraform, Bash, Python, and Go" and offered to "Start a run with Ansible, Bash,
// Terraform, or Python", while the server runs seven tools, OpenTofu and PowerShell among them.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";

test("a tour step that lists the tools names every tool a run can use", () => {
	// The tools are read from the launch form's Tool picker, which offers the server's built-in
	// set, rather than listed here. A step naming two or more of them presents itself as the list.
	const { app, document } = loadPage("runs", { quiet: true });
	const tools = Array.from(document.querySelectorAll("#launch-tool option"))
		.map((o) => o.textContent.trim());
	assert.ok(tools.length > 0, "the launch form offers no tools, so this checks nothing");
	const named = (body, tool) => new RegExp("\\b" + tool + "\\b").test(body);
	const listing = app.TOURS.flatMap((t) => t.steps)
		.filter((s) => tools.filter((tool) => named(s.body, tool)).length >= 2);
	assert.ok(listing.length > 0, "no tour step lists the tools, so this checks nothing");
	for (const step of listing) {
		const missing = tools.filter((tool) => !named(step.body, tool));
		assert.deepEqual(missing, [], "\"" + step.title + "\" lists the tools without " +
			missing.join(", ") + ": " + step.body);
	}
});
