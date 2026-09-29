// Tests for esc in 05-workflow-editor.js, the escape used when workflow markup is built as a
// string, plus the draft storage key beside it.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "05-workflow-editor.js"]);

test("esc escapes every character that can break out of markup", () => {
	const tests = [
		// Test 0: Ampersand first, so entities do not double up on the specials.
		{ In: "a&b", Want: "a&amp;b" },
		// Test 1: Angle brackets.
		{ In: "<script>", Want: "&lt;script&gt;" },
		// Test 2: Double quote, the attribute delimiter the escape exists for.
		{ In: 'a"b', Want: "a&quot;b" },
		// Test 3: Single quote.
		{ In: "a'b", Want: "a&#39;b" },
		// Test 4: Already-escaped input escapes again rather than passing through.
		{ In: "&lt;", Want: "&amp;lt;" },
		// Test 5: A full attribute breakout payload.
		{
			In: `"><img src=x onerror='alert(1)'>`,
			Want: "&quot;&gt;&lt;img src=x onerror=&#39;alert(1)&#39;&gt;",
		},
		// Test 6: Plain text is untouched.
		{ In: "deploy web tier", Want: "deploy web tier" },
		// Test 7: Null renders empty, not the word null.
		{ In: null, Want: "" },
		// Test 8: Undefined renders empty too.
		{ In: undefined, Want: "" },
		// Test 9: A number stringifies.
		{ In: 0, Want: "0" },
		// Test 10: A boolean stringifies.
		{ In: false, Want: "false" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.esc(tc.In), tc.Want, "test " + i);
	}
});

test("esc output is safe to interpolate: no raw specials survive", () => {
	const hostile = [
		"<><><>",
		`'';!--"<XSS>=&{()}`,
		'"onmouseover="alert(1)',
		"&amp;&lt;&gt;",
		"a<b>c&d\"e'f",
	];
	for (const input of hostile) {
		const out = app.esc(input);
		assert.doesNotMatch(out, /[<>"']/, "raw special in " + JSON.stringify(out));
		assert.doesNotMatch(out, /&(?!(amp|lt|gt|quot|#39);)/, "bare ampersand in " + JSON.stringify(out));
	}
});

test("wfDraftKey is the stable draft storage key", () => {
	assert.equal(app.wfDraftKey, "st_wf_draft");
});
