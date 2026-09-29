// Tests for the changes page: what somebody set out to do, and every run it took.
//
// The distinctions worth holding are the ones a careless render would erase. A change that broke
// and was then put right is neither a success nor a failure. A change still in flight has no close
// time. And a listing built by scanning recent runs cannot see older changes, which it has to say
// rather than present a partial answer as the whole one.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

const CHANGES = {
	changes: [
		{ change: "OPS-482", outcome: "mixed", total: 2, actors: ["admin", "deploy-bot"],
			opened_at: "2026-09-01T10:00:00Z", closed_at: "2026-09-01T12:00:00Z" },
		{ change: "REL-9", outcome: "in_progress", total: 1, actors: ["admin"],
			opened_at: "2026-09-02T10:00:00Z" },
	],
	total: 2, scanned: 40,
};

test("a change that broke and was fixed reads as mixed, not failed", async () => {
	const page = loadPage("changes", { routes: { "/v1/changes": CHANGES } });
	await page.app.loadChanges();
	const chips = [...page.document.querySelectorAll(".chip")];
	const mixed = chips.find((c) => c.textContent === "mixed");
	assert.ok(mixed, "the mixed outcome is missing, so a rollback and its fix read as a failure");
	assert.ok(/really happened/.test(mixed.dataset.tip || ""),
		"mixed does not explain itself, so a reader guesses what it means");
});

test("an open change shows no close time", async () => {
	const page = loadPage("changes", { routes: { "/v1/changes": CHANGES } });
	await page.app.loadChanges();
	const text = page.document.getElementById("changes").textContent;
	assert.ok(text.includes("still open"),
		"a change still in flight was given a close time, which says the work was finished: " + text);
});

test("a change links to its own runs rather than a second detail page", async () => {
	const page = loadPage("changes", { routes: { "/v1/changes": CHANGES } });
	await page.app.loadChanges();
	const link = page.document.querySelector("#changes a");
	assert.ok(link, "the change is not a link, so there is no way into its runs");
	assert.ok(link.getAttribute("href").includes("label%3Achange%3DOPS-482"),
		"the link does not filter the runs list by the change label: " + link.getAttribute("href"));
});

test("a listing that could not see everything says so", async () => {
	const page = loadPage("changes", {
		routes: { "/v1/changes": { ...CHANGES, partial: true, scanned: 2000 } },
	});
	await page.app.loadChanges();
	const note = page.document.getElementById("changes-note");
	assert.equal(note.hidden, false, "a partial listing is presented as the whole one");
	assert.ok(note.textContent.includes("2000"),
		"the note does not say how far back the listing reached: " + note.textContent);
});

test("no changes yet reads as an invitation rather than an error", async () => {
	const page = loadPage("changes", { routes: { "/v1/changes": { changes: [], total: 0, scanned: 0 } } });
	await page.app.loadChanges();
	const status = page.document.getElementById("status").textContent;
	assert.ok(/Label a run/.test(status), "an empty list does not say how to make one: " + status);
});
