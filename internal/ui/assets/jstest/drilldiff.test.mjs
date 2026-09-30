// Tests for the Diff block in a task's drill drawer. The callback forwards the diff a module
// reports in Ansible's own structure, serialized as JSON, and the drawer printed that JSON: a one
// line change to a file read as headers and whole file bodies with every newline escaped.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";

// drillDiff opens the drill for a changed cell carrying diff and returns the Diff block's text, or
// null when the drawer shows no Diff block.
function drillDiff(diff) {
	const page = loadPage("detail");
	page.app.ensureDrill();
	page.app.showDrill({ host: "h1", task: "write a file", outcome: "changed", diff });
	for (const field of page.document.querySelectorAll("#drill-body .field")) {
		const label = field.querySelector(".label");
		if (label && label.textContent === "Diff") return field.querySelector("pre").textContent;
	}
	return null;
}

test("a file change reads as the unified diff Ansible prints, not as JSON", () => {
	const diff = JSON.stringify([{
		before_header: "/srv/app/motd.txt", before: "line one\nline two changed at 701515\nline three\n",
		after_header: "/srv/app/motd.txt", after: "line one\nline two changed at 221504\nline three\n",
	}]);
	assert.equal(drillDiff(diff), [
		"--- before: /srv/app/motd.txt",
		"+++ after: /srv/app/motd.txt",
		"@@ -1,3 +1,3 @@",
		" line one",
		"-line two changed at 701515",
		"+line two changed at 221504",
		" line three",
	].join("\n"));
});

test("a change deep in a long file shows three lines of context around it", () => {
	const lines = Array.from({ length: 20 }, (_, i) => "line " + (i + 1));
	const after = lines.slice();
	after[9] = "line 10, edited";
	const diff = JSON.stringify({
		before: lines.join("\n") + "\n", after: after.join("\n") + "\n",
	});
	assert.equal(drillDiff(diff), [
		"--- before",
		"+++ after",
		"@@ -7,7 +7,7 @@",
		" line 7", " line 8", " line 9",
		"-line 10",
		"+line 10, edited",
		" line 11", " line 12", " line 13",
	].join("\n"));
});

test("a structured before and after, the file module's state, diffs field by field", () => {
	const diff = JSON.stringify({
		before: { path: "/srv/app/flag", state: "absent" },
		after: { path: "/srv/app/flag", state: "touch" },
	});
	const text = drillDiff(diff);
	assert.match(text, /^-\s+"state": "absent"$/m, "the old state is not shown as removed");
	assert.match(text, /^\+\s+"state": "touch"$/m, "the new state is not shown as added");
	assert.match(text, /^ \s+"path": "\/srv\/app\/flag",$/m,
		"the unchanged field is not kept as context");
});

test("a module's own rendered diff is shown as the module wrote it", () => {
	const prepared = "--- before\n+++ after\n@@ -1 +1 @@\n-nginx 1.24\n+nginx 1.26\n";
	assert.equal(drillDiff(JSON.stringify({ prepared })), prepared);
});

test("a diff the field cap cut short, or one that is already text, is shown as it came", () => {
	const cut = '[{"before_header": "/srv/app/motd.txt", "before": "line one\\nline tw';
	assert.equal(drillDiff(cut), cut);
	assert.equal(drillDiff("-a\n+b"), "-a\n+b");
});
