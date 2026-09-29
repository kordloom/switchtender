// Failure-path coverage for every page that ships a "Loading ..." placeholder. The suite so far
// drives each page's happy path, so a loader that throws before it clears its own placeholder
// leaves the reader looking at "Loading runs." forever with nothing saying the server refused.
// Each case here answers the page's endpoints with a broken response and asserts the placeholder
// stopped being visible, and that what replaced it names the failure rather than going blank.
//
// A visible placeholder is the assertion rather than the text, because setStatus("") hides the
// line without clearing it: the words stay in the DOM on the success path, and a reader sees
// nothing. What matters is whether "Loading runs." is still on the screen.
import { test } from "node:test";
import assert from "node:assert/strict";

import { reply, textReply, failWith } from "./net.mjs";
import { drive, PLACEHOLDER_PAGES, statusLine } from "./failures.mjs";

// BROKEN_SHAPE is a body with the right keys carrying the wrong types, which is what a partial
// outage or a version skew produces: the status code is fine, so nothing before the render looks.
const BROKEN_SHAPE = {
	runs: "not a list", hosts: null, entries: 7, policies: { a: 1 }, users: "x",
	templates: 3, projects: false, inventories: "y", sources: 1, workers: "z",
	credentials: 0, schedules: "s", tasks: {}, findings: "f", events: "e",
};

// FAILURES is the set of broken answers every placeholder page is driven against.
const FAILURES = [
	{
		name: "a 500",
		response: () => reply({ error: "boom" }, { status: 500 }),
		wantWords: true,
	},
	{
		name: "a dropped connection",
		response: () => failWith(new Error("network down")),
		wantWords: true,
	},
	{
		// A proxy, a captive portal, or a sign-in redirect answers 200 with HTML. res.json() then
		// rejects with a SyntaxError, which is not a shape any of these loaders check for.
		name: "an HTML body behind a 200",
		response: () => textReply("<!doctype html><html><body>Sign in</body></html>",
			{ headers: { "content-type": "text/html" } }),
		wantWords: true,
	},
	{
		// An empty body parses to null, so every `data.things || []` becomes a TypeError. That is a
		// throw from inside the try, which proves the catch covers the render and not only the fetch.
		name: "an empty 200 body",
		response: () => textReply("", { headers: { "content-type": "application/json" } }),
		wantWords: true,
	},
	{
		name: "a truncated JSON body",
		response: () => textReply('{"runs": [{"id": "run_1"', { headers: { "content-type": "application/json" } }),
		wantWords: true,
	},
	{
		name: "a body of the wrong shape",
		response: () => reply(BROKEN_SHAPE),
		// A wrong-shaped body can legitimately read as an empty list, so the words are not required;
		// what is required is that the placeholder stops being shown.
		wantWords: false,
	},
];

for (const entry of PLACEHOLDER_PAGES) {
	for (const failure of FAILURES) {
		test(`${entry.page}: ${failure.name} clears the loading placeholder`, async () => {
			const { document, seen } = await drive(entry, failure.response());
			assert.ok(seen.length > 0,
				`${entry.page} never called the API, so nothing can ever clear its placeholder`);
			const status = statusLine(document);
			assert.notEqual(status.visible, entry.text,
				`${entry.page} still shows "${entry.text}" after ${failure.name}`);
			if (!failure.wantWords) return;
			assert.ok(status.visible.length > 0,
				`${entry.page} went blank after ${failure.name} instead of saying what went wrong`);
			assert.ok(/fail|error|could not|unavailable|refus|500|down/i.test(status.visible),
				`${entry.page} said "${status.visible}" after ${failure.name}, which does not read as a failure`);
		});
	}
}

test("the users page clears its token placeholder too, not only the account one", async () => {
	// users.html carries a second placeholder, token-list-status, loaded by its own call. A page
	// with two lists has two ways to hang, and only one of them is the page's main status line.
	const entry = { page: "users", text: "Loading tokens.", load: (app) => app.loadTokens() };
	const { document } = await drive(entry, reply({ error: "boom" }, { status: 500 }));
	const status = statusLine(document, "token-list-status");
	assert.notEqual(status.visible, "Loading tokens.",
		"the token list still reads \"Loading tokens.\" after the server returned 500");
	assert.ok(status.visible.length > 0, "the token list went blank instead of naming the failure");
});

