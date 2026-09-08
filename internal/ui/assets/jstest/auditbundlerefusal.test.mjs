// Tests for what the audit page says when the server refuses to publish a bundle because the chain
// itself does not hold.
//
// The server answers that case with a 409 naming what failed and where, using the same coordinates
// GET /v1/audit/verify reports. The page rendered every non-ok response through one line, "Could not
// build the bundle: ...", which filed the product's central alarm under the same sentence as a
// dropped connection and threw away the server's coordinates. A detected tamper is the finding this
// page exists to surface, so it has to read as a finding and it has to agree with the verify badge
// sitting inches above it.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// bundlePage mounts the audit page with a canned verify answer and a canned bundle answer, then
// clicks Download bundle. The verify route answers healthy so that anything the badge ends up
// saying came from the bundle refusal rather than from the page's own opening verify pass.
async function bundlePage(bundleAnswer, init) {
	const page = loadPage("audit", {
		routes: {
			"/v1/audit/verify": reply({ ok: true, count: 9, anchored: 1, broke_at: 0 }),
			"/v1/audit/bundle": reply(bundleAnswer, init),
		},
	});
	page.app.wireAudit();
	await page.clock.flush();
	fire(page.document.getElementById("audit-bundle"), "click");
	await page.clock.flush();
	return page;
}

test("a broken chain is reported as tampering, not as a download that failed", async () => {
	const page = await bundlePage({
		error: "entry 3 does not recompute (sequence 3)",
		reason: "chain_break", broke_at: 3, broke_seq: 3, count: 9,
	}, { status: 409 });

	const badge = page.document.getElementById("audit-badge");
	assert.equal(badge.className, "chip failed",
		"a chain this server checked and rejected left the badge reading healthy");
	assert.match(badge.textContent, /entry 3/,
		"the badge does not name where the chain broke: " + badge.textContent);

	const status = page.document.getElementById("status").textContent;
	assert.doesNotMatch(status, /Could not build the bundle/,
		"the tamper finding is still filed as a generic download failure: " + status);
	assert.match(status, /does not recompute/,
		"the refusal does not say what was found: " + status);
	assert.match(status, /sequence 3/,
		"the server's chain sequence was dropped: " + status);
});

test("an unsatisfied anchor is reported as an anchor problem and keeps the server's diagnosis", async () => {
	const page = await bundlePage({
		error: "the chain no longer satisfies an anchor recorded over it",
		reason: "anchor_unsatisfied", count: 412,
		anchor_problems: ["anchor_7: the chain is shorter than the anchored size 500"],
	}, { status: 409 });

	const badge = page.document.getElementById("audit-badge");
	assert.equal(badge.className, "chip failed", "an unsatisfied anchor passed as healthy");
	assert.doesNotMatch(badge.textContent, /entry 0/,
		"the badge names entry 0, which is not an entry: " + badge.textContent);
	assert.match(badge.textContent, /anchor/i,
		"the badge does not say an anchor is unsatisfied: " + badge.textContent);

	// The server's own diagnosis is the actionable part, and a generic message drops it.
	const status = page.document.getElementById("status").textContent;
	assert.match(status, /anchor_7/,
		"the server's diagnosis of which anchor failed was dropped: " + status);
});

test("a fault that is not the chain still reads as a failure", async () => {
	// The point of the 409 branch is to separate a chain this server checked and rejected from this
	// server faulting. Widening it to every error would lose exactly the distinction it was added
	// for, so a 500 must still read as a failure and must not claim tampering.
	const page = await bundlePage({ error: "could not read the chain" }, { status: 500 });

	const status = page.document.getElementById("status").textContent;
	assert.match(status, /Could not build the bundle/,
		"a server fault stopped reading as a failure: " + status);
	assert.doesNotMatch(status, /does not recompute|altered/,
		"a server fault was reported as tampering: " + status);

	const badge = page.document.getElementById("audit-badge");
	assert.notEqual(badge.textContent, "Tampered at entry 0",
		"a server fault moved the verdict badge to a tamper finding");
});
