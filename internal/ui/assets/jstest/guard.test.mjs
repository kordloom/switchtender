// Tests for the double-submit guard in 10-modals-credentials.js and for the launch dialogs that
// use it. Every one of these buttons starts a real run against real hosts, so a second click while
// the first request is still in flight has to be dropped rather than launched.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts([
	// 09-audit.js carries the shared modal helpers both launch dialogs open through.
	"01-boot.js", "07-nav-theme.js", "08-auth-status.js", "09-audit.js", "10-modals-credentials.js",
	"12-templates-notify.js", "13-fileviewer-inventory.js", "20-held-copy-stream.js",
]);

// deferred hands back a promise and its settle functions, so a test can hold a launch open and
// click again while it is still in flight.
function deferred() {
	let resolve, reject;
	const promise = new Promise((res, rej) => { resolve = res; reject = rej; });
	return { promise, resolve, reject };
}

// fakeButton is the part of a button the guard touches.
const fakeButton = () => ({ disabled: false });

test("guardedSubmit drops a second click while the first is in flight", async () => {
	const btn = fakeButton();
	const d = deferred();
	let calls = 0;
	let disabledOnEntry = null;
	const submit = app.guardedSubmit(btn, async () => {
		calls++;
		disabledOnEntry = btn.disabled;
		await d.promise;
	});

	const first = submit();
	// The control is disabled before the await, not after it, which is the whole point.
	assert.equal(btn.disabled, true);
	assert.equal(disabledOnEntry, true);
	const second = submit();
	const third = submit();
	d.resolve();
	await Promise.all([first, second, third]);
	assert.equal(calls, 1);
});

test("guardedSubmit keeps the control disabled after a launch succeeds", async () => {
	// Success navigates to the new run, so re-enabling would only invite a second launch at the
	// same hosts while the page is still tearing down.
	const btn = fakeButton();
	let calls = 0;
	const submit = app.guardedSubmit(btn, async () => { calls++; });
	await submit();
	assert.equal(btn.disabled, true);
	await submit();
	assert.equal(calls, 1);
});

test("guardedSubmit re-enables the control and reports the error on failure", async () => {
	const btn = fakeButton();
	let calls = 0;
	const seen = [];
	const submit = app.guardedSubmit(btn, async () => {
		calls++;
		if (calls === 1) throw new Error("HTTP 500");
	}, (err) => seen.push(err.message));

	await submit();
	assert.deepEqual(seen, ["HTTP 500"]);
	assert.equal(btn.disabled, false);
	// The operator can fix the input and try again, and the retry runs.
	await submit();
	assert.equal(calls, 2);
	assert.equal(btn.disabled, true);
});

test("guardedSubmit passes its arguments through and rethrows with no reporter", async () => {
	const seen = [];
	const submit = app.guardedSubmit(null, async (a, b) => { seen.push([a, b]); });
	await submit("payload", 2);
	assert.deepEqual(seen, [["payload", 2]]);

	const boom = app.guardedSubmit(null, async () => { throw new Error("nope"); });
	await assert.rejects(boom(), /nope/);
});

// Below the helper, the three launch dialogs are driven through a stub DOM, because the bug that
// mattered was in the wiring: a guard that exists but is not wired in still launches twice.

// stubDOM points document.getElementById and document.querySelector at a registry of fake
// elements and returns the registry, so a dialog can be opened and its button clicked.
function stubDOM(ids) {
	const registry = {};
	for (const id of ids) {
		registry[id] = {
			id, value: "", checked: false, hidden: true, disabled: false, textContent: "",
			innerHTML: "", selectedOptions: [], dataset: {}, style: {}, attrs: {},
			appendChild(child) { return child; },
			addEventListener() {},
			// A dialog announces itself and takes focus when it opens, so the double has to answer
			// those the way an element does; without them this stub only proves the code never
			// touched the page it is standing in for.
			setAttribute(name, value) { this.attrs[name] = String(value); },
			getAttribute(name) { return this.attrs[name] ?? null; },
			focus() { registry.focused = this; },
			querySelector() { return null; },
			querySelectorAll() { return []; },
		};
	}
	app.document.getElementById = (id) => registry[id] || null;
	app.document.querySelector = () => null;
	return registry;
}

// launchCalls installs a fetch that never settles until released, counting the launches it was
// asked to make. Returns the counter and the release function.
function launchCalls() {
	const state = { count: 0, release: null };
	const d = deferred();
	state.release = () => d.resolve();
	app.fetch = () => {
		state.count++;
		return d.promise.then(() => ({
			ok: true, status: 200, json: async () => ({ id: "run-1" }),
		}));
	};
	return state;
}

