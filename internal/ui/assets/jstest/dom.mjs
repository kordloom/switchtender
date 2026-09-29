// dom.mjs implements the slice of the DOM the application script actually uses: a node tree with
// attributes, classes, datasets, a CSS selector engine, and real event dispatch. It exists so a
// test can drive a page handler the way a reader drives the page, rather than asserting on a pure
// helper the handler happens to call.
//
// What is real: the tree and its mutations, id and class and attribute reflection, textContent,
// innerHTML in both directions, querySelector and querySelectorAll over the selector subset listed
// on parseSelector, closest and matches, and listener dispatch through capture, target, and bubble
// phases with a live event object.
//
// What is not real: layout, so every geometry read is zero; CSS, so nothing is computed or
// inherited and hidden only sets an attribute; and the HTML parser, which is a tag scanner rather
// than a spec parser. See parseHTML for exactly what markup it accepts.

// VOID_TAGS never have children and close themselves, with or without a trailing slash.
const VOID_TAGS = new Set([
	"area", "base", "br", "col", "embed", "hr", "img", "input", "link", "meta", "param", "source",
	"track", "wbr",
]);

// RAW_TAGS hold text that is not markup, so the parser consumes to the closing tag verbatim.
const RAW_TAGS = new Set(["script", "style"]);

// TABLE_TAGS expose a rows collection, which the row removal helper reads to find an emptied list.
const TABLE_TAGS = new Set(["TABLE", "TBODY", "THEAD", "TFOOT"]);

// BOOL_ATTRS reflect as booleans: present means true, and assigning false removes the attribute.
const BOOL_ATTRS = ["hidden", "disabled", "checked", "selected", "multiple", "readOnly", "required"];

// STRING_ATTRS reflect as strings, defaulting to empty when the attribute is absent. These are the
// ones the application either sets as a property and reads as an attribute or selects on.
const STRING_ATTRS = {
	className: "class", type: "type", href: "href", src: "src", name: "name",
	placeholder: "placeholder",
};

// ENTITIES covers the named character references the templates and the script use.
const ENTITIES = {
	amp: "&", lt: "<", gt: ">", quot: '"', apos: "'", nbsp: " ", times: "×",
	middot: "·", mdash: "—", ndash: "–", hellip: "…", copy: "©",
};

// decodeEntities resolves named and numeric character references in parsed text.
function decodeEntities(text) {
	return text.replace(/&(#x[0-9a-fA-F]+|#[0-9]+|[a-zA-Z]+);/g, (whole, body) => {
		if (body[0] === "#") {
			const code = body[1] === "x" || body[1] === "X"
				? parseInt(body.slice(2), 16)
				: parseInt(body.slice(1), 10);
			return Number.isFinite(code) ? String.fromCodePoint(code) : whole;
		}
		return Object.hasOwn(ENTITIES, body) ? ENTITIES[body] : whole;
	});
}

// escapeText escapes a text node for the innerHTML getter.
function escapeText(text) {
	return String(text).replace(/&/g, "&amp;").replace(/</g, "&lt;").replace(/>/g, "&gt;");
}

