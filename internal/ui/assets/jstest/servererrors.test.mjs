// Tests that the UI shows the sentence the server wrote rather than an internal path and a status
// code. The server already writes text a reader can act on, "project files are not enabled", "no
// facts gathered for this host yet", the licensing refusal with its pricing link, and every one of
// those was being thrown away in favor of strings like "/projects/proj_1/files returned 404".
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";
import { reply } from "./net.mjs";

// withReply loads a page whose every request gets one canned response.
function withReply(body, status) {
	return loadPage("runs", {
		routes: [[() => true, reply(body, { status })]],
		quiet: true,
	});
}

test("a 404 carrying an explanation shows the explanation, not the path", async () => {
	const { app } = withReply({ error: "project files are not enabled" }, 404);
	await assert.rejects(() => app.getJSON("/projects/proj_1/files"), (err) => {
		assert.equal(err.message, "project files are not enabled");
		return true;
	});
});

test("a 403 carrying a licensing explanation shows it rather than the role sentence", async () => {
	const licensing = "The period change register requires a Team license; this install runs " +
		"Community. https://switchtender.com/pricing";
	const { app } = withReply({ error: licensing }, 403);
	await assert.rejects(() => app.getJSON("/audit/register"), (err) => {
		assert.equal(err.message, licensing, "the role sentence replaced a licensing refusal");
		return true;
	});
});

test("a 403 carrying only a bare token still explains the role", async () => {
	const { app } = withReply({ error: "forbidden" }, 403);
	await assert.rejects(() => app.getJSON("/audit"), (err) => {
		assert.match(err.message, /role/i, "a bare token was shown where the role sentence says more");
		return true;
	});
});

test("a failure with no body at all never shows the request path", async () => {
	const { app } = withReply("", 503);
	await assert.rejects(() => app.getJSON("/inventories"), (err) => {
		assert.doesNotMatch(err.message, /\/inventories/, "the internal path leaked to the reader");
		assert.match(err.message, /503/);
		return true;
	});
});