test("the survey dialog launches once for a double click", async () => {
	const els = stubDOM([
		"survey-modal", "survey-form", "survey-title", "survey-status", "survey-cancel", "survey-go",
	]);
	const calls = launchCalls();
	app.openSurvey({ id: "tpl-1", name: "Deploy", survey: [] });

	const go = els["survey-go"];
	const clicks = [go.onclick(), go.onclick(), go.onclick()];
	assert.equal(calls.count, 1);
	assert.equal(go.disabled, true);
	calls.release();
	await Promise.all(clicks);
	assert.equal(calls.count, 1);
});

test("the launch-with-overrides dialog launches once for a double click", async () => {
	const els = stubDOM([
		"prompt-modal", "prompt-title", "prompt-status", "prompt-limit", "prompt-vars",
		"prompt-labels", "prompt-dry-run", "prompt-survey", "prompt-inventory",
		"prompt-field-credentials", "prompt-credentials", "prompt-close", "prompt-go",
	]);
	const calls = launchCalls();
	// openPromptLaunch fetches the inventory list first; that call has to settle before the button
	// is wired, so the stub answers it and only then becomes the launch counter.
	app.fetch = async () => ({ ok: true, status: 200, json: async () => ({ inventories: [] }) });
	await app.openPromptLaunch({ id: "tpl-1", name: "Deploy", survey: [] });
	launchCallsInto(calls);

	const go = els["prompt-go"];
	const clicks = [go.onclick(), go.onclick()];
	assert.equal(calls.count, 1);
	assert.equal(go.disabled, true);
	calls.release();
	await Promise.all(clicks);
	assert.equal(calls.count, 1);
});

test("rejected overrides leave the launch button usable", async () => {
	// Validation happens outside the guard, so bad JSON in the extra vars box reports and leaves
	// the button live rather than locking the dialog for good.
	const els = stubDOM([
		"prompt-modal", "prompt-title", "prompt-status", "prompt-limit", "prompt-vars",
		"prompt-labels", "prompt-dry-run", "prompt-survey", "prompt-inventory",
		"prompt-field-credentials", "prompt-credentials", "prompt-close", "prompt-go",
	]);
	const calls = { count: 0 };
	app.fetch = async () => ({ ok: true, status: 200, json: async () => ({ inventories: [] }) });
	await app.openPromptLaunch({ id: "tpl-1", name: "Deploy", survey: [] });
	app.fetch = async () => {
		calls.count++;
		return { ok: true, status: 200, json: async () => ({ id: "run-1" }) };
	};

	els["prompt-vars"].value = "{not json";
	const go = els["prompt-go"];
	await go.onclick();
	assert.equal(calls.count, 0);
	assert.equal(go.disabled, false);
	assert.equal(els["prompt-status"].textContent, "Extra vars must be valid JSON.");

	// Corrected, the same button launches.
	els["prompt-vars"].value = '{"env":"prod"}';
	await go.onclick();
	assert.equal(calls.count, 1);
});

// launchCallsInto re-points fetch at an existing counter, for a dialog that had to fetch before
// its launch button was wired.
function launchCallsInto(state) {
	const d = deferred();
	state.release = () => d.resolve();
	state.count = 0;
	app.fetch = () => {
		state.count++;
		return d.promise.then(() => ({
			ok: true, status: 200, json: async () => ({ id: "run-1" }),
		}));
	};
}

test("the run launch form launches once for a double submit", async () => {
	const submits = [];
	const els = stubDOM([
		"launch-form", "launch-tool", "launch-project", "launch-inventory-id", "launch-command",
		"launch-field-command", "launch-playbook", "launch-inventory", "launch-shards",
		"launch-queue", "launch-credentials", "launch-dry-run", "launch-require-approval",
		"launch-status",
	]);
	const form = els["launch-form"];
	form.addEventListener = (name, fn) => { if (name === "submit") submits.push(fn); };
	const button = fakeButton();
	form.querySelector = (sel) => (sel === 'button[type="submit"]' ? button : null);
	els["launch-tool"].value = "ansible";
	els["launch-shards"].value = "0";
	// fillCredentialPicker and fillSelect fetch on wiring; answer them, then count launches.
	app.fetch = async () => ({ ok: true, status: 200, json: async () => ({}) });
	app.wireLaunchForm();
	const calls = launchCalls();

	const submit = submits[0];
	let prevented = 0;
	const event = { preventDefault: () => { prevented++; } };
	submit(event);
	submit(event);
	// Every submit is canceled, including the one the guard drops, or the dropped one would post
	// the form the old fashioned way.
	assert.equal(prevented, 2);
	assert.equal(calls.count, 1);
	assert.equal(button.disabled, true);
	calls.release();
	await new Promise((r) => setTimeout(r, 0));
	assert.equal(calls.count, 1);
});
