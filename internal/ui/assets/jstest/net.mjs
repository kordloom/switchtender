// net.mjs is the controllable network for the sandbox: a fetch that answers from routes and records
// every call, and an EventSource that records the URL it was opened with and can emit events on
// demand. Both exist so a test asserts what the page asked the server for, which is the part of a
// handler a pure helper test never sees.

import { sandboxOf } from "./loader.mjs";

// SPEC marks the objects the response helpers build, so a plain payload is never mistaken for one.
const SPEC = Symbol("jstest response spec");

// reply answers with a JSON body, 200 unless init says otherwise.
export function reply(body, init) {
	return { [SPEC]: "json", body, init: init || {} };
}

// textReply answers with a plain text body.
export function textReply(text, init) {
	return { [SPEC]: "text", body: String(text), init: init || {} };
}

// failWith rejects the fetch, which is how a dropped connection reaches the page rather than an
// error status the page can read.
export function failWith(error) {
	return { [SPEC]: "fail", error: error instanceof Error ? error : new Error(String(error)) };
}

// delayed holds a response back for a virtual delay, so a test can order two in-flight requests
// against each other. The delay is on the sandbox clock, so nothing settles until a tick.
export function delayed(ms, value) {
	return { [SPEC]: "delay", ms, value };
}

// sequence answers one route with a different response per call, in order. Running past the end is
// a test that expected fewer requests than the page made, so it fails rather than repeating.
export function sequence(...values) {
	return { [SPEC]: "sequence", queue: values.slice(), used: 0 };
}

// deferred hands back a promise and its settle functions, for a test that has to hold a request
// open while it does something else.
export function deferred() {
	let resolve, reject;
	const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
	return { promise, resolve, reject };
}

// installFetch replaces the sandbox's fetch with one answering from routes, and returns the
// recorder. Routes may be an object keyed by URL prefix or an array of matcher and response pairs;
// a matcher is a prefix string, a regular expression, or a predicate over the request. A response
// is a plain payload sent as JSON, one of the helpers above, or a function of the request returning
// either. Any URL no route claims fails loudly, naming the URL: a silent empty answer is how a
// broken request passes for a working one.
export function installFetch(app, routes, options) {
	const sandbox = sandboxOf(app);
	const handle = {
		// calls records every request in order.
		calls: [],
		// unmatched records the URLs no route answered.
		unmatched: [],
		// routes holds the matcher and response pairs, newest last.
		routes: normalizeRoutes(routes),

		// urls lists the URLs requested so far.
		get urls() { return handle.calls.map((c) => c.url); },

		// route adds a matcher, which wins over any earlier one it overlaps.
		route(matcher, response) {
			handle.routes.push({ matcher, response });
			return handle;
		},

		// only replaces the whole route table, for a test whose second phase answers differently.
		only(newRoutes) {
			handle.routes = normalizeRoutes(newRoutes);
			return handle;
		},

		// calledWith returns every recorded call whose URL contains the fragment.
		calledWith(fragment) {
			return handle.calls.filter((c) => c.url.includes(fragment));
		},

		// assertClean throws when any request went unanswered, which a test calls at the end so a
		// handler that quietly swallowed a failed request cannot pass.
		assertClean() {
			if (handle.unmatched.length) {
				throw new Error("jstest fetch: no route answered " + handle.unmatched.join(", "));
			}
		},
	};

	sandbox.fetch = async (input, init) => {
		const url = String(input);
		const opts = init || {};
		const query = url.indexOf("?");
		const request = {
			url,
			path: query === -1 ? url : url.slice(0, query),
			search: query === -1 ? "" : url.slice(query),
			params: new URLSearchParams(query === -1 ? "" : url.slice(query + 1)),
			method: (opts.method || "GET").toUpperCase(),
			headers: Object.assign({}, opts.headers),
			body: opts.body,
		};
		handle.calls.push(request);
		const route = handle.routes.find((r) => matches(r.matcher, request));
		if (!route) {
			handle.unmatched.push(request.method + " " + url);
			const message = "jstest fetch: no route for " + request.method + " " + url;
			if (!options || !options.quiet) console.error(message);
			throw new Error(message);
		}
		return settle(route.response, request, sandbox);
	};
	return handle;
}

// normalizeRoutes accepts either an object keyed by prefix or a list of pairs and returns pairs.
function normalizeRoutes(routes) {
	if (!routes) return [];
	if (Array.isArray(routes)) {
		return routes.map((r) => (Array.isArray(r) ? { matcher: r[0], response: r[1] } : r));
	}
	return Object.entries(routes).map(([matcher, response]) => ({ matcher, response }));
}

