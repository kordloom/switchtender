// Tests for what the audit page says after verifying the chain. It used to collapse every failure
// into "Tampered at entry 0", which names an entry that cannot exist and points a reader at a
// tampering that did not happen when the real finding is an anchor the chain no longer satisfies. And
// an install that has never anchored anything was told "Chain verified", while the dossier drawn from
// the same chain says nothing outside this install fixes its position. The two artifacts have to
// agree, because a reader who sees them disagree cannot tell which one is lying.
import { test } from "node:test";
import assert from "node:assert/strict";

import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";
import { fire } from "./dom.mjs";

// verifyPage mounts the audit page with one canned answer from the verify endpoint.
function verifyPage(answer) {
	return loadPage("audit", {
		routes: { "/v1/audit/verify": reply(answer) },
	});
}

// verdict clicks Verify and returns the badge once the answer has landed.
async function verdict(page) {
	page.app.wireAudit();
	fire(page.document.getElementById("audit-verify"), "click");
	await page.clock.flush();
	return page.document.getElementById("audit-badge");
}

test("an anchor failure is reported as an anchor failure, not as tampering", async () => {
	const page = verifyPage({
		ok: false, count: 412, broke_at: 0, anchored: 2,
		anchor_problems: ["anchor_7: the chain is shorter than the anchored size 500"],
	});
	const badge = await verdict(page);

	assert.equal(badge.className, "chip failed", "an unsatisfied anchor passed as healthy");
	assert.doesNotMatch(badge.textContent, /entry 0/,
		"the badge names entry 0, which is not an entry: " + badge.textContent);
	assert.match(badge.textContent, /anchor/i, "the badge does not say an anchor is unsatisfied");
	// The server's own diagnosis is the actionable part and used to be dropped on the floor.
	assert.match(page.document.getElementById("status").textContent, /shorter than the anchored size/,
		"the anchor diagnostics were thrown away");
});

test("a real break still names the entry it broke at", async () => {
	const badge = await verdict(verifyPage({ ok: false, count: 412, broke_at: 87, anchored: 1 }));
	assert.equal(badge.className, "chip failed");
	assert.match(badge.textContent, /87/, "a tampered chain no longer names where it broke");
});

test("an unanchored chain is not reported as verified", async () => {
	const page = verifyPage({ ok: true, count: 412, anchored: 0 });
	const badge = await verdict(page);

	assert.doesNotMatch(badge.textContent, /verified/i,
		"an unanchored chain is still called verified: " + badge.textContent);
	assert.match(badge.textContent, /not anchored|no anchor/i,
		"the badge does not say the chain has never been anchored");
	assert.notEqual(badge.className, "chip ok",
		"an unanchored chain wears the same badge as an anchored one");
	assert.match(page.document.getElementById("status").textContent, /audit anchor/,
		"nothing tells the reader how to anchor the chain");
});

test("an anchored, intact chain says so, and how many anchors held", async () => {
	const badge = await verdict(verifyPage({ ok: true, count: 412, anchored: 3 }));
	assert.equal(badge.className, "chip ok");
	assert.match(badge.textContent, /412/);
	assert.match(badge.textContent, /3 anchor/, "the badge does not say what held the chain in place");
});