// escapeAttr escapes an attribute value for the innerHTML getter, quotes included so a value
// carrying one reads back as the same value rather than as broken markup.
function escapeAttr(text) {
	return escapeText(text).replace(/"/g, "&quot;");
}

// dataAttrName converts a dataset key to its attribute name, so dataset.themeKey is data-theme-key.
function dataAttrName(key) {
	return "data-" + key.replace(/[A-Z]/g, (c) => "-" + c.toLowerCase());
}

// datasetKey converts a data attribute name back to its dataset key.
function datasetKey(attr) {
	return attr.slice(5).replace(/-([a-z])/g, (_, c) => c.toUpperCase());
}

// STText is a text node. It carries only what the script reads from one.
class STText {
	// constructor builds a detached text node holding the given string.
	constructor(text, ownerDocument) {
		this.nodeType = 3;
		this.nodeValue = String(text);
		this.parentNode = null;
		this.ownerDocument = ownerDocument || null;
	}

	// textContent is the node's string, readable and writable like the browser's.
	get textContent() { return this.nodeValue; }
	set textContent(v) { this.nodeValue = String(v); }

	// data mirrors textContent, which is the name character data is usually read under.
	get data() { return this.nodeValue; }
	set data(v) { this.nodeValue = String(v); }

	// remove detaches the node from its parent.
	remove() {
		if (this.parentNode) this.parentNode.removeChild(this);
	}

	// cloneNode returns a detached copy of the text.
	cloneNode() { return new STText(this.nodeValue, this.ownerDocument); }
}

// STElement is an element node: the tree, its attributes, and its listeners.
class STElement {
	// constructor builds a detached element of the given tag.
	constructor(tagName, ownerDocument) {
		this.nodeType = 1;
		this.tagName = String(tagName || "div").toUpperCase();
		this.ownerDocument = ownerDocument || null;
		this.parentNode = null;
		this.childNodes = [];
		this.attrs = new Map();
		this.style = {};
		this.listeners = new Map();
		// Layout is not simulated, so every geometry read is a plain number a test can overwrite.
		this.scrollTop = 0;
		this.scrollHeight = 0;
		this.clientHeight = 0;
		this.offsetWidth = 0;
		this.offsetHeight = 0;
		this.title = "";
		this.tabIndex = -1;
		this._classList = null;
		this._dataset = null;
	}

	// nodeName is the tag name, which is what the browser reports for an element.
	get nodeName() { return this.tagName; }

	// children lists the element children, excluding text nodes.
	get children() { return this.childNodes.filter((n) => n.nodeType === 1); }

	// firstChild, lastChild, and the sibling pair walk the tree the way the script does.
	get firstChild() { return this.childNodes[0] || null; }
	get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }
	get firstElementChild() { return this.children[0] || null; }
	get lastElementChild() { return this.children[this.children.length - 1] || null; }

	// nextSibling returns the node after this one under the same parent, or null. A test may hand an
	// element a stand-in parent, so a parent without children is treated as no parent at all.
	get nextSibling() {
		const kids = this.parentNode && this.parentNode.childNodes;
		return Array.isArray(kids) ? kids[kids.indexOf(this) + 1] || null : null;
	}

	// previousSibling returns the node before this one under the same parent, or null.
	get previousSibling() {
		const kids = this.parentNode && this.parentNode.childNodes;
		return Array.isArray(kids) ? kids[kids.indexOf(this) - 1] || null : null;
	}

	// nextElementSibling skips text nodes on the way to the next element.
	get nextElementSibling() {
		let n = this.nextSibling;
		while (n && n.nodeType !== 1) n = n.nextSibling;
		return n;
	}

	// value is the control's current value, seeded from the value attribute the way a browser seeds
	// a field from its markup and detached from it once anything assigns to the property.
	get value() {
		if (this.currentValue !== undefined) return this.currentValue;
		return this.attrs.get("value") ?? "";
	}

	set value(v) { this.currentValue = v === null || v === undefined ? "" : String(v); }

	// id reflects the id attribute and registers the element so getElementById can find it.
	get id() { return this.attrs.get("id") || ""; }
	set id(v) { this.setAttribute("id", v); }

	// classList adds, removes, toggles, and tests classes against the class attribute.
	get classList() {
		if (this._classList) return this._classList;
		const el = this;
		const read = () => (el.attrs.get("class") || "").split(/\s+/).filter(Boolean);
		const write = (names) => { el.setAttribute("class", names.join(" ")); };
		this._classList = {
			add(...names) {
				const cur = read();
				for (const n of names) if (n && !cur.includes(n)) cur.push(n);
				write(cur);
			},
			remove(...names) { write(read().filter((n) => !names.includes(n))); },
			toggle(name, force) {
				const on = force === undefined ? !read().includes(name) : Boolean(force);
				if (on) this.add(name); else this.remove(name);
				return on;
			},
			contains: (name) => read().includes(name),
			get length() { return read().length; },
			get value() { return read().join(" "); },
			[Symbol.iterator]() { return read()[Symbol.iterator](); },
		};
		return this._classList;
	}

	// dataset reads and writes data attributes under their camel case keys.
	get dataset() {
		if (this._dataset) return this._dataset;
		const el = this;
		this._dataset = new Proxy({}, {
			get(_, prop) {
				if (typeof prop !== "string") return undefined;
				const v = el.attrs.get(dataAttrName(prop));
				return v === undefined ? undefined : v;
			},
			set(_, prop, value) { el.setAttribute(dataAttrName(prop), value); return true; },
			has(_, prop) { return typeof prop === "string" && el.attrs.has(dataAttrName(prop)); },
			deleteProperty(_, prop) { el.attrs.delete(dataAttrName(prop)); return true; },
			ownKeys() {
				return [...el.attrs.keys()].filter((k) => k.startsWith("data-")).map(datasetKey);
			},
			getOwnPropertyDescriptor(_, prop) {
				if (typeof prop !== "string" || !el.attrs.has(dataAttrName(prop))) return undefined;
				return {
					value: el.attrs.get(dataAttrName(prop)),
					writable: true, enumerable: true, configurable: true,
				};
			},
		});
		return this._dataset;
	}

	// textContent joins every descendant's text, and assigning replaces the children with one text
	// node, which is how the script clears a node before refilling it.
	get textContent() {
		let out = "";
		for (const n of this.childNodes) out += n.nodeType === 3 ? n.nodeValue : n.textContent;
		return out;
	}

	set textContent(v) {
		this.clearChildren();
		const text = v === null || v === undefined ? "" : String(v);
		if (text !== "") this.appendChild(new STText(text, this.ownerDocument));
	}

	// innerHTML parses markup into children on assignment and serializes the children back on read.
	// The read is a fresh serialization rather than an echo of what was assigned, so it reflects
	// anything the script built or removed afterward.
	get innerHTML() { return this.childNodes.map(serialize).join(""); }

	set innerHTML(markup) {
		this.clearChildren();
		this.assignedHTML = String(markup);
		for (const node of parseHTML(String(markup), this.ownerDocument)) this.appendChild(node);
	}

	// outerHTML serializes the element itself along with its children.
	get outerHTML() { return serialize(this); }

	// rows lists a table section's rows, which the row removal helper counts to spot an empty list.
	get rows() {
		return TABLE_TAGS.has(this.tagName) ? this.querySelectorAll("tr") : undefined;
	}

	// options lists a select's option elements.
	get options() {
		return this.tagName === "SELECT" || this.tagName === "DATALIST"
			? this.querySelectorAll("option") : undefined;
	}

	// selectedOptions lists the picked options of a multi select, which the launch form reads.
	get selectedOptions() {
		return this.tagName === "SELECT"
			? this.querySelectorAll("option").filter((o) => o.selected) : undefined;
	}

	// isConnected reports whether the element hangs off the document, which is what makes it
	// reachable by id.
	get isConnected() {
		let n = this;
		while (n.parentNode) n = n.parentNode;
		const doc = this.ownerDocument;
		return Boolean(doc) && (n === doc.documentElement || n === doc);
	}

	// setAttribute writes an attribute, registering an id so the document can resolve it.
	setAttribute(name, value) {
		const key = String(name);
		this.attrs.set(key, value === null || value === undefined ? "" : String(value));
		if (key === "id" && this.ownerDocument) this.ownerDocument.registerID(this);
	}

	// getAttribute returns an attribute's value, or null when it is absent.
	getAttribute(name) {
		const v = this.attrs.get(String(name));
		return v === undefined ? null : v;
	}

	// hasAttribute reports whether the attribute is present at all.
	hasAttribute(name) { return this.attrs.has(String(name)); }

	// removeAttribute drops an attribute.
	removeAttribute(name) { this.attrs.delete(String(name)); }

	// getAttributeNames lists the attributes currently set.
	getAttributeNames() { return [...this.attrs.keys()]; }

	// appendChild adds a node as the last child, detaching it from any previous parent.
	appendChild(node) {
		if (node.parentNode) node.parentNode.removeChild(node);
		node.parentNode = this;
		if (!node.ownerDocument) node.ownerDocument = this.ownerDocument;
		this.childNodes.push(node);
		return node;
	}

	// append adds nodes and strings as children, wrapping the strings in text nodes.
	append(...nodes) {
		for (const n of nodes) {
			this.appendChild(typeof n === "string" ? new STText(n, this.ownerDocument) : n);
		}
	}

	// prepend adds nodes and strings before the current first child.
	prepend(...nodes) {
		const first = this.firstChild;
		for (const n of nodes) {
			this.insertBefore(typeof n === "string" ? new STText(n, this.ownerDocument) : n, first);
		}
	}

	// insertBefore places a node ahead of ref, appending when ref is null or is not a child.
	insertBefore(node, ref) {
		if (node.parentNode) node.parentNode.removeChild(node);
		node.parentNode = this;
		if (!node.ownerDocument) node.ownerDocument = this.ownerDocument;
		const i = ref ? this.childNodes.indexOf(ref) : -1;
		if (i >= 0) this.childNodes.splice(i, 0, node); else this.childNodes.push(node);
		return node;
	}

	// removeChild detaches a child node.
	removeChild(node) {
		const i = this.childNodes.indexOf(node);
		if (i >= 0) this.childNodes.splice(i, 1);
		node.parentNode = null;
		return node;
	}

	// replaceChild swaps one child for another.
	replaceChild(next, prev) {
		this.insertBefore(next, prev);
		this.removeChild(prev);
		return prev;
	}

	// replaceChildren drops every child and appends the given ones.
	replaceChildren(...nodes) {
		this.clearChildren();
		this.append(...nodes);
	}

	// remove detaches this element from its parent.
	remove() {
		if (this.parentNode) this.parentNode.removeChild(this);
	}

	// replaceWith swaps this element for the given nodes in its parent, a no-op when detached.
	replaceWith(...nodes) {
		const parent = this.parentNode;
		if (!parent) return;
		for (const n of nodes) {
			parent.insertBefore(typeof n === "string" ? new STText(n, this.ownerDocument) : n, this);
		}
		parent.removeChild(this);
	}

	// clearChildren detaches every child, used by textContent and innerHTML assignment.
	clearChildren() {
		for (const n of this.childNodes) n.parentNode = null;
		this.childNodes = [];
	}

	// contains reports whether a node is this element or one of its descendants.
	contains(node) {
		let n = node;
		while (n) {
			if (n === this) return true;
			n = n.parentNode;
		}
		return false;
	}

	// cloneNode copies the element's tag and attributes, and its subtree when deep. Listeners and
	// properties set outside attributes are not copied, matching the browser.
	cloneNode(deep) {
		const copy = new STElement(this.tagName, this.ownerDocument);
		for (const [k, v] of this.attrs) copy.setAttribute(k, v);
		Object.assign(copy.style, this.style);
		if (deep) for (const n of this.childNodes) copy.appendChild(n.cloneNode(true));
		return copy;
	}

	// querySelector returns the first descendant matching the selector, or null.
	querySelector(selector) {
		const found = this.querySelectorAll(selector);
		return found.length ? found[0] : null;
	}

	// querySelectorAll returns every descendant matching the selector, in tree order.
	querySelectorAll(selector) {
		const groups = parseSelector(selector);
		const out = [];
		for (const el of descendants(this)) {
			if (groups.some((g) => matchComplex(el, g, this))) out.push(el);
		}
		return out;
	}

	// matches reports whether this element satisfies the selector.
	matches(selector) {
		return parseSelector(selector).some((g) => matchComplex(this, g, this));
	}

	// closest walks up from this element, itself included, to the nearest match.
	closest(selector) {
		let n = this;
		while (n && n.nodeType === 1) {
			if (n.matches(selector)) return n;
			n = n.parentNode;
		}
		return null;
	}

	// addEventListener registers a listener, honoring the capture flag in either form.
	addEventListener(type, fn, options) {
		if (typeof fn !== "function") return;
		const capture = options === true || Boolean(options && options.capture === true);
		const list = this.listeners.get(type) || [];
		list.push({ fn, capture, once: Boolean(options && options.once) });
		this.listeners.set(type, list);
	}

	// removeEventListener drops a listener registered with the same function and phase.
	removeEventListener(type, fn, options) {
		const capture = options === true || Boolean(options && options.capture === true);
		const list = this.listeners.get(type);
		if (!list) return;
		this.listeners.set(type, list.filter((l) => !(l.fn === fn && l.capture === capture)));
	}

	// dispatchEvent runs the listeners along the path to this node and back, and returns false when
	// a listener called preventDefault.
	dispatchEvent(event) { return dispatch(this, event); }

	// click dispatches a bubbling click, which is how a test presses a control.
	click() { return this.dispatchEvent(makeEvent("click")); }

	// focus and blur are recorded rather than simulated, since nothing here has a focus ring.
	focus() { if (this.ownerDocument) this.ownerDocument.activeElement = this; }
	blur() {
		const doc = this.ownerDocument;
		if (doc && doc.activeElement === this) doc.activeElement = null;
	}

	// getBoundingClientRect returns a zero rectangle, since layout is not simulated.
	getBoundingClientRect() {
		return { top: 0, left: 0, right: 0, bottom: 0, width: 0, height: 0, x: 0, y: 0 };
	}

	// scrollIntoView does nothing, since there is no viewport.
	scrollIntoView() {}
}

