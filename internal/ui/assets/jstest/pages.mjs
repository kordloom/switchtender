// pages.mjs mounts the DOM skeleton of a real page into a sandbox. The skeleton is parsed from the
// template the server ships, in ../../templates, rather than transcribed here: a handler reaches for
// ids by name, and a test that invents its own ids proves only that the test and the handler agree
// with each other.

import { readFileSync } from "node:fs";

import { parseHTML } from "./dom.mjs";
import { installEventSource, installFetch } from "./net.mjs";
import { ALL_PARTS, clockOf, loadParts, sandboxOf } from "./loader.mjs";

// PAGE_PATHS is where each page is served, so location matches the template being mounted.
const PAGE_PATHS = {
	runs: () => "/ui/runs",
	detail: (vars) => "/ui/runs/" + (vars.RunID || ""),
	overview: () => "/ui/",
};

// mountPage parses a template into the sandbox document and points location at that page. Values in
// vars fill the template's fields: a {{.Field}} becomes its value, an {{if .Field}} block is kept
// only when the value is truthy, and a {{range .Field}} block is repeated once per element with
// {{.}} standing for it. Returns the mounted document.
export function mountPage(app, name, options) {
	const opts = options || {};
	const vars = opts.vars || {};
	const sandbox = sandboxOf(app);
	const markup = renderTemplate(readTemplate(name), vars);
	const nodes = parseHTML(markup, sandbox.document);
	const html = nodes.find((n) => n.nodeType === 1 && n.tagName === "HTML");
	if (!html) throw new Error("jstest: template " + name + " has no html element");
	sandbox.document.setDocumentElement(html);
	const path = opts.path || (PAGE_PATHS[name] ? PAGE_PATHS[name](vars) : "/ui/" + name);
	sandbox.location.pathname = path;
	sandbox.location.search = opts.search || "";
	sandbox.location.href = sandbox.location.origin + path + (opts.search || "");
	// The mount itself is not a navigation the page performed, so it is not left in the record.
	sandbox.location.navigations.length = 0;
	return sandbox.document;
}

// loadPage is the whole setup for one page in a single call: the script parts evaluated, the
// template mounted, and the network and stream recorders installed. It returns the pieces a test
// drives the page through.
export function loadPage(name, options) {
	const opts = options || {};
	const app = loadParts(opts.parts || ALL_PARTS);
	const document = mountPage(app, name, opts);
	const net = installFetch(app, opts.routes || {}, opts);
	const streams = installEventSource(app);
	return { app, document, net, streams, clock: clockOf(app) };
}

// readTemplate reads one page template from the server's template directory.
function readTemplate(name) {
	return readFileSync(new URL("../../templates/" + name + ".html", import.meta.url), "utf8");
}

// renderTemplate resolves the template actions the page templates use. It handles field
// substitution, a conditional block, and a repeated block, none of them nested, which is every
// action these templates contain. Anything else left over is stripped rather than rendered, so an
// unhandled action shows up as missing markup rather than as literal braces in the page.
function renderTemplate(source, vars) {
	let out = source.replace(/\{\{range \.(\w+)\}\}([\s\S]*?)\{\{end\}\}/g, (_, key, body) => {
		const list = vars[key];
		if (!Array.isArray(list)) return "";
		return list.map((item) => body.replace(/\{\{\s*\.\s*\}\}/g, String(item))).join("");
	});
	out = out.replace(/\{\{if \.(\w+)\}\}([\s\S]*?)\{\{end\}\}/g, (_, key, body) => (vars[key] ? body : ""));
	out = out.replace(/\{\{\s*\.(\w+)\s*\}\}/g, (_, key) => {
		const v = vars[key];
		return v === undefined || v === null ? "" : String(v);
	});
	return out.replace(/\{\{[^}]*\}\}/g, "");
}
