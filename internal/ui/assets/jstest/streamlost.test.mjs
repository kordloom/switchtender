// Tests for the live indicator's honesty about a stream's state. A transient error means the
// browser is retrying, so reconnecting is the truth. An error on a closed source means the
// browser gave up and never will retry, an expired token's 401 for example, and the indicator
// used to say reconnecting forever. It must say the live view is lost and offer the reload.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

// mountStream mounts the detail page and opens a single-run stream directly.
async function mountStream() {
	const page = loadPage("detail");
	// Awaited because opening a stream now mints a ticket first rather than concatenating the
	// reader's own token into the URL.
	await page.app.openStream("run_1", 0);
	const source = page.streams.last();
	assert.ok(source, "a stream was opened");
	return { page, source };
}

test("a transient stream error reads as reconnecting", async () => {
	const { page, source } = await mountStream();
	await source.emitRaw("error");
	const indicator = page.document.getElementById("live-indicator");
	assert.equal(indicator.textContent, "reconnecting");
	assert.equal(indicator.hidden, false);
});

test("a closed stream re-mints a fresh ticket and resumes instead of dying", async () => {
	// A stream ticket is single-use, so the browser's own retry can never succeed on a secured
	// install: every drop landed on readyState 2 and the view said lost forever. The client owns
	// the recovery now, re-opening with a fresh ticket from its resume cursor.
	const { page, source } = await mountStream();
	source.readyState = 2;
	await source.emitRaw("error");
	const indicator = page.document.getElementById("live-indicator");
	assert.equal(indicator.textContent, "reconnecting");
	await page.clock.tick(1100);
	const next = page.streams.last();
	assert.ok(next && next !== source, "a fresh stream was opened after the backoff");
});

test("a closed stream out of retries reads as lost with a reload path", async () => {
	const { page, source } = await mountStream();
	page.app.streamState.retries = 6;
	source.readyState = 2;
	await source.emitRaw("error");
	const indicator = page.document.getElementById("live-indicator");
	assert.ok(indicator.textContent.includes("live view lost"), indicator.textContent);
	assert.ok(indicator.querySelector("a"), "the lost state carries the reload link");
});

test("a stream that recovers after a transient error reads live again", async () => {
	const { page, source } = await mountStream();
	await source.emitRaw("error");
	await source.emitRaw("open");
	const indicator = page.document.getElementById("live-indicator");
	assert.equal(indicator.textContent, "live");
});