// GOOD is a well-formed one-record answer under every list key at once, so one body serves
// whichever page is being driven. The brief for this suite is that every prefilled placeholder is
// proven to clear on success as well as on failure; the loop below is the success half.
const GOOD = (() => {
	const one = [{
		id: "id_1", run_id: "run_1", name: "prod", host: "web01.example", task: "install nginx",
		owner: "worker-1", actor: "operator", actor_type: "human", on_behalf_of: "",
		playbook: "playbooks/site.yml", command: "site.yml", tool: "ansible",
		status: "succeeded", worst: "ok", outcome: "ok", last_outcome: "ok", effect: "hold",
		method: "POST", path: "/v1/runs", hash: "a".repeat(64), prev_hash: "b".repeat(64),
		content_digest: "sha256:abc", url: "https://example.test/repo.git", branch: "main",
		description: "the production inventory", kind: "run", email: "op@example.test",
		role: "operator", problem: "missing credential", object_name: "nightly",
		object_id: "tpl_1", object_type: "template", fix_path: "/ui/templates",
		severity: "broken", cron: "0 2 * * *", timezone: "UTC", template_id: "tpl_1",
		created_at: "2026-08-04T12:00:00Z", at: "2026-08-04T12:00:00Z",
		ran_at: "2026-08-04T12:00:00Z", last_run: "2026-08-04T12:00:00Z",
		last_seen: "2026-08-04T12:00:00Z", started_at: "2026-08-04T12:00:00Z",
		finished_at: "2026-08-04T12:00:10Z", next_run_at: "2026-08-05T02:00:00Z",
		seq: 1, failures: 0, total: 5, ok: 5, changed: 0, unreachable: 0, active: 1,
		completed: 4, failed: 0, drifted_tasks: 0, tasks: 3, runs: 5, host_count: 1,
		avg_seconds: 1.5, last_seconds: 1.4, duration_seconds: 10, enabled: true,
		flaky: false, recent: ["ok", "ok"], recent_runs: ["run_a", "run_b"], hosts: ["web01"],
	}];
	return {
		runs: one, hosts: one, entries: one, policies: one, users: one, templates: one,
		projects: one, inventories: one, sources: one, workers: one, credentials: one,
		schedules: one, tasks: one, findings: one, tokens: one, events: [], shards: [], steps: [],
		count: 1, has_more: false, next_after: 0, summary: { total: 1, succeeded: 1, failed: 0 },
		checked_templates: 1, checked_schedules: 1, checked_credentials: 1,
		// The comparison page reads its own document rather than a list.
		a: { id: "run_new", status: "succeeded" }, b: { id: "run_old", status: "succeeded" },
		same_source: true, duration_delta_seconds: -2,
		totals: { ok: 1, broke: 0, recovered: 0, still_failing: 0, added: 0, removed: 0 },
		id: "run_1", status: "succeeded", log: "TASK [install nginx] ok: [web01]",
	};
})();

// GOOD_OVERRIDES cover the two pages whose records are not shaped like a list row: the task trend
// spark is a list of durations rather than outcomes, and a comparison's tasks carry both sides.
const GOOD_OVERRIDES = {
	tasks: { tasks: [Object.assign({}, GOOD.tasks[0], { recent: [1.4, 1.5, 1.6] })] },
	compare: {
		tasks: [{ task: "install nginx", a_seconds: 1.4, b_seconds: 1.6, delta_seconds: -0.2 }],
		hosts: [{ host: "web01", verdict: "ok", a: { worst: "ok", failures: 0, changed: 0 },
			b: { worst: "ok", failures: 0, changed: 0 } }],
	},
};

for (const entry of PLACEHOLDER_PAGES) {
	test(`${entry.page}: a good answer stops showing the loading placeholder`, async () => {
		const { document } = await drive(entry,
			reply(Object.assign({}, GOOD, GOOD_OVERRIDES[entry.page] || {})));
		const status = statusLine(document);
		assert.notEqual(status.visible, entry.text,
			`${entry.page} still shows "${entry.text}" after the server answered normally`);
		assert.ok(!/fail|error|unavailable|could not/i.test(status.visible),
			`${entry.page} reported a good answer as a failure: "${status.visible}"`);
	});
}

test("the users page stops showing its token placeholder on a good answer", async () => {
	const entry = { page: "users", text: "Loading tokens.", load: (app) => app.loadTokens() };
	const { document } = await drive(entry, reply(GOOD));
	const status = statusLine(document, "token-list-status");
	assert.notEqual(status.visible, "Loading tokens.",
		"the token list still reads \"Loading tokens.\" after the server answered normally");
});
