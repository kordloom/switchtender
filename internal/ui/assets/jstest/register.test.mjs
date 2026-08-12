// Tests for the standalone change register, internal/dossier/register.html. It renders the largest
// table the product produces and it is read as evidence, usually from a file on someone's disk with
// no server behind it, so the controls on it have to work there: sorting has to reorder the rows,
// the filter has to remove them, the export has to carry the ones the filter kept, and the section
// navigation has to move without a fragment link, which a browser will not follow in a local file.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { fireWindow } from "./loader.mjs";
import { mountRegister, rows, runIDs, visibleRunIDs } from "./registerdoc.mjs";

// THREE is a small register whose columns disagree about order, so a sort has something to do.
const THREE = rows([
	{ Run: "run_a", Actor: "zoe", When: "2026-07-01 09:00", Outcome: "succeeded" },
	{ Run: "run_b", Actor: "adam", When: "2026-07-03 09:00", Outcome: "failed" },
	{ Run: "run_c", Actor: "mia", When: "2026-07-02 09:00", Outcome: "succeeded" },
]);

// many builds n rows, so the page limit and the print expansion have enough to work on.
function many(n) {
	const out = [];
	for (let i = 1; i <= n; i++) {
		out.push({ Run: "run_" + String(i).padStart(3, "0"), Actor: "operator" + i });
	}
	return rows(out);
}

// header returns one column heading of the change table.
function header(document, index) {
	return document.querySelectorAll("#register-table thead th")[index];
}

// ACTOR is the column index of the actor heading, which several tests sort and filter on.
const ACTOR = 3;

// WHEN is the column index of the timestamp heading.
const WHEN = 0;

test("a column heading sorts the register, and pressing it again reverses it", () => {
	const page = mountRegister({ Rows: THREE, Total: 3 });
	assert.deepEqual(runIDs(page.table), ["run_a", "run_b", "run_c"],
		"the register did not start in the order it was generated in");

	header(page.document, ACTOR).click();
	assert.deepEqual(runIDs(page.table), ["run_b", "run_c", "run_a"],
		"sorting by actor did not reorder the rows");
	assert.equal(header(page.document, ACTOR).getAttribute("aria-sort"), "ascending");

	header(page.document, ACTOR).click();
	assert.deepEqual(runIDs(page.table), ["run_a", "run_c", "run_b"],
		"pressing the heading again did not reverse the order");
	assert.equal(header(page.document, ACTOR).getAttribute("aria-sort"), "descending");
});

test("sorting one column drops the direction marker from the last one", () => {
	const page = mountRegister({ Rows: THREE, Total: 3 });
	header(page.document, ACTOR).click();
	header(page.document, WHEN).click();
	assert.deepEqual(runIDs(page.table), ["run_a", "run_c", "run_b"],
		"sorting by the timestamp did not put the register in time order");
	assert.equal(header(page.document, ACTOR).getAttribute("aria-sort"), null,
		"two columns are claiming to be the sorted one");
	assert.equal(header(page.document, ACTOR).dataset.dir, undefined);
});

test("the keyboard sorts a column the same way the pointer does", () => {
	const page = mountRegister({ Rows: THREE, Total: 3 });
	fire(header(page.document, ACTOR), "keydown", { key: "Enter" });
	assert.deepEqual(runIDs(page.table), ["run_b", "run_c", "run_a"],
		"Enter on a column heading did not sort it");
});

test("the filter removes the rows that do not match and says how many are left", () => {
	const page = mountRegister({ Rows: THREE, Total: 3 });
	const filter = page.document.getElementById("register-filter");
	filter.value = "adam";
	fire(filter, "input");

	assert.deepEqual(visibleRunIDs(page.table), ["run_b"], "the filter left rows that do not match");
	assert.equal(runIDs(page.table).length, 3, "the filter removed rows from the document");
	assert.equal(page.document.getElementById("register-count").textContent,
		"Showing 1 of 3 changes.");
	assert.equal(page.document.getElementById("register-empty").hidden, true);

	filter.value = "";
	fire(filter, "input");
	assert.deepEqual(visibleRunIDs(page.table), ["run_a", "run_b", "run_c"],
		"clearing the filter did not bring the rows back");
});

test("a filter that matches nothing empties the table and says so", () => {
	const page = mountRegister({ Rows: THREE, Total: 3 });
	const filter = page.document.getElementById("register-filter");
	filter.value = "nothing-matches-this";
	fire(filter, "input");

	assert.deepEqual(visibleRunIDs(page.table), []);
	assert.equal(page.document.getElementById("register-empty").hidden, false,
		"an empty table said nothing about being empty");
	assert.equal(page.document.getElementById("register-count").textContent,
		"Showing 0 of 3 changes.");
});

test("the CSV export carries the headings and the rows the filter kept, and nothing else", () => {
	const page = mountRegister({ Rows: THREE, Total: 3 });
	const filter = page.document.getElementById("register-filter");
	filter.value = "adam";
	fire(filter, "input");
	page.document.getElementById("register-csv").click();

	assert.equal(page.downloads.length, 1, "pressing Export CSV did not produce a file");
	const file = page.downloads[0];
	assert.equal(file.name, "switchtender-change-register-2026-07-01-to-2026-07-08.csv");
	assert.equal(file.type, "text/csv");
	const lines = file.content.trim().split("\n");
	assert.equal(lines[0], "When (UTC),Run,Change,Actor,Source,Risk,Held by,Decision,Outcome");
	assert.equal(lines.length, 2, "the export carried rows the filter had removed");
	assert.equal(lines[1], "2026-07-03 09:00,run_b,ansible site.yml,adam,template tpl_x,low,—,—,failed");
});

