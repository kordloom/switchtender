// Tests for the live indicator's honesty about a stream's state. A transient error means the
// browser is retrying, so reconnecting is the truth. An error on a closed source means the
// browser gave up and never will retry, an expired token's 401 for example, and the indicator
// used to say reconnecting forever. It must say the live view is lost and offer the reload.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

// mountStream mounts the detail page and opens a single-run stream directly.
function mountStream() {
	const page = loadPage("detail");
	page.app.openStream("run_1", 0);
	const source = page.streams.last();
	assert.ok(source, "a stream was opened");
	return { page, source };
}

test("a transient stream error reads as reconnecting", async () => {
	const { page, source } = mountStream();
	await source.emitRaw("error");
	const indicator = page.document.getElementById("live-indicator");
	assert.equal(indicator.textContent, "reconnecting");
	assert.equal(indicator.hidden, false);
});

test("a closed stream reads as lost with a reload path, not as reconnecting", async () => {
	const { page, source } = mountStream();
	source.readyState = 2;
	await source.emitRaw("error");
	const indicator = page.document.getElementById("live-indicator");
	assert.ok(indicator.textContent.includes("live view lost"), indicator.textContent);
	assert.ok(indicator.querySelector("a"), "the lost state carries the reload link");
});

test("a stream that recovers after a transient error reads live again", async () => {
	const { page, source } = mountStream();
	await source.emitRaw("error");
	await source.emitRaw("open");
	const indicator = page.document.getElementById("live-indicator");
	assert.equal(indicator.textContent, "live");
});
