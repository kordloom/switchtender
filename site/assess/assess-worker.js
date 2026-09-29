// Runs the assessment reader off the page's main thread.
//
// A large export takes the reader tens of seconds and a great deal of memory, and on the page's own
// thread that froze the page for the whole read. Worse, a file too large for the memory the browser
// would give it ended the Go program, and every later assessment on the page failed until a reload.
// Here the page stays responsive, and a Go program that ends is replaced from the module this worker
// already compiled, so a file too large for the reader costs that one file. Nothing is fetched to
// replace it: the module and this script were loaded with the page, which is what lets the page say
// that nothing leaves once it has loaded.
"use strict";

importScripts("/assess/wasm_exec.js");

// tooLarge is what a file that ended the reader is told.
var tooLarge = "This file needed more memory than the reader in this page could get, so it was " +
	"not read. Run switchtender assess on it instead, which reads it with no such limit.";

// compiled is the reader, fetched and compiled once when the worker starts.
var compiled = fetch("/assess/assess.wasm")
	.then(function (res) {
		if (!res.ok) { throw new Error("the reader could not be fetched: " + res.status); }
		return res.arrayBuffer();
	})
	.then(function (bytes) { return WebAssembly.compile(bytes); });

// current is the running Go program, or null when the next request has to start one.
var current = null;

// start runs a fresh Go program from the compiled reader and resolves once it has published the
// functions the page calls.
function start() {
	return compiled.then(function (module) {
		var go = new Go();
		return WebAssembly.instantiate(module, go.importObject).then(function (instance) {
			// run resolves only when the Go program ends, which it does on its own only when the
			// runtime cannot continue, most often for want of memory.
			go.run(instance);
			current = go;
			return go;
		});
	});
}

self.onmessage = function (e) {
	var msg = e.data;
	var ready = current && !current.exited ? Promise.resolve(current) : start();
	ready.then(function (go) {
		if (msg.kind === "formats") {
			self.postMessage({ id: msg.id, formats: self.switchtenderFormats() });
			return;
		}
		var result;
		try {
			result = self.switchtenderAssess(msg.format, new Uint8Array(msg.bytes));
		} catch (err) {
			result = null;
		}
		if (go.exited || !result) {
			current = null;
			self.postMessage({ id: msg.id, result: { ok: false, error: tooLarge } });
			return;
		}
		self.postMessage({ id: msg.id, result: result });
	}).catch(function (err) {
		self.postMessage({ id: msg.id, result: { ok: false, error: "The reader could not start: " + err } });
	});
};
