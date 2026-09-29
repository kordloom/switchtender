// Tests for how the Inventories page reads a stored inventory's content.
import { test } from "node:test";
import assert from "node:assert/strict";

import { ALL_PARTS, loadParts } from "./loader.mjs";

// TestParseInventoryReadsWhatASourceStores pins the JSON a dynamic source keeps. Read as INI it
// became hosts named "{" and "\"db\":", a count of ten with no groups, and an empty document read
// as one host, so the page misreported every inventory a source maintained.
test("a source's JSON inventory is read for its hosts and groups", () => {
	const app = loadParts(ALL_PARTS);
	const doc = JSON.stringify({
		_meta: { hostvars: { web1: { ansible_host: "10.0.0.1" }, web2: {}, db1: {} } },
		all: { children: ["ungrouped", "web", "db"] },
		web: { hosts: ["web1", "web2"] },
		db: { hosts: ["db1"] },
		ungrouped: {},
	}, null, 2);
	const tests = [
		// Test 0: The ansible-inventory --list shape.
		{ In: doc, WantFormat: "json", WantHosts: ["db1", "web1", "web2"], WantGroups: ["db", "web"] },
		// Test 1: An empty document holds no hosts.
		{ In: "{}", WantFormat: "json", WantHosts: [], WantGroups: [] },
		// Test 2: JSON that does not parse is reported as JSON with nothing counted, not as INI.
		{ In: "{ not json", WantFormat: "json", WantHosts: [], WantGroups: [] },
		// Test 3: INI still reads as INI.
		{ In: "[web]\nweb1\nweb2\n", WantFormat: "ini", WantHosts: ["web1", "web2"], WantGroups: ["web"] },
	];
	for (const [i, tc] of tests.entries()) {
		const got = app.parseInventory(tc.In);
		assert.equal(got.format, tc.WantFormat, "test " + i + ": format");
		assert.deepEqual([...got.hosts].sort(), tc.WantHosts, "test " + i + ": hosts");
		assert.deepEqual([...got.groups].sort(), tc.WantGroups, "test " + i + ": groups");
	}
});
