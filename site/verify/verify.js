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
	function render(report, name) {
		var ok = report.ok === true;
		var html = '<div class="verdict ' + (ok ? "ok" : "no") + '">' +
			(ok ? "VERIFIED" : "NOT VERIFIED") + "   " + esc(report.level || "") + "</div>";
		var rows = [];
		if (report.bundle_id) rows.push(["bundle", report.bundle_id + " from " + (report.producer || "")]);
		if (report.subject) rows.push(["subject", report.subject]);
		if (report.key_id) rows.push(["signature", (report.signature_ok ? "ok, key " : "failed, key ") + report.key_id]);
		if (report.fingerprint_match === true) rows.push(["pin", "matches the fingerprint you pinned"]);
		if (report.fingerprint_match === false) rows.push(["pin", "DOES NOT match the fingerprint you pinned"]);
		if (report.chain_present) {
			rows.push(["chain", (report.chain_profile || "") + ", " + (report.chain_mode || "") +
				", " + (report.claims_checked || 0) + " claims, head matched " + !!report.head_matched]);
		}
		if (report.anchors_matched || report.anchor_proofs_carried) {
			rows.push(["anchors", report.anchors_matched + " matched by coordinates, " +
				(report.anchor_proofs_carried || 0) + " proof(s) carried, " +
				(report.anchor_proofs_verified || 0) + " verified"]);
		}
		(report.anchor_attestations || []).forEach(function (a) { rows.push(["attested", a]); });
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
			try {
				render(JSON.parse(raw), f.name);
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
