// registerdoc.mjs mounts the standalone change register, ../../../dossier/register.html, into a
// sandbox and runs the script the generated document carries. The register is a self-contained file
// a reviewer opens from disk, so its script is inline rather than under js/ and the page harness
// cannot reach it; this is the same idea applied to a report instead of a served page.
//
// The template is rendered here rather than by Go, so this file carries a small evaluator for the
// text/template actions the register uses: field substitution, if with else and else-if, range, and
// the eq comparison. Anything else throws, and a rendered document still holding an action throws
// too, so a renderer that quietly produced an empty table fails the test rather than passing it.

import { readFileSync } from "node:fs";

import { parseHTML } from "./dom.mjs";
import { evalIn, loadParts, sandboxOf } from "./loader.mjs";

// REGISTER_PATH is the template the product ships, read rather than transcribed.
const REGISTER_PATH = new URL("../../../dossier/register.html", import.meta.url);

// escapeHTML escapes a value the way html/template does when it substitutes one into text.
function escapeHTML(value) {
	return String(value)
		.replaceAll("&", "&amp;").replaceAll("<", "&lt;").replaceAll(">", "&gt;")
		.replaceAll('"', "&#34;").replaceAll("'", "&#39;");
}

// tokenize splits a template into literal text and the actions between the braces.
function tokenize(source) {
	const out = [];
	let i = 0;
	for (;;) {
		const open = source.indexOf("{{", i);
		if (open === -1) {
			if (i < source.length) out.push({ text: source.slice(i) });
			return out;
		}
		if (open > i) out.push({ text: source.slice(i, open) });
		const close = source.indexOf("}}", open);
		if (close === -1) throw new Error("register template: unclosed action");
		out.push({ action: source.slice(open + 2, close).trim() });
		i = close + 2;
	}
}

// parseNodes reads tokens into a tree until it meets one of the closing actions, which it returns
// alongside so the caller can tell an else from an end.
function parseNodes(tokens, start, closers) {
	const nodes = [];
	let i = start;
	while (i < tokens.length) {
		const token = tokens[i];
		if (token.text !== undefined) {
			nodes.push({ kind: "text", text: token.text });
			i++;
			continue;
		}
		const action = token.action;
		if (closers.some((c) => action === c || action.startsWith(c + " "))) {
			return { nodes, next: i + 1, closer: action };
		}
		if (action.startsWith("if ")) {
			const built = parseIf(tokens, i, action.slice(3).trim());
			nodes.push(built.node);
			i = built.next;
			continue;
		}
		if (action.startsWith("range ")) {
			const body = parseNodes(tokens, i + 1, ["end"]);
			nodes.push({ kind: "range", field: action.slice(6).trim(), body: body.nodes });
			i = body.next;
			continue;
		}
		if (action === "." || action.startsWith(".")) {
			nodes.push({ kind: "field", field: action });
			i++;
			continue;
		}
		throw new Error("register template: unsupported action " + JSON.stringify(action));
	}
	throw new Error("register template: missing " + closers.join(" or "));
}

// parseIf reads an if action with any number of else-if branches and an optional else.
function parseIf(tokens, start, condition) {
	const branches = [];
	let cond = condition;
	let i = start + 1;
	for (;;) {
		const body = parseNodes(tokens, i, ["else if", "else", "end"]);
		branches.push({ cond, body: body.nodes });
		i = body.next;
		if (body.closer === "end") return { node: { kind: "if", branches, otherwise: [] }, next: i };
		if (body.closer.startsWith("else if")) {
			cond = body.closer.slice("else if".length).trim();
			continue;
		}
		const rest = parseNodes(tokens, i, ["end"]);
		return { node: { kind: "if", branches, otherwise: rest.nodes }, next: rest.next };
	}
}

// lookup resolves a field reference against the current dot, where a bare dot is the dot itself.
function lookup(scope, field) {
	if (field === ".") return scope;
	if (!/^\.\w+$/.test(field)) {
		throw new Error("register template: unsupported field " + JSON.stringify(field));
	}
	if (scope === null || typeof scope !== "object") {
		throw new Error("register template: " + field + " read off a non-object");
	}
	const name = field.slice(1);
	if (!Object.hasOwn(scope, name)) {
		throw new Error("register template: no value for " + field);
	}
	return scope[name];
}

// truthy applies Go's notion of an empty value, so an empty list or string takes the else branch.
function truthy(value) {
	return Array.isArray(value) ? value.length > 0 : Boolean(value);
}

// evalCond evaluates the two condition forms the register uses: a field on its own and eq against
// a string literal.
function evalCond(cond, scope) {
	const compare = /^eq\s+(\.\w+)\s+"([^"]*)"$/.exec(cond);
	if (compare) return String(lookup(scope, compare[1])) === compare[2];
	return truthy(lookup(scope, cond));
}

