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
async function mount(report, opts = {}) {
	const document = createDocument();
	const nodes = parseHTML(readFileSync(PAGE, "utf8"), document);
	const html = nodes.find((n) => n.nodeType === 1 && n.tagName === "HTML");
	document.setDocumentElement(html || nodes[0]);

	const calls = [];
	const seen = [];
	const scope = {
		document,
		// The page fetches the wasm and instantiates it. Neither is what is under test here, so both
		// resolve immediately and the verifier is the stub below.
		fetch: () => Promise.resolve({}),
		WebAssembly: { instantiateStreaming: () => Promise.resolve({ instance: {} }) },
		Go: function () { this.importObject = {}; this.run = () => {}; },
		loomsealVerify: (bytes, pin) => {
			calls.push(pin);
			seen.push(bytes.slice());
			if (opts.verify) return JSON.stringify(opts.verify(bytes, pin));
			// The real verifier compares a pin only once the signature holds, so a report for a bundle
			// that failed earlier carries no comparison even when a pin was given.
			if (pin === undefined || opts.stopsBeforeKey) return JSON.stringify(report);
			return JSON.stringify({ ...report, fingerprint_match: pin === "sha256:good" });
		},
		FileReader: class {
			readAsArrayBuffer() {
				this.result = opts.bytes ? opts.bytes.slice() : new Uint8Array([1, 2, 3]);
				this.onload();
			}
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
		seen,
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

test("a counter-signature nobody can check is not rendered as endorsement", async () => {
	// LoomSeal's own audit found that attestations sit outside the producer signature, so any holder
	// of a bundle can attach one signed by a key minted for the purpose, under any role they choose,
	// and it verifies. The browser verifier takes a producer fingerprint and has no way to pin a
	// counter-signer, so this page can never establish who vouched. SwitchTender also never emits
	// these, which means one appearing is itself evidence somebody added it.
	const page = await mount({ ...SOUND, head_attestors: ["independent-auditor (sha256:deadbeef)"] });
	const text = page.drop("sha256:good").textContent;

	assert.doesNotMatch(text, /^\s*VERIFIED/,
		"a receipt carrying an unverifiable counter-signature read as a clean verification: " +
		text.slice(0, 140));
	assert.match(text, /RIDER/,
		"the verdict does not mention the rider: " + text.slice(0, 140));
	assert.match(text, /SwitchTender never adds them/,
		"the reader is not told that a SwitchTender receipt should never carry one");
	assert.match(text, /NOT CHECKED/,
		"the attestor row does not say the counter-signers went unchecked");
});

test("a receipt with no riders says nothing about them", async () => {
	// The disclosure must not fire on every receipt, or it becomes noise a reader learns to skip.
	const page = await mount(SOUND);
	const text = page.drop("sha256:good").textContent;

	assert.doesNotMatch(text, /RIDER|rider|attestors/,
		"a receipt carrying no counter-signatures still mentioned them: " + text.slice(0, 200));
	assert.match(text, /VERIFIED/, "a clean pinned receipt no longer reads as verified");
});

test("every count on the page takes the noun in the number it needs", async () => {
	// The rider paragraph, the anchors row and the anchored row each hedged with "(s)", so one
	// counter-signature, one proof or one claim read as though the page could not count it.
	const cases = [
		{ n: 1, rider: /carries 1 counter-signature\. .*attached this one .*Treat it as unverified/,
			proof: /1 proof carried/, claim: /1 claim after it/ },
		{ n: 2, rider: /carries 2 counter-signatures\. .*attached these .*Treat them as unverified/,
			proof: /2 proofs carried/, claim: /2 claims after it/ },
	];
	for (const c of cases) {
		const page = await mount({
			...SOUND,
			head_attestors: Array.from({ length: c.n }, (_, i) => "auditor-" + i + " (sha256:beef)"),
			anchors_matched: 1,
			anchor_proofs_carried: c.n,
			anchor_proofs_verified: c.n,
			anchored_through_seq: 7,
			unanchored_claims: c.n,
		});
		const text = page.drop("sha256:good").textContent;

		assert.match(text, c.rider, "the rider paragraph miscounts " + c.n + ": " + text.slice(0, 400));
		assert.match(text, c.proof, "the anchors row miscounts " + c.n + ": " + text);
		assert.match(text, c.claim, "the anchored row miscounts " + c.n + ": " + text);
		assert.doesNotMatch(text, /\(s\)/, "a count still hedges its plural: " + text);
	}
});

test("a pin that was given but never reached is not reported as no pin", async () => {
	// The verifier stops at a failed signature before it compares the pin. The page rendered that
	// as "pin NONE", which told a visitor who had pinned a fingerprint that they had not, and left
	// them wondering whether the box had been read at all.
	const failed = { ...SOUND, ok: false, signature_ok: false, level: "not verified",
		problems: ["signature does not verify over the canonical bundle"] };
	const page = await mount(failed, { stopsBeforeKey: true });
	const text = page.drop("sha256:good").textContent;

	assert.doesNotMatch(text, /pin\s+NONE/,
		"a pinned run that stopped at the signature read as unpinned: " + text.slice(0, 240));
	assert.match(text, /not compared/,
		"the pin row does not say the pin was never compared: " + text.slice(0, 240));
	assert.deepEqual(page.calls, ["sha256:good"],
		"the fingerprint the visitor typed was not passed to the verifier");
});

test("a failed verdict does not repeat itself", async () => {
	// A failure's level is "not verified", and printing it beside the verdict read "NOT VERIFIED
	// not verified", which looks like a page that does not know what it is saying.
	const page = await mount({ ok: false, level: "not verified", problems: ["parse: unexpected EOF"] });
	const out = page.drop();
	const verdict = out.querySelector(".verdict").textContent;

	assert.doesNotMatch(verdict, /not verified/,
		"the failed verdict repeats itself: " + verdict);
	assert.doesNotMatch(out.textContent, /so this says the bundle was signed/,
		"a file that did not verify was described as signed: " + out.textContent.slice(0, 240));
});

test("a passing verdict still names the level it reached", async () => {
	const page = await mount(SOUND);
	const verdict = page.drop("sha256:good").querySelector(".verdict").textContent;

	assert.match(verdict, /VERIFIED\s+full/, "the level a sound bundle reached is gone: " + verdict);
});

test("every row label is set apart from its value", async () => {
	// Labels were padded to a fixed eleven characters, so "timestamped", which is eleven long, ran
	// straight into its value: "timestamped2026-09-29T02:53:28Z".
	const page = await mount({
		...SOUND,
		anchor_attestations: ["2026-09-29T02:53:28Z by freetsa.org"],
		head_attestors: ["sha256:aaaa"],
	});
	const text = page.drop("sha256:good").textContent;
	for (const label of ["bundle", "signature", "pin", "chain", "timestamped", "rider", "attestors"]) {
		assert.match(text, new RegExp("\\b" + label + " {2,}\\S"),
			label + " is not followed by a gap before its value: " + text.slice(0, 400));
	}
});

// BUNDLE stands in for a bundle: JSON with digits through the middle, the way a hex digest has them.
const BUNDLE = new TextEncoder().encode(
	'{"id":"lsb_6a3fb404f13c","chain":[{"seq":1,"hash":"4f1a9c3e07d2"},{"seq":2,"hash":"b81d6e5a2c90"}]}');

// rejectsChange is a verifier stub that passes the original bytes and fails any other bytes, the way
// a signed bundle does: the signature covers what the file says, so a changed file is not that file.
function rejectsChange(original, report, except) {
	return (bytes, pin) => {
		let at = -1;
		for (let i = 0; i < bytes.length; i++) if (bytes[i] !== original[i]) { at = i; break; }
		const pass = at === -1 || at === except;
		const answer = pass ? report : { ...report, ok: false, signature_ok: false, level: "not verified",
			problems: ["signature does not verify over the canonical bundle"] };
		return pin ? { ...answer, fingerprint_match: pin === "sha256:good" } : answer;
	};
}

// differences lists every offset where two byte arrays disagree.
function differences(a, b) {
	const out = [];
	for (let i = 0; i < Math.max(a.length, b.length); i++) if (a[i] !== b[i]) out.push(i);
	return out;
}

test("a verified bundle offers to change one digit, and a failed one does not", async () => {
	const good = await mount(SOUND, { bytes: BUNDLE, verify: rejectsChange(BUNDLE, SOUND) });
	const text = good.drop("sha256:good").textContent;
	assert.ok(good.document.getElementById("tamper"), "a verified bundle offers no way to change it");
	assert.match(text, /Now try to cheat/, "the offer does not say what it is for: " + text.slice(-200));

	const bad = { ...SOUND, ok: false, signature_ok: false, problems: ["signature does not verify"] };
	const failed = await mount(bad, { bytes: BUNDLE });
	failed.drop();
	assert.equal(failed.document.getElementById("tamper"), null,
		"a bundle that already failed was offered a change, which proves nothing");
});

test("changing one digit fails the verdict and says what changed", async () => {
	const page = await mount(SOUND, { bytes: BUNDLE, verify: rejectsChange(BUNDLE, SOUND) });
	page.drop("sha256:good");
	fire(page.document.getElementById("tamper"), "click");
	const text = page.document.getElementById("out").textContent;

	assert.match(text, /NOT VERIFIED/, "a changed bundle did not read as failed: " + text.slice(0, 160));
	assert.doesNotMatch(text, /^\s*VERIFIED/, "a changed bundle still reads as verified");
	const said = text.match(/Changed byte (\d+) from "(\d)" to "(\d)"/);
	assert.ok(said, "the page does not say which byte changed and to what: " + text.slice(0, 200));

	// What the page says it did is exactly what it did: the verifier saw the original first and then
	// a copy that differs in the one byte named, from the digit named to the digit named.
	assert.deepEqual(differences(page.seen[0], BUNDLE), [], "the first check was not the original bytes");
	const changed = page.seen[page.seen.length - 1];
	assert.deepEqual(differences(changed, BUNDLE), [Number(said[1])],
		"the copy differs from the original somewhere other than the byte the page named");
	assert.equal(String.fromCharCode(BUNDLE[said[1]]), said[2], "the page named the wrong original digit");
	assert.equal(String.fromCharCode(changed[said[1]]), said[3], "the page named the wrong new digit");
});

test("a changed digit is never zero, and never the digit it replaces", async () => {
	for (const [digit, want] of [["0", "1"], ["1", "2"], ["8", "9"], ["9", "1"]]) {
		const bytes = new TextEncoder().encode('{"a":"' + digit + '"}');
		const page = await mount(SOUND, { bytes, verify: rejectsChange(bytes, SOUND) });
		page.drop("sha256:good");
		fire(page.document.getElementById("tamper"), "click");
		const text = page.document.getElementById("out").textContent;
		assert.ok(text.includes('from "' + digit + '" to "' + want + '"'),
			digit + " should become " + want + ": " + text.slice(0, 160));
	}
});

test("the search moves past a digit the verifier still accepts", async () => {
	// A digit outside what the signature covers would pass unchanged, and a demonstration that
	// changed only that digit would show nothing. The page moves to the next digit, and says which.
	const bytes = new TextEncoder().encode("x".repeat(10) + "7" + "x".repeat(8) + "5" + "x".repeat(10));
	const page = await mount(SOUND, { bytes, verify: rejectsChange(bytes, SOUND, 19) });
	page.drop("sha256:good");
	fire(page.document.getElementById("tamper"), "click");
	const text = page.document.getElementById("out").textContent;

	assert.match(text, /NOT VERIFIED/, "the page showed a pass for a changed file: " + text.slice(0, 160));
	assert.match(text, /Changed byte 10 from "7"/,
		"the page did not move past the digit the verifier accepted: " + text.slice(0, 200));
});

test("a file with no digit has its middle byte changed instead", async () => {
	const bytes = new TextEncoder().encode("x".repeat(10));
	const page = await mount(SOUND, { bytes, verify: rejectsChange(bytes, SOUND) });
	page.drop("sha256:good");
	fire(page.document.getElementById("tamper"), "click");
	const text = page.document.getElementById("out").textContent;

	assert.match(text, /Changed byte 5 from "x" to "y"/, "the middle byte was not the one changed: " + text.slice(0, 200));
	assert.match(text, /NOT VERIFIED/);
});

test("the original can be checked again after the change", async () => {
	const page = await mount(SOUND, { bytes: BUNDLE, verify: rejectsChange(BUNDLE, SOUND) });
	page.drop("sha256:good");
	fire(page.document.getElementById("tamper"), "click");
	assert.ok(page.document.getElementById("restore"), "no way back to the original after the change");
	fire(page.document.getElementById("restore"), "click");
	const text = page.document.getElementById("out").textContent;

	assert.match(text, /VERIFIED/, "the original did not verify again: " + text.slice(0, 160));
	assert.doesNotMatch(text, /NOT VERIFIED|Changed byte/, "the change was still showing after going back");
	assert.ok(page.document.getElementById("tamper"), "the offer did not come back with the original");
});
