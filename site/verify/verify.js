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
	function pad(s) { return (s + "           ").slice(0, 11); }

	// esc escapes text for safe insertion, since a bundle is untrusted input.
	function esc(s) {
		return String(s).replace(/[&<>"']/g, function (c) {
			return { "&": "&amp;", "<": "&lt;", ">": "&gt;", '"': "&quot;", "'": "&#39;" }[c];
		});
	}

	// render turns the verifier's report into the verdict block and a details table. The wording
	// mirrors the command line so the two never drift in a reader's memory.
	function render(report, name, pinned) {
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
		var html = '<div class="verdict ' + cls + '">' + word + "   " + esc(report.level || "") + "</div>";
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
		} else {
			rows.push(["pin", "NONE, so this says the bundle was signed, not who signed it"]);
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

		var body = rows.map(function (r) { return esc(pad(r[0])) + esc(String(r[1])); }).join("\n");
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
		reader.onload = function () {
			var bytes = new Uint8Array(reader.result);
			var pin = fp.value.trim();
			var raw = pin ? loomsealVerify(bytes, pin) : loomsealVerify(bytes);
			var pinned = pin !== "";
			try {
				render(JSON.parse(raw), f.name, pinned);
			} catch (e) {
				out.innerHTML = '<div class="verdict no">NOT VERIFIED   report could not be read</div>';
			}
		};
		reader.onerror = function () {
			out.innerHTML = '<div class="verdict no">NOT VERIFIED   the file could not be read</div>';
		};
		reader.readAsArrayBuffer(f);
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
			main.textContent = "Drop a bundle here, or click to choose one";
		})
		.catch(function () {
			main.textContent = "The verifier failed to load";
			sub.textContent = "Reload the page, or use loomseal verify from the command line.";
		});
})();
