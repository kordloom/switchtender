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

// An INI inventory is read in one pass: a host seen before is found in a set, not by rescanning
// the hosts read so far. The rescan made reading a 100,000 host inventory take two minutes and one
// of 250,000 take longer than a page can wait, and a duplicate line must still count once.
test("an INI inventory is read without rescanning the hosts already read", () => {
	const app = loadParts(ALL_PARTS);
	// The page scripts run in their own context with their own Array, so that is the one to watch.
	const proto = app.Array.prototype;
	const original = proto.includes;
	let scans = 0;
	proto.includes = function (...args) {
		scans++;
		return original.apply(this, args);
	};
	try {
		const lines = ["[web]"];
		for (let i = 0; i < 2000; i++) lines.push("host" + i, "host" + i);
		const got = app.parseInventory(lines.join("\n"));
		assert.equal(got.format, "ini");
		assert.equal(got.hosts.length, 2000, "a repeated host counts once");
		assert.deepEqual([...got.hosts].slice(0, 3), ["host0", "host1", "host2"], "order is first seen");
		assert.deepEqual([...got.groups], ["web"]);
		assert.equal(scans, 0, "the parser rescanned an array " + scans + " times");
	} finally {
		proto.includes = original;
	}
});
