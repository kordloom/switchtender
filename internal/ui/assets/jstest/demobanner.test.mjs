// Tests for the banner a read-only demo leads every page with. The hosted demo is reseeded every
// night, and its banner says so. A demo someone runs on their own machine is never reset, and
// without terraform it seeds no held run, yet it claimed the nightly reset and pointed at a filter
// that showed no runs.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { sandboxOf } from "./loader.mjs";

// bannerOn mounts the runs page as a demo served at host and returns the banner's text and links.
function bannerOn(host, demo) {
	const page = loadPage("runs");
	const sandbox = sandboxOf(page.app);
	sandbox.location.hostname = host;
	page.document.body.dataset.demo = demo ? "true" : "";
	const banner = page.app.readOnlyBanner();
	const links = [];
	for (const node of banner.childNodes) {
		if (node.tagName === "A") links.push(node.getAttribute("href"));
	}
	return { text: banner.textContent, links };
}

test("the hosted demo says it resets every night and starts at the held run", () => {
	const { text, links } = bannerOn("demo.switchtender.com", true);
	assert.match(text, /reset every night/);
	assert.ok(links.includes("/ui/runs?status=pending_approval"), "no link to the held run: " + links);
});

test("a demo on another host does not claim a nightly reset", () => {
	for (const host of ["127.0.0.1", "localhost", "demo.example.com"]) {
		const { text, links } = bannerOn(host, true);
		assert.doesNotMatch(text, /every night|tomorrow/, host + " claims a nightly reset: " + text);
		assert.ok(!links.includes("/ui/runs?status=pending_approval"),
			host + " points at a held run that may not exist");
		assert.ok(links.includes("/ui/runs"), host + " does not point at the runs it seeded");
	}
});

test("a read-only server that is not a demo only says it is read-only", () => {
	const { text } = bannerOn("switchtender.internal", false);
	assert.equal(text, "This server is read-only, so nothing here can be changed.");
});
