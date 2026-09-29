// failures.mjs is the shared table the failure-path suites drive: every "Loading ..." placeholder
// the templates ship, the loader that is supposed to replace it, and the small helpers those
// suites need. It is a plain module rather than a test file so importing it runs no tests.

import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// PARTS is every part the server assembles into app.js.
export const PARTS = ALL_PARTS;

// settle drains chained awaits. One flush releases only the microtasks already queued, and every
// loader here awaits at least twice, so a single flush leaves the later awaits pending.
export async function settle(clock, rounds) {
	for (let i = 0; i < (rounds || 12); i++) await clock.flush();
}

// PLACEHOLDER_PAGES is every "Loading ..." placeholder in ../../templates, paired with the loader
// the boot dispatch runs for that page and whatever the page carries on its body. Keep it in step
// with the templates: a placeholder with no entry here is a spinner nobody has proven ever stops.
export const PLACEHOLDER_PAGES = [
	{ page: "activity", text: "Loading activity.", load: (app) => app.loadActivityPage() },
	{ page: "audit", text: "Loading audit trail.", load: (app) => app.loadAudit() },
	{ page: "compare", text: "Loading comparison.", load: (app) => app.loadCompare(),
		vars: { RunID: "run_1" } },
	{ page: "credentials", text: "Loading credentials.", load: (app) => app.loadCredentials() },
	{ page: "detail", text: "Loading run.", load: (app) => app.loadDetail("run_1"),
		vars: { RunID: "run_1", MatrixCap: "2000" } },
	{ page: "doctor", text: "Running checks.", load: (app) => app.loadDoctor() },
	{ page: "drift", text: "Loading drift status.", load: (app) => app.loadDrift() },
	{ page: "fleet", text: "Loading fleet health.", load: (app) => app.loadFleet() },
	{ page: "host", text: "Loading host history.", load: (app) => app.loadHost("web01"),
		vars: { Host: "web01" } },
	{ page: "inventories", text: "Loading inventories.", load: (app) => app.loadInventories() },
	{ page: "jobtemplates", text: "Loading templates.", load: (app) => app.loadTemplates() },
	{ page: "overview", text: "Loading overview.", load: (app) => app.loadOverview() },
	{ page: "policies", text: "Loading policies.", load: (app) => app.loadPolicies() },
	{ page: "projects", text: "Loading projects.", load: (app) => app.loadProjects() },
	{ page: "runs", text: "Loading runs.", load: (app) => app.loadRuns() },
	{ page: "schedules", text: "Loading schedules.", load: (app) => app.loadSchedules() },
	{ page: "sources", text: "Loading sources.", load: (app) => app.loadSources() },
	{ page: "tasks", text: "Loading task trends.", load: (app) => app.loadTasks() },
	{ page: "users", text: "Loading users.", load: (app) => app.loadUsers() },
	{ page: "workers", text: "Loading workers.", load: (app) => app.loadWorkers() },
];

// pageEntry returns one placeholder page's entry by name.
export function pageEntry(name) {
	const entry = PLACEHOLDER_PAGES.find((e) => e.page === name);
	if (!entry) throw new Error("jstest: no placeholder page named " + name);
	return entry;
}

// statusLine returns the status element and what a reader would see in it: a hidden line reads as
// empty however much text is still parked inside, since setStatus("") only hides it.
export function statusLine(document, id) {
	const el = document.getElementById(id || "status");
	if (!el) return { el: null, visible: "", text: "" };
	const text = (el.textContent || "").trim();
	return { el, text, visible: el.hidden ? "" : text };
}

// drive mounts a page, answers every request it makes with one canned response, runs its loader,
// and lets the chained awaits finish. It returns the loaded page plus the URLs that were asked for.
export async function drive(entry, response, options) {
	const opts = options || {};
	const seen = [];
	const routes = [[(req) => { seen.push(req.url); return true; }, response]];
	const loaded = loadPage(entry.page, {
		parts: PARTS, routes, vars: entry.vars || {}, quiet: true, search: opts.search || "",
	});
	if (opts.before) opts.before(loaded);
	await entry.load(loaded.app);
	await settle(loaded.clock, opts.rounds);
	return Object.assign({ seen }, loaded);
}