// The reflected attributes are defined on the prototype rather than written out one by one, and are
// configurable so a test can shadow one with its own accessor.
for (const [prop, attr] of Object.entries(STRING_ATTRS)) {
	Object.defineProperty(STElement.prototype, prop, {
		configurable: true,
		get() { return this.attrs.get(attr) || ""; },
		set(v) { this.setAttribute(attr, v); },
	});
}
for (const prop of BOOL_ATTRS) {
	const attr = prop.toLowerCase();
	Object.defineProperty(STElement.prototype, prop, {
		configurable: true,
		get() { return this.attrs.has(attr); },
		set(v) { if (v) this.setAttribute(attr, ""); else this.attrs.delete(attr); },
	});
}

// descendants walks a subtree in document order, the root excluded, elements only.
function descendants(root) {
	const out = [];
	const walk = (node) => {
		for (const child of node.childNodes) {
			if (child.nodeType !== 1) continue;
			out.push(child);
			walk(child);
		}
	};
	walk(root);
	return out;
}

// serialize renders a node back to markup for the innerHTML getter.
function serialize(node) {
	if (node.nodeType === 3) return escapeText(node.nodeValue);
	const tag = node.tagName.toLowerCase();
	let out = "<" + tag;
	for (const [k, v] of node.attrs) out += v === "" ? " " + k : ' ' + k + '="' + escapeAttr(v) + '"';
	out += ">";
	if (VOID_TAGS.has(tag)) return out;
	for (const child of node.childNodes) out += serialize(child);
	return out + "</" + tag + ">";
}