// render walks a parsed template against a scope and returns the markup.
function render(nodes, scope) {
	let out = "";
	for (const node of nodes) {
		if (node.kind === "text") out += node.text;
		else if (node.kind === "field") out += escapeHTML(lookup(scope, node.field));
		else if (node.kind === "range") {
			const list = lookup(scope, node.field);
			if (!Array.isArray(list)) throw new Error("register template: range over a non-list");
			for (const item of list) out += render(node.body, item);
		} else {
			const hit = node.branches.find((b) => evalCond(b.cond, scope));
			out += render(hit ? hit.body : node.otherwise, scope);
		}
	}
	return out;
}

// renderRegister renders the shipped register template against a view, the same shape registerView
// has in Go. It throws when anything is left unrendered, so a gap in the evaluator surfaces as a
// failure rather than as a page with a missing table.
export function renderRegister(view) {
	const source = readFileSync(REGISTER_PATH, "utf8");
	const markup = render(parseNodes(tokenize(source).concat([{ action: "end" }]), 0, ["end"]).nodes, view);
	if (markup.includes("{{")) throw new Error("register template: an action was left unrendered");
	return markup;
}

// rows builds n register rows, numbered so a test can tell one from another and name the values it
// wants to sort and filter on.
export function rows(specs) {
	return specs.map((spec) => Object.assign({
		When: "2026-07-01 09:00", Run: "run_x", Change: "ansible site.yml", Actor: "root",
		Source: "template tpl_x", Risk: "low", Held: "", Decision: "", DecisionSeq: 0,
		Outcome: "succeeded", DryRun: false,
	}, spec));
}

// view fills a register view with defaults, so a test states only what it cares about.
export function view(overrides) {
	return Object.assign({
		Status: "verified", StatusText: "The chain verifies and carries 1 anchor(s).",
		From: "2026-07-01", To: "2026-07-08", Rows: [], Total: 0, Approved: 0, Rejected: 0,
		Failed: 0, ChainCount: 3, Receipt: "3:abcdef", AnchorProblems: [],
		// The register can be cut at the store boundary, so the document carries the fields that say
		// so. A whole register is the default here; a test that wants the notice overrides them.
		Truncated: false, Limit: 5000, CoveredTo: "",
		GeneratedAt: "2026-07-08T00:00:00Z",
	}, overrides);
}

// mountRegister renders the register, parses it into a sandbox, and runs the script the document
// carries. It returns the document along with the downloads the export produced, so a test asserts
// on the file that was written rather than on the fact that a button was pressed.
export function mountRegister(overrides) {
	const data = view(overrides);
	const app = loadParts([]);
	const sandbox = sandboxOf(app);
	const nodes = parseHTML(renderRegister(data), sandbox.document);
	const html = nodes.find((n) => n.nodeType === 1 && n.tagName === "HTML");
	if (!html) throw new Error("jstest: the register rendered without an html element");
	sandbox.document.setDocumentElement(html);

	// Object URLs are handed back out as the blob behind them, so a download reads as its content.
	const blobs = new Map();
	const mint = sandbox.URL.createObjectURL;
	sandbox.URL.createObjectURL = (blob) => {
		const url = mint(blob);
		blobs.set(url, blob);
		return url;
	};
	const downloads = [];
	sandbox.document.addEventListener("click", (e) => {
		const el = e.target;
		if (!el || el.tagName !== "A" || !el.download) return;
		const blob = blobs.get(el.getAttribute("href"));
		downloads.push({
			name: el.download,
			type: blob ? blob.type : "",
			content: blob ? blob.parts.join("") : "",
		});
	});

	const script = sandbox.document.querySelector("script");
	if (!script || script.textContent.trim() === "") {
		throw new Error("jstest: the register carries no inline script");
	}
	evalIn(app, script.textContent, "register.html:script");

	const table = sandbox.document.getElementById("register-table");
	if (!table) throw new Error("jstest: the register rendered without its table");
	if (table.tBodies[0].rows.length !== data.Rows.length) {
		throw new Error("jstest: the register rendered " + table.tBodies[0].rows.length +
			" rows for " + data.Rows.length + " changes");
	}
	return { app, document: sandbox.document, table, downloads, data };
}

// runIDs lists the run id of each row currently in the table, in document order, which is what a
// sort has to change.
export function runIDs(table) {
	return Array.from(table.tBodies[0].rows).map((tr) => tr.cells[1].textContent.trim());
}

// visibleRunIDs lists only the rows a reader can actually see, which is what a filter and a page
// limit have to change.
export function visibleRunIDs(table) {
	return Array.from(table.tBodies[0].rows).filter((tr) => !tr.hidden)
		.map((tr) => tr.cells[1].textContent.trim());
}