// matches tests one route matcher against a request.
function matches(matcher, request) {
	if (typeof matcher === "function") return Boolean(matcher(request));
	if (matcher instanceof RegExp) return matcher.test(request.url);
	return request.url === String(matcher) || request.url.startsWith(String(matcher));
}

// settle turns whatever a route holds into the response the page receives, resolving functions,
// promises, sequences, and delays until a body is left.
async function settle(value, request, sandbox) {
	if (typeof value === "function") return settle(await value(request), request, sandbox);
	if (value && typeof value.then === "function") return settle(await value, request, sandbox);
	if (!value || !value[SPEC]) return makeResponse(request.url, 200, JSON.stringify(value ?? {}));
	switch (value[SPEC]) {
	case "json":
		return makeResponse(request.url, value.init.status || 200, JSON.stringify(value.body ?? {}),
			value.init.headers);
	case "text":
		return makeResponse(request.url, value.init.status || 200, value.body, value.init.headers);
	case "fail":
		throw value.error;
	case "sequence":
		if (!value.queue.length) {
			throw new Error("jstest fetch: route ran out of queued responses at request " +
				(value.used + 1) + " for " + request.url);
		}
		value.used++;
		return settle(value.queue.shift(), request, sandbox);
	case "delay":
		await new Promise((resolve) => sandbox.setTimeout(resolve, value.ms));
		return settle(value.value, request, sandbox);
	default:
		throw new Error("jstest fetch: unknown response kind " + String(value[SPEC]));
	}
}

// makeResponse builds the part of a Response the script reads.
function makeResponse(url, status, bodyText, headers) {
	const table = new Map(Object.entries(headers || {}).map(([k, v]) => [k.toLowerCase(), String(v)]));
	return {
		url,
		status,
		ok: status >= 200 && status < 300,
		statusText: String(status),
		headers: {
			get: (name) => {
				const key = String(name).toLowerCase();
				return table.has(key) ? table.get(key) : null;
			},
		},
		text: async () => bodyText,
		json: async () => JSON.parse(bodyText || "null"),
	};
}

// installEventSource replaces the sandbox's EventSource with a recorder. Each stream keeps the URL
// it was opened with, so a test asserts the resume cursor the page actually asked for rather than
// what a helper returned, and can push events into the page on demand.
export function installEventSource(app) {
	const sandbox = sandboxOf(app);
	const handle = {
		// sources lists every stream opened, in order.
		sources: [],

		// last returns the most recently opened stream.
		last() { return handle.sources[handle.sources.length - 1] || null; },

		// urls lists the URLs the page opened streams with.
		get urls() { return handle.sources.map((s) => s.url); },
	};

	// TestEventSource records its URL and dispatches only what a test emits.
	class TestEventSource {
		// constructor records the stream and leaves it open.
		constructor(url) {
			this.url = String(url);
			this.readyState = 1;
			this.closed = false;
			this.listeners = new Map();
			this.onopen = null;
			this.onerror = null;
			this.onmessage = null;
			handle.sources.push(this);
		}

		// addEventListener registers a named stream listener.
		addEventListener(type, fn) {
			const list = this.listeners.get(type) || [];
			list.push(fn);
			this.listeners.set(type, list);
		}

		// removeEventListener drops a stream listener.
		removeEventListener(type, fn) {
			this.listeners.set(type, (this.listeners.get(type) || []).filter((f) => f !== fn));
		}

		// close marks the stream shut, which is what the page does at the end signal.
		close() {
			this.closed = true;
			this.readyState = 2;
		}

		// emit delivers one server event to the page. The payload is serialized, since every event
		// this server sends is JSON and the page parses it as such. It returns whatever the
		// listeners returned, so a test can await an async end handler.
		emit(type, data) {
			return this.emitRaw(type, data === undefined ? undefined : JSON.stringify(data));
		}

		// emitRaw delivers a payload already in wire form, for testing what the page does with one
		// it cannot parse.
		emitRaw(type, text) {
			const event = { type, data: text, lastEventId: "", target: this };
			const results = [];
			for (const fn of (this.listeners.get(type) || []).slice()) results.push(fn(event));
			const inline = this["on" + type];
			if (typeof inline === "function") results.push(inline(event));
			return Promise.all(results);
		}
	}

	sandbox.EventSource = TestEventSource;
	return handle;
}
