// loader.mjs evaluates named source parts from ../js/ inside a stubbed browser sandbox, so the
// application script is testable under node --test with no dependencies. The parts are concatenated
// in the order given, exactly as the server assembles app.js, and the returned handle resolves both
// global functions and top-level const bindings.
//
// WHAT THIS HARNESS SIMULATES
//
//   The DOM, in dom.mjs. A real node tree: children and parents, attributes, id, class and
//   classList, dataset, textContent, innerHTML in both directions, hidden and disabled reflected as
//   attributes, cloning, closest and matches, querySelector and querySelectorAll over a useful
//   subset of CSS, and event dispatch through capture, target, and bubble phases with an event
//   object carrying target, preventDefault, and stopPropagation. document.getElementById is an
//   index over the tree, so it finds what the page built and not what a stub was told to return.
//
//   Time, in clock.mjs. setTimeout, setInterval, and requestAnimationFrame are virtual and fire
//   only when a test ticks the clock. A debounce or a coalescing window is therefore something a
//   test steps through deliberately, and a page that polls does not keep the test process alive.
//
//   The network, in net.mjs. installFetch answers from routes and records every call, so a test
//   asserts the URL, method, headers, and body the page actually sent. A URL no route claims fails
//   loudly rather than resolving empty. installEventSource records the URL a stream was opened with
//   and lets a test emit events into the page.
//
//   The page skeleton, in pages.mjs. mountPage builds the DOM from the real template in
//   ../../templates, so the ids a handler reaches for are the ids the server ships.
//
// WHAT IT DOES NOT SIMULATE, so a passing test here is not a claim about any of it
//
//   Layout and CSS. Nothing is measured, nothing is computed, nothing cascades. Every rectangle is
//   zero and every scroll offset is zero, so anything about position, size, visibility through CSS,
//   or overflow is untestable here. hidden sets an attribute; whether it hides anything is the
//   stylesheet's business.
//
//   Rendering and the browser's own behavior. There is no paint, no focus ring, no form submission,
//   no navigation: assigning location.href is recorded, not followed. Default actions do not happen,
//   so a click on a link does nothing beyond running its listeners.
//
//   HTML parsing to spec. innerHTML and the templates go through a tag scanner, described on
//   parseHTML in dom.mjs. Every element has to be closed explicitly and there are no content models,
//   so markup the browser would repair is markup this rejects.
//
//   Everything not stubbed. Observers record and never fire, clipboard and window.open and confirm
//   are recorders, and Blob and object URLs are placeholders. If a flow depends on one of these
//   doing something, the test has to say what it should do rather than assume.
import { readFileSync } from "node:fs";
import vm from "node:vm";

import { createClock } from "./clock.mjs";
import { createDocument, makeEvent } from "./dom.mjs";

// IDENT matches a plain identifier, the only property names worth resolving in the vm context.
const IDENT = /^[A-Za-z_$][A-Za-z0-9_$]*$/;

// LOADED maps a returned handle to the sandbox behind it, so the fetch, stream, and page helpers
// can reach the same globals the parts were evaluated against.
const LOADED = new WeakMap();

// makeStorage returns an in-memory Web Storage stand-in.
function makeStorage() {
	const map = new Map();
	return {
		getItem: (k) => (map.has(k) ? map.get(k) : null),
		setItem: (k, v) => { map.set(k, String(v)); },
		removeItem: (k) => { map.delete(k); },
		clear: () => { map.clear(); },
		key: (i) => [...map.keys()][i] ?? null,
		get length() { return map.size; },
	};
}

// makeLocation returns a location stand-in that records navigations rather than following them, so
// a handler that sends the reader to a new run can be asserted on instead of ending the test.
function makeLocation() {
	let href = "http://test/ui/";
	const location = {
		origin: "http://test", protocol: "http:", host: "test", hostname: "test", port: "",
		pathname: "/ui/", search: "", hash: "",
		// navigations records every assignment to href and every reload, in order.
		navigations: [],
		assign(url) { location.href = url; },
		replace(url) { location.href = url; },
		reload() { location.navigations.push("reload"); },
		toString() { return href; },
	};
	Object.defineProperty(location, "href", {
		configurable: true,
		enumerable: true,
		get: () => href,
		set: (v) => { href = String(v); location.navigations.push(href); },
	});
	return location;
}