test("the CSV export follows the order the reader sorted the table into", () => {
	const page = mountRegister({ Rows: THREE, Total: 3 });
	header(page.document, ACTOR).click();
	page.document.getElementById("register-csv").click();

	const lines = page.downloads[0].content.trim().split("\n").slice(1);
	assert.deepEqual(lines.map((l) => l.split(",")[1]), ["run_b", "run_c", "run_a"]);
});

test("the CSV export defuses a cell a spreadsheet would run as a formula", () => {
	const page = mountRegister({
		Rows: rows([{ Run: "run_a", Actor: "=cmd|'/C calc'!A0" }]), Total: 1,
	});
	page.document.getElementById("register-csv").click();

	const line = page.downloads[0].content.trim().split("\n")[1];
	assert.match(line, /,'=cmd\|'\/C calc'!A0,/,
		"a formula payload in an actor name left the export live");
});

test("the page limit holds the rest of a long register back until Show all", () => {
	const page = mountRegister({ Rows: many(60), Total: 60 });
	assert.equal(visibleRunIDs(page.table).length, 50, "the page limit did not hold any rows back");
	assert.equal(visibleRunIDs(page.table)[49], "run_050");
	assert.equal(page.document.getElementById("register-count").textContent,
		"Showing 50 of 60 changes.");

	page.document.getElementById("register-showall").click();
	assert.equal(visibleRunIDs(page.table).length, 60, "Show all left rows hidden");
	assert.equal(page.document.getElementById("register-showall").hidden, true,
		"Show all is still offered with everything already shown");
});

test("the rows per page choice is what the table shows", () => {
	const page = mountRegister({ Rows: many(60), Total: 60 });
	const size = page.document.getElementById("register-pagesize");
	size.value = "25";
	fire(size, "change");
	assert.equal(visibleRunIDs(page.table).length, 25);
	assert.equal(page.document.getElementById("register-count").textContent,
		"Showing 25 of 60 changes.");
});

test("the page limit and the filter compose rather than fight", () => {
	const page = mountRegister({ Rows: many(60), Total: 60 });
	const filter = page.document.getElementById("register-filter");
	filter.value = "operator1";
	fire(filter, "input");
	// operator1 and operator10 through operator19 match, and all eleven fit inside the page limit.
	assert.equal(visibleRunIDs(page.table).length, 11);
	assert.equal(page.document.getElementById("register-count").textContent,
		"Showing 11 of 60 changes.");
});

test("the CSV export ignores the page limit, so an evidence file is the whole filtered period", () => {
	const page = mountRegister({ Rows: many(60), Total: 60 });
	page.document.getElementById("register-csv").click();
	const lines = page.downloads[0].content.trim().split("\n");
	assert.equal(lines.length, 61, "the export stopped at the first page of the register");
	assert.match(page.downloads[0].content, /run_060/);
});

test("printing opens every collapsed section and lifts the page limit", () => {
	const page = mountRegister({ Rows: many(60), Total: 60, AnchorProblems: ["Anchor 4 is stale."] });
	const details = page.document.querySelectorAll("details");
	assert.equal(details.length, 2, "the register lost one of its collapsible sections");
	details[0].removeAttribute("open");
	assert.equal(details[0].hasAttribute("open"), false);

	fireWindow(page.app, "beforeprint");
	assert.equal(details[0].hasAttribute("open"), true,
		"a section the reader collapsed would have printed collapsed");
	assert.equal(visibleRunIDs(page.table).length, 60,
		"printing would have carried only the first page of the register");

	fireWindow(page.app, "afterprint");
	assert.equal(details[0].hasAttribute("open"), false, "printing left the section reopened");
	assert.equal(visibleRunIDs(page.table).length, 50, "printing left the page limit lifted");
});

test("the sections are reached with data-jump rather than with a fragment link", () => {
	const page = mountRegister({
		Rows: THREE, Total: 3, AnchorProblems: ["Anchor 4 is stale."], Receipt: "9:abc",
	});
	for (const a of page.document.querySelectorAll("a")) {
		assert.equal(a.getAttribute("href"), null,
			"the register carries a link, which a file URL will not follow");
	}
	const links = page.document.querySelectorAll(".nav-link");
	assert.deepEqual(Array.from(links).map((b) => b.dataset.jump),
		["summary", "changes", "anchors", "chain"]);
	for (const link of links) {
		assert.ok(page.document.getElementById(link.dataset.jump),
			"the navigation offers " + link.dataset.jump + ", which is not a section in the document");
	}

	links[1].click();
	assert.equal(links[1].getAttribute("aria-current"), "true");
	assert.equal(links[0].getAttribute("aria-current"), null);
	links[0].click();
	assert.equal(links[0].getAttribute("aria-current"), "true");
	assert.equal(links[1].getAttribute("aria-current"), null,
		"two sections are marked as the one being read");
});

test("a summary card moves the reader to the table it is counting", () => {
	const page = mountRegister({ Rows: THREE, Total: 3, Approved: 1, Rejected: 1, Failed: 1 });
	const cards = page.document.querySelectorAll(".card");
	assert.equal(cards.length, 4);
	for (const card of cards) assert.equal(card.dataset.jump, "changes");

	cards[1].click();
	const changes = page.document.querySelector('.nav-link[data-jump="changes"]');
	assert.equal(changes.getAttribute("aria-current"), "true",
		"pressing a summary card did not move the reader to the changes section");
});

test("a register with no anchor problems offers neither the section nor its navigation", () => {
	const page = mountRegister({ Rows: THREE, Total: 3, AnchorProblems: [] });
	assert.equal(page.document.getElementById("anchors"), null);
	assert.deepEqual(Array.from(page.document.querySelectorAll(".nav-link")).map((b) => b.dataset.jump),
		["summary", "changes", "chain"]);
});
