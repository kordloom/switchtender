// Tests for the harness itself: the DOM in dom.mjs, the virtual clock, the fetch recorder, and the
// page mounts. A harness whose own behavior is untested can hand every test built on it a false
// pass, which is the failure this whole thing exists to stop, so the pieces the flow tests lean on
// are checked here rather than assumed.
import { readdirSync } from "node:fs";
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { clockOf, loadParts, sandboxOf } from "./loader.mjs";
import { delayed, failWith, installFetch, reply, sequence } from "./net.mjs";
import { loadPage, mountPage } from "./pages.mjs";

// blank loads the smallest sandbox that still has the whole browser stand-in in it.
function blank() {
	return loadParts(["01-boot.js"]);
}

test("every page template mounts, so a handler finds the ids the server ships", () => {
	const names = readdirSync(new URL("../../templates", import.meta.url))
		.filter((f) => f.endsWith(".html")).map((f) => f.slice(0, -5));
	assert.ok(names.length >= 20, "the template directory looks wrong: " + names.length + " files");
	for (const name of names) {
		const document = mountPage(blank(), name, { vars: { RunID: "run_1", MatrixCap: 20000 } });
		assert.ok(document.body, name + ": no body");
		assert.ok(document.body.dataset.page, name + ": the page has no data-page marker");
		assert.ok(document.querySelectorAll("[id]").length > 0, name + ": no addressable elements");
	}
});

test("getElementById resolves the document tree, not a list of guesses", () => {
	const document = mountPage(blank(), "runs");
	assert.equal(document.getElementById("runs-search").tagName, "INPUT");
	assert.equal(document.getElementById("nothing-here"), null);

	// An element built but never inserted is not on the page, so it is not found.
	const loose = document.createElement("div");
	loose.id = "later";
	assert.equal(document.getElementById("later"), null);
	document.body.appendChild(loose);
	assert.equal(document.getElementById("later"), loose);
	// And it stops being found once it is taken back off.
	loose.remove();
	assert.equal(document.getElementById("later"), null);
});

test("the selector engine covers the forms the script queries with", () => {
	const document = blank().document;
	document.body.innerHTML =
		'<div class="page-head"><button class="button primary" type="submit">Go</button></div>' +
		'<table class="runs"><tbody id="runs">' +
		'<tr class="skeleton-row"><td class="col-num"></td></tr>' +
		'<tr class="row-nav"><td class="col-num">1</td><td data-tip="open">x</td></tr>' +
		'</tbody></table>' +
		'<div id="status"></div><div id="launch-status" hidden></div>';

	const tests = [
		// Test 0: A tag name.
		{ In: "tbody", Want: 1 },
		// Test 1: An id.
		{ In: "#runs", Want: 1 },
		// Test 2: A class.
		{ In: ".row-nav", Want: 1 },
		// Test 3: A compound of tag and class.
		{ In: "table.runs", Want: 1 },
		// Test 4: Two classes on one element.
		{ In: ".button.primary", Want: 1 },
		// Test 5: An attribute by presence.
		{ In: "[data-tip]", Want: 1 },
		// Test 6: An attribute by value.
		{ In: 'button[type="submit"]', Want: 1 },
		// Test 7: An unquoted attribute value.
		{ In: "button[type=submit]", Want: 1 },
		// Test 8: A suffix match, which is how the live regions are found.
		{ In: '[id$="-status"]', Want: 1 },
		// Test 9: A comma list.
		{ In: '[id="status"], [id$="-status"]', Want: 2 },
		// Test 10: A descendant combinator.
		{ In: ".page-head .button", Want: 1 },
		// Test 11: A negation, which is how loaded rows are counted apart from placeholders.
		{ In: "tr:not(.skeleton-row)", Want: 1 },
		// Test 12: A descendant of a negation.
		{ In: "tr:not(.skeleton-row) td.col-num", Want: 1 },
		// Test 13: A child combinator.
		{ In: "tbody > tr", Want: 2 },
		// Test 14: Nothing matches.
		{ In: ".nope", Want: 0 },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(document.querySelectorAll(tc.In).length, tc.Want, "test " + i + ": " + tc.In);
	}

	// closest and matches walk the same grammar from an element rather than from the root.
	const cell = document.querySelector("tr.row-nav td.col-num");
	assert.equal(cell.closest("table").className, "runs");
	assert.equal(cell.closest(".nope"), null);
	assert.ok(cell.matches("td.col-num"));
	assert.ok(!cell.matches("td.col-labels"));
	// A selector the engine cannot compile is refused rather than quietly matching nothing.
	assert.throws(() => document.querySelectorAll("tr + tr"), /unsupported selector/);
});

test("innerHTML clears the children, parses the markup, and reads back", () => {
	const document = blank().document;
	const host = document.createElement("div");
	host.appendChild(document.createElement("span"));
	host.innerHTML = '<p class="a">first &amp; last</p><input id="f" value="v" hidden>';

	assert.equal(host.children.length, 2, "the old children survived the assignment");
	assert.equal(host.querySelector("p").textContent, "first & last");
	assert.equal(host.querySelector("#f").value, "v");
	assert.equal(host.querySelector("#f").hidden, true);
	// The read is a serialization of what is there now, not an echo of what was assigned.
	host.querySelector("p").remove();
	assert.equal(host.innerHTML, '<input id="f" value="v" hidden>');
});