// makeSandbox builds the browser-shaped global object the parts evaluate against. Anything that
// would leave the process, fetch above all, fails loudly rather than silently succeeding.
function makeSandbox() {
	const clock = createClock();
	const document = createDocument();
	// Object URLs are minted per sandbox so one test's handles never appear in another's.
	let blobSeq = 0;
	const ObjectURL = class URL extends globalThis.URL {};
	ObjectURL.createObjectURL = () => "blob:jstest/" + (++blobSeq);
	ObjectURL.revokeObjectURL = () => {};

	const sandbox = {
		document,
		location: makeLocation(),
		history: {
			state: null,
			replaceState(state) { this.state = state; },
			pushState(state) { this.state = state; },
			back() {},
		},
		matchMedia: () => ({ matches: false, addEventListener() {}, removeEventListener() {} }),
		localStorage: makeStorage(),
		sessionStorage: makeStorage(),
		navigator: {
			platform: "TestPlatform",
			userAgent: "jstest",
			// clipboard records what the page copied instead of touching a real one.
			clipboard: {
				copied: [],
				async writeText(text) { sandbox.navigator.clipboard.copied.push(String(text)); },
				async readText() { return ""; },
			},
		},
		fetch: () => { throw new Error("fetch called in unit test"); },
		EventSource: function EventSource() {
			throw new Error("EventSource opened in unit test: call installEventSource first");
		},
		// The window level dialogs are recorders with an answer a test can change.
		confirm: () => true,
		prompt: () => null,
		alert: () => {},
		// open records the popup a download or an external link would have raised.
		open: (url, target, features) => {
			sandbox.openedWindows.push({ url, target, features });
			return { closed: false };
		},
		openedWindows: [],
		Blob: class Blob {
			constructor(parts, options) {
				this.parts = parts || [];
				this.type = (options && options.type) || "";
				this.size = this.parts.reduce((n, p) => n + String(p).length, 0);
			}
		},
		Event: class Event {
			constructor(type, init) { Object.assign(this, makeEvent(type, init)); }
		},
		// Observers record what they were asked to watch and never fire, so anything depending on
		// one has to be driven directly.
		MutationObserver: class MutationObserver {
			constructor(callback) { this.callback = callback; this.observed = []; }
			observe(target, options) { this.observed.push({ target, options }); }
			disconnect() {}
			takeRecords() { return []; }
		},
		getComputedStyle: () => ({ getPropertyValue: () => "" }),
		URLSearchParams,
		URL: ObjectURL,
		setTimeout: (fn, ms, ...args) => clock.setTimeout(fn, ms, ...args),
		clearTimeout: (id) => clock.clearTimeout(id),
		setInterval: (fn, ms, ...args) => clock.setInterval(fn, ms, ...args),
		clearInterval: (id) => clock.clearInterval(id),
		requestAnimationFrame: (fn) => clock.requestAnimationFrame(fn),
		cancelAnimationFrame: (id) => clock.cancelAnimationFrame(id),
		performance: { now: () => clock.now },
		console,
	};
	// A real window listens for resize and scroll; code guards its dropdowns and canvases against
	// them. Layout never changes in a test, so these record the registration and never fire, the
	// same posture the observers take.
	const winListeners = [];
	sandbox.addEventListener = (type, handler, opts) => { winListeners.push({ type, handler, opts }); };
	sandbox.removeEventListener = (type, handler) => {
		const i = winListeners.findIndex((l) => l.type === type && l.handler === handler);
		if (i !== -1) winListeners.splice(i, 1);
	};
	sandbox.innerWidth = 1280;
	sandbox.innerHeight = 800;
	sandbox.window = sandbox;
	sandbox.self = sandbox;
	return { sandbox, clock };
}

// loadParts reads the named part files from ../js/, concatenates them in order, and evaluates the
// result in a fresh sandbox. The returned handle exposes the parts' global functions directly and
// falls back to resolving top-level const and let bindings by name, so a test reaches
// app.fmtDuration and app.NOTIFY_KINDS the same way.
export function loadParts(names) {
	const { sandbox, clock } = makeSandbox();
	const context = vm.createContext(sandbox);
	let source = "";
	for (const name of names) {
		source += readFileSync(new URL("../js/" + name, import.meta.url), "utf8") + "\n";
	}
	vm.runInContext(source, context, { filename: "app.js:" + names.join("+") });
	const handle = new Proxy(sandbox, {
		get(target, prop, receiver) {
			const value = Reflect.get(target, prop, receiver);
			if (value !== undefined || typeof prop !== "string" || !IDENT.test(prop)) return value;
			try {
				return vm.runInContext(prop, context);
			} catch {
				return undefined;
			}
		},
	});
	LOADED.set(handle, { sandbox, context, clock });
	return handle;
}

// ALL_PARTS is every source part in the order the server concatenates them, for a test that drives
// a whole page rather than one function and needs whatever that page reaches for.
export const ALL_PARTS = [
	"01-boot.js", "02-page-data.js", "03-page-docs.js", "04-tour.js", "05-workflow-editor.js",
	"06-workflow-canvas.js", "07-nav-theme.js", "08-auth-status.js", "09-audit.js",
	"10-modals-credentials.js", "11-projects-migrate.js", "12-templates-notify.js",
	"13-fileviewer-inventory.js", "14-inventories-workers.js", "15-overview-doctor.js",
	"16-runs-list.js", "17-cron.js", "18-host-page.js", "19-cron-preview.js",
	"20-held-copy-stream.js", "21-user-profile.js", "22-run-detail.js", "23-run-matrix.js",
];

// sandboxOf returns the raw globals behind a loaded handle, for the helpers that install stubs.
export function sandboxOf(app) {
	const loaded = LOADED.get(app);
	if (!loaded) throw new Error("jstest: handle was not produced by loadParts");
	return loaded.sandbox;
}

// clockOf returns the virtual clock a loaded handle runs on, so a test can advance time.
export function clockOf(app) {
	const loaded = LOADED.get(app);
	if (!loaded) throw new Error("jstest: handle was not produced by loadParts");
	return loaded.clock;
}
