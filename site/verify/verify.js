// Verify page logic. It loads the LoomSeal verifier, compiled from the same Go the command line
// runs to WebAssembly, and hands it the dropped file's exact bytes. Nothing here reaches the
// network beyond fetching the verifier itself: the bundle never leaves the machine, and a verdict
// is reached from the file alone. The script lives in its own file rather than inline so the site's
// content security policy can stay strict.
(function () {
	"use strict";
	var drop = document.getElementById("drop");
	var main = document.getElementById("drop-main");
	var sub = document.getElementById("drop-sub");
	var file = document.getElementById("file");
	var fp = document.getElementById("fp");
	var out = document.getElementById("out");
	var ready = false;

	// pad right-justifies a label column so the verdict lines read like the command line's.
	// pad widens a row label to width, so every value starts in one column. The width comes from the
	// longest label on the page, since a fixed one ran "timestamped" into its value.
	function pad(s, width) { while (s.length < width) s += " "; return s; }

	// esc escapes text for safe insertion, since a bundle is untrusted input.
	function esc(s) {
		return String(s).replace(/[&<>"']/g, function (c) {
			return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
		});
	}

	// render turns the verifier's report into the verdict block and a details table. The wording
	// mirrors the command line so the two never drift in a reader's memory.
	function render(report, name, pinned, note) {
		var ok = report.ok === true;
		// A signature proves a bundle was signed. It does not prove who signed it, because any key
		// signs its own bundle. Without a pin this page can say the chain is intact and cannot say
		// whose chain it is, and the visitor who leaves the box blank is exactly the one who will not
		// know that. So an unpinned pass is its own verdict rather than a quiet VERIFIED.
		var unpinned = ok && !pinned;
		// SwitchTender never emits head attestations. Its own bundle builder refuses to write them
		// and its own verify command refuses to read them, by name. So one appearing in a receipt
		// means a holder attached it after the fact, and this page cannot check who: the browser
		// verifier takes a producer fingerprint and has no way to pin a counter-signer, so anyone
		// can sign a rider under any role they like and it verifies.
		var riders = ok && (report.head_attestors || []).length > 0;
		var soft = unpinned || riders;
		var cls = ok ? (soft ? "part" : "ok") : "no";
		var word = ok ? (riders ? "INTACT, WITH AN UNVERIFIABLE RIDER"
			: (unpinned ? "INTACT, BUT UNIDENTIFIED" : "VERIFIED")) : "NOT VERIFIED";
		// The level names what a passing bundle achieved. On a failure it is "not verified", which
		// only repeated the verdict beside it, so it is shown for a pass alone.
		var level = ok && report.level ? "   " + esc(report.level) : "";
		var html = note ? '<p class="tampered">' + esc(note) + "</p>" : "";
		html += '<div class="verdict ' + cls + '">' + word + level + "</div>";
		if (riders) {
			html += '<p class="unpinned">This receipt carries ' + (report.head_attestors || []).length +
				' counter-signature(s). SwitchTender never adds them, so somebody attached these after ' +
				'the receipt was produced. This page cannot check who: it can pin the producing key and ' +
				'has no way to pin a counter-signer, and any key signs under any role it chooses. ' +
				'Treat them as unverified until you have checked them with <code>loomseal verify ' +
				'--attestor</code> against a key you obtained yourself.</p>';
		}
		if (unpinned) {
			html += '<p class="unpinned">Nothing here was altered after it was signed. Who signed it is ' +
				'unchecked, because no fingerprint was pinned, and any key signs its own bundle. ' +
				'Pin the fingerprint the producing install publishes at ' +
				'<code>/.well-known/loomseal.json</code> and run it again.</p>';
		}
		var rows = [];
		if (report.bundle_id) rows.push(["bundle", report.bundle_id + " from " + (report.producer || "")]);
		if (report.subject) rows.push(["subject", report.subject]);
		if (report.key_id) rows.push(["signature", (report.signature_ok ? "ok, key " : "failed, key ") + report.key_id]);
		if (report.fingerprint_match === true) {
			rows.push(["pin", "matches the fingerprint you pinned, so this is that install's key"]);
		} else if (report.fingerprint_match === false) {
			rows.push(["pin", "DOES NOT match the fingerprint you pinned"]);
		} else if (pinned) {
			// The verifier compares the pin only once the signature holds, so a bundle that failed
			// before that carries no comparison. Reading that as NONE told a visitor who had pinned a
			// fingerprint that they had not.
			rows.push(["pin", "not compared, because verification stopped before it reached the key"]);
		} else if (ok) {
			rows.push(["pin", "NONE, so this says the bundle was signed, not who signed it"]);
		} else {
			rows.push(["pin", "NONE"]);
		}
		if (report.chain_present) {
			rows.push(["chain", (report.chain_profile || "") + ", " + (report.chain_mode || "") +
				", " + (report.claims_checked || 0) + " claims, head matched " + !!report.head_matched]);
		}
		if (report.anchors_matched || report.anchor_proofs_carried) {
			rows.push(["anchors", report.anchors_matched + " matched by coordinates, " +
				(report.anchor_proofs_carried || 0) + " proof(s) carried, " +
				(report.anchor_proofs_verified || 0) + " verified"]);
		}
		(report.anchor_attestations || []).forEach(function (a) { rows.push(["timestamped", a]); });
		(report.head_attestors || []).forEach(function (a) { rows.push(["rider", a]); });
		if ((report.head_attestors || []).length > 0) {
			rows.push(["attestors", report.attestors_pinned === true
				? "checked against the keys you pinned"
				: "NOT CHECKED, and this page has no way to check them"]);
		}
		if (report.anchored_through_seq) {
			var line = "through seq " + report.anchored_through_seq;
			if (report.unanchored_claims) {
				line += ", " + report.unanchored_claims + " claim(s) after it";
				if (report.unanchored_window) line += " spanning " + report.unanchored_window;
			}
			rows.push(["anchored", line]);
		}
		(report.problems || []).forEach(function (p) { rows.push(["problem", p]); });

		var width = rows.reduce(function (w, r) { return Math.max(w, r[0].length); }, 0) + 2;
		var body = rows.map(function (r) { return esc(pad(r[0], width)) + esc(String(r[1])); }).join("\n");
		html += "<pre><code>" + body + "</code></pre>";
		html += '<details><summary>Full report</summary><pre><code>' +
			esc(JSON.stringify(report, null, 2)) + "</code></pre></details>";
		out.innerHTML = html;
		if (name) sub.textContent = "Checked " + name;
	}

	// check reads the file as bytes and runs it through the verifier. It is read as an ArrayBuffer,
	// never as text, because a signature covers the canonical bytes and letting the browser
	// re-encode the file on the way in could change the verdict.
	function check(f) {
		if (!ready) return;
		var reader = new FileReader();
		reader.onload = function () { run(new Uint8Array(reader.result), f.name); };
		reader.onerror = function () {
			out.innerHTML = '<div class="verdict no">NOT VERIFIED   the file could not be read</div>';
		};
		reader.readAsArrayBuffer(f);
	}

	// verifyBytes runs the verifier on bytes and returns its report. It throws when the verifier's
	// answer is not a report, which the callers show as an unreadable report.
	function verifyBytes(bytes, pin) {
		return JSON.parse(pin ? loomsealVerify(bytes, pin) : loomsealVerify(bytes));
	}

	// run verifies bytes, shows the verdict, and, when the bundle passes, offers to change one digit
	// and check it again.
	function run(bytes, name) {
		var pin = fp.value.trim();
		try {
			var report = verifyBytes(bytes, pin);
			render(report, name, pin !== "");
			if (report.ok === true) offerTamper(bytes, name);
		} catch (e) {
			out.innerHTML = '<div class="verdict no">NOT VERIFIED   report could not be read</div>';
		}
	}

	// button builds a plain button for the result area. Handlers are attached here, never inline,
	// so the page's content security policy can stay strict.
	function button(id, label, onclick) {
		var b = document.createElement("button");
		b.type = "button";
		b.id = id;
		b.className = "tamper-btn";
		b.textContent = label;
		b.addEventListener("click", onclick);
		return b;
	}

	// offerTamper adds the control that makes the claim checkable in ten seconds: the visitor changes
	// one digit of the file they just verified and watches the verdict fail.
	function offerTamper(bytes, name) {
		var box = document.createElement("div");
		box.className = "tamper";
		var text = document.createElement("p");
		text.textContent = "Now try to cheat. Change one digit in this file and check it again.";
		box.appendChild(text);
		box.appendChild(button("tamper", "Change one digit and check again", function () {
			tamper(bytes, name);
		}));
		out.appendChild(box);
	}

	// changeOneDigit returns a copy of bytes with one digit replaced by a different one, along with
	// the report the verifier gives the copy. It starts in the middle of the file and works outward,
	// and it keeps the first change the verifier rejects. A signed bundle rejects a change to anything
	// it signed, so the first digit does it, and the search only matters for a digit that sits outside
	// what the signature covers. The replacement is never zero, so a number never gains a leading
	// zero. A file with no digit gets its middle byte changed instead.
	function changeOneDigit(bytes, pin) {
		var mid = bytes.length >> 1;
		var first = null;
		var tried = 0;
		function attempt(at, to) {
			var copy = bytes.slice();
			var was = copy[at];
			copy[at] = to;
			var change = { bytes: copy, at: at, from: was, to: to, report: verifyBytes(copy, pin) };
			if (!first) first = change;
			return change.report.ok === false ? change : null;
		}
		for (var d = 0; d < bytes.length && tried < 64; d++) {
			var spots = d === 0 ? [mid] : [mid + d, mid - d];
			for (var i = 0; i < spots.length && tried < 64; i++) {
				var at = spots[i];
				if (at < 0 || at >= bytes.length || bytes[at] < 0x30 || bytes[at] > 0x39) continue;
				tried++;
				var rejected = attempt(at, 0x31 + ((bytes[at] - 0x30) % 9));
				if (rejected) return rejected;
			}
		}
		var flipped = bytes.length ? attempt(mid, bytes[mid] ^ 0x01) : null;
		return flipped || first;
	}

	// tamper shows the verdict for the file with one digit changed, says exactly what changed, and
	// offers the original back.
	function tamper(bytes, name) {
		var pin = fp.value.trim();
		try {
			var change = changeOneDigit(bytes, pin);
			if (!change) return;
			var shown = String.fromCharCode(change.from) + '" to "' + String.fromCharCode(change.to);
			render(change.report, name, pin !== "",
				'Changed byte ' + change.at + ' from "' + shown + '". Nothing else in the file was touched.');
			var box = document.createElement("div");
			box.className = "tamper";
			box.appendChild(button("restore", "Check the original again", function () {
				run(bytes, name);
			}));
			out.appendChild(box);
		} catch (e) {
			out.innerHTML = '<div class="verdict no">NOT VERIFIED   report could not be read</div>';
		}
	}

	drop.addEventListener("click", function () { if (ready) file.click(); });
	drop.addEventListener("keydown", function (e) {
		if (e.key === "Enter" || e.key === " ") { e.preventDefault(); if (ready) file.click(); }
	});
	file.addEventListener("change", function () { if (file.files[0]) check(file.files[0]); });
	["dragenter", "dragover"].forEach(function (t) {
		drop.addEventListener(t, function (e) { e.preventDefault(); drop.classList.add("over"); });
	});
	["dragleave", "drop"].forEach(function (t) {
		drop.addEventListener(t, function (e) { e.preventDefault(); drop.classList.remove("over"); });
	});
	drop.addEventListener("drop", function (e) {
		if (e.dataTransfer.files[0]) check(e.dataTransfer.files[0]);
	});

	// SAMPLE_PIN is the key id the public demo install publishes at /.well-known/loomseal.json, the
	// key that signed sample-bundle.json. The sample is checked against it, so its verdict names the
	// install that produced it. The pin must match the sample's signer, and a test holds them together.
	var SAMPLE_PIN = "sha256:47a80df250cfc939fe3c405a5727242caae91b5c9786812afe569fafee4b5dca";
	var sample = document.getElementById("sample");
	sample.addEventListener("click", function () {
		if (!ready) return;
		fetch("/verify/sample-bundle.json")
			.then(function (res) {
				if (!res.ok) throw new Error("sample status " + res.status);
				return res.arrayBuffer();
			})
			.then(function (buf) {
				fp.value = SAMPLE_PIN;
				run(new Uint8Array(buf), "the public demo's sample bundle");
			})
			.catch(function () {
				out.innerHTML = '<div class="verdict no">NOT VERIFIED   the sample could not be loaded</div>';
			});
	});

	if (!WebAssembly || !WebAssembly.instantiateStreaming) {
		main.textContent = "This browser cannot run the verifier";
		sub.textContent = "Use loomseal verify from the command line instead.";
		return;
	}
	var go = new Go();
	WebAssembly.instantiateStreaming(fetch("/verify/loomseal.wasm"), go.importObject)
		.then(function (res) {
			go.run(res.instance);
			ready = true;
			sample.disabled = false;
			main.textContent = "Drop a bundle here, or click to choose one";
		})
		.catch(function () {
			main.textContent = "The verifier failed to load";
			sub.textContent = "Reload the page, or use loomseal verify from the command line.";
		});
})();