test("dispatch runs capture, target, and bubble, and honors both ways of stopping", () => {
	const document = blank().document;
	document.body.innerHTML = '<div id="outer"><button id="inner">Go</button></div>';
	const outer = document.getElementById("outer");
	const inner = document.getElementById("inner");
	const seen = [];
	document.addEventListener("click", () => seen.push("document capture"), true);
	outer.addEventListener("click", () => seen.push("outer capture"), true);
	inner.addEventListener("click", (e) => {
		seen.push("target");
		assert.equal(e.target, inner);
		assert.equal(e.currentTarget, inner);
	});
	outer.addEventListener("click", () => seen.push("outer bubble"));
	inner.onclick = () => seen.push("inline");

	inner.click();
	assert.deepEqual(seen, ["document capture", "outer capture", "target", "inline", "outer bubble"]);

	// preventDefault is readable by whoever dispatched, which is how a canceled submit is asserted.
	inner.addEventListener("click", (e) => e.preventDefault());
	assert.equal(fire(inner, "click").defaultPrevented, true);

	// stopPropagation ends the walk, so the delegated listener above never sees the click.
	seen.length = 0;
	const quiet = document.createElement("button");
	outer.appendChild(quiet);
	quiet.addEventListener("click", (e) => { seen.push("quiet"); e.stopPropagation(); });
	quiet.click();
	assert.deepEqual(seen, ["document capture", "outer capture", "quiet"]);
});

test("the clock fires nothing until a test asks for it", async () => {
	const app = blank();
	const clock = clockOf(app);
	const sandbox = sandboxOf(app);
	const fired = [];
	sandbox.setTimeout(() => fired.push("late"), 250);
	const early = sandbox.setTimeout(() => fired.push("canceled"), 10);
	sandbox.clearTimeout(early);
	sandbox.setInterval(() => fired.push("tick"), 100);

	await clock.flush();
	assert.deepEqual(fired, [], "a timer fired without the clock being advanced");
	await clock.tick(100);
	assert.deepEqual(fired, ["tick"]);
	await clock.tick(150);
	assert.deepEqual(fired, ["tick", "tick", "late"], "the interval or the timeout fired out of order");
});

test("the fetch recorder answers routes, records calls, and refuses to invent one", async () => {
	const app = blank();
	const clock = clockOf(app);
	const net = installFetch(app, {
		"/v1/runs": sequence(reply({ runs: ["a"] }), reply({ runs: ["b"] })),
		"/v1/slow": delayed(500, reply({ ok: true })),
		"/v1/gone": failWith(new Error("network gone")),
	}, { quiet: true });

	const first = await (await app.fetch("/v1/runs?limit=1")).json();
	const second = await (await app.fetch("/v1/runs?limit=1", { method: "POST", body: "{}" })).json();
	assert.deepEqual([first.runs, second.runs], [["a"], ["b"]], "a queued route did not advance");
	await assert.rejects(app.fetch("/v1/runs"), /ran out of queued responses/);

	// Every call is on the record, with what it was sent with.
	assert.deepEqual(net.urls.slice(0, 2), ["/v1/runs?limit=1", "/v1/runs?limit=1"]);
	assert.equal(net.calls[0].method, "GET");
	assert.equal(net.calls[1].method, "POST");
	assert.equal(net.calls[1].body, "{}");
	assert.equal(net.calls[0].params.get("limit"), "1");

	// A delayed response waits on the same clock the page's timers run on.
	let settled = false;
	const slow = app.fetch("/v1/slow").then(() => { settled = true; });
	await clock.tick(499);
	assert.equal(settled, false, "a delayed response settled early");
	await clock.tick(1);
	await slow;
	assert.equal(settled, true);

	// A simulated network failure rejects, the way a dropped connection reaches the page.
	await assert.rejects(app.fetch("/v1/gone"), /network gone/);

	// An unrouted URL fails loudly and by name rather than resolving empty, and the miss is on the
	// record so a handler that swallowed it cannot leave the test green.
	await assert.rejects(app.fetch("/v1/unrouted"), /no route for GET \/v1\/unrouted/);
	assert.deepEqual(net.unmatched, ["GET /v1/unrouted"]);
	assert.throws(() => net.assertClean(), /no route answered GET \/v1\/unrouted/);
});

test("the stream recorder keeps the URL the page opened and feeds events back", async () => {
	const { app, streams } = loadPage("detail", {
		parts: ["01-boot.js"],
		vars: { RunID: "run_1", MatrixCap: 0 },
	});
	const source = new app.EventSource("/v1/runs/run_1/stream?after=3");
	assert.equal(streams.last(), source);
	assert.equal(streams.last().url, "/v1/runs/run_1/stream?after=3");

	const seen = [];
	source.addEventListener("event", (e) => seen.push(e.data));
	await source.emit("event", { seq: 4 });
	assert.deepEqual(seen, ['{"seq":4}'], "the payload did not arrive in wire form");
	source.close();
	assert.equal(source.closed, true);
});
