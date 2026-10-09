// Drives the browser assessment. The reader and the report come from the WebAssembly build of the
// same Go code the assess command runs, running in a worker, so this file chooses a file, hands over
// its bytes, and prints what comes back. It computes nothing about the estate itself: a number or a
// sentence derived here would be a second implementation of the grading, and it would be the one
// that disagrees.
(function () {
	"use strict";

	var drop = document.getElementById("drop");
	var dropMain = document.getElementById("drop-main");
	var dropSub = document.getElementById("drop-sub");
	var formatSel = document.getElementById("format");
	var result = document.getElementById("result");
	var headline = document.getElementById("headline");
	var figures = document.getElementById("figures");
	var reportText = document.getElementById("report-text");
	var saveBtn = document.getElementById("save");
	var ready = false;
	var busy = false;
	var lastReport = "";
	var lastName = "export";

	// maxBytes bounds what is read into memory. A real estate export is a few megabytes; something
	// far larger is a mistaken file, and saying so beats the tab dying with no explanation.
	var maxBytes = 64 * 1024 * 1024;

	// setDrop sets the drop zone's two lines. A file name goes in a span of its own, set in a font
	// already on the machine, because it is text from the visitor's disk: see .file-name.
	function setDrop(main, sub, name, after) {
		dropMain.textContent = main;
		if (name !== undefined) {
			var span = document.createElement("span");
			span.className = "file-name";
			span.textContent = name;
			dropMain.appendChild(span);
			dropMain.appendChild(document.createTextNode(after || ""));
		}
		if (sub !== undefined) { dropSub.textContent = sub; }
	}

	function show(kind, text) {
		result.classList.remove("hidden");
		headline.className = "headline " + kind;
		headline.textContent = text;
	}

	function failure(text) {
		figures.innerHTML = "";
		reportText.textContent = "";
		saveBtn.disabled = true;
		show("no", text);
	}

	function figure(value, label) {
		var d = document.createElement("div");
		d.className = "figure";
		var b = document.createElement("b");
		b.textContent = String(value);
		var s = document.createElement("span");
		s.textContent = label;
		d.appendChild(b);
		d.appendChild(s);
		return d;
	}

	// render puts the one sentence this page exists to produce above the detail. The sentence is
	// about the gap, not about the migration: somebody arrives wondering whether to move, and what
	// they can act on today is how much of their estate currently runs unasked. The module writes
	// it, beside the report it summarizes, so the two cannot disagree.
	function render(r) {
		lastReport = r.report;
		saveBtn.disabled = false;
		show(r.headlineKind === "gap" ? "gap" : "clear", r.headline);
		figures.innerHTML = "";
		figures.appendChild(figure(r.objects, "objects come across"));
		figures.appendChild(figure(r.leftOut, "do not come across"));
		figures.appendChild(figure(r.templates, "templates graded"));
		// Beside the grades, not under them: for an AWX estate the two grades below are zeros
		// almost every time, because a job template's playbook lives in a repository nothing has
		// fetched, and the reader has to see what those zeros rest on.
		figures.appendChild(figure(r.unread, "playbooks not read yet"));
		figures.appendChild(figure(r.irreversible, "cannot be undone"));
		figures.appendChild(figure(r.highRisk, "carry a destructive signal"));
		reportText.textContent = r.report;
	}

	var stays = "Nothing is uploaded. Your export never leaves this machine.";

	// The reader runs in a worker, so a large export does not freeze the page, and a file too large
	// for the memory the browser will give it costs that file rather than every later one. Each
	// request carries an id, and its answer comes back under the same one.
	var worker = null;
	var pending = {};
	var nextID = 1;

	function call(msg, transfer) {
		return new Promise(function (resolve) {
			msg.id = nextID++;
			pending[msg.id] = resolve;
			worker.postMessage(msg, transfer || []);
		});
	}

	function assess(name, bytes) {
		lastName = name.replace(/\.[^.]+$/, "") || "export";
		busy = true;
		return call({ kind: "assess", format: formatSel.value, bytes: bytes }, [bytes]).then(function (reply) {
			busy = false;
			var r = reply.result;
			if (!r || !r.ok) {
				failure(r && r.error ? r.error : "This file could not be read as a " + formatSel.value + " export.");
				return false;
			}
			render(r);
			return true;
		});
	}

	function take(file) {
		if (!ready || busy) { return; }
		if (file.size > maxBytes) {
			failure("That file is " + Math.round(file.size / 1048576) + "MB, which is larger than " +
				"this page will read. Run switchtender assess on it instead.");
			return;
		}
		setDrop("Reading ", "A large export can take a minute. " + stays, file.name, "...");
		// The bytes go to the reader as they are on disk, so it decodes them exactly as the command
		// does, byte order mark and UTF-16 included.
		var reader = new FileReader();
		reader.onerror = function () {
			setDrop("Drop an export here, or choose a file", stays);
			failure("That file could not be opened.");
		};
		reader.onload = function () {
			assess(file.name, reader.result).then(function (read) {
				if (read) {
					setDrop("Assessed ", stays, file.name, ". Drop another to replace it.");
				} else {
					setDrop("Drop another export, or choose a file", stays);
				}
			});
		};
		reader.readAsArrayBuffer(file);
	}

	function pick() {
		if (!ready) { return; }
		var input = document.createElement("input");
		input.type = "file";
		input.accept = ".json,.yml,.yaml,.txt,application/json,text/plain";
		input.addEventListener("change", function () {
			if (input.files && input.files[0]) { take(input.files[0]); }
		});
		input.click();
	}

	drop.addEventListener("click", pick);
	drop.addEventListener("keydown", function (e) {
		if (e.key === "Enter" || e.key === " ") { e.preventDefault(); pick(); }
	});
	["dragenter", "dragover"].forEach(function (name) {
		drop.addEventListener(name, function (e) { e.preventDefault(); drop.classList.add("over"); });
	});
	["dragleave", "drop"].forEach(function (name) {
		drop.addEventListener(name, function (e) { e.preventDefault(); drop.classList.remove("over"); });
	});
	drop.addEventListener("drop", function (e) {
		if (e.dataTransfer && e.dataTransfer.files && e.dataTransfer.files[0]) {
			take(e.dataTransfer.files[0]);
		}
	});

	// The report is the document somebody forwards to whoever owns the approval question, so it has
	// to leave this page as a file rather than as a selection to copy out of a scrolling box.
	saveBtn.addEventListener("click", function () {
		if (!lastReport) { return; }
		var blob = new Blob([lastReport], { type: "text/plain" });
		var url = URL.createObjectURL(blob);
		var a = document.createElement("a");
		a.href = url;
		a.download = lastName + "-switchtender-assessment.txt";
		document.body.appendChild(a);
		a.click();
		document.body.removeChild(a);
		URL.revokeObjectURL(url);
	});

	if (typeof WebAssembly !== "object" || typeof Worker !== "function") {
		setDrop("This browser cannot run the reader",
			"It needs WebAssembly and web workers. Run switchtender assess on the export instead.");
		return;
	}

	worker = new Worker("/assess/assess-worker.js");
	worker.onmessage = function (e) {
		var resolve = pending[e.data.id];
		delete pending[e.data.id];
		if (resolve) { resolve(e.data); }
	};
	// A worker that fails outright, rather than a reader inside it that ends, cannot be replaced
	// without loading the page's files again, so the page says so instead of doing it quietly.
	worker.onerror = function (e) {
		ready = false;
		busy = false;
		setDrop("The reader stopped", "Reload the page, or run switchtender assess on the export. " +
			(e && e.message ? e.message : ""));
		Object.keys(pending).forEach(function (id) {
			pending[id]({ result: { ok: false, error: "The reader stopped before it finished this file." } });
			delete pending[id];
		});
	};

	call({ kind: "formats" }).then(function (reply) {
		if (!reply.formats) {
			throw new Error(reply.result && reply.result.error ? reply.result.error : "no formats");
		}
		// The formats come from the module rather than from the HTML, so this page offers exactly
		// what the command reads and cannot drift into offering one it does not.
		var names = reply.formats;
		formatSel.innerHTML = "";
		names.forEach(function (n) {
			var o = document.createElement("option");
			o.value = n;
			o.textContent = n;
			formatSel.appendChild(o);
		});
		formatSel.value = names.indexOf("awx") >= 0 ? "awx" : names[0];
		formatSel.disabled = false;
		ready = true;
		setDrop("Drop an export here, or choose a file");
	}).catch(function (e) {
		setDrop("The reader could not be loaded", "Reload the page, or run switchtender assess on the export. " + e);
	});
})();
