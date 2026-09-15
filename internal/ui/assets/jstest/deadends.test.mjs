// Tests for the three places a reader could previously reach and find nothing to do: a run id that
// does not exist, a client filter that matches nothing, and a span beat rendered as "SPAN on span".
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";

test("a run that does not exist hides its actions and offers a way out", async () => {
	const page = loadPage("detail", {
		routes: [[/^\/v1\/runs\/run_gone$/, reply({ error: "no such run" }, { status: 404 })]],
		quiet: true,
	});
	await page.app.loadDetail("run_gone");

	const actions = page.document.querySelector("main.content .actions");
	assert.equal(actions.hidden, true,
		"the action row stayed live on a run that could not be read, so every button acted on nothing");

	const box = page.document.getElementById("run-deadend");
	assert.ok(box, "nothing explained that the run does not exist");
	assert.match(box.textContent, /run_gone/, "the explanation did not name the run");
	const hrefs = Array.from(box.querySelectorAll("a")).map((a) => a.getAttribute("href"));
	assert.ok(hrefs.includes("/ui/runs"), "no way back to the run list");
	assert.ok(hrefs.includes("/ui/audit"), "no pointer at the trail that still records it");
});

test("a span beat reads as a sentence rather than SPAN on span", () => {
	const { app } = loadPage("audit", { quiet: true });
	const text = app.auditChange("SPAN", "/v1/span/22?count=0&cadence_s=300");
	assert.doesNotMatch(text, /SPAN on span/, "the generic fallthrough still renders span beats");
	assert.match(text, /unbroken/i, "the sentence does not say what a beat attests");
	assert.match(text, /22/, "the sentence does not name the beat");
});
