// Tests for the navigation helpers in 07-nav-theme.js: route descriptions for link tips, the
// theme list, and the theme switcher group.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "07-nav-theme.js"]);

test("describeRoute names every interface path, most specific first", () => {
	const tests = [
		// Test 0: The overview root, with and without the trailing slash.
		{ In: "/ui/", Want: "Click to open the overview" },
		{ In: "/ui", Want: "Click to open the overview" },
		// Test 2: The runs list.
		{ In: "/ui/runs", Want: "Click to open the runs list" },
		// Test 3: A run id matches the detail pattern, not the list.
		{ In: "/ui/runs/run_42", Want: "Click to open this run" },
		// Test 4: A host page.
		{ In: "/ui/hosts/web-1", Want: "Click to open this host's history" },
		// Test 5: Fleet health.
		{ In: "/ui/fleet", Want: "Click to open fleet health" },
		// Test 6: Credentials.
		{ In: "/ui/credentials", Want: "Click to open credentials" },
		// Test 7: The audit trail.
		{ In: "/ui/audit", Want: "Click to open the audit trail" },
		// Test 8: The docs root.
		{ In: "/ui/docs", Want: "Click to open the documentation" },
		// Test 9: A docs page.
		{ In: "/ui/docs/install", Want: "Click to open this guide" },
		// Test 10: A path outside the interface says nothing.
		{ In: "/metrics", Want: "" },
		// Test 11: A deeper run subpath matches nothing rather than lying.
		{ In: "/ui/runs/run_1/events", Want: "" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.describeRoute(tc.In), tc.Want, "test " + i + ": " + tc.In);
	}
});

test("THEMES lists the three appearances with stable keys", () => {
	assert.deepEqual(
		[...app.THEMES].map((t) => t.key),
		["signature", "light", "dark"],
	);
	assert.deepEqual(
		[...app.THEMES].map((t) => t.label),
		["Loom", "Linen", "Ink"],
	);
});

test("themeGroup builds a hint and one labeled button per theme", () => {
	const g = app.themeGroup();
	assert.equal(g.className, "theme-group");
	assert.equal(g.children.length, 2);

	const [hint, row] = g.children;
	assert.equal(hint.className, "theme-hint");
	assert.equal(hint.textContent, "Theme");
	assert.equal(row.className, "theme-row");
	assert.equal(row.children.length, 3);

	const keys = [];
	for (const btn of row.children) {
		assert.equal(btn.tagName, "BUTTON");
		assert.equal(btn.type, "button");
		assert.equal(btn.className, "theme-btn");
		assert.equal(btn.getAttribute("aria-pressed"), "false");
		// Every button is named for assistive tech and for the hover tip.
		assert.equal(btn.getAttribute("aria-label"), btn.dataset.tip);
		assert.ok(btn.textContent.length > 0);
		keys.push(btn.dataset.themeKey);
	}
	assert.deepEqual(keys, ["signature", "light", "dark"]);
});
