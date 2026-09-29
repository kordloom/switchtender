// Tests for the estate page: the fleet on a date, and what moved since.
//
// Both questions read one history, so they share a page. The distinctions that matter to whoever
// reads it are the ones a wrong answer would hide: an empty result that means "no records that far
// back" rather than "no fleet", a removed host that means "no reading since" rather than "machine
// gone", and a changed fact shown as what became what rather than as the word "changed".
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadPage } from "./pages.mjs";

test("the estate at a date lists hosts with the run that observed them", async () => {
	const page = loadPage("estate", {
		routes: {
			"/v1/estate": {
				at: "2026-03-01T23:59:59Z",
				hosts: [{ host: "web01", run_id: "run_a", gathered_at: "2026-03-01T12:00:00Z",
					facts: { kernel: "5.15.0" } }],
				total: 1,
				horizon: "2026-02-01T00:00:00Z",
			},
		},
	});
	await page.app.loadEstate();
	const text = page.document.getElementById("estate").textContent;
	assert.ok(text.includes("web01"), "the host is missing: " + text);
	assert.ok(text.includes("kernel=5.15.0"), "the facts are missing: " + text);
	const note = page.document.getElementById("estate-horizon");
	assert.ok(note.textContent.includes("reaches back"), "the horizon is not stated: " + note.textContent);
});

test("a question older than the records says so rather than showing an empty fleet", async () => {
	const page = loadPage("estate", {
		routes: {
			"/v1/estate": { at: "2020-01-01T00:00:00Z", hosts: [], total: 0,
				horizon: "2026-02-01T00:00:00Z", before_history: true },
		},
	});
	await page.app.loadEstate();
	const note = page.document.getElementById("estate-horizon");
	assert.ok(note.textContent.includes("history begins"),
		"an empty answer before the records reads as an empty fleet: " + note.textContent);
	assert.equal(note.hidden, false, "the explanation is hidden");
});

test("a diff names what each fact became, and what removed really means", async () => {
	const page = loadPage("estate", {
		routes: {
			"/v1/estate/diff": {
				from: "2026-03-01T23:59:59Z", to: "2026-09-01T23:59:59Z",
				hosts: [
					{ host: "web01", state: "changed", facts: { kernel: { from: "5.15.0", to: "6.8.0" } } },
					{ host: "db01", state: "unobserved" },
					{ host: "new01", state: "added" },
				],
				unchanged: 4, total: 3,
			},
		},
	});
	page.document.getElementById("estate-from").value = "2026-03-01";
	await page.app.loadEstate();
	const text = page.document.getElementById("estate").textContent;
	// The value at each end, because "kernel changed" is not an answer anybody can act on.
	assert.ok(text.includes("5.15.0") && text.includes("6.8.0"),
		"a changed fact does not show what it became: " + text);
	assert.ok(text.includes("unobserved"), "the unobserved host is missing");

	// Nobody looked is not the same as looked and found unchanged, and the page has to say which.
	const chips = [...page.document.querySelectorAll(".chip")];
	const unobserved = chips.find((c) => c.textContent === "unobserved");
	assert.ok(unobserved && /carried forward rather than confirmed/.test(unobserved.title || ""),
		"unobserved does not explain that nothing looked at the host");

	// The column header changes with the question, so the second column is never read as a run id.
	assert.equal(page.document.getElementById("estate-col-state").textContent, "Change");
});

test("hosts that sat still are counted, not silently absent", async () => {
	const page = loadPage("estate", {
		routes: { "/v1/estate/diff": { hosts: [], unchanged: 12, total: 0 } },
	});
	page.document.getElementById("estate-from").value = "2026-03-01";
	await page.app.loadEstate();
	const status = page.document.getElementById("status").textContent;
	assert.ok(/12/.test(status),
		"a window where nothing moved reads as a query that found nothing: " + status);
});