// parseHTML turns markup into a list of nodes. It is a tag scanner, not a spec parser: it handles
// nested elements, void elements with or without a trailing slash, self-closing tags, quoted and
// unquoted attribute values, comments, the doctype, raw text inside script and style, and named or
// numeric character references. It does not implement implicit tag closing, so every non-void
// element has to be closed explicitly; it has no notion of the table or paragraph content models;
// an unquoted attribute value ending in a slash reads as a self-closing tag; and it does not run
// scripts or apply styles. Every template and every innerHTML string in this codebase is written
// closed and quoted, so the shortfall is in what the harness would accept, not in what it parses.
export function parseHTML(markup, ownerDocument) {
	const root = new STElement("template", ownerDocument);
	const stack = [root];
	const top = () => stack[stack.length - 1];
	let i = 0;
	const push = (text) => {
		if (text !== "") top().appendChild(new STText(decodeEntities(text), ownerDocument));
	};
	while (i < markup.length) {
		const lt = markup.indexOf("<", i);
		if (lt === -1) {
			push(markup.slice(i));
			break;
		}
		push(markup.slice(i, lt));
		if (markup.startsWith("<!--", lt)) {
			const end = markup.indexOf("-->", lt);
			i = end === -1 ? markup.length : end + 3;
			continue;
		}
		if (markup.startsWith("<!", lt) || markup.startsWith("<?", lt)) {
			const end = markup.indexOf(">", lt);
			i = end === -1 ? markup.length : end + 1;
			continue;
		}
		if (markup.startsWith("</", lt)) {
			const end = markup.indexOf(">", lt);
			const name = markup.slice(lt + 2, end === -1 ? markup.length : end).trim().toLowerCase();
			for (let s = stack.length - 1; s > 0; s--) {
				if (stack[s].tagName.toLowerCase() === name) {
					stack.length = s;
					break;
				}
			}
			i = end === -1 ? markup.length : end + 1;
			continue;
		}
		const open = /^<([a-zA-Z][^\s/>]*)((?:[^>"']|"[^"]*"|'[^']*')*)>/.exec(markup.slice(lt));
		if (!open) {
			push("<");
			i = lt + 1;
			continue;
		}
		const tag = open[1].toLowerCase();
		const el = new STElement(tag, ownerDocument);
		for (const [, name, , dq, sq, bare] of open[2].matchAll(
			/([^\s=/>]+)(\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'=<>`]+)))?/g)) {
			if (name === "/") continue;
			const value = dq ?? sq ?? bare ?? "";
			el.setAttribute(name, decodeEntities(value));
		}
		top().appendChild(el);
		i = lt + open[0].length;
		if (VOID_TAGS.has(tag) || open[2].trimEnd().endsWith("/")) continue;
		if (RAW_TAGS.has(tag)) {
			const close = markup.toLowerCase().indexOf("</" + tag, i);
			const end = close === -1 ? markup.length : close;
			if (end > i) el.appendChild(new STText(markup.slice(i, end), ownerDocument));
			const gt = close === -1 ? -1 : markup.indexOf(">", close);
			i = gt === -1 ? markup.length : gt + 1;
			continue;
		}
		stack.push(el);
	}
	const nodes = root.childNodes.slice();
	for (const n of nodes) n.parentNode = null;
	root.childNodes = [];
	return nodes;
}

// SELECTOR_CACHE keeps parsed selectors, since the same handful are queried over and over.
const SELECTOR_CACHE = new Map();

// parseSelector compiles a selector list into groups of compounds joined by combinators. The
// supported grammar is: comma separated selectors; the descendant and child combinators; and
// compounds built from a tag name, *, #id, .class, [attr], [attr=value] with =, ^=, $=, *=, or ~=,
// :scope, and :not() around one compound. Anything else, sibling combinators and structural pseudo
// classes above all, is rejected loudly rather than quietly matching nothing.
function parseSelector(selector) {
	const cached = SELECTOR_CACHE.get(selector);
	if (cached) return cached;
	const groups = [];
	for (const raw of splitTop(String(selector), ",")) {
		const text = raw.trim();
		if (!text) continue;
		const parts = [];
		let combinator = " ";
		for (const token of text.split(/\s+/)) {
			if (token === ">") {
				combinator = ">";
				continue;
			}
			for (const [j, piece] of token.split(">").entries()) {
				if (piece === "") continue;
				parts.push({ compound: parseCompound(piece, selector), combinator: j > 0 ? ">" : combinator });
				combinator = " ";
			}
		}
		if (parts.length) groups.push(parts);
	}
	SELECTOR_CACHE.set(selector, groups);
	return groups;
}

// splitTop splits on a separator that is not inside brackets or parentheses.
function splitTop(text, sep) {
	const out = [];
	let depth = 0;
	let start = 0;
	for (let i = 0; i < text.length; i++) {
		const c = text[i];
		if (c === "[" || c === "(") depth++;
		else if (c === "]" || c === ")") depth--;
		else if (c === sep && depth === 0) {
			out.push(text.slice(start, i));
			start = i + 1;
		}
	}
	out.push(text.slice(start));
	return out;
}

// COMPOUND_TOKEN matches one piece of a compound selector.
const COMPOUND_TOKEN =
	/^(?:\*|[a-zA-Z][\w-]*|#[\w-]+|\.[\w-]+|\[[^\]]+\]|:scope|:not\([^)]+\))/;

// parseCompound compiles one compound selector into the tests an element has to pass.
function parseCompound(text, whole) {
	const compound = { tag: null, id: null, classes: [], attrs: [], scope: false, not: [] };
	let rest = text;
	while (rest) {
		const m = COMPOUND_TOKEN.exec(rest);
		if (!m) throw new Error("jstest dom: unsupported selector " + JSON.stringify(whole));
		const token = m[0];
		rest = rest.slice(token.length);
		if (token === "*") continue;
		else if (token === ":scope") compound.scope = true;
		else if (token.startsWith(":not(")) {
			compound.not.push(parseCompound(token.slice(5, -1).trim(), whole));
		} else if (token[0] === "#") compound.id = token.slice(1);
		else if (token[0] === ".") compound.classes.push(token.slice(1));
		else if (token[0] === "[") compound.attrs.push(parseAttr(token.slice(1, -1), whole));
		else compound.tag = token.toUpperCase();
	}
	return compound;
}

// parseAttr compiles one attribute test from the inside of a bracket.
function parseAttr(text, whole) {
	const m = /^([^\s~^$*|=]+)\s*(?:([~^$*|]?=)\s*(.*))?$/.exec(text.trim());
	if (!m) throw new Error("jstest dom: unsupported attribute selector in " + JSON.stringify(whole));
	const name = m[1];
	if (!m[2]) return { name, op: null, value: null };
	let value = m[3].trim();
	if ((value.startsWith('"') && value.endsWith('"')) || (value.startsWith("'") && value.endsWith("'"))) {
		value = value.slice(1, -1);
	}
	return { name, op: m[2], value };
}

// matchCompound tests one element against one compound selector.
function matchCompound(el, compound, scope) {
	if (el.nodeType !== 1) return false;
	if (compound.scope && el !== scope) return false;
	if (compound.tag && el.tagName !== compound.tag) return false;
	if (compound.id !== null && el.getAttribute("id") !== compound.id) return false;
	for (const cls of compound.classes) {
		if (!el.classList.contains(cls)) return false;
	}
	for (const attr of compound.attrs) {
		const got = el.getAttribute(attr.name);
		if (got === null) return false;
		if (attr.op === null) continue;
		if (attr.op === "=" && got !== attr.value) return false;
		if (attr.op === "^=" && !got.startsWith(attr.value)) return false;
		if (attr.op === "$=" && !got.endsWith(attr.value)) return false;
		if (attr.op === "*=" && !got.includes(attr.value)) return false;
		if (attr.op === "~=" && !got.split(/\s+/).includes(attr.value)) return false;
	}
	for (const neg of compound.not) {
		if (matchCompound(el, neg, scope)) return false;
	}
	return true;
}

// matchComplex tests an element against a full selector, right to left along the combinators.
function matchComplex(el, parts, scope) {
	const last = parts.length - 1;
	if (!matchCompound(el, parts[last].compound, scope)) return false;
	let cur = el;
	for (let i = last - 1; i >= 0; i--) {
		if (parts[i + 1].combinator === ">") {
			cur = cur.parentNode;
			if (!cur || !matchCompound(cur, parts[i].compound, scope)) return false;
			continue;
		}
		let ancestor = cur.parentNode;
		while (ancestor && !matchCompound(ancestor, parts[i].compound, scope)) ancestor = ancestor.parentNode;
		if (!ancestor) return false;
		cur = ancestor;
	}
	return true;
}

// makeEvent builds the event object a listener receives. It carries the fields the script reads:
// the target, the key for keyboard handlers, the data payload for stream events, and the two ways
// of stopping what happens next.
export function makeEvent(type, init) {
	const event = {
		type,
		target: null,
		currentTarget: null,
		bubbles: true,
		cancelable: true,
		defaultPrevented: false,
		stopped: false,
		stoppedImmediate: false,
		// results collects what each listener returned, so a test can wait for an async handler.
		results: [],
		preventDefault() { this.defaultPrevented = true; },
		stopPropagation() { this.stopped = true; },
		stopImmediatePropagation() { this.stopped = true; this.stoppedImmediate = true; },
	};
	return Object.assign(event, init);
}

// dispatch runs an event down the capture path, at the target, and back up the bubble path, which
// is what makes a delegated listener on a table see a click on a cell.
function dispatch(target, event) {
	if (!event.target) event.target = target;
	const path = [];
	for (let n = target; n; n = n.parentNode) path.push(n);
	const call = (node, capture) => {
		const list = node.listeners && node.listeners.get(event.type);
		if (list) {
			for (const listener of list.slice()) {
				if (listener.capture !== capture) continue;
				if (listener.once) node.removeEventListener(event.type, listener.fn, listener.capture);
				event.currentTarget = node;
				event.results.push(listener.fn.call(node, event));
				if (event.stoppedImmediate) return;
			}
		}
		if (capture) return;
		const inline = node["on" + event.type];
		if (typeof inline === "function") {
			event.currentTarget = node;
			event.results.push(inline.call(node, event));
		}
	};
	for (let i = path.length - 1; i >= 1; i--) {
		call(path[i], true);
		if (event.stopped) return !event.defaultPrevented;
	}
	call(target, true);
	if (!event.stoppedImmediate) call(target, false);
	if (!event.stopped && event.bubbles) {
		for (let i = 1; i < path.length; i++) {
			call(path[i], false);
			if (event.stopped) break;
		}
	}
	return !event.defaultPrevented;
}

// fire dispatches an event of the given type at an element and returns the event, so a test can
// read what the handlers did to it. It does not wait for anything asynchronous a handler started.
export function fire(el, type, init) {
	const event = makeEvent(type, init);
	el.dispatchEvent(event);
	return event;
}

// press dispatches an event and waits for whatever its handlers returned, which is how a control
// whose handler is an async submit is driven to completion. A handler that only settles on a timer
// has to be driven with the clock instead, or this waits forever.
export async function press(el, type, init) {
	const event = fire(el, type || "click", init);
	await Promise.all(event.results.filter((r) => r && typeof r.then === "function"));
	return event;
}

// STDocument is the document object the parts evaluate against: an element factory, an id index
// over the tree, and the same selector engine the elements use.
class STDocument {
	// constructor builds an empty document with an html, head, and body already in place.
	constructor() {
		this.nodeType = 9;
		this.byID = new Map();
		this.listeners = new Map();
		this.activeElement = null;
		this.parentNode = null;
		this.documentElement = this.createElement("html");
		this.head = this.createElement("head");
		this.body = this.createElement("body");
		this.documentElement.appendChild(this.head);
		this.documentElement.appendChild(this.body);
		// The root's parent is the document itself, so an event dispatched on a control reaches the
		// delegated listeners the script installs at document level.
		this.documentElement.parentNode = this;
	}

	// createElement builds a detached element owned by this document.
	createElement(tagName) { return new STElement(tagName, this); }

	// createElementNS ignores the namespace, which nothing here depends on.
	createElementNS(_ns, tagName) { return new STElement(tagName, this); }

	// createTextNode builds a detached text node.
	createTextNode(text) { return new STText(text, this); }

	// createDocumentFragment returns a container whose children can be moved in one call.
	createDocumentFragment() { return new STElement("#fragment", this); }

	// registerID records an element under its id, so the index can resolve it later.
	registerID(el) {
		const id = el.getAttribute("id");
		if (!id) return;
		const list = this.byID.get(id) || [];
		if (!list.includes(el)) list.push(el);
		this.byID.set(id, list);
	}

	// getElementById returns the element carrying the id, provided it is in the document. An element
	// that was built and never inserted is not found, exactly as in a browser.
	getElementById(id) {
		const list = this.byID.get(String(id));
		if (!list) return null;
		for (const el of list) {
			if (el.getAttribute("id") === String(id) && el.isConnected) return el;
		}
		return null;
	}

	// setDocumentElement replaces the whole tree, used when a page skeleton is mounted.
	setDocumentElement(html) {
		this.documentElement = html;
		html.parentNode = this;
		this.head = html.querySelector("head") || this.head;
		this.body = html.querySelector("body") || this.body;
	}

	// querySelector returns the first match anywhere in the document.
	querySelector(selector) {
		const found = this.querySelectorAll(selector);
		return found.length ? found[0] : null;
	}

	// querySelectorAll returns every match in the document, the root element included.
	querySelectorAll(selector) {
		const groups = parseSelector(selector);
		const root = this.documentElement;
		const out = [];
		for (const el of [root, ...descendants(root)]) {
			if (groups.some((g) => matchComplex(el, g, root))) out.push(el);
		}
		return out;
	}

	// addEventListener registers a document level listener, which is where delegated handlers live.
	addEventListener(type, fn, options) {
		STElement.prototype.addEventListener.call(this, type, fn, options);
	}

	// removeEventListener drops a document level listener.
	removeEventListener(type, fn, options) {
		STElement.prototype.removeEventListener.call(this, type, fn, options);
	}

	// dispatchEvent runs a document level event, such as DOMContentLoaded or a keydown.
	dispatchEvent(event) { return dispatch(this, event); }
}

// createDocument returns a fresh document for one sandbox.
export function createDocument() { return new STDocument(); }

export { STElement, STText, STDocument };
