// Tests for what switchtender.com/verify tells a visitor, which is the page carrying this product's
// central claim: that you can check a receipt without trusting the vendor.
//
// The defect these exist for: a signature proves a bundle was signed and does not prove who signed
// it, because any key signs its own bundle. The page has always had a fingerprint input, and leaving
// it blank rendered no pin row at all, so the verdict read VERIFIED and nothing said the identity
// check had not happened. A visitor who pastes a receipt, fills in nothing, and reads one word ends
// up believing something stronger than what was checked.
//
// This is the same shape as the three findings in loomseal on 2026-09-10 and as the approval-ordering
// defect in the audit chain: the cryptography held, and what failed was disclosure of scope. So these
// tests assert on what the page SAYS, not on whether the verifier is correct.
//
// site/ has no other test coverage, and this file mounts the real page's markup rather than inventing
// element ids, so a test cannot pass by agreeing with itself.

import { test } from "node:test";
import assert from "node:assert/strict";
import { readFileSync } from "node:fs";

import { createDocument, fire, parseHTML } from "./dom.mjs";

const PAGE = new URL("../../../../site/verify/index.html", import.meta.url).pathname;
const SCRIPT = new URL("../../../../site/verify/verify.js", import.meta.url).pathname;

// mount parses the real verify page into a document, stubs the WebAssembly verifier with a canned
// report, and runs the page's own script against it. Returns a function that drops a file and gives
// back the rendered output element.
async function mount(report) {
	const document = createDocument();
	const nodes = parseHTML(readFileSync(PAGE, "utf8"), document);
	const html = nodes.find((n) => n.nodeType === 1 && n.tagName === "HTML");
	document.setDocumentElement(html || nodes[0]);

	const calls = [];
	const scope = {
		document,
		// The page fetches the wasm and instantiates it. Neither is what is under test here, so both
		// resolve immediately and the verifier is the stub below.
		fetch: () => Promise.resolve({}),
		WebAssembly: { instantiateStreaming: () => Promise.resolve({ instance: {} }) },
		Go: function () { this.importObject = {}; this.run = () => {}; },
		loomsealVerify: (bytes, pin) => {
			calls.push(pin);
			return JSON.stringify(pin === undefined ? report : { ...report, fingerprint_match: pin === "sha256:good" });
		},
		FileReader: class {
			readAsArrayBuffer() { this.result = new Uint8Array([1, 2, 3]); this.onload(); }
		},
		console,
	};

	const source = readFileSync(SCRIPT, "utf8");
	const fn = new Function(...Object.keys(scope), source);
	fn(...Object.values(scope));
	// The page arms its drop handler in the wasm promise's continuation. Let those run.
	await Promise.resolve();
	await Promise.resolve();
	await Promise.resolve();

	return {
		document,
		calls,
		drop(pin) {
			if (pin !== undefined) document.getElementById("fp").value = pin;
			const file = document.getElementById("file");
			file.files = [{ name: "receipt.json" }];
			fire(file, "change");
			return document.getElementById("out");
		},
	};
}

// A report from a bundle whose chain and signature are entirely sound. Whether the signer is the
// install the reader expects is a separate question, and the whole point of these tests.
const SOUND = {
	ok: true,
	level: "full",
	bundle_id: "lsb_6a3fb404f13c",
	producer: "switchtender",
	subject: "run_0607a1fc65e9c724",
	signature_ok: true,
	key_id: "sha256:47a80df250cfc939",
	chain_present: true,
	chain_profile: "switchtender-audit-v1",
	claims_checked: 3,
	head_matched: true,
};

test("an unpinned pass does not read as a clean verification", async () => {
	const page = await mount(SOUND);
	const out = page.drop();
	const text = out.textContent;

	assert.doesNotMatch(text, /^\s*VERIFIED/,
		"a bundle verified with no fingerprint rendered a clean VERIFIED, so a visitor who left the " +
		"box blank is told the signer was checked when it was not: " + text.slice(0, 120));
	assert.match(text, /UNIDENTIFIED/,
		"the verdict does not say the signer is unidentified: " + text.slice(0, 120));
	assert.match(text, /not who signed it/,
		"nothing tells the reader what the missing pin costs them");
	assert.match(text, /well-known\/loomseal\.json/,
		"the reader is told the check is missing but not where to get what fixes it");
});

test("the pin row is present even when nothing was pinned", async () => {
	const page = await mount(SOUND);
	const text = page.drop().textContent;

	// The row used to be rendered only when a fingerprint was supplied, so its absence was the
	// disclosure, and an absence discloses nothing.
	assert.match(text, /pin\s+NONE/,
		"no pin row was rendered for an unpinned run, so the omission is silent: " + text.slice(0, 200));
});

test("a matching pin reads as a full verification", async () => {
	const page = await mount(SOUND);
	const text = page.drop("sha256:good").textContent;

	assert.match(text, /VERIFIED/, "a correctly pinned bundle did not read as verified");
	assert.doesNotMatch(text, /UNIDENTIFIED/,
		"a pinned bundle still reads as unidentified: " + text.slice(0, 120));
	assert.match(text, /matches the fingerprint you pinned/,
		"the pin row does not confirm the match");
	assert.deepEqual(page.calls, ["sha256:good"],
		"the fingerprint the visitor typed was not passed to the verifier");
});

test("a mismatched pin is not softened", async () => {
	const page = await mount(SOUND);
	const text = page.drop("sha256:wrong").textContent;

	assert.match(text, /DOES NOT match/,
		"a bundle signed by a key other than the pinned one did not say so: " + text.slice(0, 160));
});

test("a failed verification still reads as failed", async () => {
	const page = await mount({ ...SOUND, ok: false, signature_ok: false, problems: ["signature does not verify"] });
	const text = page.drop().textContent;

	// The unpinned wording must never soften a genuine failure into a middle state.
	assert.match(text, /NOT VERIFIED/, "a broken bundle did not read as failed: " + text.slice(0, 120));
	assert.doesNotMatch(text, /UNIDENTIFIED/,
		"a failing bundle was rendered as merely unidentified, which reads as milder than it is");
	// The verdict word is not the only thing that can lie. The unpinned paragraph asserts the file
	// was not altered, which on a failed signature is the opposite of what happened, and a reader
	// who gets both sentences has been told two contradictory things about the same file.
	assert.doesNotMatch(text, /Nothing here was altered/,
		"a bundle that failed verification was told nothing had been altered: " + text.slice(0, 200));
});

test("the fingerprint input does not present itself as optional", async () => {
	const page = await mount(SOUND);
	const placeholder = page.document.getElementById("fp").getAttribute("placeholder") || "";

	assert.doesNotMatch(placeholder, /optional/i,
		"the field that decides whether identity is checked is labelled optional: " + placeholder);
});
