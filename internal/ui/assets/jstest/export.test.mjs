// Tests for the CSV field escape in 02-page-data.js. Exported tables carry values that came from
// outside the product, host names out of an imported inventory above all, so a cell that a
// spreadsheet would run as a formula has to leave as text.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "02-page-data.js"]);

test("csvCell defuses cells a spreadsheet would run as a formula", () => {
	const tests = [
		// Test 0: The classic formula lead.
		{ In: "=1+1", Want: "'=1+1" },
		// Test 1: A plus lead is a formula in Excel and Sheets alike.
		{ In: "+1+1", Want: "'+1+1" },
		// Test 2: A minus lead too, which costs a leading quote on negative numbers.
		{ In: "-1", Want: "'-1" },
		// Test 3: An at lead reaches the legacy macro functions.
		{ In: "@SUM(1+1)", Want: "'@SUM(1+1)" },
		// Test 4: A leading tab is stripped by the parser, exposing the formula behind it.
		{ In: "\t=1+1", Want: "'\t=1+1" },
		// Test 5: A leading carriage return does the same.
		{ In: "\r=1+1", Want: "'\r=1+1" },
		// Test 6: The command execution payload an attacker would put in a host name.
		{ In: "=cmd|'/C calc'!A0", Want: "'=cmd|'/C calc'!A0" },
		// Test 7: A payload carrying quotes and commas is defused and then quoted as usual.
		{
			In: '=HYPERLINK("http://evil","click")',
			Want: '"\'=HYPERLINK(""http://evil"",""click"")"',
		},
		// Test 8: A plain host name passes through untouched.
		{ In: "web1.example.com", Want: "web1.example.com" },
		// Test 9: A leading digit is not a formula.
		{ In: "2026-08-03T12:00:00Z", Want: "2026-08-03T12:00:00Z" },
		// Test 10: An equals sign inside the value is harmless.
		{ In: "env=prod", Want: "env=prod" },
		// Test 11: A comma still forces quoting.
		{ In: "web1, web2", Want: '"web1, web2"' },
		// Test 12: An embedded quote is doubled inside the quoted field.
		{ In: 'say "hi"', Want: '"say ""hi"""' },
		// Test 13: A newline still forces quoting.
		{ In: "line one\nline two", Want: '"line one\nline two"' },
		// Test 14: Both defenses at once.
		{ In: "-1,000", Want: "\"'-1,000\"" },
		// Test 15: Empty stays empty.
		{ In: "", Want: "" },
		// Test 16: A missing value is an empty field, not the string "undefined".
		{ In: undefined, Want: "" },
		// Test 17: A null value is an empty field too.
		{ In: null, Want: "" },
		// Test 18: A number is stringified.
		{ In: 42, Want: "42" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.csvCell(tc.In), tc.Want, "test " + i);
	}
});

test("a CSV row assembles from defused fields", () => {
	// The export joins each row exactly this way, so the whole line is worth pinning.
	const row = ["=2+5", "web1", "ok, done"];
	assert.equal(row.map(app.csvCell).join(","), "'=2+5,web1,\"ok, done\"");
});

test("csvCell is safe to hand straight to Array.map", () => {
	// Array.map passes the index and the array as extra arguments; they must not change the field.
	assert.deepEqual(["=1", "b"].map(app.csvCell), ["'=1", "b"]);
});
