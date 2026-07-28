"use strict";

// A stored flat theme is stamped before anything renders, so a forced theme never flashes the
// signature default. A ?theme= query parameter, named by theme, picks and persists one, so a
// themed link is shareable. The script sits at the end of body, so document.body exists.
(function () {
	let theme = null;
	try { theme = localStorage.getItem("st_theme"); } catch { /* storage may be unavailable */ }
	const param = new URLSearchParams(location.search).get("theme");
	if (param) {
		const byName = { stitch: "signature", kord: "light", seal: "dark" };
		const key = byName[param.toLowerCase()];
		if (key === "light" || key === "dark") theme = key;
		else if (key === "signature") theme = null;
		if (key) {
			try {
				if (theme) localStorage.setItem("st_theme", theme);
				else localStorage.removeItem("st_theme");
			} catch { /* storage may be unavailable */ }
		}
	}
	if (theme === "light" || theme === "dark") document.body.dataset.theme = theme;
	syncBrandLogos();
})();

// syncBrandLogos points every brand picture at artwork the active theme can show: a forced light
// theme needs the ink logo and a forced dark theme the light one. A forced theme removes the
// media-driven source and sets the image directly, since Safari does not reliably re-evaluate a
// picture whose source attributes change; the default restores the source to follow the OS.
function syncBrandLogos() {
	const theme = document.body.dataset.theme;
	for (const pic of document.querySelectorAll(".brand picture, .side-brand picture")) {
		const img = pic.querySelector("img");
		if (!img) continue;
		const source = pic.querySelector("source");
		// The removed source is kept on the picture so the default theme can restore it.
		if (source && !pic.stSource) pic.stSource = source;
		if (theme === "light" || theme === "dark") {
			if (source) source.remove();
			const want = theme === "dark"
				? "/ui/assets/logo-train-tracks-dark.png"
				: "/ui/assets/logo-train-tracks.png";
			if (img.getAttribute("src") !== want) img.src = want;
		} else if (pic.stSource && !source) {
			pic.insertBefore(pic.stSource, img);
			img.src = "/ui/assets/logo-train-tracks.png";
		}
	}
}

// API is the versioned base path every server call is made under. Infrastructure routes such as
// the UI shell and the sign-on redirect are served unversioned and are not reached through this.
const API = "/v1";

// OUTCOME_RANK orders outcomes from least to most severe for rollups.
const OUTCOME_RANK = { skipped: 0, ok: 1, changed: 2, unreachable: 3, failed: 4 };

// NAV_GROUPS defines the drawer navigation, grouped by concern. Items marked admin are hidden from
// signed in non-admins; the server still enforces the real policy.
const NAV_GROUPS = [
	{ label: "Execution", items: [
		{ key: "overview", href: "/ui/", label: "Overview", desc: "At a glance" },
		{ key: "runs", href: "/ui/runs", label: "Runs", desc: "Every playbook execution" },
		{ key: "fleet", href: "/ui/fleet", label: "Fleet health", desc: "Flaky host detection" },
		{ key: "drift", href: "/ui/drift", label: "Drift", desc: "Divergence from desired state" },
		{ key: "tasks", href: "/ui/tasks", label: "Task trends", desc: "Duration trends per task" },
		{ key: "workers", href: "/ui/workers", label: "Workers", desc: "Executor fleet status" },
	] },
	{ label: "Automation", items: [
		{ key: "projects", href: "/ui/projects", label: "Projects", desc: "Git-sourced playbooks", admin: true },
		{ key: "inventories", href: "/ui/inventories", label: "Inventories", desc: "Stored host inventories", admin: true },
		{ key: "sources", href: "/ui/sources", label: "Sources", desc: "Dynamic inventory sync", admin: true },
		{ key: "templates", href: "/ui/templates", label: "Templates", desc: "Saved launch presets" },
		{ key: "workflows", href: "/ui/workflows", label: "Workflow", desc: "Visual pipeline builder" },
		{ key: "schedules", href: "/ui/schedules", label: "Schedules", desc: "Cron-driven runs" },
		{ key: "migrate", href: "/ui/migrate", label: "Migrate", desc: "Import from AWX or Semaphore", admin: true },
	] },
	{ label: "Access", items: [
		{ key: "credentials", href: "/ui/credentials", label: "Credentials", desc: "Secrets and keys", admin: true },
		{ key: "users", href: "/ui/users", label: "Users", desc: "Accounts and roles", admin: true },
		{ key: "audit", href: "/ui/audit", label: "Audit", desc: "Tamper-evident change log", admin: true },
		{ key: "policies", href: "/ui/policies", label: "Policies", desc: "Approval rules", admin: true },
	] },
	{ label: "Help", items: [
		{ key: "docs", href: "/ui/docs", label: "Docs", desc: "Guides and reference" },
	] },
];

// PAGE_NAV maps a page identifier to the nav key it should highlight.
const PAGE_NAV = {
	overview: "overview", runs: "runs", detail: "runs", fleet: "fleet", host: "fleet",
	tasks: "tasks", workers: "workers", drift: "drift", projects: "projects", inventories: "inventories",
	sources: "sources", jobtemplates: "templates", schedules: "schedules", workflows: "workflows",
	migrate: "migrate", credentials: "credentials", users: "users", audit: "audit",
	policies: "policies", docs: "docs",
};

// NAV_ICONS holds the inline SVG body for each nav key, stroked in the current color.
const NAV_ICONS = {
	overview: '<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/>',
	runs: '<circle cx="12" cy="12" r="9"/><polygon points="10 8 16 12 10 16"/>',
	fleet: '<path d="M3 12h4l2 6 4-12 2 6h6"/>',
	drift: '<path d="M10.29 3.86 1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/><line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/>',
	tasks: '<path d="M3 17l6-6 4 4 8-8"/><path d="M17 7h4v4"/>',
	workers: '<rect x="3" y="4" width="18" height="7" rx="1"/><rect x="3" y="13" width="18" height="7" rx="1"/><line x1="7" y1="7.5" x2="7.01" y2="7.5"/><line x1="7" y1="16.5" x2="7.01" y2="16.5"/>',
	projects: '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
	inventories: '<line x1="8" y1="6" x2="21" y2="6"/><line x1="8" y1="12" x2="21" y2="12"/><line x1="8" y1="18" x2="21" y2="18"/><line x1="3" y1="6" x2="3.01" y2="6"/><line x1="3" y1="12" x2="3.01" y2="12"/><line x1="3" y1="18" x2="3.01" y2="18"/>',
	sources: '<path d="M23 4v6h-6"/><path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"/>',
	templates: '<polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/>',
	schedules: '<circle cx="12" cy="12" r="9"/><polyline points="12 7 12 12 15 14"/>',
	workflows: '<circle cx="5" cy="6" r="2.4"/><circle cx="19" cy="6" r="2.4"/><circle cx="12" cy="18" r="2.4"/><path d="M6.7 7.6 10.6 16M17.3 7.6 13.4 16"/>',
	migrate: '<path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/>',
	credentials: '<rect x="5" y="11" width="14" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
	users: '<path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>',
	audit: '<path d="M9 5H7a2 2 0 0 0-2 2v12a2 2 0 0 0 2 2h10a2 2 0 0 0 2-2V7a2 2 0 0 0-2-2h-2"/><rect x="9" y="3" width="6" height="4" rx="1"/><path d="m9 14 2 2 4-4"/>',
	policies: '<path d="M12 3l7 3v5c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z"/><polyline points="9 12 11 14 15 10"/>',
	docs: '<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>',
};

// mountTopbar adds docs and repository links to the top bar on every page, so the guides and the
// source are one click away from anywhere in the product.
function mountTopbar() {
	const bar = document.querySelector(".topbar");
	if (!bar || bar.querySelector(".topbar-links")) return;
	if (document.body.dataset.page !== "login" && !bar.querySelector(".search-btn")) {
		const search = document.createElement("button");
		search.type = "button";
		search.className = "search-btn";
		const mac = /Mac|iPhone|iPad/.test(navigator.platform || navigator.userAgent);
		search.innerHTML = svgIcon('<circle cx="11" cy="11" r="7"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>') +
			"<span>Search</span>" + '<span class="kbd">' + (mac ? "⌘K" : "Ctrl K") + "</span>";
		search.setAttribute("aria-label", "Search pages and actions");
		search.setAttribute("aria-haspopup", "dialog");
		search.addEventListener("click", openPalette);
		const brand = bar.querySelector(".brand");
		if (brand) brand.after(search); else bar.appendChild(search);
	}
	const nav = document.createElement("nav");
	nav.className = "topbar-links";
	if (document.body.dataset.page !== "login") {
		const tourWrap = document.createElement("div");
		tourWrap.className = "tour-launch";
		const tour = document.createElement("button");
		tour.type = "button";
		tour.className = "topbar-link tour-start";
		tour.textContent = "Tour";
		tour.setAttribute("aria-haspopup", "true");
		tour.setAttribute("aria-expanded", "false");
		tour.addEventListener("click", (e) => { e.stopPropagation(); toggleTourMenu(tour, tourWrap); });
		tourWrap.appendChild(tour);
		nav.appendChild(tourWrap);
	}
	const docs = document.createElement("a");
	docs.href = "/ui/docs";
	docs.className = "topbar-link";
	docs.textContent = "Docs";
	nav.appendChild(docs);
	const gh = document.createElement("a");
	gh.href = "https://github.com/kordloom/switchtender";
	gh.className = "topbar-link topbar-icon";
	gh.target = "_blank";
	gh.rel = "noopener";
	gh.title = "View on GitHub";
	gh.setAttribute("aria-label", "View on GitHub");
	gh.innerHTML = '<svg viewBox="0 0 16 16" width="18" height="18" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.02-1.49-2.01.44-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 012-.27c.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>';
	nav.appendChild(gh);
	bar.appendChild(nav);
}

// EXPORT_PAGES are the pages whose main table gets CSV and JSON export of the shown rows.
// Credentials stays out on purpose, so secret-adjacent data never leaves by accident.
const EXPORT_PAGES = ["runs", "fleet", "drift", "tasks", "workers", "schedules", "jobtemplates",
	"users", "audit", "host", "projects", "inventories", "sources", "policies"];

// tableRowsData reads the rendered table into headers and rows, skipping the actions column and
// anything hidden, so an export matches exactly what the user sees after filtering.
function tableRowsData(table) {
	const ths = Array.from(table.tHead.rows[0].cells);
	const skip = new Set();
	const headers = [];
	ths.forEach((th, i) => {
		const label = th.textContent.trim();
		if (th.classList.contains("col-actions") || label === "Actions" || label === "") skip.add(i);
		else headers.push(label);
	});
	const rows = [];
	for (const tr of table.tBodies[0].rows) {
		if (tr.hidden || tr.classList.contains("skeleton-row")) continue;
		const row = [];
		Array.from(tr.cells).forEach((cell, i) => {
			if (skip.has(i)) return;
			row.push(cell.textContent.replace(/\s+/g, " ").trim());
		});
		rows.push(row);
	}
	return { headers, rows };
}

// downloadBlob hands the browser a generated file.
function downloadBlob(name, type, content) {
	const url = URL.createObjectURL(new Blob([content], { type }));
	const a = document.createElement("a");
	a.href = url;
	a.download = name;
	document.body.appendChild(a);
	a.click();
	a.remove();
	URL.revokeObjectURL(url);
}

// mountTableExport adds CSV and JSON export buttons beside the list filter, so any table can
// leave the app for an audit, a spreadsheet, or a colleague.
function mountTableExport() {
	const page = document.body.dataset.page;
	if (!EXPORT_PAGES.includes(page)) return;
	const table = document.querySelector("main.content table");
	if (!table || !table.tHead || !table.tBodies[0]) return;
	let host = document.querySelector(".list-filter") || document.querySelector(".runs-toolbar");
	if (!host) {
		host = document.createElement("div");
		host.className = "list-filter";
		table.parentNode.insertBefore(host, table);
	}
	const stamp = () => new Date().toISOString().slice(0, 10);
	const make = (label, tip, fn) => {
		const btn = document.createElement("button");
		btn.type = "button";
		btn.className = "button table-export";
		btn.innerHTML = svgIcon('<path d="M12 3v12"/><polyline points="7 10 12 15 17 10"/><path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/>');
		btn.appendChild(document.createTextNode(label));
		btn.dataset.tip = tip;
		btn.addEventListener("click", fn);
		host.appendChild(btn);
	};
	make("CSV", "Export the shown rows as CSV", () => {
		const { headers, rows } = tableRowsData(table);
		const esc = (v) => /[",\n]/.test(v) ? '"' + v.replaceAll('"', '""') + '"' : v;
		const csv = [headers, ...rows].map((r) => r.map(esc).join(",")).join("\n") + "\n";
		downloadBlob("switchtender-" + page + "-" + stamp() + ".csv", "text/csv", csv);
	});
	make("JSON", "Export the shown rows as JSON", () => {
		const { headers, rows } = tableRowsData(table);
		const objs = rows.map((r) => Object.fromEntries(headers.map((h, i) => [h, r[i] ?? ""])));
		downloadBlob("switchtender-" + page + "-" + stamp() + ".json", "application/json",
			JSON.stringify(objs, null, 2) + "\n");
	});
}

// PAGE_DOCS maps each page to its most relevant guide, linked from the page header.
const PAGE_DOCS = {
	overview: { slug: "quickstart", label: "Quickstart" },
	runs: { slug: "tutorial-run-a-job", label: "Run a job" },
	fleet: { slug: "reliability", label: "Reliability" },
	drift: { slug: "drift", label: "Drift detection" },
	tasks: { slug: "concepts", label: "Concepts" },
	workers: { slug: "reliability", label: "Reliability" },
	projects: { slug: "concepts", label: "Concepts" },
	inventories: { slug: "concepts", label: "Concepts" },
	sources: { slug: "concepts", label: "Concepts" },
	jobtemplates: { slug: "tutorial-save-a-template", label: "Save a template" },
	workflows: { slug: "concepts", label: "Concepts" },
	schedules: { slug: "tutorial-schedule-a-job", label: "Schedule a job" },
	migrate: { slug: "tutorial-migrate", label: "Migrate your setup" },
	credentials: { slug: "tutorial-set-a-secret", label: "Set a secret" },
	users: { slug: "configuration", label: "Configuration" },
	audit: { slug: "features", label: "Features" },
	policies: { slug: "features", label: "Features" },
};

// mountPageDocs adds a small guide link to the page header, so every page points at its docs.
function mountPageDocs() {
	const ref = PAGE_DOCS[document.body.dataset.page];
	const head = document.querySelector(".page-head");
	if (!ref || !head) return;
	const a = document.createElement("a");
	a.className = "docs-link";
	a.href = "/ui/docs/" + ref.slug;
	a.dataset.tip = "Open the " + ref.label + " guide";
	a.innerHTML = svgIcon(NAV_ICONS.docs);
	a.appendChild(document.createTextNode(ref.label));
	let actions = head.querySelector(".head-actions");
	if (!actions) {
		actions = document.createElement("div");
		actions.className = "head-actions";
		for (const child of Array.from(head.children)) {
			if (!child.classList.contains("page-head-text")) actions.appendChild(child);
		}
		head.appendChild(actions);
	}
	actions.appendChild(a);
}

// LIST_PAGES are the pages whose main table is a searchable list.
// LIST_PAGES get the client-side row filter. The runs page is excluded because it searches on the
// server, across every run rather than only the loaded page.
const LIST_PAGES = ["jobtemplates", "credentials", "projects", "inventories",
	"sources", "schedules", "users", "workers", "fleet", "tasks", "host", "policies", "drift"];

// mountListFilter adds a search box above the main list table and filters its rows by text as you
// type, so every list is searchable. It reads the rows live, so it works no matter when they load.
function mountListFilter() {
	const table = document.querySelector("main.content table");
	if (!table || !table.tBodies[0]) return;
	const tbody = table.tBodies[0];
	const wrap = document.createElement("div");
	wrap.className = "list-filter";
	const input = document.createElement("input");
	input.type = "search";
	input.className = "input list-filter-input";
	input.placeholder = "Filter this list…";
	input.setAttribute("aria-label", "Filter list");
	const count = document.createElement("span");
	count.className = "muted list-filter-count";
	wrap.appendChild(input);
	wrap.appendChild(count);
	table.parentNode.insertBefore(wrap, table);
	input.addEventListener("input", () => {
		const q = input.value.trim().toLowerCase();
		let shown = 0;
		for (const row of tbody.rows) {
			const match = q === "" || row.textContent.toLowerCase().includes(q);
			row.hidden = !match;
			if (match) shown++;
		}
		count.textContent = q ? shown + " shown" : "";
	});
}

// TOURS is the guided-tour registry. Each tour runs on one page and walks a sequence of steps; a
// step with a selector spotlights that element, and a step without one shows a centered card. The
// launcher in the top bar lists them, and the welcome tour also runs on a first visit.
const TOURS = [
	{
		id: "welcome", title: "Sixty-second tour", desc: "The whole product at a glance",
		page: "overview", path: "/ui/",
		steps: [
			{ title: "Welcome to SwitchTender", body: "One binary runs Ansible, Terraform, Bash, Python, and Go, with no Kubernetes. Here is the sixty-second tour." },
			{ sel: ".page-head .button.primary", title: "Launch any tool", body: "Start a run with Ansible, Bash, Terraform, or Python, each with a dry run, and mix them in a single pipeline." },
			{ sel: ".panel-runs", title: "Watch every run", body: "Runs stream live here, with a host matrix, sharded splits, and multi-step pipelines all in one place." },
			{ sel: "#tiles a[href='/ui/migrate']", title: "Bring your work with you", body: "Migrating from another tool? Import projects, inventories, templates, and schedules in a few clicks." },
			{ sel: ".tile-search", title: "Find anything fast", body: "This search filters instantly, and every list in SwitchTender is searchable the same way." },
			{ sel: ".side|.nav-toggle", title: "The rest of the yard", body: "Job templates, credentials with external secrets, schedules, and fleet analytics all live in the navigation." },
			{ title: "You are set", body: "Explore the demo freely. Nothing here can be broken. Replay this tour anytime from Tour in the top bar." },
		],
	},
	{
		id: "pitch", title: "Why teams switch", desc: "The sixty-second pitch, hands free",
		page: "overview", path: "/ui/", auto: true,
		steps: [
			{ title: "Run everything. Watch every host. Prove every change.", body: "Your whole automation stack in one binary, with no Kubernetes to stand up first. Sit back, this tour drives itself.", hold: 6000 },
			{ sel: ".panel-runs", title: "Watch every host", body: "Every run is a live host-by-task matrix, not a wall of scrollback. A failure shows the moment it happens, on the host it happened to.", hold: 7000 },
			{ sel: "#ask-panel", title: "Ask the fleet anything", body: "Advisory AI answers from run, health, and drift data. It proposes and never executes. Run it on local Ollama or your own cloud key.", hold: 7000 },
			{ page: "workflows", path: "/ui/workflows", sel: "#wf-canvas", title: "Drag a pipeline together", body: "Wire all seven tools, and any tool you plug in, into one graph with per-step retries. AWX's signature feature, without the Kubernetes bill.", hold: 7500 },
			{ page: "policies", path: "/ui/policies", sel: "#policy-open", title: "The gate nobody skips", body: "Policy holds a prod terraform destroy for an admin's sign-off, automatically. Approvals are enforced, not suggested.", hold: 7000 },
			{ page: "audit", path: "/ui/audit", sel: "#audit-verify", title: "Prove every change", body: "Every change links into a tamper-evident hash chain. One click verifies it here, and a signed export verifies offline.", hold: 7000 },
			{ page: "overview", path: "/ui/", sel: "#tiles a[href='/ui/migrate']", title: "Switching is one command", body: "Projects, inventories, templates, surveys, and schedules import from AWX or Semaphore in a single pass.", hold: 6500 },
			{ title: "That is the moat", body: "Running many tools is table stakes. A control plane that proves itself is not. Press Explore and try anything, nothing here can break.", hold: 8000 },
		],
	},
	{
		id: "migrate", title: "Coming from AWX", desc: "Move your automation over",
		page: "migrate", path: "/ui/migrate",
		steps: [
			{ title: "Leave AWX or Semaphore behind", body: "Import your projects, inventories, templates, surveys, and schedules in a single pass." },
			{ title: "Preview before you commit", body: "Every import runs as a dry run first, showing exactly what it will create. Apply it when it looks right." },
			{ title: "No lock-in", body: "You can export and leave anytime, too. SwitchTender earns the switch. It does not trap you." },
		],
	},
];

// tourByID returns the tour with the given id, or null when none matches.
function tourByID(id) {
	return TOURS.find((t) => t.id === id) || null;
}

// tourStepPage returns the page a step runs on, falling back to the tour's home page for steps
// that do not hop.
function tourStepPage(tour, idx) {
	const step = tour.steps[idx];
	return (step && step.page) || tour.page;
}

// toggleTourMenu opens or closes the guided-tour launcher, a small menu of the available tours
// anchored under the Tour link in the top bar.
function toggleTourMenu(button, wrap) {
	if (wrap.querySelector(".tour-menu")) {
		closeTourMenu();
		return;
	}
	const menu = document.createElement("div");
	menu.className = "tour-menu";
	menu.setAttribute("role", "menu");
	for (const t of TOURS) {
		const item = document.createElement("button");
		item.type = "button";
		item.className = "tour-menu-item";
		item.setAttribute("role", "menuitem");
		item.innerHTML = '<span class="tour-menu-title"></span><span class="tour-menu-desc muted"></span>';
		item.querySelector(".tour-menu-title").textContent = t.title;
		item.querySelector(".tour-menu-desc").textContent = t.desc;
		item.addEventListener("click", () => launchTour(t.id));
		menu.appendChild(item);
	}
	wrap.appendChild(menu);
	button.setAttribute("aria-expanded", "true");
	const first = menu.querySelector(".tour-menu-item");
	if (first) first.focus();
	window.setTimeout(() => {
		document.addEventListener("click", tourMenuOutside);
		document.addEventListener("keydown", tourMenuKey);
	}, 0);
}

// closeTourMenu removes the launcher menu and its listeners, returning focus to the Tour button
// when the close came from the keyboard.
function closeTourMenu(restoreFocus) {
	const menu = document.querySelector(".tour-menu");
	if (menu) menu.remove();
	const btn = document.querySelector(".tour-start");
	if (btn) {
		btn.setAttribute("aria-expanded", "false");
		if (restoreFocus) btn.focus();
	}
	document.removeEventListener("click", tourMenuOutside);
	document.removeEventListener("keydown", tourMenuKey);
}

// tourMenuOutside closes the launcher when a click lands outside it.
function tourMenuOutside(e) {
	if (!e.target.closest(".tour-launch")) closeTourMenu(false);
}

// tourMenuKey drives the launcher from the keyboard: arrows rove through the tours and Escape
// closes, handing focus back to the Tour button.
function tourMenuKey(e) {
	if (e.key === "Escape") {
		closeTourMenu(true);
		return;
	}
	if (e.key !== "ArrowDown" && e.key !== "ArrowUp") return;
	const items = Array.from(document.querySelectorAll(".tour-menu-item"));
	if (items.length === 0) return;
	e.preventDefault();
	const idx = items.indexOf(document.activeElement);
	const next = e.key === "ArrowDown"
		? items[(idx + 1 + items.length) % items.length]
		: items[(idx - 1 + items.length) % items.length];
	next.focus();
}

// launchTour runs a tour by id, navigating to its page first when the tour lives elsewhere and
// resuming it there through a timestamped session handoff.
function launchTour(id) {
	closeTourMenu(false);
	const tour = tourByID(id);
	if (!tour) return;
	if (tour.page === document.body.dataset.page) {
		startTour(id, { auto: !!tour.auto });
	} else {
		sessionStorage.setItem("st_tour_start", JSON.stringify({ id, auto: !!tour.auto, at: Date.now() }));
		window.location.assign(tour.path);
	}
}

// tourState holds the running tour's overlay elements and current step, or null when no tour is open.
let tourState = null;

// mountTour starts a tour requested from the launcher on another page, and shows the welcome tour
// once on a first visit to the overview. The welcome timer yields to a tour that is already
// running and to a sign-in redirect in flight.
function mountTour() {
	const pending = readPendingTour();
	if (pending) {
		const tour = tourByID(pending.id);
		if (tour && tourStepPage(tour, pending.step) === document.body.dataset.page) {
			window.setTimeout(() => startTour(pending.id, pending), 300);
			return;
		}
	}
	if (document.body.dataset.page !== "overview") return;
	if (localStorage.getItem("st_tour_done")) return;
	window.setTimeout(() => {
		if (!tourState && !window.ymRedirecting) startTour("welcome");
	}, 400);
}

// readPendingTour consumes the cross-page tour handoff, ignoring an entry older than a minute so a
// failed navigation cannot surprise-start a tour on a later organic visit.
function readPendingTour() {
	const raw = sessionStorage.getItem("st_tour_start");
	if (!raw) return null;
	sessionStorage.removeItem("st_tour_start");
	try {
		const p = JSON.parse(raw);
		if (p && p.id && Date.now() - p.at < 60000) {
			return { id: p.id, step: p.step || 0, auto: !!p.auto };
		}
	} catch { /* stale or malformed handoff */ }
	return null;
}

// startTour builds the spotlight overlay for the named tour and shows a step, the first by default
// or a later one when resuming after a page hop. Calling it while a tour runs restarts it. When
// opts.auto is set the tour drives itself, advancing on a per-step timer until paused.
function startTour(tourId, opts) {
	const tour = tourByID(tourId) || TOURS[0];
	endTour(false);
	const blocker = document.createElement("div");
	blocker.className = "tour-blocker";
	const hole = document.createElement("div");
	hole.className = "tour-hole";
	const pop = document.createElement("div");
	pop.className = "tour-pop";
	pop.setAttribute("role", "dialog");
	pop.setAttribute("aria-modal", "true");
	pop.setAttribute("aria-labelledby", "tour-title");
	pop.innerHTML =
		'<div class="tour-pop-body"><h3 class="tour-title" id="tour-title"></h3>' +
		'<p class="tour-text"></p></div>' +
		'<div class="tour-foot"><span class="tour-count muted"></span>' +
		'<div class="tour-btns">' +
		'<button type="button" class="button tour-play" aria-pressed="false" hidden>Pause</button>' +
		'<button type="button" class="button tour-skip">Skip</button>' +
		'<button type="button" class="button tour-back">Back</button>' +
		'<button type="button" class="button primary tour-next">Next</button>' +
		"</div></div>" +
		'<div class="tour-bar" aria-hidden="true"></div>';
	document.body.appendChild(blocker);
	document.body.appendChild(hole);
	document.body.appendChild(pop);

	tourState = {
		tour, step: (opts && opts.step) || 0, steps: tour.steps, blocker, hole, pop,
		auto: !!(opts && opts.auto), timer: 0,
	};
	pop.querySelector(".tour-play").addEventListener("click", () => setTourAuto(!tourState.auto));
	pop.querySelector(".tour-skip").addEventListener("click", () => endTour(true));
	pop.querySelector(".tour-back").addEventListener("click", () => moveTour(-1));
	pop.querySelector(".tour-next").addEventListener("click", () => moveTour(1));
	pop.addEventListener("keydown", (e) => {
		if (e.key !== "Tab") return;
		const focusable = pop.querySelectorAll("button:not([hidden])");
		if (focusable.length === 0) return;
		const first = focusable[0];
		const last = focusable[focusable.length - 1];
		if (e.shiftKey && document.activeElement === first) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && document.activeElement === last) {
			e.preventDefault();
			first.focus();
		}
	});
	document.addEventListener("keydown", tourKey);
	window.addEventListener("resize", tourReflow);
	window.addEventListener("scroll", tourReflow, true);
	showTourStep();
}

// moveTour advances or rewinds the tour, ending it past the last step. A step that lives on
// another page hands the tour off through sessionStorage and navigates there. Manual movement
// pauses a self-driving tour; movement from the step timer keeps it rolling.
function moveTour(delta, fromAuto) {
	if (!tourState) return;
	clearTourTimer();
	if (!fromAuto) tourState.auto = false;
	const next = tourState.step + delta;
	if (next >= tourState.steps.length) {
		endTour(true);
		return;
	}
	if (next < 0) return;
	const page = tourStepPage(tourState.tour, next);
	if (page !== document.body.dataset.page) {
		const step = tourState.steps[next];
		sessionStorage.setItem("st_tour_start", JSON.stringify({
			id: tourState.tour.id, step: next, auto: tourState.auto, at: Date.now(),
		}));
		window.location.assign(step.path || tourState.tour.path);
		return;
	}
	tourState.step = next;
	showTourStep();
}

// setTourAuto starts or stops the tour's self-advance and re-renders the step so the timer,
// progress bar, and Play control match.
function setTourAuto(on) {
	if (!tourState) return;
	tourState.auto = on;
	showTourStep();
}

// clearTourTimer cancels a pending self-advance.
function clearTourTimer() {
	if (!tourState || !tourState.timer) return;
	window.clearTimeout(tourState.timer);
	tourState.timer = 0;
}

// showTourStep fills the popover for the current step, scrolls its target into view, positions the
// spotlight, and focuses the control that drives the tour: Play while self-advancing, Next
// otherwise. On a self-driving tour it also arms the step timer and runs the progress bar.
function showTourStep() {
	if (!tourState) return;
	clearTourTimer();
	const step = tourState.steps[tourState.step];
	const { pop } = tourState;
	pop.querySelector(".tour-title").textContent = step.title;
	pop.querySelector(".tour-text").textContent = step.body;
	pop.querySelector(".tour-count").textContent = (tourState.step + 1) + " / " + tourState.steps.length;
	pop.querySelector(".tour-back").hidden = tourState.step === 0;
	const isLast = tourState.step === tourState.steps.length - 1;
	pop.querySelector(".tour-next").textContent = isLast ? "Explore" : "Next";
	pop.querySelector(".tour-skip").hidden = isLast;

	const play = pop.querySelector(".tour-play");
	play.hidden = !tourState.tour.auto;
	play.textContent = tourState.auto ? "Pause" : "Play";
	play.setAttribute("aria-pressed", tourState.auto ? "true" : "false");
	const bar = pop.querySelector(".tour-bar");
	bar.style.transition = "none";
	bar.style.width = "0";
	if (tourState.auto) {
		const hold = step.hold || 6500;
		void bar.offsetWidth;
		bar.style.transition = "width " + hold + "ms linear";
		bar.style.width = "100%";
		tourState.timer = window.setTimeout(() => moveTour(1, true), hold);
	}

	const el = tourTarget(step);
	if (el) el.scrollIntoView({ block: "center", inline: "nearest" });
	renderTourPosition();
	(tourState.auto ? play : pop.querySelector(".tour-next")).focus();
}

// renderTourPosition places the spotlight and popover for the current step without scrolling, so it
// is safe to call on scroll and resize.
function renderTourPosition() {
	if (!tourState) return;
	const step = tourState.steps[tourState.step];
	const el = tourTarget(step);
	if (el) {
		placeTourAt(el.getBoundingClientRect());
	} else {
		placeTourCentered();
	}
}

// tourTarget resolves a step's selector to the first visible match. A selector can list
// alternatives separated by a pipe, so one step can point at the docked sidebar on wide viewports
// and the drawer toggle on narrow ones.
function tourTarget(step) {
	if (!step.sel) return null;
	for (const sel of step.sel.split("|")) {
		const el = document.querySelector(sel.trim());
		if (el && el.getClientRects().length) return el;
	}
	return null;
}

// placeTourAt cuts the spotlight hole to a target rect and floats the popover below it, or above when
// there is more room there, clamped to the viewport.
function placeTourAt(rect) {
	const { hole, pop } = tourState;
	const pad = 6;
	hole.style.width = (rect.width + pad * 2) + "px";
	hole.style.height = (rect.height + pad * 2) + "px";
	hole.style.top = (rect.top - pad) + "px";
	hole.style.left = (rect.left - pad) + "px";

	const gap = 12;
	const pw = Math.min(340, window.innerWidth - 24);
	pop.style.width = pw + "px";
	const ph = pop.offsetHeight;
	let top = rect.bottom + gap;
	if (top + ph > window.innerHeight - 12 && rect.top - gap - ph > 12) {
		top = rect.top - gap - ph;
	}
	top = Math.max(12, Math.min(top, window.innerHeight - ph - 12));
	let left = rect.left + rect.width / 2 - pw / 2;
	left = Math.max(12, Math.min(left, window.innerWidth - pw - 12));
	pop.style.top = top + "px";
	pop.style.left = left + "px";
}

// placeTourCentered collapses the hole to a full dim and centers the popover for a step with no
// target.
function placeTourCentered() {
	const { hole, pop } = tourState;
	hole.style.width = "0px";
	hole.style.height = "0px";
	hole.style.top = "50%";
	hole.style.left = "50%";
	const pw = Math.min(360, window.innerWidth - 24);
	pop.style.width = pw + "px";
	pop.style.top = Math.max(12, window.innerHeight / 2 - pop.offsetHeight / 2) + "px";
	pop.style.left = (window.innerWidth / 2 - pw / 2) + "px";
}

// tourReflow repositions the current step when the window scrolls or resizes.
function tourReflow() {
	renderTourPosition();
}

// tourKey drives the tour from the keyboard: Escape ends it and arrows move between steps. Enter
// advances only when focus is not on a tour button, so a focused Back or Skip activates normally.
function tourKey(e) {
	if (!tourState) return;
	if (e.key === "Escape") {
		endTour(true);
	} else if (e.key === "Enter") {
		if (e.target.closest && e.target.closest(".tour-btns")) return;
		e.preventDefault();
		moveTour(1);
	} else if (e.key === "ArrowRight") {
		e.preventDefault();
		moveTour(1);
	} else if (e.key === "ArrowLeft") {
		e.preventDefault();
		moveTour(-1);
	}
}

// endTour tears down the overlay and its listeners, handing focus back to the Tour button so a
// keyboard user is not dropped at the top of the page. When completed is true it records that the
// tour has been seen so it does not auto-start again.
function endTour(completed) {
	if (completed) localStorage.setItem("st_tour_done", "1");
	if (!tourState) return;
	clearTourTimer();
	document.removeEventListener("keydown", tourKey);
	window.removeEventListener("resize", tourReflow);
	window.removeEventListener("scroll", tourReflow, true);
	tourState.blocker.remove();
	tourState.hole.remove();
	tourState.pop.remove();
	tourState = null;
	const btn = document.querySelector(".tour-start");
	if (btn) btn.focus();
}

// WF_CARD_W is the fixed node width used to anchor edge endpoints; WF_HANDLE_Y is the handle's
// vertical offset from a node's top, so edges leave and enter at the same height.
const WF_CARD_W = 190;
const WF_HANDLE_Y = 26;

// wfState holds the workflow graph and interaction state while the editor is open.
let wfState = null;

// wfDraftKey is the localStorage key holding the unsent workflow graph, so a refresh, a stray
// navigation, or a sign-in round trip does not lose the work.
const wfDraftKey = "st_wf_draft";

// wfHistoryCap bounds the undo stack.
const wfHistoryCap = 50;

// mountWorkflow initializes the visual workflow editor: the node graph, the canvas drag and link
// interactions, the keyboard bindings, and the toolbar that adds steps and runs the graph as a
// pipeline. Any saved draft is restored before first paint.
function mountWorkflow() {
	const canvas = document.getElementById("wf-canvas");
	if (!canvas) return;
	wfState = {
		nodes: [], edges: [], seq: 0, editing: null, canvas,
		world: canvas.querySelector(".wf-world"),
		nodesLayer: canvas.querySelector(".wf-nodes"),
		edgesLayer: canvas.querySelector(".wf-edges"),
		hint: canvas.querySelector(".wf-hint"),
		drag: null, link: null, linkFrom: null, selectedEdge: null,
		past: [], future: [], lastSnapKey: null, lastSnapAt: 0,
		opener: null, submitting: false,
		scale: 1, panX: 40, panY: 40, pan: null, pointers: new Map(), pinch: null,
	};
	document.getElementById("wf-add").addEventListener("click", () => openStepModal(null));
	document.getElementById("wf-run").addEventListener("click", runWorkflow);
	document.getElementById("wf-step-tool").addEventListener("change", syncStepFields);
	document.getElementById("wf-step-form").addEventListener("submit", saveStep);
	document.getElementById("wf-step-delete").addEventListener("click", deleteStepFromModal);
	document.getElementById("wf-step-draft-go").addEventListener("click", draftStep);
	document.getElementById("wf-step-draft").addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			e.preventDefault();
			draftStep();
		}
	});
	document.getElementById("wf-name").addEventListener("input", wfSave);
	document.getElementById("wf-inventory").addEventListener("input", wfSave);
	const modal = document.getElementById("wf-step-modal");
	const card = modal.querySelector(".modal-card");
	card.setAttribute("role", "dialog");
	card.setAttribute("aria-modal", "true");
	document.getElementById("wf-step-close").addEventListener("click", closeStepModal);
	modal.addEventListener("click", (e) => { if (e.target === modal) closeStepModal(); });
	modal.addEventListener("keydown", (e) => {
		if (e.key !== "Tab" || modal.hidden) return;
		const focusable = modal.querySelectorAll(
			"button:not([disabled]):not([hidden]), input:not([disabled]), " +
			"select:not([disabled]), textarea:not([disabled])");
		if (focusable.length === 0) return;
		const first = focusable[0];
		const last = focusable[focusable.length - 1];
		if (e.shiftKey && document.activeElement === first) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && document.activeElement === last) {
			e.preventDefault();
			first.focus();
		}
	});
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape" && !modal.hidden) { closeStepModal(); return; }
		if (e.key === "Escape" && wfState.linkFrom) {
			wfState.linkFrom = null;
			wfSetStatus("", "");
			return;
		}
		const tag = e.target.tagName;
		if (tag === "INPUT" || tag === "TEXTAREA" || tag === "SELECT" || !modal.hidden) return;
		const mod = e.metaKey || e.ctrlKey;
		if (mod && !e.shiftKey && e.key.toLowerCase() === "z") {
			e.preventDefault();
			wfUndo();
		} else if ((mod && e.shiftKey && e.key.toLowerCase() === "z") || (mod && e.key.toLowerCase() === "y")) {
			e.preventDefault();
			wfRedo();
		} else if ((e.key === "Delete" || e.key === "Backspace") && wfState.selectedEdge) {
			e.preventDefault();
			removeEdge(wfState.selectedEdge);
		} else if (e.key === "+" || e.key === "=") {
			e.preventDefault();
			zoomBy(1.2);
		} else if (e.key === "-" || e.key === "_") {
			e.preventDefault();
			zoomBy(1 / 1.2);
		} else if (e.key === "0") {
			e.preventDefault();
			resetView();
		} else if (e.key.toLowerCase() === "f") {
			e.preventDefault();
			fitView();
		}
	});
	canvas.addEventListener("pointerdown", wfPointerDown);
	canvas.addEventListener("pointermove", wfPointerMove);
	canvas.addEventListener("pointerup", wfPointerUp);
	canvas.addEventListener("pointercancel", wfCancelPointer);
	canvas.addEventListener("lostpointercapture", wfCancelPointer);
	canvas.addEventListener("wheel", wfWheel, { passive: false });
	document.getElementById("wf-zoom-in").addEventListener("click", () => zoomBy(1.2));
	document.getElementById("wf-zoom-out").addEventListener("click", () => zoomBy(1 / 1.2));
	document.getElementById("wf-zoom-fit").addEventListener("click", fitView);
	document.getElementById("wf-zoom-level").addEventListener("click", fitView);
	wfState.edgesLayer.addEventListener("click", (e) => {
		const hit = e.target.closest(".wf-edge-hit");
		if (hit) selectEdge(hit.dataset.from, hit.dataset.to);
	});
	wfState.edgesLayer.addEventListener("focusin", (e) => {
		const hit = e.target.closest(".wf-edge-hit");
		if (hit) selectEdge(hit.dataset.from, hit.dataset.to);
	});
	wfState.edgesLayer.addEventListener("keydown", (e) => {
		const hit = e.target.closest(".wf-edge-hit");
		if (hit && (e.key === "Delete" || e.key === "Backspace" || e.key === "Enter")) {
			e.preventDefault();
			removeEdge({ from: hit.dataset.from, to: hit.dataset.to });
		}
	});
	window.addEventListener("resize", renderEdges);
	const hadViewport = wfRestore();
	renderWorkflow();
	if (hadViewport) {
		applyViewport();
	} else if (wfState.nodes.length > 0) {
		fitView();
	} else {
		applyViewport();
	}
}

// WF_MIN_SCALE and WF_MAX_SCALE bound the zoom so the graph never shrinks past legibility or
// grows past usefulness. WF_NODE_H is the node card height used for the fit bounding box.
const WF_MIN_SCALE = 0.2;
const WF_MAX_SCALE = 2.5;
const WF_NODE_H = 62;

// clampScale keeps a scale inside the allowed zoom range, and falls back to full size for a value
// that is not a finite number so a corrupt draft cannot write a broken transform.
function clampScale(k) {
	if (!Number.isFinite(k)) return 1;
	return Math.max(WF_MIN_SCALE, Math.min(WF_MAX_SCALE, k));
}

// applyViewport writes the current pan and scale to the world transform and the backing grid, and
// updates the zoom readout. It touches only styles, so pan and zoom stay on the compositor and
// never re-lay-out the graph.
function applyViewport() {
	const { world, canvas, panX, panY, scale } = wfState;
	world.style.transform = "translate(" + panX + "px, " + panY + "px) scale(" + scale + ")";
	canvas.style.backgroundSize = 22 * scale + "px " + 22 * scale + "px";
	canvas.style.backgroundPosition = panX + "px " + panY + "px";
	const label = document.getElementById("wf-zoom-level");
	if (label) label.textContent = Math.round(scale * 100) + "%";
}

// zoomAt scales toward a target scale while keeping the world point under the given screen point
// fixed, so zooming homes in on whatever the cursor is over.
function zoomAt(clientX, clientY, target) {
	const k = clampScale(target);
	const rect = wfState.canvas.getBoundingClientRect();
	const sx = clientX - rect.left;
	const sy = clientY - rect.top;
	const wx = (sx - wfState.panX) / wfState.scale;
	const wy = (sy - wfState.panY) / wfState.scale;
	wfState.scale = k;
	wfState.panX = sx - wx * k;
	wfState.panY = sy - wy * k;
	applyViewport();
	saveViewSoon();
}

// zoomBy zooms by a factor around the center of the canvas, used by the buttons and keys.
function zoomBy(factor) {
	const rect = wfState.canvas.getBoundingClientRect();
	zoomAt(rect.left + rect.width / 2, rect.top + rect.height / 2, wfState.scale * factor);
}

// resetView returns to full size with the graph's top-left tucked into the corner.
function resetView() {
	wfState.scale = 1;
	wfState.panX = 40;
	wfState.panY = 40;
	applyViewport();
	wfSave();
}

// fitView frames the whole graph in the canvas with a margin, scaling down for a large graph and
// never past full size, and centers it. With no nodes it resets.
function fitView() {
	if (wfState.nodes.length === 0) {
		resetView();
		return;
	}
	let minX = Infinity, minY = Infinity, maxX = -Infinity, maxY = -Infinity;
	for (const n of wfState.nodes) {
		minX = Math.min(minX, n.x);
		minY = Math.min(minY, n.y);
		maxX = Math.max(maxX, n.x + WF_CARD_W);
		maxY = Math.max(maxY, n.y + WF_NODE_H);
	}
	const rect = wfState.canvas.getBoundingClientRect();
	const pad = 48;
	const w = maxX - minX;
	const h = maxY - minY;
	const k = clampScale(Math.min(
		(rect.width - pad * 2) / Math.max(w, 1),
		(rect.height - pad * 2) / Math.max(h, 1),
		1));
	wfState.scale = k;
	wfState.panX = (rect.width - w * k) / 2 - minX * k;
	wfState.panY = (rect.height - h * k) / 2 - minY * k;
	applyViewport();
	wfSave();
}

// wfSave persists the graph and the toolbar fields as the working draft. Storage failures are
// ignored, since the editor still works without drafts.
function wfSave() {
	try {
		localStorage.setItem(wfDraftKey, JSON.stringify({
			nodes: wfState.nodes, edges: wfState.edges, seq: wfState.seq,
			name: document.getElementById("wf-name").value,
			inventory: document.getElementById("wf-inventory").value,
			view: { scale: wfState.scale, panX: wfState.panX, panY: wfState.panY },
		}));
	} catch { /* storage full or blocked */ }
}

// wfViewSaveTimer debounces persisting the viewport, so a burst of wheel or pan events writes the
// draft once when it settles rather than on every frame.
let wfViewSaveTimer = null;

// saveViewSoon schedules a draft save after the pan or zoom settles.
function saveViewSoon() {
	clearTimeout(wfViewSaveTimer);
	wfViewSaveTimer = setTimeout(wfSave, 250);
}

// wfRestore loads the saved draft into the editor state, ignoring anything malformed. It reports
// whether a saved viewport was restored, so the mount can fit the graph on a first visit instead.
function wfRestore() {
	let draft = null;
	try {
		draft = JSON.parse(localStorage.getItem(wfDraftKey) || "null");
	} catch {
		return false;
	}
	if (!draft || !Array.isArray(draft.nodes) || !Array.isArray(draft.edges)) return false;
	wfState.nodes = draft.nodes;
	wfState.edges = draft.edges;
	wfState.seq = typeof draft.seq === "number" ? draft.seq : draft.nodes.length;
	document.getElementById("wf-name").value = draft.name || "";
	document.getElementById("wf-inventory").value = draft.inventory || "";
	const v = draft.view;
	if (v && Number.isFinite(v.scale) && Number.isFinite(v.panX) && Number.isFinite(v.panY)) {
		wfState.scale = clampScale(v.scale);
		wfState.panX = v.panX;
		wfState.panY = v.panY;
		return true;
	}
	return false;
}

// wfSnapshot pushes the current graph onto the undo stack before a mutation. A coalesce key merges
// rapid repeats, so a burst of arrow-key nudges undoes as one step.
function wfSnapshot(key) {
	const now = Date.now();
	if (key && wfState.lastSnapKey === key && now - wfState.lastSnapAt < 1000) {
		wfState.lastSnapAt = now;
		return;
	}
	wfState.lastSnapKey = key || null;
	wfState.lastSnapAt = now;
	wfPushHistory(JSON.stringify({ nodes: wfState.nodes, edges: wfState.edges }));
}

// wfPushHistory records a serialized graph as an undo point and clears the redo stack.
function wfPushHistory(snapshot) {
	wfState.past.push(snapshot);
	if (wfState.past.length > wfHistoryCap) wfState.past.shift();
	wfState.future = [];
}

// wfUndo restores the previous graph state.
function wfUndo() {
	if (wfState.past.length === 0) return;
	wfState.future.push(JSON.stringify({ nodes: wfState.nodes, edges: wfState.edges }));
	applyGraph(JSON.parse(wfState.past.pop()));
}

// wfRedo reapplies an undone graph state.
function wfRedo() {
	if (wfState.future.length === 0) return;
	wfState.past.push(JSON.stringify({ nodes: wfState.nodes, edges: wfState.edges }));
	applyGraph(JSON.parse(wfState.future.pop()));
}

// applyGraph replaces the graph, redraws, and persists the draft, used by undo and redo.
function applyGraph(g) {
	wfState.nodes = g.nodes;
	wfState.edges = g.edges;
	wfState.selectedEdge = null;
	wfState.lastSnapKey = null;
	renderWorkflow();
	wfSave();
}

// wfPoint converts a pointer event to world coordinates, undoing the current pan and zoom so a
// node dropped under the cursor lands where the cursor is, at any zoom.
function wfPoint(e) {
	const r = wfState.canvas.getBoundingClientRect();
	return {
		x: (e.clientX - r.left - wfState.panX) / wfState.scale,
		y: (e.clientY - r.top - wfState.panY) / wfState.scale,
	};
}

// syncStepFields shows the playbook field for Ansible and the command field for the other tools.
// The AI draft row appears only for the inline script tools, where a draft can fill the command.
function syncStepFields() {
	const tool = document.getElementById("wf-step-tool").value;
	const ansible = tool === "ansible";
	document.getElementById("wf-step-playbook-field").hidden = !ansible;
	document.getElementById("wf-step-command-field").hidden = ansible;
	const drafts = tool === "bash" || tool === "python" || tool === "go";
	document.getElementById("wf-step-draft-field").hidden = !drafts;
}

// draftStep asks the AI endpoint for a script matching the description and fills the command field
// with the draft for the user to review and edit. Advisory only: nothing runs until the step is
// saved and the workflow is submitted, through the same gates as any other run.
async function draftStep() {
	const status = document.getElementById("wf-step-status");
	const prompt = document.getElementById("wf-step-draft").value.trim();
	const tool = document.getElementById("wf-step-tool").value;
	if (!prompt) {
		status.textContent = "Describe the step first.";
		return;
	}
	const btn = document.getElementById("wf-step-draft-go");
	btn.disabled = true;
	status.textContent = "Drafting.";
	try {
		const res = await fetch(API + "/ai/draft", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify({ tool, prompt }),
		});
		if (res.status === 401) {
			requireLogin();
			return;
		}
		if (res.status === 404) {
			status.textContent = "AI is not enabled on this server.";
			return;
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
		document.getElementById("wf-step-command").value = data.draft || "";
		status.textContent = "Draft ready. Review it before saving.";
	} catch (err) {
		status.textContent = "Draft failed: " + err.message;
	} finally {
		btn.disabled = false;
	}
}

// openStepModal opens the step editor for a new step, or for the given node to edit it in place.
function openStepModal(node) {
	wfState.opener = document.activeElement;
	wfState.editing = node ? node.id : null;
	document.getElementById("wf-step-status").textContent = "";
	document.getElementById("wf-step-name").value = node ? node.name : "";
	document.getElementById("wf-step-tool").value = node ? node.tool : "ansible";
	document.getElementById("wf-step-playbook").value = node ? node.playbook : "";
	document.getElementById("wf-step-command").value = node ? node.command : "";
	document.getElementById("wf-step-inventory").value = node ? node.inventory : "";
	document.getElementById("wf-step-dryrun").checked = node ? node.dryRun : false;
	document.getElementById("wf-step-continue").checked = node ? node.continueOnFailure : false;
	document.getElementById("wf-step-retries").value = node ? node.retries : 0;
	document.getElementById("wf-step-delete").hidden = !node;
	syncStepFields();
	document.getElementById("wf-step-modal").hidden = false;
	document.getElementById("wf-step-name").focus();
}

// closeStepModal hides the step editor and returns focus to whatever opened it.
function closeStepModal() {
	document.getElementById("wf-step-modal").hidden = true;
	wfState.editing = null;
	if (wfState.opener && wfState.opener.focus) wfState.opener.focus();
	wfState.opener = null;
}

// saveStep validates the step form and creates or updates the node, keeping step names unique so
// dependencies stay unambiguous and requiring the tool's input so a broken step is caught here
// instead of at run time.
function saveStep(e) {
	e.preventDefault();
	const name = document.getElementById("wf-step-name").value.trim();
	const tool = document.getElementById("wf-step-tool").value;
	const status = document.getElementById("wf-step-status");
	if (!name) { status.textContent = "Name is required."; return; }
	const clash = wfState.nodes.some((n) => n.name === name && n.id !== wfState.editing);
	if (clash) { status.textContent = "A step named " + name + " already exists."; return; }
	const fields = {
		name, tool,
		playbook: document.getElementById("wf-step-playbook").value.trim(),
		command: document.getElementById("wf-step-command").value.trim(),
		inventory: document.getElementById("wf-step-inventory").value.trim(),
		dryRun: document.getElementById("wf-step-dryrun").checked,
		continueOnFailure: document.getElementById("wf-step-continue").checked,
		retries: Math.max(0, parseInt(document.getElementById("wf-step-retries").value, 10) || 0),
	};
	if (tool === "ansible" && !fields.playbook) {
		status.textContent = "An Ansible step needs a playbook.";
		return;
	}
	if (tool !== "ansible" && !fields.command) {
		status.textContent = "A " + tool + " step needs a command.";
		return;
	}
	wfSnapshot();
	if (wfState.editing) {
		const node = wfState.nodes.find((n) => n.id === wfState.editing);
		Object.assign(node, fields);
	} else {
		wfState.nodes.push(Object.assign({ id: "n" + (wfState.seq++) }, spawnPosition(), fields));
	}
	closeStepModal();
	renderWorkflow();
	wfSave();
}

// spawnPosition picks a free grid slot for a new node inside the visible part of the canvas at the
// current pan and zoom, skipping occupied spots so new steps never stack on existing ones.
function spawnPosition() {
	const rect = wfState.canvas.getBoundingClientRect();
	const viewW = rect.width / wfState.scale;
	const baseX = -wfState.panX / wfState.scale + 40;
	const baseY = -wfState.panY / wfState.scale + 40;
	const cols = Math.max(1, Math.floor((viewW - 40) / 210));
	for (let i = 0; i < 1000; i++) {
		const x = baseX + (i % cols) * 210;
		const y = baseY + Math.floor(i / cols) * 130;
		if (!wfState.nodes.some((n) => Math.abs(n.x - x) < 30 && Math.abs(n.y - y) < 30)) {
			return { x, y };
		}
	}
	return { x: baseX, y: baseY };
}

// deleteStepFromModal removes the node currently open in the editor along with its edges.
function deleteStepFromModal() {
	if (wfState.editing) removeNode(wfState.editing);
	closeStepModal();
}

// removeNode deletes a node and every edge touching it. Undo can bring it back.
function removeNode(id) {
	wfSnapshot();
	wfState.nodes = wfState.nodes.filter((n) => n.id !== id);
	wfState.edges = wfState.edges.filter((e) => e.from !== id && e.to !== id);
	if (wfState.linkFrom === id) wfState.linkFrom = null;
	renderWorkflow();
	wfSave();
}

// renderWorkflow redraws the nodes and the edges and toggles the empty hint.
function renderWorkflow() {
	renderNodes();
	renderEdges();
	if (wfState.hint) wfState.hint.hidden = wfState.nodes.length > 0;
}

// renderNodes reconciles the node cards with the model, positioning each and wiring its handles.
// Cards are focusable: Enter edits, arrows move, L starts a link, and Delete removes.
function renderNodes() {
	const layer = wfState.nodesLayer;
	layer.textContent = "";
	for (const node of wfState.nodes) {
		const el = document.createElement("div");
		el.className = "wf-node";
		el.style.left = node.x + "px";
		el.style.top = node.y + "px";
		el.dataset.id = node.id;
		el.tabIndex = 0;
		el.setAttribute("role", "group");
		el.setAttribute("aria-label", node.name + ", " + node.tool +
			" step. Enter edits, arrow keys move, L starts a link, Delete removes.");
		const target = node.tool === "ansible" ? node.playbook : node.command;
		el.innerHTML =
			'<div class="wf-node-head"><span class="wf-node-name"></span>' +
			'<button type="button" class="wf-node-del" aria-label="Delete step">&times;</button></div>' +
			'<div class="wf-node-meta"><span class="wf-tool"></span><span class="wf-node-target mono"></span></div>' +
			'<span class="wf-handle wf-in" aria-hidden="true"></span>' +
			'<span class="wf-handle wf-out" aria-hidden="true"></span>';
		el.querySelector(".wf-node-name").textContent = node.name;
		el.querySelector(".wf-tool").textContent = node.tool;
		el.querySelector(".wf-node-target").textContent = target || "";
		el.querySelector(".wf-node-del").addEventListener("click", (ev) => { ev.stopPropagation(); removeNode(node.id); });
		el.querySelector(".wf-out").addEventListener("pointerdown", (ev) => startLink(ev, node.id));
		el.addEventListener("pointerdown", (ev) => startDrag(ev, node.id));
		el.addEventListener("keydown", (ev) => nodeKey(ev, node.id));
		layer.appendChild(el);
	}
}

// nodeKey handles keyboard interaction on a focused node: edit, move, link, and delete. Completing
// a pending link happens on Enter over the target node.
function nodeKey(e, id) {
	const node = wfState.nodes.find((n) => n.id === id);
	if (!node || e.target.closest(".wf-node-del")) return;
	if (e.key === "Enter" || e.key === " ") {
		e.preventDefault();
		if (wfState.linkFrom && wfState.linkFrom !== id) {
			linkTo(wfState.linkFrom, id);
			wfState.linkFrom = null;
			renderEdges();
		} else {
			openStepModal(node);
		}
	} else if (e.key === "Delete" || e.key === "Backspace") {
		e.preventDefault();
		removeNode(id);
	} else if (e.key.toLowerCase() === "l") {
		e.preventDefault();
		wfState.linkFrom = id;
		wfSetStatus("Linking from " + node.name +
			". Focus another step and press Enter to add the dependency. Escape cancels.", "");
	} else if (e.key.startsWith("Arrow")) {
		e.preventDefault();
		wfSnapshot("move-" + id);
		const d = e.shiftKey ? 1 : 10;
		if (e.key === "ArrowLeft") node.x = Math.max(0, node.x - d);
		if (e.key === "ArrowRight") node.x += d;
		if (e.key === "ArrowUp") node.y = Math.max(0, node.y - d);
		if (e.key === "ArrowDown") node.y += d;
		positionNode(id);
		renderEdges();
		wfSave();
	}
}

// positionNode moves a node's card in place without rebuilding the layer, so focus and listeners
// survive a drag or a keyboard move.
function positionNode(id) {
	const node = wfState.nodes.find((n) => n.id === id);
	const el = wfState.nodesLayer.querySelector('[data-id="' + id + '"]');
	if (node && el) {
		el.style.left = node.x + "px";
		el.style.top = node.y + "px";
	}
}

// renderEdges draws every dependency edge with an invisible wide hit path over it for selection,
// plus the in-progress link while one is being dragged.
function renderEdges() {
	const svg = wfState.edgesLayer;
	// The SVG lives in world space inside the transformed layer, so it is sized to the content
	// extent. Anything drawn past it still shows because the layer allows overflow.
	let extentX = 1, extentY = 1;
	for (const n of wfState.nodes) {
		extentX = Math.max(extentX, n.x + WF_CARD_W + 80);
		extentY = Math.max(extentY, n.y + WF_NODE_H + 80);
	}
	svg.setAttribute("width", extentX);
	svg.setAttribute("height", extentY);
	let paths = "";
	for (const e of wfState.edges) {
		const a = wfState.nodes.find((n) => n.id === e.from);
		const b = wfState.nodes.find((n) => n.id === e.to);
		if (!(a && b)) continue;
		const sel = wfState.selectedEdge &&
			wfState.selectedEdge.from === e.from && wfState.selectedEdge.to === e.to;
		const d = edgeD(a.x + WF_CARD_W, a.y + WF_HANDLE_Y, b.x, b.y + WF_HANDLE_Y);
		paths += '<path class="wf-edge' + (sel ? " wf-edge-selected" : "") + '" d="' + d + '"/>';
		paths += '<path class="wf-edge-hit" d="' + d + '" tabindex="0" role="button" ' +
			'data-from="' + e.from + '" data-to="' + e.to + '" ' +
			'aria-label="Dependency link. Press Delete to remove it."/>';
	}
	if (wfState.link && wfState.link.cursor) {
		const a = wfState.nodes.find((n) => n.id === wfState.link.from);
		if (a) {
			paths += '<path class="wf-edge wf-edge-live" d="' +
				edgeD(a.x + WF_CARD_W, a.y + WF_HANDLE_Y, wfState.link.cursor.x, wfState.link.cursor.y) + '"/>';
		}
	}
	svg.innerHTML = paths;
}

// edgeD returns the SVG cubic path data between two points, curving horizontally so edges read as
// flow.
function edgeD(x1, y1, x2, y2) {
	const dx = Math.max(40, Math.abs(x2 - x1) / 2);
	return "M" + x1 + " " + y1 + " C" + (x1 + dx) + " " + y1 + " " +
		(x2 - dx) + " " + y2 + " " + x2 + " " + y2;
}

// selectEdge marks a dependency edge as selected so Delete can remove it. The class flips in place
// rather than re-rendering, so keyboard focus on the hit path survives.
function selectEdge(from, to) {
	wfState.selectedEdge = { from, to };
	for (const p of wfState.edgesLayer.querySelectorAll(".wf-edge-selected")) {
		p.classList.remove("wf-edge-selected");
	}
	const hit = wfState.edgesLayer.querySelector(
		'.wf-edge-hit[data-from="' + from + '"][data-to="' + to + '"]');
	if (hit && hit.previousElementSibling) hit.previousElementSibling.classList.add("wf-edge-selected");
	wfSetStatus("Dependency selected. Press Delete to remove it.", "");
}

// deselectEdge clears the edge selection and its status line.
function deselectEdge() {
	if (!wfState.selectedEdge) return;
	wfState.selectedEdge = null;
	for (const p of wfState.edgesLayer.querySelectorAll(".wf-edge-selected")) {
		p.classList.remove("wf-edge-selected");
	}
	wfSetStatus("", "");
}

// removeEdge deletes a dependency edge. Undo can bring it back.
function removeEdge(sel) {
	wfSnapshot();
	wfState.edges = wfState.edges.filter((e) => !(e.from === sel.from && e.to === sel.to));
	wfState.selectedEdge = null;
	renderEdges();
	wfSetStatus("Dependency removed.", "");
	wfSave();
}

// startDrag begins moving a node with the primary button, unless the press landed on a handle or
// the delete control. The pre-drag graph is captured so a completed move becomes one undo step.
function startDrag(e, id) {
	if (e.button !== 0 || !e.isPrimary) return;
	if (e.target.closest(".wf-handle") || e.target.closest(".wf-node-del")) return;
	const node = wfState.nodes.find((n) => n.id === id);
	const p = wfPoint(e);
	wfState.drag = {
		id, dx: p.x - node.x, dy: p.y - node.y, sx: p.x, sy: p.y, moved: false,
		before: JSON.stringify({ nodes: wfState.nodes, edges: wfState.edges }),
	};
	wfState.canvas.setPointerCapture(e.pointerId);
}

// startLink begins drawing a dependency edge out of a node's output handle.
function startLink(e, id) {
	if (e.button !== 0 || !e.isPrimary) return;
	e.stopPropagation();
	wfState.link = { from: id, cursor: wfPoint(e) };
	wfState.canvas.setPointerCapture(e.pointerId);
}

// wfPointerDown starts a canvas pan on empty space or a two-finger pinch, and clears any edge
// selection. A press on a node or a handle is left to that element's own drag and link handlers.
function wfPointerDown(e) {
	wfState.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
	if (wfState.pointers.size === 2 && !wfState.drag && !wfState.link) {
		startPinch();
		wfState.pan = null;
		return;
	}
	const onControl = e.target.closest(".wf-node") || e.target.closest(".wf-edge-hit") ||
		e.target.closest(".wf-zoom");
	if (onControl || wfState.drag || wfState.link || e.button !== 0) return;
	deselectEdge();
	wfState.pan = { x: e.clientX, y: e.clientY, px: wfState.panX, py: wfState.panY };
	wfState.canvas.classList.add("wf-panning");
	wfState.canvas.setPointerCapture(e.pointerId);
}

// startPinch records the two-finger baseline so a pinch scales from where the fingers began.
function startPinch() {
	const pts = [...wfState.pointers.values()];
	wfState.pinch = {
		dist: ptDist(pts[0], pts[1]),
		cx: (pts[0].x + pts[1].x) / 2, cy: (pts[0].y + pts[1].y) / 2,
		scale: wfState.scale, panX: wfState.panX, panY: wfState.panY,
	};
}

// ptDist is the distance between two pointer positions.
function ptDist(a, b) {
	return Math.hypot(a.x - b.x, a.y - b.y);
}

// wfWheel pans on a plain scroll and zooms toward the cursor when a modifier or a trackpad pinch is
// held, so a mouse wheel and a trackpad both feel right.
function wfWheel(e) {
	e.preventDefault();
	if (e.ctrlKey || e.metaKey) {
		zoomAt(e.clientX, e.clientY, wfState.scale * Math.exp(-e.deltaY * 0.0015));
		return;
	}
	wfState.panX -= e.deltaX;
	wfState.panY -= e.deltaY;
	applyViewport();
	saveViewSoon();
}

// wfPointerMove updates a pinch, a pan, an in-progress node drag, or a link as the pointer moves. A
// drag starts only past a small threshold, so a shaky tap still opens the editor, and only the
// dragged card is repositioned so large graphs stay smooth.
function wfPointerMove(e) {
	if (wfState.pointers.has(e.pointerId)) {
		wfState.pointers.set(e.pointerId, { x: e.clientX, y: e.clientY });
	}
	if (wfState.pinch && wfState.pointers.size >= 2) {
		const pts = [...wfState.pointers.values()];
		const rect = wfState.canvas.getBoundingClientRect();
		const k = clampScale(wfState.pinch.scale * (ptDist(pts[0], pts[1]) / wfState.pinch.dist));
		const wx = (wfState.pinch.cx - rect.left - wfState.pinch.panX) / wfState.pinch.scale;
		const wy = (wfState.pinch.cy - rect.top - wfState.pinch.panY) / wfState.pinch.scale;
		wfState.scale = k;
		wfState.panX = (pts[0].x + pts[1].x) / 2 - rect.left - wx * k;
		wfState.panY = (pts[0].y + pts[1].y) / 2 - rect.top - wy * k;
		applyViewport();
		saveViewSoon();
		return;
	}
	if (wfState.pan) {
		wfState.panX = wfState.pan.px + (e.clientX - wfState.pan.x);
		wfState.panY = wfState.pan.py + (e.clientY - wfState.pan.y);
		applyViewport();
		saveViewSoon();
		return;
	}
	if (wfState.drag) {
		const p = wfPoint(e);
		if (!wfState.drag.moved &&
			Math.abs(p.x - wfState.drag.sx) < 4 && Math.abs(p.y - wfState.drag.sy) < 4) {
			return;
		}
		wfState.drag.moved = true;
		const node = wfState.nodes.find((n) => n.id === wfState.drag.id);
		node.x = Math.max(0, p.x - wfState.drag.dx);
		node.y = Math.max(0, p.y - wfState.drag.dy);
		positionNode(node.id);
		renderEdges();
	} else if (wfState.link) {
		wfState.link.cursor = wfPoint(e);
		renderEdges();
	}
}

// wfPointerUp ends a pan or pinch, finishes a drag, or completes a link when it lands on a node.
function wfPointerUp(e) {
	wfState.pointers.delete(e.pointerId);
	if (wfState.pinch && wfState.pointers.size < 2) {
		wfState.pinch = null;
		wfSave();
	}
	if (wfState.pan) {
		wfState.pan = null;
		wfState.canvas.classList.remove("wf-panning");
		wfSave();
	}
	if (wfState.link) {
		// Pointer capture retargets the event to the canvas, so find the drop node by geometry.
		const el = document.elementFromPoint(e.clientX, e.clientY);
		const over = el ? el.closest(".wf-node") : null;
		const fromId = wfState.link.from;
		wfState.link = null;
		if (over) linkTo(fromId, over.dataset.id);
		renderEdges();
	}
	if (wfState.drag) {
		if (wfState.drag.moved) {
			wfPushHistory(wfState.drag.before);
			wfSave();
		} else {
			openStepModal(wfState.nodes.find((n) => n.id === wfState.drag.id));
		}
		wfState.drag = null;
	}
}

// wfCancelPointer clears a pan, pinch, drag, or link whose gesture was canceled, for example by an
// edge swipe on a touch screen, so nothing stays glued to the pointer.
function wfCancelPointer(e) {
	wfState.pointers.delete(e.pointerId);
	if (wfState.pinch && wfState.pointers.size < 2) wfState.pinch = null;
	if (wfState.pan) {
		wfState.pan = null;
		wfState.canvas.classList.remove("wf-panning");
	}
	if (wfState.drag) {
		if (wfState.drag.moved) {
			wfPushHistory(wfState.drag.before);
			wfSave();
		}
		wfState.drag = null;
	}
	if (wfState.link) {
		wfState.link = null;
		renderEdges();
	}
}

// linkTo adds a dependency edge from one node to another, rejecting self-links, duplicates, and
// edges that would create a cycle. The status line announces the new dependency.
function linkTo(fromId, toId) {
	if (fromId === toId) return;
	if (wfState.edges.some((e) => e.from === fromId && e.to === toId)) return;
	if (reaches(toId, fromId)) {
		wfSetStatus("That link would create a cycle.", "err");
		return;
	}
	const from = wfState.nodes.find((n) => n.id === fromId);
	const to = wfState.nodes.find((n) => n.id === toId);
	if (!from || !to) return;
	wfSnapshot();
	wfState.edges.push({ from: fromId, to: toId });
	wfSetStatus(to.name + " now waits for " + from.name + ".", "");
	wfSave();
}

// reaches reports whether following edges from start eventually arrives at goal, used to block
// cycles before they are added.
function reaches(start, goal) {
	const seen = new Set();
	const stack = [start];
	while (stack.length) {
		const cur = stack.pop();
		if (cur === goal) return true;
		if (seen.has(cur)) continue;
		seen.add(cur);
		for (const e of wfState.edges) {
			if (e.from === cur) stack.push(e.to);
		}
	}
	return false;
}

// wfSetStatus writes the editor status line, red for an error.
function wfSetStatus(msg, kind) {
	const el = document.getElementById("status");
	if (!el) return;
	el.className = kind === "err" ? "error-text" : "muted";
	el.textContent = msg;
	el.hidden = !msg;
}

// runWorkflow serializes the graph into pipeline steps and submits it, then opens the new run. An
// in-flight guard stops a double click from starting the workflow twice, and a 401 saves the draft
// and routes through sign-in so the graph survives the round trip.
async function runWorkflow() {
	if (wfState.submitting) return;
	if (wfState.nodes.length === 0) { wfSetStatus("Add at least one step.", "err"); return; }
	const steps = wfState.nodes.map((n) => {
		const step = { name: n.name, tool: n.tool };
		if (n.tool === "ansible") step.playbook = n.playbook;
		else step.command = n.command;
		if (n.inventory) step.inventory = n.inventory;
		if (n.dryRun) step.dry_run = true;
		if (n.continueOnFailure) step.continue_on_failure = true;
		if (n.retries > 0) step.retries = n.retries;
		const deps = wfState.edges.filter((e) => e.to === n.id)
			.map((e) => {
				const src = wfState.nodes.find((x) => x.id === e.from);
				return src ? src.name : null;
			})
			.filter(Boolean);
		if (deps.length) step.depends_on = deps;
		return step;
	});
	const body = {
		name: document.getElementById("wf-name").value.trim() || "workflow",
		inventory: document.getElementById("wf-inventory").value.trim(),
		steps,
	};
	const runBtn = document.getElementById("wf-run");
	wfState.submitting = true;
	runBtn.disabled = true;
	wfSetStatus("Starting workflow.", "");
	try {
		const res = await fetch(API + "/pipelines", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify(body),
		});
		if (res.status === 401) {
			wfSave();
			requireLogin();
			return;
		}
		if (!res.ok) {
			const detail = await res.json().catch(() => ({}));
			throw new Error(detail.error || "HTTP " + res.status);
		}
		const run = await res.json();
		try { localStorage.removeItem(wfDraftKey); } catch { /* draft already gone */ }
		window.location.assign("/ui/runs/" + run.id);
	} catch (err) {
		wfSetStatus("Could not start workflow: " + err.message, "err");
		wfState.submitting = false;
		runBtn.disabled = false;
	}
}

document.addEventListener("DOMContentLoaded", () => {
	consumeSSOFragment();
	mountTopbar();
	mountLiveRegions();
	explainReadOnly();
	if (LIST_PAGES.includes(document.body.dataset.page)) mountListFilter();
	const close = document.getElementById("drill-close");
	if (close) {
		close.addEventListener("click", () => { document.getElementById("drill").hidden = true; });
	}
	const page = document.body.dataset.page;
	if (page === "overview") {
		loadOverview();
		wireAsk();
	} else if (page === "runs") {
		wireModal("launch");
		if (!isReadOnly()) wireLaunchForm();
		wirePropose();
		wireRunsSearch();
		wireRunsFilters();
		loadRuns();
	} else if (page === "detail") {
		loadDetail(document.body.dataset.runId);
	} else if (page === "fleet") {
		loadFleet();
	} else if (page === "drift") {
		loadDrift();
	} else if (page === "host") {
		loadHost(document.body.dataset.host);
	} else if (page === "tasks") {
		loadTasks();
	} else if (page === "schedules") {
		wireModal("schedule");
		wireScheduleForm();
		loadSchedules();
	} else if (page === "workflows") {
		mountWorkflow();
	} else if (page === "login") {
		loadLogin();
	} else if (page === "credentials") {
		wireModal("cred");
		wireCredentialForm();
		loadCredentials();
	} else if (page === "audit") {
		wireAudit();
		loadAudit();
	} else if (page === "policies") {
		wireModal("policy");
		wirePolicyForm();
		loadPolicies();
	} else if (page === "projects") {
		wireModal("project");
		wireProjectForm();
		loadProjects();
	} else if (page === "jobtemplates") {
		wireModal("template");
		wireTemplateForm();
		loadTemplates();
	} else if (page === "users") {
		wireModal("user");
		wireUserForm();
		loadUsers();
	} else if (page === "workers") {
		loadWorkers();
	} else if (page === "inventories") {
		wireModal("inventory");
		wireInventoryForm();
		loadInventories();
	} else if (page === "sources") {
		wireModal("source");
		wireSourceForm();
		loadSources();
	} else if (page === "migrate") {
		wireMigrate();
	}
	buildNav();
	wirePalette();
	wireHinttips();
	mountPageDocs();
	mountTableExport();
	if (isReadOnly()) applyReadOnly();
	setInterval(refreshRelTimes, 20000);
	mountTour();
});

// showSkeletonRows fills a table body with shimmering placeholders while its data loads.
function showSkeletonRows(tbody, rows, cols) {
	tbody.innerHTML = "";
	for (let i = 0; i < rows; i++) {
		const tr = document.createElement("tr");
		tr.className = "skeleton-row";
		for (let c = 0; c < cols; c++) {
			const cell = document.createElement("td");
			const bar = document.createElement("span");
			bar.className = "skeleton";
			cell.appendChild(bar);
			tr.appendChild(cell);
		}
		tbody.appendChild(tr);
	}
}

// applyReadOnly keeps the forms and controls visible so the demo conveys what the product does, but
// disables the actions that would mutate and adds a banner. Row action buttons are dimmed by CSS.
function applyReadOnly() {
	const main = document.querySelector(".content");
	if (main && !main.querySelector(".ro-banner")) {
		const banner = document.createElement("div");
		banner.className = "ro-banner";
		banner.textContent = "Read-only demo. Browse the data freely. Changes are disabled.";
		main.insertBefore(banner, main.firstChild);
	}
	for (const form of document.querySelectorAll("form")) {
		for (const btn of form.querySelectorAll("button")) btn.disabled = true;
		const actions = form.querySelector(".launch-actions") || form;
		if (!actions.querySelector(".ro-note")) {
			const note = document.createElement("span");
			note.className = "ro-note";
			note.textContent = "Disabled in the demo";
			actions.appendChild(note);
		}
	}
	for (const btn of document.querySelectorAll(".wf-toolbar button")) btn.disabled = true;
}

// buildNav injects the menu toggle and the slide-in drawer on every page but sign in, highlighting
// the current page and hiding admin links from non-admins. The toggle opens and closes the drawer.
function buildNav() {
	if (document.body.dataset.page === "login") return;
	const topbar = document.querySelector(".topbar");
	if (!topbar) return;

	const role = localStorage.getItem("st_role");
	const showAdmin = !role || role === "admin";
	const activeKey = PAGE_NAV[document.body.dataset.page] || "";

	const burger = '<line x1="3" y1="6" x2="21" y2="6"/><line x1="3" y1="12" x2="21" y2="12"/>' +
		'<line x1="3" y1="18" x2="21" y2="18"/>';
	const cross = '<line x1="6" y1="6" x2="18" y2="18"/><line x1="6" y1="18" x2="18" y2="6"/>';

	const toggle = document.createElement("button");
	toggle.className = "nav-toggle";
	toggle.setAttribute("aria-label", "Open menu");
	toggle.setAttribute("aria-expanded", "false");
	toggle.innerHTML = svgIcon(burger);
	topbar.insertBefore(toggle, topbar.firstChild);

	const backdrop = document.createElement("div");
	backdrop.className = "nav-backdrop";
	backdrop.hidden = true;

	const drawer = document.createElement("nav");
	drawer.className = "nav-drawer";
	drawer.hidden = true;
	drawer.setAttribute("aria-label", "Main navigation");

	// fillGroups renders the grouped nav links into a container, shared by the drawer and the
	// docked sidebar so the two never drift apart.
	const fillGroups = (root) => {
		for (const group of NAV_GROUPS) {
			const items = group.items.filter((it) => showAdmin || !it.admin);
			if (!items.length) continue;
			const g = document.createElement("div");
			g.className = "nav-group";
			const gl = document.createElement("div");
			gl.className = "nav-group-label";
			gl.textContent = group.label;
			g.appendChild(gl);
			for (const it of items) {
				const a = document.createElement("a");
				a.className = "nav-item" + (it.key === activeKey ? " active" : "");
				a.href = it.href;
				a.innerHTML = svgIcon(NAV_ICONS[it.key] || "");
				a.appendChild(document.createTextNode(it.label));
				if (it.key === activeKey) a.setAttribute("aria-current", "page");
				g.appendChild(a);
			}
			root.appendChild(g);
		}
	};
	fillGroups(drawer);
	drawer.appendChild(themeGroup());

	const side = document.createElement("aside");
	side.className = "side";
	const sideBrand = document.createElement("a");
	sideBrand.className = "side-brand";
	sideBrand.href = "/ui/";
	sideBrand.innerHTML = '<picture><source media="(prefers-color-scheme: dark)" srcset="/ui/assets/logo-train-tracks-dark.png"><img src="/ui/assets/logo-train-tracks.png" alt=""></picture>SwitchTender';
	side.appendChild(sideBrand);
	const sideNav = document.createElement("nav");
	sideNav.setAttribute("aria-label", "Primary navigation");
	fillGroups(sideNav);
	side.appendChild(sideNav);
	side.appendChild(themeGroup());

	document.body.appendChild(backdrop);
	document.body.appendChild(drawer);
	document.body.appendChild(side);
	syncThemeButtons();
	syncBrandLogos();

	// Pin the drawer directly under the top bar, tracking its height across zoom and resize.
	const syncHeight = () => document.documentElement.style
		.setProperty("--topbar-h", topbar.offsetHeight + "px");
	syncHeight();
	window.addEventListener("resize", syncHeight);

	let isOpen = false;
	const setOpen = (v) => {
		isOpen = v;
		backdrop.hidden = !v;
		drawer.hidden = !v;
		document.body.classList.toggle("nav-open", v);
		toggle.setAttribute("aria-expanded", v ? "true" : "false");
		toggle.setAttribute("aria-label", v ? "Close menu" : "Open menu");
		toggle.innerHTML = svgIcon(v ? cross : burger);
		if (v) { const first = drawer.querySelector(".nav-item"); if (first) first.focus(); }
	};
	toggle.addEventListener("click", () => setOpen(!isOpen));
	backdrop.addEventListener("click", () => setOpen(false));
	document.addEventListener("keydown", (e) => {
		if (e.key === "Escape" && isOpen) { setOpen(false); toggle.focus(); }
	});
}

// svgIcon wraps inner SVG markup in a stroked 24 by 24 icon that inherits the current color.
function svgIcon(inner) {
	return '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.7" ' +
		'stroke-linecap="round" stroke-linejoin="round" aria-hidden="true">' + inner + '</svg>';
}

// THEMES lists the selectable appearances: the signature look, then the flat family themes.
const THEMES = [
	{ key: "signature", label: "Stitch", desc: "The signature glow", tip: "Stitch, the default theme", icon: '<path d="M3 16c3.5-4 9-4 12.5-1" stroke-dasharray="3.2 2.6"/><line x1="14" y1="16.5" x2="21" y2="9.5"/>' },
	{ key: "light", label: "Kord", desc: "Clean white, the kordloom.com style", tip: "Kord, the white theme", icon: '<circle cx="12" cy="12" r="4"/><line x1="12" y1="2" x2="12" y2="5"/><line x1="12" y1="19" x2="12" y2="22"/><line x1="2" y1="12" x2="5" y2="12"/><line x1="19" y1="12" x2="22" y2="12"/><line x1="4.9" y1="4.9" x2="7" y2="7"/><line x1="17" y1="17" x2="19.1" y2="19.1"/><line x1="4.9" y1="19.1" x2="7" y2="17"/><line x1="17" y1="7" x2="19.1" y2="4.9"/>' },
	{ key: "dark", label: "Seal", desc: "Warm ink black, the loomseal.com style", tip: "Seal, the dark theme", icon: '<path d="M21 12.8A9 9 0 1 1 11.2 3a7 7 0 0 0 9.8 9.8z"/>' },
];

// currentTheme returns the active theme key, defaulting to the signature look.
function currentTheme() {
	const t = document.body.dataset.theme;
	return t === "light" || t === "dark" ? t : "signature";
}

// setTheme applies and persists a theme choice, then refreshes every switcher control.
function setTheme(key) {
	if (key === "light" || key === "dark") document.body.dataset.theme = key;
	else delete document.body.dataset.theme;
	try {
		if (key === "light" || key === "dark") localStorage.setItem("st_theme", key);
		else localStorage.removeItem("st_theme");
	} catch { /* storage may be unavailable */ }
	syncThemeButtons();
	syncBrandLogos();
}

// syncThemeButtons marks the active theme on every switcher segment.
function syncThemeButtons() {
	const active = currentTheme();
	for (const btn of document.querySelectorAll(".theme-btn")) {
		const on = btn.dataset.themeKey === active;
		btn.classList.toggle("active", on);
		btn.setAttribute("aria-pressed", on ? "true" : "false");
	}
}

// themeGroup builds the compact appearance row used at the bottom of the drawer and the
// sidebar: a muted gear hint and one icon button per theme, named by tooltip.
function themeGroup() {
	const g = document.createElement("div");
	g.className = "theme-group";
	const row = document.createElement("div");
	row.className = "theme-row";
	const hint = document.createElement("span");
	hint.className = "theme-hint";
	hint.textContent = "Theme";
	row.appendChild(hint);
	for (const t of THEMES) {
		const btn = document.createElement("button");
		btn.type = "button";
		btn.className = "theme-btn";
		btn.dataset.themeKey = t.key;
		btn.dataset.tip = t.tip;
		btn.setAttribute("aria-label", t.tip);
		btn.setAttribute("aria-pressed", "false");
		btn.innerHTML = svgIcon(t.icon);
		btn.appendChild(document.createTextNode(t.label));
		btn.addEventListener("click", () => setTheme(t.key));
		row.appendChild(btn);
	}
	g.appendChild(row);
	return g;
}

// paletteState holds the command palette's elements once built, plus the filtered entries and the
// highlighted index.
let paletteState = null;

// paletteEntries returns everything the palette can jump to: each nav destination the current role
// can see, then a few direct actions.
function paletteEntries() {
	const role = localStorage.getItem("st_role");
	const showAdmin = !role || role === "admin";
	const out = [];
	for (const group of NAV_GROUPS) {
		for (const it of group.items) {
			if (it.admin && !showAdmin) continue;
			out.push({ label: it.label, desc: it.desc || "", group: group.label, icon: NAV_ICONS[it.key] || "", href: it.href });
		}
	}
	out.push({ label: "Launch a run", desc: "Open the launch panel on the runs page", group: "Action", icon: NAV_ICONS.runs, href: "/ui/runs" });
	out.push({
		label: "View the source", desc: "github.com/kordloom/switchtender", group: "Action",
		icon: '<path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/><polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/>',
		href: "https://github.com/kordloom/switchtender", external: true,
	});
	for (const t of THEMES) {
		out.push({ label: "Theme: " + t.label, desc: t.desc, group: "Theme", icon: t.icon, action: () => setTheme(t.key) });
	}
	return out;
}

// paletteScore ranks an entry against a query: label prefix beats label substring beats
// description, and zero filters the entry out.
function paletteScore(entry, q) {
	if (!q) return 1;
	const label = entry.label.toLowerCase();
	if (label.startsWith(q)) return 4;
	if (label.includes(q)) return 3;
	if (entry.desc.toLowerCase().includes(q)) return 2;
	if (entry.group.toLowerCase().includes(q)) return 1;
	return 0;
}

// buildPalette constructs the palette dialog once and wires its input and keyboard handling.
function buildPalette() {
	if (paletteState) return paletteState;
	const overlay = document.createElement("div");
	overlay.className = "cmdk";
	overlay.hidden = true;
	overlay.setAttribute("role", "dialog");
	overlay.setAttribute("aria-modal", "true");
	overlay.setAttribute("aria-label", "Command palette");
	const card = document.createElement("div");
	card.className = "cmdk-card";
	const head = document.createElement("div");
	head.className = "cmdk-head";
	head.innerHTML = svgIcon('<circle cx="11" cy="11" r="7"/><line x1="21" y1="21" x2="16.65" y2="16.65"/>');
	const input = document.createElement("input");
	input.className = "cmdk-input";
	input.placeholder = "Jump to a page or action…";
	input.setAttribute("aria-label", "Search pages and actions");
	head.appendChild(input);
	const list = document.createElement("div");
	list.className = "cmdk-list";
	list.setAttribute("role", "listbox");
	const foot = document.createElement("div");
	foot.className = "cmdk-foot";
	foot.innerHTML = '<span><span class="kbd">↑↓</span> navigate</span>' +
		'<span><span class="kbd">↵</span> open</span><span><span class="kbd">esc</span> close</span>';
	card.appendChild(head);
	card.appendChild(list);
	card.appendChild(foot);
	overlay.appendChild(card);
	document.body.appendChild(overlay);
	overlay.addEventListener("mousedown", (e) => { if (e.target === overlay) closePalette(); });
	input.addEventListener("input", () => renderPalette(input.value));
	input.addEventListener("keydown", (e) => {
		if (e.key === "ArrowDown") { e.preventDefault(); movePaletteActive(1); }
		else if (e.key === "ArrowUp") { e.preventDefault(); movePaletteActive(-1); }
		else if (e.key === "Enter") { e.preventDefault(); openPaletteActive(); }
		else if (e.key === "Escape") { e.preventDefault(); closePalette(); }
	});
	paletteState = { overlay, input, list, active: 0, shown: [] };
	return paletteState;
}

// renderPalette filters the entries for a query and redraws the result list.
function renderPalette(query) {
	const st = paletteState;
	const q = (query || "").trim().toLowerCase();
	st.shown = paletteEntries()
		.map((entry) => ({ entry, score: paletteScore(entry, q) }))
		.filter((r) => r.score > 0)
		.sort((a, b) => b.score - a.score)
		.map((r) => r.entry);
	st.active = 0;
	st.list.innerHTML = "";
	if (!st.shown.length) {
		const none = document.createElement("div");
		none.className = "cmdk-empty";
		none.textContent = "No matches.";
		st.list.appendChild(none);
		return;
	}
	st.shown.forEach((entry, i) => {
		const item = document.createElement("div");
		item.className = "cmdk-item" + (i === st.active ? " active" : "");
		item.setAttribute("role", "option");
		item.setAttribute("aria-selected", i === st.active ? "true" : "false");
		item.innerHTML = svgIcon(entry.icon) +
			'<span class="cmdk-item-label"></span><span class="cmdk-item-desc"></span><span class="cmdk-item-group"></span>';
		item.querySelector(".cmdk-item-label").textContent = entry.label;
		item.querySelector(".cmdk-item-desc").textContent = entry.desc;
		item.querySelector(".cmdk-item-group").textContent = entry.group;
		item.addEventListener("mouseenter", () => setPaletteActive(i));
		item.addEventListener("click", () => { st.active = i; openPaletteActive(); });
		st.list.appendChild(item);
	});
}

// setPaletteActive moves the highlight to the given index.
function setPaletteActive(i) {
	const st = paletteState;
	st.active = i;
	st.list.querySelectorAll(".cmdk-item").forEach((el, j) => {
		el.classList.toggle("active", j === i);
		el.setAttribute("aria-selected", j === i ? "true" : "false");
	});
}

// movePaletteActive steps the highlight up or down, wrapping, and keeps it scrolled into view.
function movePaletteActive(delta) {
	const st = paletteState;
	if (!st.shown.length) return;
	const next = (st.active + delta + st.shown.length) % st.shown.length;
	setPaletteActive(next);
	const el = st.list.querySelectorAll(".cmdk-item")[next];
	if (el) el.scrollIntoView({ block: "nearest" });
}

// openPaletteActive navigates to the highlighted entry.
function openPaletteActive() {
	const st = paletteState;
	const entry = st.shown[st.active];
	if (!entry) return;
	closePalette();
	if (entry.action) entry.action();
	else if (entry.external) window.open(entry.href, "_blank", "noopener");
	else location.href = entry.href;
}

// openPalette shows the palette with a fresh query and focuses its input.
function openPalette() {
	const st = buildPalette();
	st.overlay.hidden = false;
	st.input.value = "";
	renderPalette("");
	st.input.focus();
}

// closePalette hides the palette.
function closePalette() {
	if (paletteState) paletteState.overlay.hidden = true;
}

// wireHinttips shows a floating tip above any element carrying data-tip, on hover and keyboard
// focus. One shared element rides document.body, so no scroll container can clip it, and it is
// clamped to the viewport.
function wireHinttips() {
	const tip = document.createElement("div");
	tip.className = "hinttip";
	tip.hidden = true;
	document.body.appendChild(tip);
	let linkTimer = 0;
	const place = (target, text) => {
		tip.textContent = text;
		tip.hidden = false;
		tip.style.left = "0px";
		tip.style.top = "0px";
		const r = target.getBoundingClientRect();
		const x = Math.min(Math.max(8, r.left), window.innerWidth - tip.offsetWidth - 8);
		let y = r.top - tip.offsetHeight - 8;
		if (y < 8) y = r.bottom + 8;
		tip.style.left = x + "px";
		tip.style.top = y + "px";
	};
	// linkDest labels external destinations only. Same-origin links stay quiet: the status bar
	// already names them, and a tip box over the neighboring row reads as broken UI.
	const linkDest = (a) => {
		const href = a.getAttribute("href");
		if (!href || href === "#" || href.startsWith("javascript")) return "";
		try {
			const u = new URL(href, location.href);
			return u.origin === location.origin ? "" : "Opens " + u.hostname;
		} catch { return ""; }
	};
	const show = (e) => {
		if (!e.target.closest) return;
		const target = e.target.closest("[data-tip]");
		if (target) { clearTimeout(linkTimer); place(target, target.dataset.tip); return; }
		// Plain links reveal their destination after a beat, so casual mouse travel stays quiet.
		const a = e.target.closest("a[href]");
		if (!a || a.closest(".cmdk")) return;
		const dest = linkDest(a);
		if (!dest) return;
		clearTimeout(linkTimer);
		linkTimer = window.setTimeout(() => place(a, dest), 450);
	};
	const hide = (e) => {
		if (!e.target.closest) return;
		if (e.target.closest("[data-tip]") || e.target.closest("a[href]")) {
			clearTimeout(linkTimer);
			tip.hidden = true;
		}
	};
	document.addEventListener("mouseover", show);
	document.addEventListener("mouseout", hide);
	document.addEventListener("focusin", show);
	document.addEventListener("focusout", hide);
	document.addEventListener("scroll", () => { clearTimeout(linkTimer); tip.hidden = true; }, true);
}

// wirePalette registers the platform search shortcut that toggles the palette, on every page but
// sign in.
function wirePalette() {
	if (document.body.dataset.page === "login") return;
	document.addEventListener("keydown", (e) => {
		if ((e.metaKey || e.ctrlKey) && !e.shiftKey && !e.altKey && e.key.toLowerCase() === "k") {
			e.preventDefault();
			if (paletteState && !paletteState.overlay.hidden) closePalette();
			else openPalette();
		}
	});
}

// apiToken returns the stored API token, empty when the server runs open.
function apiToken() {
	return localStorage.getItem("st_token") || "";
}

// consumeSSOFragment stores the session token handed back in the URL fragment after single
// sign-on, then strips it from the address bar so it is not left in history or copied by accident.
function consumeSSOFragment() {
	if (!location.hash || location.hash.indexOf("access_token=") === -1) return;
	const params = new URLSearchParams(location.hash.slice(1));
	const token = params.get("access_token");
	if (!token) return;
	localStorage.setItem("st_token", token);
	if (params.get("role")) localStorage.setItem("st_role", params.get("role"));
	if (params.get("user")) localStorage.setItem("st_user", params.get("user"));
	history.replaceState(null, "", location.pathname + location.search);
}

// ssoError returns a single sign-on error passed back in the URL fragment, or empty when none.
function ssoError() {
	if (!location.hash || location.hash.indexOf("error=") === -1) return "";
	return new URLSearchParams(location.hash.slice(1)).get("error") || "";
}

// authHeaders builds the Authorization header when a token is stored.
function authHeaders() {
	const token = apiToken();
	return token ? { "Authorization": "Bearer " + token } : {};
}

// requireLogin sends the browser to the sign in page, remembering where it was. The redirect flag
// stops timers such as the welcome tour from firing into the navigation.
function requireLogin() {
	if (document.body.dataset.page === "login") return;
	window.ymRedirecting = true;
	sessionStorage.setItem("st_return", location.pathname);
	location.href = "/ui/login";
}

// getJSON fetches and decodes a JSON endpoint, redirecting to sign in on a 401.
async function getJSON(url) {
	const res = await fetch(API + url, { headers: authHeaders() });
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	if (!res.ok) {
		throw new Error(url + " returned " + res.status);
	}
	return res.json();
}

// mountLiveRegions marks every status line as a polite live region so assistive tech announces the
// async success and failure text written into it, including sign-in errors and empty states.
function mountLiveRegions() {
	const regions = document.querySelectorAll('[id="status"], [id$="-status"]');
	for (const el of regions) {
		el.setAttribute("role", "status");
		el.setAttribute("aria-live", "polite");
	}
}

// setStatus shows or clears the status line.
function setStatus(msg) {
	const el = document.getElementById("status");
	if (!el) return;
	el.className = "muted";
	if (msg) { el.textContent = msg; el.hidden = false; } else { el.hidden = true; }
}

// showEmpty renders a centered empty-state card in place of the status line.
function showEmpty(msg) {
	const el = document.getElementById("status");
	if (!el) return;
	el.hidden = false;
	el.className = "empty-state";
	el.innerHTML = '<svg viewBox="0 0 24 24" width="34" height="34" fill="none" stroke="currentColor" ' +
		'stroke-width="1.4" stroke-linecap="round" stroke-linejoin="round">' +
		'<path d="M3 7l1.6 12.2A2 2 0 0 0 6.6 21h10.8a2 2 0 0 0 2-1.8L21 7"/>' +
		'<path d="M3 7h18M8.5 7V5.5a2 2 0 0 1 2-2h3a2 2 0 0 1 2 2V7"/></svg><p></p>';
	el.querySelector("p").textContent = msg;
}

// removeRow deletes a table row and restores the empty-state when the last row is gone, so a list
// cleared down to nothing shows its empty message instead of a bare header.
function removeRow(tr, emptyMsg) {
	const body = tr.parentNode;
	tr.remove();
	if (body && body.rows && body.rows.length === 0) {
		const table = body.closest("table");
		if (table) table.hidden = true;
		showEmpty(emptyMsg || "Nothing here yet.");
	}
}

// fmtDuration renders the span between two ISO times.
function fmtDuration(startISO, endISO) {
	if (!startISO || !endISO) return "";
	const ms = new Date(endISO) - new Date(startISO);
	if (isNaN(ms) || ms < 0) return "";
	return fmtMs(ms);
}

// fmtMs renders a millisecond duration.
function fmtMs(ms) {
	if (ms < 1000) return Math.round(ms) + "ms";
	return (ms / 1000).toFixed(1) + "s";
}

// fmtTime renders an ISO time in the local locale.
function fmtTime(iso) {
	if (!iso) return "";
	const d = new Date(iso);
	return isNaN(d) ? iso : d.toLocaleString();
}

// relTime renders an ISO time as a short relative age, such as "2m ago", falling back to the date
// for anything older than a month.
function relTime(iso) {
	if (!iso) return "";
	const d = new Date(iso);
	if (isNaN(d)) return iso;
	const s = Math.round((Date.now() - d.getTime()) / 1000);
	if (s < 5) return "just now";
	if (s < 60) return s + "s ago";
	const m = Math.round(s / 60);
	if (m < 60) return m + "m ago";
	const h = Math.round(m / 60);
	if (h < 24) return h + "h ago";
	const days = Math.round(h / 24);
	if (days < 30) return days + "d ago";
	return d.toLocaleDateString();
}

// baseName returns the last path segment, so a run shows its playbook file rather than a long path.
function baseName(p) {
	if (!p) return "";
	const i = p.lastIndexOf("/");
	return i >= 0 ? p.slice(i + 1) : p;
}

// shortId truncates a long identifier for display, keeping the full value for a tooltip.
function shortId(id) {
	return id && id.length > 15 ? id.slice(0, 13) + "…" : (id || "");
}

// isReadOnly reports whether the server serves a read-only demo, which hides mutating controls.
function isReadOnly() {
	return document.body.dataset.readonly === "true";
}

// loadAudit fills the audit table with the trail, newest first, showing each entry's chain hash.
// auditPathCell renders an audit path with any run reference linked to its run page.
function auditPathCell(path) {
	const cell = td("", "mono");
	for (const part of String(path || "").split(/(run_[a-z0-9]+)/)) {
		if (/^run_[a-z0-9]+$/.test(part)) {
			const a = document.createElement("a");
			a.href = "/ui/runs/" + part;
			a.textContent = part;
			cell.appendChild(a);
		} else if (part) {
			cell.appendChild(document.createTextNode(part));
		}
	}
	return cell;
}

async function loadAudit() {
	try {
		const data = await getJSON("/audit?limit=500");
		const entries = data.entries || [];
		if (entries.length === 0) {
			showEmpty("No audit entries yet. Every change is recorded here.");
			return;
		}
		const tbody = document.getElementById("audit");
		for (const e of entries) {
			const tr = document.createElement("tr");
			tr.appendChild(td(String(e.seq)));
			tr.appendChild(tdTime(e.at));
			tr.appendChild(td(e.actor || "-"));
			tr.appendChild(td(e.method, "mono"));
			tr.appendChild(auditPathCell(e.path));
			tr.appendChild(td((e.hash || "").slice(0, 12), "mono"));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load the audit trail: " + e.message);
	}
}

// wireAudit hooks the verify and export buttons. Verify recomputes the chain and shows a badge;
// export downloads the signed snapshot for offline verification.
function wireAudit() {
	const badge = document.getElementById("audit-badge");
	const verify = document.getElementById("audit-verify");
	if (verify) {
		verify.addEventListener("click", async () => {
			badge.hidden = false;
			badge.className = "chip none";
			badge.textContent = "Verifying...";
			try {
				const r = await getJSON("/audit/verify");
				if (r.ok) {
					badge.className = "chip ok";
					badge.textContent = "Chain verified: " + r.count + " entries";
				} else {
					badge.className = "chip failed";
					badge.textContent = "Tampered at entry " + r.broke_at;
				}
			} catch (err) {
				badge.className = "chip failed";
				badge.textContent = "Verify failed: " + err.message;
			}
		});
	}
	const exp = document.getElementById("audit-export");
	if (exp) {
		exp.addEventListener("click", async () => {
			try {
				const data = await getJSON("/audit/export");
				const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
				const url = URL.createObjectURL(blob);
				const a = document.createElement("a");
				a.href = url;
				a.download = "audit-export.json";
				a.click();
				URL.revokeObjectURL(url);
			} catch (err) {
				setStatus("Export failed: " + err.message);
			}
		});
	}
}

// tdTime builds a table cell showing a relative age with the full timestamp on hover. It carries
// the timestamp so a background timer can keep the age current.
function tdTime(iso, empty) {
	const el = td(iso ? relTime(iso) : (empty || ""));
	if (iso) {
		el.title = fmtTime(iso);
		el.dataset.time = iso;
		el.classList.add("reltime");
	}
	return el;
}

// refreshRelTimes rewrites every relative-age cell so ages stay current without a reload.
function refreshRelTimes() {
	for (const el of document.querySelectorAll(".reltime[data-time]")) {
		el.textContent = relTime(el.dataset.time);
	}
}

// wireModal wires a create dialog: the open button shows it; the close button, a backdrop click, and
// Escape hide it. name is the shared id prefix for the open, modal, and close elements.
function wireModal(name) {
	const openBtn = document.getElementById(name + "-open");
	const modal = document.getElementById(name + "-modal");
	if (!openBtn || !modal) return;
	const card = modal.querySelector(".modal-card");
	if (card) {
		card.setAttribute("role", "dialog");
		card.setAttribute("aria-modal", "true");
	}
	let opener = null;
	const close = () => {
		modal.hidden = true;
		if (opener && opener.focus) opener.focus();
	};
	openBtn.addEventListener("click", () => {
		opener = document.activeElement;
		modal.hidden = false;
		const first = modal.querySelector("input, select, textarea") ||
			modal.querySelector("button:not(.modal-close)");
		if (first) first.focus();
	});
	const closeBtn = document.getElementById(name + "-close");
	if (closeBtn) closeBtn.addEventListener("click", close);
	modal.addEventListener("click", (e) => { if (e.target === modal) close(); });
	document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !modal.hidden) close(); });
	modal.addEventListener("keydown", (e) => {
		if (e.key !== "Tab" || modal.hidden) return;
		const focusable = modal.querySelectorAll(
			"a[href], button:not([disabled]), input:not([disabled]), " +
			"select:not([disabled]), textarea:not([disabled])");
		if (focusable.length === 0) return;
		const first = focusable[0];
		const last = focusable[focusable.length - 1];
		if (e.shiftKey && document.activeElement === first) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && document.activeElement === last) {
			e.preventDefault();
			first.focus();
		}
	});
}

// closeModal hides a create dialog by name, used after a successful save.
function closeModal(name) {
	const modal = document.getElementById(name + "-modal");
	if (modal) modal.hidden = true;
}

// setModalTitle rewrites a dialog's heading, used to switch a create dialog between add and edit.
function setModalTitle(name, text) {
	const h = document.querySelector("#" + name + "-modal .modal-head h2");
	if (h) h.textContent = text;
}

// editButton builds an inline Edit action for a table row. Its click does not bubble, so the row's
// inspect drawer stays closed.
function editButton(onClick) {
	const b = document.createElement("button");
	b.className = "button";
	b.textContent = "Edit";
	b.addEventListener("click", (e) => {
		e.preventDefault();
		e.stopPropagation();
		onClick();
	});
	return b;
}

// wireLaunchForm hooks the launch panel up to POST /runs and fills the credential picker. The tool
// selector swaps the Ansible fields for a single command box, so bash, terraform, python, and go
// launch from the same panel.
function wireLaunchForm() {
	const form = document.getElementById("launch-form");
	if (!form) return;
	fillCredentialPicker();
	fillSelect(document.getElementById("launch-project"), "/projects", "projects", (p) => p.name);
	fillSelect(document.getElementById("launch-inventory-id"), "/inventories", "inventories",
		(i) => i.name);

	const toolSel = document.getElementById("launch-tool");
	const ansibleFields = ["launch-field-playbook", "launch-field-inventory",
		"launch-field-inventory-id", "launch-field-shards"];
	const commandField = document.getElementById("launch-field-command");
	const commandInput = document.getElementById("launch-command");
	const syncTool = () => {
		const tool = toolSel.value;
		const ansible = tool === "ansible" || tool === "";
		for (const id of ansibleFields) {
			const el = document.getElementById(id);
			if (el) el.hidden = !ansible;
		}
		commandField.hidden = ansible;
		if (tool === "terraform" || tool === "opentofu") commandInput.placeholder = "working directory, e.g. infra";
		else if (tool === "python") commandInput.placeholder = "print('hello from python')";
		else if (tool === "go") commandInput.placeholder = "package main\n\nfunc main() { println(\"hi\") }";
		else commandInput.placeholder = "echo hello";
	};
	toolSel.addEventListener("change", syncTool);
	syncTool();

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("launch-status");
		const tool = toolSel.value;
		const payload = {};
		if (tool && tool !== "ansible") {
			payload.tool = tool;
			payload.command = commandInput.value.trim();
		} else {
			payload.playbook = document.getElementById("launch-playbook").value.trim();
			payload.inventory = document.getElementById("launch-inventory").value.trim();
			const inventoryID = document.getElementById("launch-inventory-id").value;
			if (inventoryID) {
				payload.inventory_id = inventoryID;
				delete payload.inventory;
			}
			const shards = parseInt(document.getElementById("launch-shards").value, 10);
			if (shards >= 2) payload.shards = shards;
		}
		const projectID = document.getElementById("launch-project").value;
		if (projectID) payload.project_id = projectID;
		const queue = document.getElementById("launch-queue").value.trim();
		if (queue) payload.queue = queue;
		const picked = Array.from(document.getElementById("launch-credentials").selectedOptions)
			.map((o) => o.value);
		if (picked.length) payload.credential_ids = picked;
		if (document.getElementById("launch-dry-run").checked) payload.dry_run = true;
		if (document.getElementById("launch-require-approval").checked) payload.require_approval = true;
		status.textContent = "Launching.";
		try {
			const created = await postAction("/runs", payload);
			location.href = "/ui/runs/" + created.id;
		} catch (err) {
			status.textContent = "Launch failed: " + err.message;
		}
	});
}

// fillCredentialPicker loads stored credentials into the launch multiselect.
async function fillCredentialPicker() {
	const picker = document.getElementById("launch-credentials");
	if (!picker) return;
	try {
		const data = await getJSON("/credentials");
		for (const c of data.credentials || []) {
			const opt = document.createElement("option");
			opt.value = c.id;
			opt.textContent = c.name + " (" + c.kind + ")";
			picker.appendChild(opt);
		}
	} catch (_) { /* credentials disabled or unauthorized; picker stays empty */ }
}

// openCredentialEdit fills the credential dialog with an existing record and switches it to edit
// mode. The secret field becomes optional, so a blank keeps the stored secret; the list never
// returns secret material, so the field always starts empty.
function openCredentialEdit(c) {
	const form = document.getElementById("cred-form");
	form.dataset.editId = c.id;
	document.getElementById("cred-name").value = c.name;
	document.getElementById("cred-kind").value = c.kind;
	document.getElementById("cred-source").value = c.source || "local";
	const sec = document.getElementById("cred-secret");
	sec.value = "";
	sec.required = false;
	sec.placeholder = "Leave blank to keep the current secret";
	document.getElementById("cred-passphrase").value = "";
	toggleCredPassphrase();
	document.getElementById("cred-status").textContent = "";
	setModalTitle("cred", "Edit credential");
	document.getElementById("cred-modal").hidden = false;
}

// toggleCredPassphrase shows the passphrase field only for a locally stored SSH key, the one case
// where a passphrase unlocks the key at run time, and clears it when hidden so it is never sent.
function toggleCredPassphrase() {
	const kind = document.getElementById("cred-kind").value;
	const source = document.getElementById("cred-source").value;
	const field = document.getElementById("cred-passphrase-field");
	const show = kind === "ssh_key" && source === "local";
	field.hidden = !show;
	if (!show) document.getElementById("cred-passphrase").value = "";
}

// wireCredentialForm hooks the credential dialog up to POST /credentials for a new record and PUT
// /credentials/{id} when editing. The New button resets the dialog to add mode, where a secret is
// required; on edit the secret is only sent when changed.
function wireCredentialForm() {
	const form = document.getElementById("cred-form");
	const secPlaceholder = document.getElementById("cred-secret").placeholder;
	const source = document.getElementById("cred-source");
	const sourcePlaceholders = {
		command: "vault kv get -field=password secret/prod-fleet",
		vault: '{"addr":"https://vault:8200","path":"secret/data/ci","field":"token"}',
		vault_dynamic: '{"addr":"https://vault:8200","path":"database/creds/app","field":"password"}',
		gsm: '{"project":"my-project","secret":"ci-token","version":"latest"}',
		aws: '{"secret_id":"prod/db-password","region":"us-east-1"}',
		azure: '{"vault":"prod-kv","secret":"db-password"}',
		conjur: '{"url":"https://conjur.example.com","account":"prod","login":"host/app","api_key":"...","variable":"db/password"}',
	};
	source.addEventListener("change", () => {
		document.getElementById("cred-secret").placeholder = sourcePlaceholders[source.value] || secPlaceholder;
		toggleCredPassphrase();
	});
	document.getElementById("cred-kind").addEventListener("change", toggleCredPassphrase);
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("cred-name").value = "";
		document.getElementById("cred-source").value = "local";
		const sec = document.getElementById("cred-secret");
		sec.value = "";
		sec.required = true;
		sec.placeholder = secPlaceholder;
		document.getElementById("cred-passphrase").value = "";
		toggleCredPassphrase();
		document.getElementById("cred-status").textContent = "";
		setModalTitle("cred", "Add a credential");
	};
	const openBtn = document.getElementById("cred-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("cred-status");
		const editId = form.dataset.editId;
		const payload = {
			name: document.getElementById("cred-name").value.trim(),
			kind: document.getElementById("cred-kind").value,
			source: document.getElementById("cred-source").value,
		};
		const secret = document.getElementById("cred-secret").value;
		if (secret) payload.secret = secret;
		const passphrase = document.getElementById("cred-passphrase").value;
		if (passphrase && payload.kind === "ssh_key" && payload.source === "local") {
			payload.passphrase = passphrase;
		}
		try {
			if (editId) {
				await postAction("/credentials/" + editId, payload, "PUT");
			} else {
				await postAction("/credentials", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("cred");
			document.getElementById("credentials").innerHTML = "";
			loadCredentials();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		}
	});
}

// loadCredentials populates the credential table with delete actions.
async function loadCredentials() {
	try {
		const data = await getJSON("/credentials");
		const creds = data.credentials || [];
		if (creds.length === 0) {
			showEmpty("No credentials yet.");
			return;
		}
		renderNeedsSecret(creds);
		const tbody = document.getElementById("credentials");
		for (const c of creds) {
			const tr = document.createElement("tr");
			tr.appendChild(td(c.name));
			tr.appendChild(td(c.kind, "mono"));
			tr.appendChild(tdTime(c.created_at));
			const actions = document.createElement("td");
			const del = document.createElement("button");
			del.className = "button danger";
			del.textContent = "Delete";
			del.addEventListener("click", async (e) => {
				e.preventDefault();
				if (!window.confirm("Delete credential " + c.name + "?")) return;
				try {
					const res = await fetch(API + "/credentials/" + c.id, {
						method: "DELETE", headers: authHeaders(),
					});
					if (!res.ok) throw new Error("HTTP " + res.status);
					removeRow(tr, "No credentials yet.");
				} catch (err) {
					setStatus("Delete failed: " + err.message);
				}
			});
			actions.appendChild(editButton(() => openCredentialEdit(c)));
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(del);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load credentials: " + e.message);
	}
}

// renderNeedsSecret fills the panel listing credentials that still have no secret and lets an admin
// set each one in place, so a freshly imported set is finished on a single screen instead of opening
// each credential. In the read-only demo the inputs are disabled.
function renderNeedsSecret(creds) {
	const panel = document.getElementById("cred-needs");
	if (!panel) return;
	panel.innerHTML = "";
	const pending = creds.filter((c) => c.needs_secret);
	if (pending.length === 0) {
		panel.hidden = true;
		return;
	}
	const readOnly = isReadOnly();

	const head = document.createElement("div");
	head.className = "cred-needs-head";
	const title = document.createElement("strong");
	const setTitle = (n) => {
		title.textContent = n + (n === 1 ? " credential needs a secret" : " credentials need a secret");
	};
	setTitle(pending.length);
	const sub = document.createElement("span");
	sub.className = "cred-needs-sub";
	sub.textContent = "Set a secret on each to make it usable. Imported credentials arrive this way. ";
	const guide = document.createElement("a");
	guide.href = "/ui/docs/tutorial-set-a-secret";
	guide.textContent = "How secrets work";
	guide.dataset.tip = "Open the Set a secret guide";
	sub.appendChild(guide);
	head.appendChild(title);
	head.appendChild(sub);
	panel.appendChild(head);

	const list = document.createElement("div");
	list.className = "cred-needs-list";
	let remaining = pending.length;
	for (const c of pending) {
		const row = document.createElement("div");
		row.className = "cred-needs-row";
		const meta = document.createElement("div");
		meta.className = "cred-needs-meta";
		const name = document.createElement("span");
		name.className = "cred-needs-name";
		name.textContent = c.name;
		const kind = document.createElement("span");
		kind.className = "cred-needs-kind";
		kind.textContent = c.kind;
		meta.appendChild(name);
		meta.appendChild(kind);

		const input = document.createElement("textarea");
		input.className = "input mono cred-needs-input";
		input.rows = 2;
		input.placeholder = "Paste the secret";
		input.disabled = readOnly;
		const save = document.createElement("button");
		save.className = "button primary";
		save.textContent = "Save";
		save.disabled = readOnly;
		const status = document.createElement("span");
		status.className = "cred-needs-status muted";
		if (readOnly) status.textContent = "Disabled in the demo";

		save.addEventListener("click", async () => {
			const secret = input.value;
			if (!secret.trim()) {
				status.textContent = "Enter a secret first.";
				return;
			}
			save.disabled = true;
			status.textContent = "Saving…";
			try {
				await postAction("/credentials/" + c.id, { name: c.name, secret }, "PUT");
				row.remove();
				remaining -= 1;
				if (remaining === 0) {
					panel.hidden = true;
				} else {
					setTitle(remaining);
				}
			} catch (err) {
				save.disabled = false;
				status.textContent = "Save failed: " + err.message;
			}
		});

		row.appendChild(meta);
		row.appendChild(input);
		row.appendChild(save);
		row.appendChild(status);
		list.appendChild(row);
	}
	panel.appendChild(list);
	panel.hidden = false;
}

// fillSelect loads options into a select from a list endpoint.
async function fillSelect(el, url, listKey, labelFor) {
	try {
		const data = await getJSON(url);
		for (const item of data[listKey] || []) {
			const opt = document.createElement("option");
			opt.value = item.id;
			opt.textContent = labelFor(item);
			el.appendChild(opt);
		}
	} catch (_) { /* feature disabled or unauthorized; the select keeps its defaults */ }
}

// openProjectEdit fills the project dialog with an existing record and switches it to edit mode so
// the next save issues a PUT rather than a create.
function openProjectEdit(p) {
	const form = document.getElementById("project-form");
	form.dataset.editId = p.id;
	document.getElementById("project-name").value = p.name;
	document.getElementById("project-repo").value = p.repo_url;
	document.getElementById("project-branch").value = p.branch || "";
	document.getElementById("project-credential").value = p.credential_id || "";
	document.getElementById("project-deps").checked = p.install_deps !== false;
	document.getElementById("project-image").value = p.image || "";
	document.getElementById("project-pull-credential").value = p.pull_credential_id || "";
	document.getElementById("project-status").textContent = "";
	setModalTitle("project", "Edit project");
	document.getElementById("project-modal").hidden = false;
}

// wireProjectForm hooks the project dialog up to POST /projects for a new record and PUT
// /projects/{id} when editing. The New button resets the dialog to add mode.
function wireProjectForm() {
	fillSelect(document.getElementById("project-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	fillSelect(document.getElementById("project-pull-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	const form = document.getElementById("project-form");
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("project-name").value = "";
		document.getElementById("project-repo").value = "";
		document.getElementById("project-branch").value = "";
		document.getElementById("project-credential").value = "";
		document.getElementById("project-deps").checked = true;
		document.getElementById("project-image").value = "";
		document.getElementById("project-pull-credential").value = "";
		document.getElementById("project-status").textContent = "";
		setModalTitle("project", "Add a project");
	};
	const openBtn = document.getElementById("project-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("project-status");
		const editId = form.dataset.editId;
		const payload = {
			name: document.getElementById("project-name").value.trim(),
			repo_url: document.getElementById("project-repo").value.trim(),
			branch: document.getElementById("project-branch").value.trim(),
			credential_id: document.getElementById("project-credential").value,
			install_deps: document.getElementById("project-deps").checked,
			image: document.getElementById("project-image").value.trim(),
			pull_credential_id: document.getElementById("project-pull-credential").value,
		};
		try {
			if (editId) {
				await postAction("/projects/" + editId, payload, "PUT");
			} else {
				await postAction("/projects", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("project");
			document.getElementById("projects").innerHTML = "";
			loadProjects();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		}
	});
}

// loadProjects populates the project table with delete actions.
async function loadProjects() {
	try {
		const data = await getJSON("/projects");
		const projects = data.projects || [];
		if (projects.length === 0) {
			showEmpty("No projects yet.");
			return;
		}
		const tbody = document.getElementById("projects");
		for (const p of projects) {
			const tr = document.createElement("tr");
			tr.appendChild(td(p.name));
			// The repository is a real link when it is https, so a reader can jump straight to the
			// source; ssh and scp-style remotes stay plain text.
			const repoCell = td("", "mono");
			if (/^https?:\/\//.test(p.repo_url || "")) {
				const a = document.createElement("a");
				a.href = p.repo_url;
				a.target = "_blank";
				a.rel = "noopener";
				a.textContent = p.repo_url;
				a.addEventListener("click", (e) => e.stopPropagation());
				repoCell.appendChild(a);
			} else {
				repoCell.textContent = p.repo_url;
			}
			tr.appendChild(repoCell);
			tr.appendChild(td(p.branch || "default", "mono"));
			tr.appendChild(tdTime(p.created_at));
			const actions = deleteCell("/projects/" + p.id, "project " + p.name, tr, "No projects yet.");
			actions.insertBefore(editButton(() => openProjectEdit(p)), actions.firstChild);
			tr.appendChild(actions);
			inspectable(tr, p.name, [
				{ label: "Repository", value: p.repo_url },
				{ label: "Branch", value: p.branch || "default" },
				{ label: "Credential", value: p.credential_id },
				{ label: "Image", value: p.image },
				{ label: "Install deps", value: p.install_deps ? "yes" : "no" },
				{ label: "Pull credential", value: p.pull_credential_id },
				{ label: "Created", value: fmtTime(p.created_at) },
				{ label: "ID", value: p.id },
			]);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load projects: " + e.message);
	}
}

// wireMigrate hooks the Preview and Import buttons up to the import endpoint. Preview shows the plan;
// Import writes it. The buttons are wired even in the read-only demo, where applyReadOnly disables
// them.
function wireMigrate() {
	const preview = document.getElementById("migrate-preview");
	const apply = document.getElementById("migrate-apply");
	if (preview) preview.addEventListener("click", () => runMigrate(false));
	if (apply) apply.addEventListener("click", () => runMigrate(true));
}

// runMigrate posts the raw export text to the import endpoint and renders the result. It sends the
// textarea contents verbatim as the body, since the endpoint reads the export document itself rather
// than a JSON wrapper, and reuses the same bearer auth and 401 handling as the other API calls.
async function runMigrate(apply) {
	const status = document.getElementById("migrate-status");
	const format = document.getElementById("migrate-format").value;
	const body = document.getElementById("migrate-export").value;
	if (!body.trim()) {
		status.textContent = "Paste an export first.";
		return;
	}
	document.getElementById("migrate-plan").innerHTML = "";
	status.textContent = apply ? "Importing." : "Building preview.";
	try {
		const path = API + "/import/" + format + (apply ? "?apply=true" : "");
		const res = await fetch(path, { method: "POST", headers: authHeaders(), body });
		if (res.status === 401) {
			requireLogin();
			throw new Error("authentication required");
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
		status.textContent = apply
			? "Imported " + (data.created || 0) + " objects."
			: "Preview ready.";
		renderMigratePlan(data);
	} catch (err) {
		status.textContent = (apply ? "Import failed: " : "Preview failed: ") + err.message;
	}
}

// renderMigratePlan draws the import plan: each non-empty resource list with its names, any warnings,
// and, once applied, the count written plus the reminder to set every imported credential's secret,
// since imports create credential shells with no secret.
function renderMigratePlan(data) {
	const el = document.getElementById("migrate-plan");
	el.innerHTML = "";

	if (data.applied) {
		const done = document.createElement("div");
		done.className = "migrate-applied";
		done.textContent = "Imported " + (data.created || 0) + " objects. Set the secret on each " +
			"imported credential before running templates that need it.";
		el.appendChild(done);
	}

	const groups = [
		["Projects", data.projects],
		["Inventories", data.inventories],
		["Sources", data.sources],
		["Credentials", data.credentials],
		["Templates", data.templates],
		["Schedules", data.schedules],
	];
	for (const [label, names] of groups) {
		if (names && names.length) el.appendChild(migrateGroup(label, names));
	}

	if (data.warnings && data.warnings.length) {
		el.appendChild(migrateGroup("Warnings", data.warnings));
	}

	if (!el.children.length) el.appendChild(emptyLine("Nothing to import from this export."));
}

// migrateGroup builds a labeled block listing the names in one import category.
function migrateGroup(label, names) {
	const group = document.createElement("div");
	group.className = "migrate-group";
	const heading = document.createElement("h2");
	heading.textContent = label + " (" + names.length + ")";
	group.appendChild(heading);
	const list = document.createElement("ul");
	list.className = "migrate-list";
	for (const name of names) {
		const item = document.createElement("li");
		item.textContent = name;
		list.appendChild(item);
	}
	group.appendChild(list);
	return group;
}

// syncTemplateTool shows the Ansible fields or the command box in the template dialog to match the
// selected tool, so a bash, terraform, python, or go template hides playbook, inventory, shards,
// and the execution image.
function syncTemplateTool() {
	const tool = document.getElementById("tpl-tool").value;
	const ansible = tool === "ansible" || tool === "";
	const ansibleFields = ["tpl-field-playbook", "tpl-field-inventory", "tpl-field-shards",
		"tpl-field-image", "tpl-field-pull-credential"];
	for (const id of ansibleFields) {
		const el = document.getElementById(id);
		if (el) el.hidden = !ansible;
	}
	const cmd = document.getElementById("tpl-field-command");
	if (cmd) cmd.hidden = ansible;
}

// openTemplateEdit fills the template dialog with an existing record and switches it to edit mode.
// The dialog does not expose inventory_id, so it is carried through the form dataset to avoid
// dropping a stored inventory reference on save.
function openTemplateEdit(t) {
	const form = document.getElementById("template-form");
	form.dataset.editId = t.id;
	form.dataset.inventoryId = t.inventory_id || "";
	document.getElementById("tpl-name").value = t.name;
	document.getElementById("tpl-project").value = t.project_id || "";
	document.getElementById("tpl-playbook").value = t.playbook;
	document.getElementById("tpl-inventory").value = t.inventory || "";
	document.getElementById("tpl-shards").value = t.shards ? String(t.shards) : "";
	document.getElementById("tpl-queue").value = t.queue || "";
	document.getElementById("tpl-image").value = t.image || "";
	document.getElementById("tpl-pull-credential").value = t.pull_credential_id || "";
	const chosen = new Set(t.credential_ids || []);
	for (const opt of document.getElementById("tpl-credentials").options) {
		opt.selected = chosen.has(opt.value);
	}
	document.getElementById("tpl-vars").value = t.extra_vars ? JSON.stringify(t.extra_vars, null, 2) : "";
	document.getElementById("tpl-survey").value =
		(t.survey && t.survey.length) ? JSON.stringify(t.survey, null, 2) : "";
	document.getElementById("tpl-tool").value = t.tool || "ansible";
	document.getElementById("tpl-command").value = t.command || "";
	document.getElementById("tpl-dry-run").checked = !!t.dry_run;
	syncTemplateTool();
	document.getElementById("tpl-status").textContent = "";
	setModalTitle("template", "Edit template");
	document.getElementById("template-modal").hidden = false;
}

// wireTemplateForm hooks the template dialog up to POST /templates for a new record and PUT
// /templates/{id} when editing. The New button resets the dialog to add mode.
function wireTemplateForm() {
	fillSelect(document.getElementById("tpl-project"), "/projects", "projects", (p) => p.name);
	fillSelect(document.getElementById("tpl-credentials"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	fillSelect(document.getElementById("tpl-pull-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	const form = document.getElementById("template-form");
	document.getElementById("tpl-tool").addEventListener("change", syncTemplateTool);
	syncTemplateTool();
	const resetToCreate = () => {
		delete form.dataset.editId;
		delete form.dataset.inventoryId;
		form.reset();
		for (const opt of document.getElementById("tpl-credentials").options) opt.selected = false;
		syncTemplateTool();
		document.getElementById("tpl-status").textContent = "";
		setModalTitle("template", "Add a template");
	};
	const openBtn = document.getElementById("template-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("tpl-status");
		const tool = document.getElementById("tpl-tool").value;
		const payload = {
			name: document.getElementById("tpl-name").value.trim(),
			project_id: document.getElementById("tpl-project").value,
		};
		if (tool && tool !== "ansible") {
			payload.tool = tool;
			payload.command = document.getElementById("tpl-command").value.trim();
		} else {
			payload.playbook = document.getElementById("tpl-playbook").value.trim();
			payload.inventory = document.getElementById("tpl-inventory").value.trim();
			const shards = parseInt(document.getElementById("tpl-shards").value, 10);
			if (shards >= 2) payload.shards = shards;
			const image = document.getElementById("tpl-image").value.trim();
			if (image) {
				payload.image = image;
				const pull = document.getElementById("tpl-pull-credential").value;
				if (pull) payload.pull_credential_id = pull;
			}
		}
		if (document.getElementById("tpl-dry-run").checked) payload.dry_run = true;
		const tqueue = document.getElementById("tpl-queue").value.trim();
		if (tqueue) payload.queue = tqueue;
		const picked = Array.from(document.getElementById("tpl-credentials").selectedOptions)
			.map((o) => o.value);
		if (picked.length) payload.credential_ids = picked;
		const varsText = document.getElementById("tpl-vars").value.trim();
		if (varsText) {
			try {
				payload.extra_vars = JSON.parse(varsText);
			} catch (_) {
				status.textContent = "Extra vars must be valid JSON.";
				return;
			}
		}
		const surveyText = document.getElementById("tpl-survey").value.trim();
		if (surveyText) {
			try {
				payload.survey = JSON.parse(surveyText);
			} catch (_) {
				status.textContent = "Survey must be a valid JSON array.";
				return;
			}
		}
		const editId = form.dataset.editId;
		if (editId && form.dataset.inventoryId) payload.inventory_id = form.dataset.inventoryId;
		try {
			if (editId) {
				await postAction("/templates/" + editId, payload, "PUT");
			} else {
				await postAction("/templates", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("template");
			document.getElementById("templates").innerHTML = "";
			loadTemplates();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		}
	});
}

// loadTemplates populates the template table with launch and delete actions.
async function loadTemplates() {
	try {
		const data = await getJSON("/templates");
		const templates = data.templates || [];
		if (templates.length === 0) {
			showEmpty("No templates yet.");
			return;
		}
		const tbody = document.getElementById("templates");
		for (const t of templates) {
			const tr = document.createElement("tr");
			tr.appendChild(td(t.name));
			tr.appendChild(typeCellEl(t));
			// The playbook cell opens a read-only view of everything the template runs.
			const whatCell = td("", "mono");
			const view = document.createElement("button");
			view.type = "button";
			view.className = "linkish mono";
			view.textContent = toolLabel(t);
			view.dataset.tip = "View what this template runs";
			view.addEventListener("click", (e) => { e.preventDefault(); openTemplateView(t); });
			whatCell.appendChild(view);
			tr.appendChild(whatCell);
			tr.appendChild(td(String(t.shards || 1)));
			tr.appendChild(tdTime(t.created_at));
			const actions = document.createElement("td");
			const launch = document.createElement("button");
			launch.className = "button primary";
			launch.textContent = "Launch";
			launch.addEventListener("click", async (e) => {
				e.preventDefault();
				if (t.survey && t.survey.length) {
					openSurvey(t);
					return;
				}
				launch.disabled = true;
				try {
					const created = await postAction("/templates/" + t.id + "/launch");
					location.href = "/ui/runs/" + created.id;
				} catch (err) {
					setStatus("Launch failed: " + err.message);
					launch.disabled = false;
				}
			});
			actions.appendChild(launch);
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(editButton(() => openTemplateEdit(t)));
			actions.appendChild(document.createTextNode(" "));
			const delBtn = deleteCell("/templates/" + t.id, "template " + t.name, tr, "No templates yet.");
			actions.appendChild(delBtn.firstChild);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load templates: " + e.message);
	}
}

// openTemplateView shows everything a template runs in a read-only dialog: its full command or
// playbook, tool, shards, and the rest, since list cells truncate.
function openTemplateView(t) {
	let overlay = document.getElementById("view-modal");
	if (!overlay) {
		overlay = document.createElement("div");
		overlay.id = "view-modal";
		overlay.className = "modal";
		overlay.hidden = true;
		overlay.innerHTML = '<div class="modal-card wide"><div class="modal-head"><h2 id="view-title"></h2>' +
			'<button type="button" class="modal-close" aria-label="Close">\u00d7</button></div>' +
			'<div class="view-rows" id="view-rows"></div><pre class="log view-code" id="view-code"></pre></div>';
		document.body.appendChild(overlay);
		overlay.addEventListener("mousedown", (e) => { if (e.target === overlay) overlay.hidden = true; });
		overlay.querySelector(".modal-close").addEventListener("click", () => { overlay.hidden = true; });
		document.addEventListener("keydown", (e) => { if (e.key === "Escape") overlay.hidden = true; });
	}
	document.getElementById("view-title").textContent = t.name;
	const rows = document.getElementById("view-rows");
	rows.innerHTML = "";
	const addRow = (k, v) => {
		if (!v && v !== 0) return;
		const key = document.createElement("span");
		key.className = "view-k";
		key.textContent = k;
		const val = document.createElement("span");
		val.className = "view-v";
		val.textContent = String(v);
		rows.appendChild(key);
		rows.appendChild(val);
	};
	addRow("Tool", (t.tool || "ansible"));
	addRow("Playbook", t.playbook);
	addRow("Inventory", t.inventory);
	addRow("Shards", t.shards && t.shards > 1 ? t.shards : "");
	addRow("Limit", t.limit);
	addRow("Created", t.created_at ? fmtTime(t.created_at) : "");
	const code = document.getElementById("view-code");
	code.hidden = !t.command;
	if (t.command) code.textContent = t.command;
	overlay.hidden = false;
}

// openSurvey renders a template's survey as a form and launches with the collected answers.
function openSurvey(t) {
	const modal = document.getElementById("survey-modal");
	const form = document.getElementById("survey-form");
	document.getElementById("survey-title").textContent = "Launch " + t.name;
	document.getElementById("survey-status").textContent = "";
	form.innerHTML = "";
	for (const f of t.survey) {
		const label = document.createElement("label");
		label.className = "field-label";
		label.textContent = (f.label || f.var) + (f.required ? " *" : "");
		let input;
		if (f.type === "choice") {
			input = document.createElement("select");
			for (const c of f.choices || []) {
				const opt = document.createElement("option");
				opt.value = c;
				opt.textContent = c;
				input.appendChild(opt);
			}
		} else if (f.type === "bool") {
			input = document.createElement("select");
			for (const v of ["false", "true"]) {
				const opt = document.createElement("option");
				opt.value = v;
				opt.textContent = v;
				input.appendChild(opt);
			}
		} else {
			input = document.createElement("input");
			input.type = f.type === "int" ? "number" : "text";
		}
		input.className = "input";
		input.dataset.var = f.var;
		input.dataset.type = f.type || "text";
		if (f.default !== undefined && f.default !== null) input.value = f.default;
		label.appendChild(input);
		form.appendChild(label);
	}
	modal.hidden = false;

	document.getElementById("survey-cancel").onclick = () => { modal.hidden = true; };
	document.getElementById("survey-go").onclick = async () => {
		const answers = {};
		for (const el of form.querySelectorAll("[data-var]")) {
			const raw = el.value;
			if (raw === "") continue;
			if (el.dataset.type === "int") answers[el.dataset.var] = parseInt(raw, 10);
			else if (el.dataset.type === "bool") answers[el.dataset.var] = raw === "true";
			else answers[el.dataset.var] = raw;
		}
		try {
			const created = await postAction("/templates/" + t.id + "/launch", { answers });
			location.href = "/ui/runs/" + created.id;
		} catch (err) {
			document.getElementById("survey-status").textContent = "Launch failed: " + err.message;
		}
	};
}

// deleteCell builds a table cell holding a delete button for a resource.
function deleteCell(path, label, tr, emptyMsg) {
	const cell = document.createElement("td");
	const del = document.createElement("button");
	del.className = "button danger";
	del.textContent = "Delete";
	del.addEventListener("click", async (e) => {
		e.preventDefault();
		e.stopPropagation();
		if (!window.confirm("Delete " + label + "?")) return;
		try {
			const res = await fetch(API + path, { method: "DELETE", headers: authHeaders() });
			if (!res.ok) throw new Error("HTTP " + res.status);
			removeRow(tr, emptyMsg);
		} catch (err) {
			setStatus("Delete failed: " + err.message);
		}
	});
	cell.appendChild(del);
	return cell;
}

// openInventoryEdit fills the inventory dialog with an existing record and switches it to edit mode
// so the next save issues a PUT rather than a create. The source config is never returned, so its
// fields start blank and a blank keeps the stored config.
function openInventoryEdit(inv) {
	const form = document.getElementById("inventory-form");
	form.dataset.editId = inv.id;
	document.getElementById("inv-name").value = inv.name;
	document.getElementById("inv-content").value = inv.content || "";
	document.getElementById("inv-queue").value = inv.queue || "";
	for (const id of ["inv-command", "inv-vault-addr", "inv-vault-path", "inv-vault-field",
		"inv-vault-token", "inv-gsm-project", "inv-gsm-secret", "inv-gsm-version", "inv-gsm-token",
		"inv-aws-secret-id", "inv-aws-region", "inv-aws-access-key", "inv-aws-secret-key",
		"inv-azure-vault", "inv-azure-secret", "inv-azure-tenant", "inv-azure-client",
		"inv-azure-client-secret"]) {
		document.getElementById(id).value = "";
	}
	const sourceSel = document.getElementById("inv-content-source");
	sourceSel.value = inv.content_source || "local";
	sourceSel.dispatchEvent(new Event("change"));
	const ids = inv.credential_ids || [];
	for (const o of document.getElementById("inv-credentials").options) o.selected = ids.includes(o.value);
	document.getElementById("inv-status").textContent = "";
	setModalTitle("inventory", "Edit inventory");
	document.getElementById("inventory-modal").hidden = false;
}

// applyInventorySource fills an inventory payload from the selected content source. A local source
// sends the pasted content; a command, Vault, Google Secret Manager, AWS Secrets Manager, or Azure
// Key Vault source assembles the config the API seals, so the operator never hand writes JSON. It
// throws with a message when a required field is missing. On edit the config is never returned, so
// leaving a source's fields blank keeps the stored config.
function applyInventorySource(payload, src, editId) {
	const val = (id) => document.getElementById(id).value.trim();
	if (src === "local") {
		const content = document.getElementById("inv-content").value;
		if (!content) throw new Error("Paste the inventory content, or pick another source.");
		payload.content = content;
		return;
	}
	if (src === "command") {
		const cmd = val("inv-command");
		if (cmd) payload.content_config = cmd;
		else if (!editId) throw new Error("Enter the command that prints the inventory.");
		return;
	}
	if (src === "vault") {
		const addr = val("inv-vault-addr"), path = val("inv-vault-path"), field = val("inv-vault-field");
		const token = val("inv-vault-token");
		if (addr || path || field || token) {
			if (!(addr && path && field)) throw new Error("Vault needs an address, path, and field.");
			const cfg = { addr, path, field };
			if (token) cfg.token = token;
			payload.content_config = JSON.stringify(cfg);
		} else if (!editId) {
			throw new Error("Fill in the Vault address, path, and field.");
		}
		return;
	}
	if (src === "gsm") {
		const project = val("inv-gsm-project"), secret = val("inv-gsm-secret");
		const version = val("inv-gsm-version"), token = val("inv-gsm-token");
		if (project || secret || version || token) {
			if (!(project && secret)) throw new Error("Google Secret Manager needs a project and secret.");
			const cfg = { project, secret };
			if (version) cfg.version = version;
			if (token) cfg.token = token;
			payload.content_config = JSON.stringify(cfg);
		} else if (!editId) {
			throw new Error("Fill in the Google Secret Manager project and secret.");
		}
		return;
	}
	if (src === "aws") {
		const secretId = val("inv-aws-secret-id"), region = val("inv-aws-region");
		const accessKey = val("inv-aws-access-key"), secretKey = val("inv-aws-secret-key");
		if (secretId || region || accessKey || secretKey) {
			if (!secretId) throw new Error("AWS Secrets Manager needs a secret id.");
			const cfg = { secret_id: secretId };
			if (region) cfg.region = region;
			if (accessKey) cfg.access_key_id = accessKey;
			if (secretKey) cfg.secret_access_key = secretKey;
			payload.content_config = JSON.stringify(cfg);
		} else if (!editId) {
			throw new Error("Fill in the AWS Secrets Manager secret id.");
		}
		return;
	}
	if (src === "azure") {
		const vault = val("inv-azure-vault"), secret = val("inv-azure-secret");
		const tenant = val("inv-azure-tenant"), client = val("inv-azure-client");
		const clientSecret = val("inv-azure-client-secret");
		if (vault || secret || tenant || client || clientSecret) {
			if (!(vault && secret)) throw new Error("Azure Key Vault needs a vault and secret.");
			const cfg = { vault, secret };
			if (tenant) cfg.tenant_id = tenant;
			if (client) cfg.client_id = client;
			if (clientSecret) cfg.client_secret = clientSecret;
			payload.content_config = JSON.stringify(cfg);
		} else if (!editId) {
			throw new Error("Fill in the Azure Key Vault vault and secret.");
		}
	}
}

// wireInventoryForm hooks the inventory dialog up to POST /inventories for a new record and PUT
// /inventories/{id} when editing. The content source select swaps the stored-content box for the
// fields of a command, Vault, or Google Secret Manager source. The New button resets the dialog to
// add mode.
function wireInventoryForm() {
	const form = document.getElementById("inventory-form");
	const creds = document.getElementById("inv-credentials");
	const sourceSel = document.getElementById("inv-content-source");
	const hint = document.getElementById("inv-source-hint");
	const sourceFields = ["inv-content", "inv-command", "inv-vault-addr", "inv-vault-path",
		"inv-vault-field", "inv-vault-token", "inv-gsm-project", "inv-gsm-secret", "inv-gsm-version",
		"inv-gsm-token"];
	fillSelect(creds, "/credentials", "credentials", (c) => c.name + " (" + c.kind + ")");

	const syncSource = () => {
		const src = sourceSel.value;
		for (const g of form.querySelectorAll("[data-source-group]")) {
			g.hidden = g.id !== "inv-source-" + src;
		}
		hint.hidden = !(form.dataset.editId && src !== "local");
	};
	sourceSel.addEventListener("change", syncSource);

	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("inv-name").value = "";
		document.getElementById("inv-queue").value = "";
		for (const id of sourceFields) document.getElementById(id).value = "";
		for (const o of creds.options) o.selected = false;
		sourceSel.value = "local";
		syncSource();
		document.getElementById("inv-status").textContent = "";
		setModalTitle("inventory", "Add an inventory");
	};
	const openBtn = document.getElementById("inventory-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("inv-status");
		const editId = form.dataset.editId;
		const payload = {
			name: document.getElementById("inv-name").value.trim(),
			content_source: sourceSel.value,
		};
		try {
			applyInventorySource(payload, sourceSel.value, editId);
		} catch (err) {
			status.textContent = err.message;
			return;
		}
		const picked = Array.from(creds.selectedOptions).map((o) => o.value);
		if (picked.length) payload.credential_ids = picked;
		const invQueue = document.getElementById("inv-queue").value.trim();
		if (invQueue) payload.queue = invQueue;
		try {
			if (editId) {
				await postAction("/inventories/" + editId, payload, "PUT");
			} else {
				await postAction("/inventories", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("inventory");
			document.getElementById("inventories").innerHTML = "";
			loadInventories();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		}
	});
}

// loadInventories populates the inventory table with delete actions.
async function loadInventories() {
	try {
		const data = await getJSON("/inventories");
		const inventories = data.inventories || [];
		if (inventories.length === 0) {
			showEmpty("No inventories yet.");
			return;
		}
		const tbody = document.getElementById("inventories");
		for (const i of inventories) {
			const tr = document.createElement("tr");
			tr.appendChild(td(i.name));
			tr.appendChild(tdTime(i.created_at));
			const actions = deleteCell("/inventories/" + i.id, "inventory " + i.name, tr, "No inventories yet.");
			actions.insertBefore(editButton(() => openInventoryEdit(i)), actions.firstChild);
			tr.appendChild(actions);
			inspectable(tr, i.name, [
				{ label: "Created", value: fmtTime(i.created_at) },
				{ label: "ID", value: i.id },
				{ label: "Content", value: i.content, block: true },
			]);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load inventories: " + e.message);
	}
}

// openSourceEdit fills the source dialog with an existing record and switches it to edit mode so
// the next save issues a PUT rather than a create.
function openSourceEdit(src) {
	const form = document.getElementById("source-form");
	form.dataset.editId = src.id;
	document.getElementById("src-name").value = src.name;
	document.getElementById("src-source").value = src.source;
	document.getElementById("src-credential").value = src.credential_id || "";
	document.getElementById("src-project").value = src.project_id || "";
	document.getElementById("src-status").textContent = "";
	setModalTitle("source", "Edit source");
	document.getElementById("source-modal").hidden = false;
}

// wireSourceForm hooks the source dialog up to POST /inventory-sources for a new record and PUT
// /inventory-sources/{id} when editing. The New button resets the dialog to add mode.
function wireSourceForm() {
	fillSelect(document.getElementById("src-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	fillSelect(document.getElementById("src-project"), "/projects", "projects", (p) => p.name);
	const form = document.getElementById("source-form");
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("src-name").value = "";
		document.getElementById("src-source").value = "";
		document.getElementById("src-credential").value = "";
		document.getElementById("src-project").value = "";
		document.getElementById("src-status").textContent = "";
		setModalTitle("source", "Add a source");
	};
	const openBtn = document.getElementById("source-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("src-status");
		const editId = form.dataset.editId;
		const payload = {
			name: document.getElementById("src-name").value.trim(),
			source: document.getElementById("src-source").value.trim(),
			credential_id: document.getElementById("src-credential").value,
			project_id: document.getElementById("src-project").value,
		};
		try {
			if (editId) {
				await postAction("/inventory-sources/" + editId, payload, "PUT");
			} else {
				await postAction("/inventory-sources", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("source");
			document.getElementById("sources").innerHTML = "";
			loadSources();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		}
	});
}

// loadSources populates the source table with refresh and delete actions.
async function loadSources() {
	try {
		const data = await getJSON("/inventory-sources");
		const sources = data.sources || [];
		if (sources.length === 0) {
			showEmpty("No inventory sources yet.");
			return;
		}
		const tbody = document.getElementById("sources");
		for (const src of sources) {
			const tr = document.createElement("tr");
			tr.appendChild(td(src.name));
			tr.appendChild(td(src.source, "mono"));
			tr.appendChild(tdTime(src.synced_at, "never"));
			const state = document.createElement("td");
			const chip = document.createElement("span");
			chip.className = src.last_error ? "chip failed" : (src.synced_at ? "chip ok" : "chip none");
			chip.textContent = src.last_error ? "error" : (src.synced_at ? "synced" : "pending");
			if (src.last_error) chip.title = src.last_error;
			state.appendChild(chip);
			tr.appendChild(state);
			const actions = document.createElement("td");
			const refresh = document.createElement("button");
			refresh.className = "button primary";
			refresh.textContent = "Refresh";
			refresh.addEventListener("click", async (e) => {
				e.preventDefault();
				refresh.disabled = true;
				try {
					await postAction("/inventory-sources/" + src.id + "/refresh");
					document.getElementById("sources").innerHTML = "";
					loadSources();
				} catch (err) {
					setStatus("Refresh failed: " + err.message);
					refresh.disabled = false;
				}
			});
			actions.appendChild(refresh);
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(editButton(() => openSourceEdit(src)));
			actions.appendChild(document.createTextNode(" "));
			const del = deleteCell("/inventory-sources/" + src.id, "source " + src.name, tr,
				"No inventory sources yet.");
			actions.appendChild(del.firstChild);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load sources: " + e.message);
	}
}

// loadWorkers populates the executor table, marking anyone silent past the lease window stale.
async function loadWorkers() {
	try {
		const data = await getJSON("/workers");
		const workers = data.workers || [];
		if (workers.length === 0) {
			showEmpty("No executors seen yet. Run something.");
			return;
		}
		const tbody = document.getElementById("workers");
		for (const w of workers) {
			const tr = document.createElement("tr");
			tr.appendChild(td(w.owner, "mono"));
			const health = document.createElement("td");
			// An executor with no active leases has nothing to renew, so silence means idle, not
			// broken. Stale is reserved for a held lease whose renewals have stopped.
			const fresh = Date.now() - new Date(w.last_seen).getTime() < 30000;
			const chip = document.createElement("span");
			if (w.active > 0 && fresh) {
				chip.className = "chip ok";
				chip.textContent = "active";
			} else if (w.active > 0) {
				chip.className = "chip failed";
				chip.textContent = "stale";
				chip.title = "Holds runs but has stopped renewing its lease.";
			} else {
				chip.className = "chip none";
				chip.textContent = "idle";
			}
			health.appendChild(chip);
			tr.appendChild(health);
			tr.appendChild(td(String(w.active)));
			tr.appendChild(td(String(w.completed || 0)));
			tr.appendChild(td(String(w.failed || 0)));
			tr.appendChild(tdTime(w.last_seen));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load workers: " + e.message);
	}
}

// wireAsk hooks the fleet question box up to the ask endpoint. Advisory only: the answer comes
// from run, health, and drift metadata the viewer can already see, and asking changes nothing.
// explainReadOnly gives every dimmed table action a tooltip in read-only mode, so a disabled
// Launch or Delete reads as policy, not breakage.
function explainReadOnly() {
	if (!isReadOnly()) return;
	document.addEventListener("mouseover", (e) => {
		const b = e.target.closest && e.target.closest("table .button");
		if (b && !b.title) b.title = "Disabled in this read-only demo. Self-host to use it.";
	});
}

function wireAsk() {
	const go = document.getElementById("ask-go");
	const input = document.getElementById("ask-input");
	if (!go || !input) return;
	if (isReadOnly()) {
		go.disabled = true;
		input.disabled = true;
		const status = document.getElementById("ask-status");
		if (status) {
			status.textContent = "Asking is off in this read-only demo. Self-host with your own AI provider, local Ollama included, and this box answers from your fleet's run, health, and drift data.";
			status.hidden = false;
		}
		return;
	}
	go.addEventListener("click", askFleet);
	input.addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			e.preventDefault();
			askFleet();
		}
	});
}

// askFleet posts the question and renders the answer, keeping the button disabled while one
// question is in flight.
async function askFleet() {
	const input = document.getElementById("ask-input");
	const go = document.getElementById("ask-go");
	const status = document.getElementById("ask-status");
	const answer = document.getElementById("ask-answer");
	const question = input.value.trim();
	if (!question) {
		status.textContent = "Type a question first.";
		status.hidden = false;
		return;
	}
	go.disabled = true;
	answer.hidden = true;
	status.textContent = "Thinking.";
	status.hidden = false;
	try {
		const res = await fetch(API + "/ai/ask", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify({ question }),
		});
		if (res.status === 401) {
			requireLogin();
			return;
		}
		if (res.status === 404) {
			status.textContent = "AI is not enabled on this server.";
			return;
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
		answer.textContent = data.answer || "";
		answer.hidden = false;
		status.hidden = true;
	} catch (err) {
		status.textContent = "Could not answer: " + err.message;
	} finally {
		go.disabled = false;
	}
}

// canOperate reports whether the signed-in role may launch or propose work. An empty role means
// the instance has no accounts and is open, so operating is allowed.
function canOperate() {
	const role = localStorage.getItem("st_role");
	return !role || role === "operator" || role === "admin";
}

// wirePropose reveals the propose box on the runs page for an operator on a writable instance and
// hooks it up to the propose endpoint. Advisory only: the proposal is held for approval and runs
// nothing until an admin releases it.
function wirePropose() {
	const panel = document.getElementById("propose-panel");
	if (!panel || isReadOnly() || !canOperate()) return;
	panel.hidden = false;
	const go = document.getElementById("propose-go");
	const input = document.getElementById("propose-input");
	go.addEventListener("click", proposeRun);
	input.addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			e.preventDefault();
			proposeRun();
		}
	});
}

// proposeRun asks the server to turn a description into a run held for approval, then opens the
// held proposal so it can be reviewed and released or rejected.
async function proposeRun() {
	const input = document.getElementById("propose-input");
	const go = document.getElementById("propose-go");
	const status = document.getElementById("propose-status");
	const intent = input.value.trim();
	if (!intent) {
		status.textContent = "Describe the run first.";
		status.hidden = false;
		return;
	}
	go.disabled = true;
	status.textContent = "Proposing.";
	status.hidden = false;
	try {
		const res = await fetch(API + "/ai/propose-run", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify({ intent }),
		});
		if (res.status === 401) {
			requireLogin();
			return;
		}
		if (res.status === 404) {
			status.textContent = "AI is not enabled on this server.";
			return;
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
		window.location.assign("/ui/runs/" + data.id);
	} catch (err) {
		status.textContent = "Could not propose a run: " + err.message;
	} finally {
		go.disabled = false;
	}
}

// loadOverview draws the home dashboard: headline metrics, recent runs, a fleet snapshot, and the
// jump tiles that navigate to every section.
async function loadOverview() {
	renderJumpTiles();
	wireTileFilter();
	try {
		const [runsRes, fleetRes] = await Promise.all([
			getJSON("/runs"),
			getJSON("/fleet"),
		]);
		const runs = runsRes.runs || [];
		const hosts = fleetRes.hosts || [];
		renderOverviewMetrics(runs, hosts);
		renderRecentRuns(runs.slice(0, 8));
		renderFleetSnapshot(hosts);
		setStatus("");
	} catch (e) {
		setStatus("Failed to load the overview: " + e.message);
	}
}

// renderOverviewMetrics fills the headline metric strip from the run and fleet data.
function renderOverviewMetrics(runs, hosts) {
	let succeeded = 0;
	let failed = 0;
	for (const r of runs) {
		if (r.status === "succeeded") succeeded++;
		else if (r.status === "failed") failed++;
	}
	const rate = runs.length ? Math.round((succeeded / runs.length) * 100) + "%" : "-";
	const el = document.getElementById("ov-metrics");
	el.innerHTML = "";
	el.appendChild(statCard(runs.length, "Total runs", ""));
	el.appendChild(statCard(rate, "Success rate", ""));
	el.appendChild(statCard(failed, "Failed", failed ? "failed" : ""));
	el.appendChild(statCard(hosts.length, "Hosts tracked", ""));
	el.hidden = false;
}

// renderRecentRuns lists the latest runs, each a link to its detail page, under a labeled header
// row that only shows when there are rows to label.
function renderRecentRuns(runs) {
	const el = document.getElementById("recent");
	el.innerHTML = "";
	const head = document.getElementById("ov-head");
	if (head) head.hidden = !runs.length;
	if (!runs.length) { el.appendChild(emptyLine("No runs yet.")); return; }
	for (const r of runs) {
		const row = document.createElement("a");
		row.className = "ov-row";
		row.href = "/ui/runs/" + r.id;
		row.appendChild(badge(r.status));
		const name = document.createElement("span");
		name.className = "ov-row-name";
		name.textContent = toolLabel(r);
		name.title = r.playbook || r.command || r.id;
		row.appendChild(name);
		const started = r.started_at || r.created_at;
		const meta = document.createElement("span");
		meta.className = "ov-row-meta";
		if (started) {
			meta.textContent = relTime(started);
			meta.title = fmtTime(started);
			meta.dataset.time = started;
			meta.classList.add("reltime");
		}
		row.appendChild(meta);
		const go = document.createElement("span");
		go.className = "ov-row-go";
		go.innerHTML = svgIcon('<polyline points="9 18 15 12 9 6"/>');
		row.appendChild(go);
		el.appendChild(row);
	}
}

// renderFleetSnapshot fills the overview side card with the hosts most worth a look: flaky
// first, then by failure count.
function renderFleetSnapshot(hosts) {
	const el = document.getElementById("ov-fleet");
	if (!el) return;
	const head = document.getElementById("ov-fleet-head");
	el.innerHTML = "";
	const ranked = hosts.slice().sort((a, b) =>
		((b.flaky ? 1 : 0) - (a.flaky ? 1 : 0)) || (b.failures - a.failures) || a.host.localeCompare(b.host)).slice(0, 6);
	if (head) head.hidden = !ranked.length;
	if (!ranked.length) { el.appendChild(emptyLine("No host history yet.")); return; }
	for (const h of ranked) {
		const row = document.createElement("a");
		row.className = "ov-row";
		row.href = "/ui/hosts/" + encodeURIComponent(h.host);
		const name = document.createElement("span");
		name.className = "ov-row-name mono";
		name.textContent = h.host;
		row.appendChild(name);
		const meta = document.createElement("span");
		meta.className = "ov-row-meta" + (h.failures ? " fail-count" : "");
		meta.textContent = h.failures + " / " + h.total + " failed";
		row.appendChild(meta);
		const chip = document.createElement("span");
		chip.className = h.flaky ? "chip flaky" : "chip none";
		chip.textContent = h.flaky ? "flaky" : "steady";
		chip.dataset.tip = h.flaky
			? "Recent outcomes alternate between pass and fail: worth a look"
			: "Recent outcomes are stable";
		row.appendChild(chip);
		const go = document.createElement("span");
		go.className = "ov-row-go";
		go.innerHTML = svgIcon('<polyline points="9 18 15 12 9 6"/>');
		row.appendChild(go);
		el.appendChild(row);
	}
}

// renderJumpTiles draws every section but the overview as three group cards, each a titled list
// of compact icon rows. Group cards cannot strand an orphan the way a flat tile wall does.
function renderJumpTiles() {
	const el = document.getElementById("tiles");
	if (!el) return;
	const role = localStorage.getItem("st_role");
	const showAdmin = !role || role === "admin";

	el.innerHTML = "";
	for (const group of NAV_GROUPS) {
		const items = group.items.filter((it) =>
			it.key !== "overview" && it.key !== "docs" && (showAdmin || !it.admin));
		if (!items.length) continue;
		const card = document.createElement("section");
		card.className = "jump-group";
		const head = document.createElement("h3");
		head.className = "jump-group-title";
		head.textContent = group.label;
		card.appendChild(head);
		for (const it of items) {
			const row = document.createElement("a");
			row.className = "jump-row";
			row.href = it.href;
			const icon = document.createElement("span");
			icon.className = "jump-icon";
			icon.innerHTML = svgIcon(NAV_ICONS[it.key] || "");
			row.appendChild(icon);
			const label = document.createElement("span");
			label.className = "jump-label";
			label.textContent = it.label;
			row.appendChild(label);
			if (it.desc) {
				const desc = document.createElement("span");
				desc.className = "jump-desc";
				desc.textContent = it.desc;
				row.appendChild(desc);
			}
			card.appendChild(row);
		}
		el.appendChild(card);
	}
}

// wireTileFilter filters the jump rows as the user types, hides groups that empty out, and jumps
// to the first match on Enter.
function wireTileFilter() {
	const input = document.getElementById("tile-filter");
	const el = document.getElementById("tiles");
	if (!input || !el) return;
	const empty = document.createElement("p");
	empty.className = "tiles-empty";
	empty.textContent = "No sections match.";
	empty.hidden = true;
	el.after(empty);

	input.addEventListener("input", () => {
		const q = input.value.trim().toLowerCase();
		let shown = 0;
		for (const row of el.querySelectorAll(".jump-row")) {
			const match = row.textContent.toLowerCase().includes(q);
			row.hidden = !match;
			if (match) shown++;
		}
		for (const card of el.querySelectorAll(".jump-group")) {
			card.hidden = !card.querySelector(".jump-row:not([hidden])");
		}
		empty.hidden = shown > 0;
	});
	input.addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			const first = el.querySelector(".jump-row:not([hidden])");
			if (first) first.click();
		}
	});
}

// emptyLine builds a muted placeholder line for an empty list.
function emptyLine(text) {
	const p = document.createElement("p");
	p.className = "muted ov-empty";
	p.textContent = text;
	return p;
}

// runsPageSize reads the runs-per-page control: a positive count, or 0 for no limit. Defaults to 20.
function runsPageSize() {
	const el = document.getElementById("runs-pagesize");
	if (!el) return 20;
	const n = parseInt(el.value, 10);
	return (Number.isNaN(n) || n < 0) ? 20 : n;
}

// runsQuery reads the runs search box, empty when there is none.
function runsQuery() {
	const el = document.getElementById("runs-search");
	return el ? el.value.trim() : "";
}

// runsFilterParams reads the status, tool, and order dropdowns into query parameters, so the server
// filters the whole run history, not just the loaded page.
function runsFilterParams() {
	let params = "";
	for (const id of ["runs-status", "runs-tool", "runs-order"]) {
		const el = document.getElementById(id);
		if (el && el.value) params += "&" + id.replace("runs-", "") + "=" + encodeURIComponent(el.value);
	}
	return params;
}

// wireRunsFilters reloads the table when a filter or order dropdown changes.
function wireRunsFilters() {
	for (const id of ["runs-status", "runs-tool", "runs-order"]) {
		const el = document.getElementById(id);
		if (el) el.addEventListener("change", loadRuns);
	}
}

// wireRunsSearch reloads the runs table from the server as the search box changes, debounced so a
// burst of keystrokes issues one request. The server searches every run, not just the loaded page.
function wireRunsSearch() {
	const el = document.getElementById("runs-search");
	if (!el) return;
	let timer;
	el.addEventListener("input", () => {
		clearTimeout(timer);
		timer = setTimeout(loadRuns, 250);
	});
}

// runsLoadGen counts run-table loads so a slow response from an earlier search or page size cannot
// overwrite the table after a newer load has already rendered.
let runsLoadGen = 0;

// loadRuns populates the run history table.
async function loadRuns() {
	const tbody = document.getElementById("runs");
	const table = document.querySelector("table.runs");
	const sizeEl = document.getElementById("runs-pagesize");
	if (sizeEl) sizeEl.onchange = () => loadRuns();
	const gen = ++runsLoadGen;
	setStatus("");
	showSkeletonRows(tbody, 6, 7);
	table.hidden = false;
	try {
		const data = await getJSON("/runs?limit=" + runsPageSize() + "&offset=0&q=" + encodeURIComponent(runsQuery()) + runsFilterParams());
		if (gen !== runsLoadGen) return;
		const runs = data.runs || [];
		tbody.innerHTML = "";
		if (runs.length === 0) {
			table.hidden = true;
			showEmpty(runsQuery() ? "No runs match your search." : "No runs yet.");
			return;
		}
		renderSummary(data.summary || {});
		appendRunRows(tbody, runs);
		wireRunsMore(tbody, runs.length, data.has_more);
	} catch (e) {
		tbody.innerHTML = "";
		table.hidden = true;
		setStatus("Failed to load runs: " + e.message);
	}
}

// toolLabel returns a short label for what a run executed: its playbook file, or its command for a
// non-Ansible tool, collapsed and truncated so a long command does not stretch the row.
function toolLabel(r) {
	if (r.playbook) return baseName(r.playbook) || r.playbook;
	const cmd = (r.command || "").replace(/\s+/g, " ").trim();
	return cmd.length > 48 ? cmd.slice(0, 47) + "…" : cmd;
}

// KIND_TIPS explains each run kind and tool chip on hover.
const KIND_TIPS = {
	pipeline: "A multi-step pipeline: each step runs after the one before it and can pass outputs on",
	split: "Split into shards across the inventory, balanced by each host's measured duration",
	dry: "A dry run: reports what would change without applying anything",
	ansible: "Runs an Ansible playbook",
	bash: "Runs a Bash script",
	terraform: "Runs Terraform",
	opentofu: "Runs OpenTofu",
	python: "Runs a Python script",
	powershell: "Runs a PowerShell script",
	go: "Runs a Go program",
};

// typeCellEl fills a table cell with the run's tool chip and any kind tags, so type lives in one
// labeled, aligned column instead of floating beside names.
function typeCellEl(r) {
	const cell = td("");
	const tool = (r.tool || "ansible").toLowerCase();
	const chip = document.createElement("span");
	chip.className = "tool-badge " + tool;
	chip.textContent = tool;
	if (KIND_TIPS[tool]) chip.dataset.tip = KIND_TIPS[tool];
	cell.appendChild(chip);
	for (const kind of [r.kind === "split" ? "split" : "", r.kind === "pipeline" ? "pipeline" : "", r.dry_run ? "dry" : ""]) {
		if (!kind) continue;
		const tag = document.createElement("span");
		tag.className = "run-kind " + kind;
		tag.textContent = kind;
		tag.dataset.tip = KIND_TIPS[kind];
		cell.appendChild(document.createTextNode(" "));
		cell.appendChild(tag);
	}
	return cell;
}

// toolBadgeEl returns a small tool badge for a non-Ansible run, or null for Ansible so the common
// case stays uncluttered.
function toolBadgeEl(r) {
	if (!r.tool || r.tool === "ansible") return null;
	const badge = document.createElement("span");
	badge.className = "tool-badge " + r.tool;
	badge.textContent = r.tool;
	return badge;
}

// appendRunRows appends one table row per run, so a page can be added without rebuilding the
// rows already shown.
function appendRunRows(tbody, runs) {
	// Continue numbering from the rows already shown, so a loaded next page extends the sequence.
	let num = tbody.querySelectorAll("tr:not(.skeleton-row)").length;
	for (const r of runs) {
		const tr = document.createElement("tr");
		tr.className = "row-nav";
		tr.tabIndex = 0;
		tr.setAttribute("role", "link");
		const openRun = () => { location.href = "/ui/runs/" + r.id; };
		tr.addEventListener("click", openRun);
		tr.addEventListener("keydown", (e) => {
			if (e.key === "Enter" || e.key === " ") { e.preventDefault(); openRun(); }
		});
		num++;
		tr.appendChild(td(String(num), "col-num"));
		tr.appendChild(tdBadge(r.status));

		const runCell = td(shortId(r.id), "mono");
		runCell.title = r.id;
		runCell.dataset.tip = "Open run details";
		tr.appendChild(runCell);

		tr.appendChild(typeCellEl(r));

		const pbCell = td(toolLabel(r));
		pbCell.title = r.playbook || r.command || "";
		tr.appendChild(pbCell);

		tr.appendChild(tdTime(r.started_at || r.created_at));
		tr.appendChild(td(fmtDuration(r.started_at, r.ended_at)));
		tbody.appendChild(tr);
	}
}

// wireRunsMore keeps a Load more control below the runs table. Each click fetches the next page
// from the current offset and appends it, so the table grows a page at a time rather than
// rendering every run at once.
function wireRunsMore(tbody, offset, hasMore) {
	let btn = document.getElementById("runs-more");
	if (!btn) {
		btn = document.createElement("button");
		btn.id = "runs-more";
		btn.className = "button load-more";
		btn.textContent = "Load more";
		const table = document.querySelector("table.runs");
		table.parentNode.insertBefore(btn, table.nextSibling);
	}
	btn.hidden = !hasMore;
	btn.onclick = async () => {
		btn.disabled = true;
		try {
			const data = await getJSON("/runs?limit=" + runsPageSize() + "&offset=" + offset + "&q=" + encodeURIComponent(runsQuery()));
			const runs = data.runs || [];
			appendRunRows(tbody, runs);
			wireRunsMore(tbody, offset + runs.length, data.has_more);
		} catch (e) {
			setStatus("Failed to load more runs: " + e.message);
		} finally {
			btn.disabled = false;
		}
	};
}

// renderSummary draws the at-a-glance stat cards above the run history.
function renderSummary(summary) {
	const el = document.getElementById("summary");
	el.innerHTML = "";
	el.appendChild(statCard(summary.total || 0, "Total runs", ""));
	el.appendChild(statCard(summary.succeeded || 0, "Succeeded", "ok"));
	el.appendChild(statCard(summary.failed || 0, "Failed", "failed"));
	el.appendChild(statCard(summary.active || 0, "Active", "running"));
	el.hidden = false;
}

// statCard builds one summary stat card.
function statCard(value, label, cls) {
	const card = document.createElement("div");
	card.className = "stat-card";
	const v = document.createElement("div");
	v.className = "stat-value" + (cls ? " " + cls : "");
	v.textContent = value;
	const l = document.createElement("div");
	l.className = "stat-label";
	l.textContent = label;
	card.appendChild(v);
	card.appendChild(l);
	return card;
}

// td builds a table cell.
function td(text, cls) {
	const el = document.createElement("td");
	el.textContent = text;
	if (cls) el.className = cls;
	return el;
}

// tdBadge builds a status badge cell.
function tdBadge(status) {
	const el = document.createElement("td");
	el.appendChild(badge(status));
	return el;
}

// badge builds a status badge span.
function badge(status) {
	const span = document.createElement("span");
	span.className = "badge " + status;
	span.textContent = status.replace(/_/g, " ");
	return span;
}

// loadFleet populates the fleet health table, hosts ranked by recent failures.
async function loadFleet() {
	try {
		const data = await getJSON("/fleet");
		const hosts = data.hosts || [];
		if (hosts.length === 0) {
			showEmpty("No host history yet. Run a playbook to build fleet health.");
			return;
		}
		const tbody = document.getElementById("fleet");
		for (const h of hosts) {
			const tr = document.createElement("tr");
			const hostCell = document.createElement("td");
			hostCell.className = "mono";
			const hostLink = document.createElement("a");
			hostLink.href = "/ui/hosts/" + encodeURIComponent(h.host);
			hostLink.textContent = h.host;
			hostCell.appendChild(hostLink);
			tr.appendChild(hostCell);
			const fails = document.createElement("td");
			fails.textContent = h.failures + " / " + h.total;
			if (h.failures > 0) {
				fails.className = "fail-count";
			}
			tr.appendChild(fails);
			const stability = document.createElement("td");
			const chip = document.createElement("span");
			chip.className = h.flaky ? "chip flaky" : "chip none";
			chip.textContent = h.flaky ? "flaky" : "steady";
			stability.appendChild(chip);
			tr.appendChild(stability);
			const sparkCell = document.createElement("td");
			sparkCell.appendChild(sparkline(h.recent || []));
			tr.appendChild(sparkCell);
			tr.appendChild(td(String(h.total)));
			const last = document.createElement("td");
			last.appendChild(outcomeChip(h.last_outcome));
			tr.appendChild(last);
			tr.appendChild(tdTime(h.last_run));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load fleet health: " + e.message);
	}
}

// loadDrift populates the drift table, hosts most drifted first, each showing its latest check.
async function loadDrift() {
	try {
		const data = await getJSON("/drift");
		const hosts = data.hosts || [];
		if (hosts.length === 0) {
			showEmpty("No drift checks yet. Run a dry run to detect drift from the desired state.");
			return;
		}
		renderDriftSummary(hosts);
		const maxDrift = Math.max(1, ...hosts.map((h) => h.drifted_tasks));
		const tbody = document.getElementById("drift");
		for (const h of hosts) {
			const tr = document.createElement("tr");
			const hostCell = document.createElement("td");
			hostCell.className = "mono";
			const hostLink = document.createElement("a");
			hostLink.href = "/ui/hosts/" + encodeURIComponent(h.host);
			hostLink.textContent = h.host;
			hostCell.appendChild(hostLink);
			tr.appendChild(hostCell);
			const state = document.createElement("td");
			const chip = document.createElement("span");
			chip.className = "chip " + (h.drifted_tasks > 0 ? "changed" : "ok");
			chip.textContent = h.drifted_tasks > 0 ? "drifted" : "in sync";
			state.appendChild(chip);
			tr.appendChild(state);
			const driftCell = td(String(h.drifted_tasks));
			const bar = document.createElement("span");
			bar.className = "mini-bar" + (h.drifted_tasks > 0 ? "" : " ok");
			const fill = document.createElement("i");
			fill.style.width = h.drifted_tasks > 0 ? Math.max(8, (h.drifted_tasks / maxDrift) * 100) + "%" : "100%";
			bar.appendChild(fill);
			bar.title = h.drifted_tasks > 0 ? h.drifted_tasks + " drifted" : "in sync";
			driftCell.appendChild(bar);
			tr.appendChild(driftCell);
			tr.appendChild(tdTime(h.checked_at));
			const runCell = document.createElement("td");
			const runLink = document.createElement("a");
			runLink.href = "/ui/runs/" + h.run_id;
			runLink.textContent = "view";
			runCell.appendChild(runLink);
			tr.appendChild(runCell);
			const actions = document.createElement("td");
			if (h.drifted_tasks > 0 && !isReadOnly() && canOperate()) {
				const btn = document.createElement("button");
				btn.type = "button";
				btn.className = "button";
				btn.textContent = "Propose reconcile";
				btn.addEventListener("click", () => proposeReconcile(h.host, btn));
				actions.appendChild(btn);
			}
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load drift status: " + e.message);
	}
}

// proposeReconcile asks the server to build a reconcile proposal for a drifted host, then opens
// the held run so an approver can review it. The proposal never executes without that approval.
async function proposeReconcile(host, btn) {
	btn.disabled = true;
	setStatus("Proposing a reconcile for " + host + ".");
	try {
		const res = await fetch(API + "/drift/reconcile", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify({ host }),
		});
		if (res.status === 401) {
			requireLogin();
			return;
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
		window.location.assign("/ui/runs/" + data.id);
	} catch (err) {
		setStatus("Could not propose a reconcile: " + err.message);
	} finally {
		btn.disabled = false;
	}
}

// loadHost populates one host's run history table, newest first.
async function loadHost(host) {
	try {
		const data = await getJSON("/hosts/" + encodeURIComponent(host) + "/runs");
		const runs = data.runs || [];
		if (runs.length === 0) {
			showEmpty("No history for this host yet.");
			return;
		}
		const tbody = document.getElementById("host-history");
		for (const r of runs) {
			const tr = document.createElement("tr");
			const runCell = document.createElement("td");
			runCell.className = "mono";
			const link = document.createElement("a");
			link.href = "/ui/runs/" + r.run_id;
			link.textContent = r.run_id;
			runCell.appendChild(link);
			tr.appendChild(runCell);
			const outcome = document.createElement("td");
			outcome.appendChild(outcomeChip(r.worst));
			tr.appendChild(outcome);
			tr.appendChild(td(String(r.ok)));
			tr.appendChild(td(String(r.changed)));
			tr.appendChild(td(String(r.failures)));
			tr.appendChild(td(r.duration_seconds ? r.duration_seconds.toFixed(1) + "s" : "0s"));
			tr.appendChild(tdTime(r.ran_at));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load host history: " + e.message);
	}
}

// loadTasks populates the task trends table with each task's recent duration aggregate.
async function loadTasks() {
	try {
		const data = await getJSON("/tasks");
		const tasks = data.tasks || [];
		if (tasks.length === 0) {
			showEmpty("No task history yet. Run a playbook to build trends.");
			return;
		}
		tasks.sort((a, b) => b.avg_seconds - a.avg_seconds);
		const tbody = document.getElementById("tasks");
		for (const t of tasks) {
			const tr = document.createElement("tr");
			tr.appendChild(td(t.task));
			const trend = document.createElement("td");
			trend.appendChild(trendChip(t.avg_seconds, t.last_seconds, t.runs));
			tr.appendChild(trend);
			const spark = document.createElement("td");
			spark.appendChild(durationSpark(t.recent));
			tr.appendChild(spark);
			tr.appendChild(td(String(t.runs)));
			tr.appendChild(td(fmtSeconds(t.avg_seconds)));
			tr.appendChild(td(fmtSeconds(t.last_seconds)));
			tr.appendChild(tdTime(t.last_run));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load task trends: " + e.message);
	}
}

// renderDriftSummary fills the drift page's stat strip: how many hosts drifted, how many are in
// sync, and when the newest check ran.
function renderDriftSummary(hosts) {
	const box = document.getElementById("drift-summary");
	if (!box) return;
	const drifted = hosts.filter((h) => h.drifted_tasks > 0).length;
	const clean = hosts.length - drifted;
	let newest = "";
	for (const h of hosts) {
		if (!newest || h.checked_at > newest) newest = h.checked_at;
	}
	box.innerHTML = "";
	const card = (value, label, cls) => {
		const c = document.createElement("div");
		c.className = "stat-card";
		const v = document.createElement("span");
		v.className = "stat-value" + (cls ? " " + cls : "");
		v.textContent = value;
		const l = document.createElement("span");
		l.className = "stat-label";
		l.textContent = label;
		c.appendChild(v);
		c.appendChild(l);
		return c;
	};
	box.appendChild(card(String(hosts.length), "hosts checked"));
	box.appendChild(card(String(drifted), "drifted", drifted > 0 ? "changed" : ""));
	box.appendChild(card(String(clean), "in sync", "ok"));
	box.appendChild(card(newest ? relTime(newest) : "never", "last check"));
	box.hidden = false;
}

// durationSpark draws a task's recent durations as a small line, oldest to newest, so the shape of
// a trend is visible at a glance. The numbers stay in the table; the mark carries only shape.
function durationSpark(values) {
	if (!values || values.length < 2) {
		const dash = document.createElement("span");
		dash.className = "muted";
		dash.textContent = "\u2013";
		return dash;
	}
	const w = 96, h = 22, pad = 3;
	const min = Math.min(...values), max = Math.max(...values);
	const span = (max - min) || 1;
	const step = (w - pad * 2) / (values.length - 1);
	const pts = values.map((v, i) => {
		const x = pad + i * step;
		const y = h - pad - ((v - min) / span) * (h - pad * 2);
		return x.toFixed(1) + "," + y.toFixed(1);
	});
	const svg = document.createElementNS("http://www.w3.org/2000/svg", "svg");
	svg.setAttribute("class", "duration-spark");
	svg.setAttribute("viewBox", "0 0 " + w + " " + h);
	svg.setAttribute("width", w);
	svg.setAttribute("height", h);
	const title = document.createElementNS(svg.namespaceURI, "title");
	title.textContent = values.map(fmtSeconds).join(" \u2192 ");
	svg.appendChild(title);
	const line = document.createElementNS(svg.namespaceURI, "polyline");
	line.setAttribute("points", pts.join(" "));
	svg.appendChild(line);
	const end = pts[pts.length - 1].split(",");
	const dot = document.createElementNS(svg.namespaceURI, "circle");
	dot.setAttribute("cx", end[0]);
	dot.setAttribute("cy", end[1]);
	dot.setAttribute("r", "2.5");
	svg.appendChild(dot);
	return svg;
}

// trendChip labels how a task's latest duration compares to its own recent average.
function trendChip(avg, last, runs) {
	const chip = document.createElement("span");
	if (runs < 2 || avg <= 0) {
		chip.className = "chip none";
		chip.textContent = "new";
	} else if (last > avg * 1.25) {
		chip.className = "chip flaky";
		chip.textContent = "slower";
	} else if (last < avg * 0.8) {
		chip.className = "chip ok";
		chip.textContent = "faster";
	} else {
		chip.className = "chip none";
		chip.textContent = "steady";
	}
	return chip;
}

// fmtSeconds renders a duration in seconds with one decimal.
function fmtSeconds(s) {
	return (s || 0).toFixed(1) + "s";
}

// loadSchedules populates the schedules table.
async function loadSchedules() {
	try {
		const tplByID = await fillTemplateSelect(null);
		const data = await getJSON("/schedules");
		const schedules = data.schedules || [];
		if (schedules.length === 0) {
			showEmpty("No schedules yet. Add one to fire a template on a cadence.");
			return;
		}
		const tbody = document.getElementById("schedules");
		for (const s of schedules) {
			const tr = document.createElement("tr");
			tr.appendChild(td(s.name || "(unnamed)"));
			tr.appendChild(td(s.cron, "mono"));
			const target = document.createElement("td");
			if (s.template_id) {
				const tpl = document.createElement("a");
				tpl.href = "/ui/templates";
				tpl.textContent = tplByID[s.template_id] || "template";
				tpl.dataset.tip = "Open templates";
				target.appendChild(tpl);
			} else {
				target.textContent = scheduleTarget(s);
			}
			tr.appendChild(target);

			const enabled = document.createElement("td");
			const chip = document.createElement("span");
			chip.className = "chip " + (s.enabled ? "ok" : "skipped");
			chip.textContent = s.enabled ? "enabled" : "disabled";
			enabled.appendChild(chip);
			tr.appendChild(enabled);

			tr.appendChild(td(fmtTime(s.next_run_at)));

			const last = document.createElement("td");
			if (s.last_run_id) {
				const link = document.createElement("a");
				link.href = "/ui/runs/" + s.last_run_id;
				link.textContent = fmtTime(s.last_run_at) || "view run";
				last.appendChild(link);
			}
			tr.appendChild(last);

			const actions = document.createElement("td");
			const del = document.createElement("button");
			del.className = "button danger";
			del.textContent = "Delete";
			del.addEventListener("click", async (e) => {
				e.preventDefault();
				if (!window.confirm("Delete schedule " + (s.name || s.id) + "?")) return;
				try {
					const res = await fetch(API + "/schedules/" + s.id, { method: "DELETE", headers: authHeaders() });
					if (!res.ok) throw new Error("HTTP " + res.status);
					removeRow(tr, "No schedules yet. Add one to fire a template on a cadence.");
				} catch (err) {
					setStatus("Delete failed: " + err.message);
				}
			});
			actions.appendChild(editButton(() => openScheduleEdit(s)));
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(del);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load schedules: " + e.message);
	}
}

// fillTemplateSelect loads stored templates into a select and returns an id to name map, so a schedule
// can pick a template and the table can show its name. Best effort: a load failure leaves the picker
// with its placeholder.
async function fillTemplateSelect(select) {
	const byID = {};
	try {
		const data = await getJSON("/templates");
		for (const t of data.templates || []) {
			byID[t.id] = t.name;
			if (select) {
				const opt = document.createElement("option");
				opt.value = t.id;
				opt.textContent = t.name;
				select.appendChild(opt);
			}
		}
	} catch (_) { /* templates disabled or unauthorized; picker keeps its placeholder */ }
	return byID;
}

// openScheduleEdit fills the schedule dialog with an existing record and switches it to edit mode.
function openScheduleEdit(s) {
	const form = document.getElementById("schedule-form");
	form.dataset.editId = s.id;
	document.getElementById("schedule-name").value = s.name || "";
	document.getElementById("schedule-cron").value = s.cron || "";
	document.getElementById("schedule-template").value = s.template_id || "";
	document.getElementById("schedule-status").textContent = "";
	setModalTitle("schedule", "Edit schedule");
	document.getElementById("schedule-modal").hidden = false;
}

// wireScheduleForm hooks the schedule dialog up to POST /schedules for a new schedule and PUT
// /schedules/{id} when editing. The New button resets the dialog to add mode.
function wireScheduleForm() {
	const form = document.getElementById("schedule-form");
	fillTemplateSelect(document.getElementById("schedule-template"));
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("schedule-name").value = "";
		document.getElementById("schedule-cron").value = "";
		document.getElementById("schedule-template").value = "";
		document.getElementById("schedule-status").textContent = "";
		setModalTitle("schedule", "Add a schedule");
	};
	const openBtn = document.getElementById("schedule-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("schedule-status");
		const editId = form.dataset.editId;
		const templateID = document.getElementById("schedule-template").value;
		if (!templateID) {
			status.textContent = "Pick a template.";
			return;
		}
		const payload = {
			name: document.getElementById("schedule-name").value.trim(),
			cron: document.getElementById("schedule-cron").value.trim(),
			template_id: templateID,
		};
		try {
			if (editId) {
				await postAction("/schedules/" + editId, payload, "PUT");
			} else {
				await postAction("/schedules", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("schedule");
			document.getElementById("schedules").innerHTML = "";
			loadSchedules();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		}
	});
}

// scheduleTarget describes what a schedule fires.
function scheduleTarget(s) {
	if (s.steps && s.steps.length) {
		return "pipeline, " + s.steps.length + " steps";
	}
	if (s.shards) {
		return "split x" + s.shards + "  " + (s.playbook || "");
	}
	return s.playbook || "";
}

// fillInventorySelect loads stored inventories into a select and returns an id to name map, so the
// policy table can show an inventory name instead of an id. It is best effort: a load failure just
// leaves the picker with its Any option.
async function fillInventorySelect(select) {
	const byID = {};
	try {
		const data = await getJSON("/inventories");
		for (const inv of data.inventories || []) {
			byID[inv.id] = inv.name;
			if (select) {
				const opt = document.createElement("option");
				opt.value = inv.id;
				opt.textContent = inv.name;
				select.appendChild(opt);
			}
		}
	} catch (_) { /* inventories disabled or unauthorized; picker keeps only Any */ }
	return byID;
}

// anyCell returns a table cell showing a muted "any", used where a policy criterion is empty and so
// matches every value.
function anyCell() {
	const cell = document.createElement("td");
	const span = document.createElement("span");
	span.className = "muted";
	span.textContent = "any";
	cell.appendChild(span);
	return cell;
}

// openPolicyEdit fills the policy dialog with an existing rule and switches it to edit mode, so a
// saved policy is changed in place rather than deleted and recreated.
function openPolicyEdit(p) {
	const form = document.getElementById("policy-form");
	form.dataset.editId = p.id;
	document.getElementById("policy-name").value = p.name;
	document.getElementById("policy-tool").value = p.tool || "";
	document.getElementById("policy-command").value = p.command_contains || "";
	document.getElementById("policy-inventory").value = p.inventory_id || "";
	document.getElementById("policy-exclude-dry").checked = !!p.exclude_dry_run;
	document.getElementById("policy-status").textContent = "";
	setModalTitle("policy", "Edit policy");
	document.getElementById("policy-modal").hidden = false;
}

// wirePolicyForm hooks the policy dialog up to POST /policies for a new rule and PUT /policies/{id}
// when editing. The New button resets the dialog to add mode.
function wirePolicyForm() {
	const form = document.getElementById("policy-form");
	fillInventorySelect(document.getElementById("policy-inventory"));
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("policy-name").value = "";
		document.getElementById("policy-tool").value = "";
		document.getElementById("policy-command").value = "";
		document.getElementById("policy-inventory").value = "";
		document.getElementById("policy-exclude-dry").checked = false;
		document.getElementById("policy-status").textContent = "";
		setModalTitle("policy", "Add a policy");
	};
	const openBtn = document.getElementById("policy-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("policy-status");
		const editId = form.dataset.editId;
		const payload = {
			name: document.getElementById("policy-name").value.trim(),
			tool: document.getElementById("policy-tool").value,
			command_contains: document.getElementById("policy-command").value.trim(),
			inventory_id: document.getElementById("policy-inventory").value,
			exclude_dry_run: document.getElementById("policy-exclude-dry").checked,
		};
		try {
			if (editId) {
				await postAction("/policies/" + editId, payload, "PUT");
			} else {
				await postAction("/policies", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("policy");
			document.getElementById("policies").innerHTML = "";
			loadPolicies();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		}
	});
}

// loadPolicies populates the policy table with edit and delete actions. Each empty criterion shows
// as "any", so a reader sees exactly how wide a rule is.
async function loadPolicies() {
	try {
		const invByID = await fillInventorySelect(null);
		const data = await getJSON("/policies");
		const policies = data.policies || [];
		if (policies.length === 0) {
			showEmpty("No policies yet. Add one to require approval for the runs it matches.");
			return;
		}
		const tbody = document.getElementById("policies");
		for (const p of policies) {
			const tr = document.createElement("tr");
			tr.appendChild(td(p.name));
			const toolCell = document.createElement("td");
			if (p.tool) {
				const badge = document.createElement("span");
				badge.className = "tool-badge " + p.tool;
				badge.textContent = p.tool;
				toolCell.appendChild(badge);
			} else {
				const span = document.createElement("span");
				span.className = "muted";
				span.textContent = "any";
				toolCell.appendChild(span);
			}
			tr.appendChild(toolCell);
			tr.appendChild(p.command_contains ? td(p.command_contains, "mono") : anyCell());
			tr.appendChild(p.inventory_id ? td(invByID[p.inventory_id] || p.inventory_id) : anyCell());
			const dry = document.createElement("td");
			if (p.exclude_dry_run) {
				const chip = document.createElement("span");
				chip.className = "chip ok";
				chip.textContent = "yes";
				dry.appendChild(chip);
			} else {
				const span = document.createElement("span");
				span.className = "muted";
				span.textContent = "no";
				dry.appendChild(span);
			}
			tr.appendChild(dry);
			tr.appendChild(tdTime(p.created_at));
			const actions = document.createElement("td");
			const del = document.createElement("button");
			del.className = "button danger";
			del.textContent = "Delete";
			del.addEventListener("click", async (e) => {
				e.preventDefault();
				if (!window.confirm("Delete policy " + p.name + "?")) return;
				try {
					const res = await fetch(API + "/policies/" + p.id, { method: "DELETE", headers: authHeaders() });
					if (!res.ok) throw new Error("HTTP " + res.status);
					removeRow(tr, "No policies yet. Add one to require approval for the runs it matches.");
				} catch (err) {
					setStatus("Delete failed: " + err.message);
				}
			});
			actions.appendChild(editButton(() => openPolicyEdit(p)));
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(del);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load policies: " + e.message);
	}
}

// sparkline builds a row of outcome ticks, oldest on the left, newest on the right.
function sparkline(recent) {
	const wrap = document.createElement("span");
	wrap.className = "spark";
	for (let i = recent.length - 1; i >= 0; i--) {
		const tick = document.createElement("span");
		tick.className = "tick " + (recent[i] || "none");
		tick.title = recent[i];
		wrap.appendChild(tick);
	}
	return wrap;
}

// outcomeChip builds a colored chip for a host outcome label.
function outcomeChip(outcome) {
	let cls = "ok";
	if (outcome === "failed" || outcome === "unreachable") {
		cls = "failed";
	} else if (outcome === "changed") {
		cls = "changed";
	} else if (outcome === "skipped") {
		cls = "skipped";
	}
	const span = document.createElement("span");
	span.className = "chip " + cls;
	span.textContent = outcome;
	return span;
}

// detailState holds the current run and its accumulated events for incremental rendering.
let detailState = null;

// loadDetail loads one run and dispatches to the split or single render path.
async function loadDetail(runId) {
	const fullLog = document.getElementById("full-log");
	if (fullLog) fullLog.href = streamURL("/runs/" + runId + "/logs");
	const exportEvents = document.getElementById("export-events");
	if (exportEvents) exportEvents.href = streamURL("/runs/" + runId + "/events?download=1");
	wireActions(runId);
	try {
		const run = await getJSON("/runs/" + runId);
		// A split or pipeline parent has no output of its own; each shard or step carries its log
		// and events. Hiding the links beats serving blanks.
		const isParent = !run.parent_id && (run.kind === "pipeline" || run.kind === "split" || run.shard_count);
		if (fullLog && isParent) fullLog.hidden = true;
		if (exportEvents && isParent) exportEvents.hidden = true;
		if (run.kind === "pipeline" && !run.parent_id) {
			await loadPipeline(runId);
		} else if ((run.kind === "split" || run.shard_count) && !run.parent_id) {
			await loadParent(runId);
		} else {
			await loadSingle(run);
		}
	} catch (e) {
		setStatus("Failed to load run: " + e.message);
	}
}

// postAction sends a POST to the API and returns the parsed JSON body, throwing on an error reply.
async function postAction(path, payload, method) {
	const opts = { method: method || "POST", headers: authHeaders() };
	if (payload !== undefined) {
		opts.headers["Content-Type"] = "application/json";
		opts.body = JSON.stringify(payload);
	}
	const res = await fetch(API + path, opts);
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	if (res.status === 204) {
		return {};
	}
	const body = await res.json().catch(() => ({}));
	if (!res.ok) {
		throw new Error(body.error || ("HTTP " + res.status));
	}
	return body;
}

// streamURL appends the stored token to a stream path, since EventSource cannot set headers.
function streamURL(path) {
	const token = apiToken();
	if (!token) return API + path;
	const sep = path.includes("?") ? "&" : "?";
	return API + path + sep + "access_token=" + encodeURIComponent(token);
}

// lastSeq returns the highest store sequence among events, or zero when none carry one. It is
// the cursor a live stream resumes from, so the browser never re-receives history it has.
function lastSeq(events) {
	let max = 0;
	for (const e of events) {
		if (e.seq && e.seq > max) max = e.seq;
	}
	return max;
}

// loadAllEvents fetches a run's full event history in bounded pages, following the nextAfter
// cursor, so one request never carries an unbounded log. A split run pages each shard this way.
async function loadAllEvents(runId) {
	const batch = 5000;
	let after = 0;
	const events = [];
	for (;;) {
		const data = await getJSON("/runs/" + runId + "/events?after=" + after + "&limit=" + batch);
		const page = data.events || [];
		for (const e of page) events.push(e);
		if (page.length < batch) break;
		after = data.next_after;
	}
	return events;
}

// loadLogin wires both sign in forms: account login mints a session token, and the raw token
// form verifies a pasted token against the API.
function loadLogin() {
	const ssoErr = ssoError();
	if (ssoErr) {
		setStatus(ssoErr);
		history.replaceState(null, "", location.pathname + location.search);
	}
	document.getElementById("account-form").addEventListener("submit", async (e) => {
		e.preventDefault();
		try {
			const res = await fetch(API + "/auth/login", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({
					username: document.getElementById("username-input").value.trim(),
					password: document.getElementById("password-input").value,
				}),
			});
			if (!res.ok) {
				setStatus("Sign in failed. Check the username and password.");
				return;
			}
			const session = await res.json();
			localStorage.setItem("st_token", session.token);
			localStorage.setItem("st_role", session.role);
			localStorage.setItem("st_user", session.username);
			location.href = sessionStorage.getItem("st_return") || "/ui/";
		} catch (err) {
			setStatus("Sign in failed: " + err.message);
		}
	});
	document.getElementById("login-form").addEventListener("submit", async (e) => {
		e.preventDefault();
		const token = document.getElementById("token-input").value.trim();
		if (!token) return;
		const res = await fetch(API + "/auth/check", {
			method: "POST", headers: { "Authorization": "Bearer " + token },
		});
		if (res.status === 204) {
			localStorage.setItem("st_token", token);
			localStorage.removeItem("st_role");
			localStorage.removeItem("st_user");
			location.href = sessionStorage.getItem("st_return") || "/ui/";
			return;
		}
		setStatus("That token was not accepted.");
	});
}

// openUserEdit fills the user dialog with an existing account and switches it to edit mode. The
// password field becomes optional, so a blank leaves the current password unchanged.
function openUserEdit(u) {
	const form = document.getElementById("user-form");
	form.dataset.editId = u.id;
	document.getElementById("user-name").value = u.username;
	const pw = document.getElementById("user-password");
	pw.value = "";
	pw.required = false;
	pw.placeholder = "Leave blank to keep current";
	document.getElementById("user-role").value = u.role;
	document.getElementById("user-status").textContent = "";
	setModalTitle("user", "Edit user");
	document.getElementById("user-modal").hidden = false;
}

// wireUserForm hooks the user dialog up to POST /users for a new account and PUT /users/{id} when
// editing. The New button resets the dialog to add mode, where a password is required.
function wireUserForm() {
	const form = document.getElementById("user-form");
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("user-name").value = "";
		const pw = document.getElementById("user-password");
		pw.value = "";
		pw.required = true;
		pw.placeholder = "";
		document.getElementById("user-role").value = "operator";
		document.getElementById("user-status").textContent = "";
		setModalTitle("user", "Add a user");
	};
	const openBtn = document.getElementById("user-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("user-status");
		const editId = form.dataset.editId;
		const payload = {
			username: document.getElementById("user-name").value.trim(),
			role: document.getElementById("user-role").value,
		};
		const pw = document.getElementById("user-password").value;
		if (pw) payload.password = pw;
		try {
			if (editId) {
				await postAction("/users/" + editId, payload, "PUT");
			} else {
				await postAction("/users", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("user");
			document.getElementById("users").innerHTML = "";
			loadUsers();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		}
	});
}

// loadUsers populates the user table with delete actions.
async function loadUsers() {
	try {
		const data = await getJSON("/users");
		const users = data.users || [];
		if (users.length === 0) {
			showEmpty("No users yet.");
			return;
		}
		const tbody = document.getElementById("users");
		for (const u of users) {
			const tr = document.createElement("tr");
			tr.appendChild(td(u.username));
			tr.appendChild(td(u.role, "mono"));
			tr.appendChild(tdTime(u.created_at));
			const actions = deleteCell("/users/" + u.id, "user " + u.username, tr, "No users yet.");
			actions.insertBefore(editButton(() => openUserEdit(u)), actions.firstChild);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load users: " + e.message);
	}
}

// wireActions hooks up the cancel and retry buttons for the run being viewed.
function wireActions(runId) {
	const cancel = document.getElementById("cancel-run");
	if (cancel) cancel.addEventListener("click", async () => {
		if (!window.confirm("Cancel this run?")) return;
		cancel.disabled = true;
		try {
			await postAction("/runs/" + runId + "/cancel");
		} catch (e) {
			setStatus("Cancel failed: " + e.message);
			cancel.disabled = false;
		}
	});
	const retry = document.getElementById("retry-run");
	if (retry) retry.addEventListener("click", async () => {
		retry.disabled = true;
		try {
			const created = await postAction("/runs/" + runId + "/retry");
			window.location.href = "/ui/runs/" + created.id;
		} catch (e) {
			setStatus("Retry failed: " + e.message);
			retry.disabled = false;
		}
	});
	const approve = document.getElementById("approve-run");
	if (approve) {
		approve.addEventListener("click", async () => {
			approve.disabled = true;
			try {
				await postAction("/runs/" + runId + "/approve");
				location.reload();
			} catch (e) {
				setStatus("Approve failed: " + e.message);
				approve.disabled = false;
			}
		});
	}
	const reject = document.getElementById("reject-run");
	if (reject) {
		reject.addEventListener("click", async () => {
			const reason = window.prompt("Reason for rejecting (optional):");
			if (reason === null) return;
			reject.disabled = true;
			try {
				await postAction("/runs/" + runId + "/reject", { reason });
				location.reload();
			} catch (e) {
				setStatus("Reject failed: " + e.message);
				reject.disabled = false;
			}
		});
	}
	const explain = document.getElementById("explain-run");
	if (explain) {
		explain.addEventListener("click", async () => {
			const panel = document.getElementById("explain-panel");
			const bodyEl = document.getElementById("explain-body");
			if (panel) panel.hidden = false;
			if (bodyEl) bodyEl.textContent = "Reading the run…";
			explain.disabled = true;
			try {
				const res = await postAction("/runs/" + runId + "/explain");
				if (bodyEl) bodyEl.textContent = res.explanation || "No explanation was returned.";
			} catch (e) {
				if (bodyEl) {
					bodyEl.textContent = e.message === "ai is not enabled"
						? "AI triage is not enabled on this server."
						: "Could not explain the run: " + e.message;
				}
			} finally {
				explain.disabled = false;
			}
		});
	}
}

// updateActions shows cancel while the run is active and retry on a finished split that did not
// fully succeed.
function updateActions(run) {
	const cancel = document.getElementById("cancel-run");
	const retry = document.getElementById("retry-run");
	if (!cancel || !retry) return;
	const approve = document.getElementById("approve-run");
	const reject = document.getElementById("reject-run");
	const explain = document.getElementById("explain-run");
	if (isReadOnly()) {
		cancel.hidden = true;
		retry.hidden = true;
		if (approve) approve.hidden = true;
		if (reject) reject.hidden = true;
		if (explain) explain.hidden = true;
		return;
	}
	const held = run.status === "pending_approval";
	cancel.hidden = isTerminal(run.status) || held;
	if (approve) approve.hidden = !held;
	if (reject) reject.hidden = !held;
	const splitParent = (run.kind === "split" || run.shard_count) && !run.parent_id;
	retry.hidden = !(splitParent && isTerminal(run.status) && run.status !== "succeeded");
	if (explain) {
		const heldProposal = held && (run.proposed_from || run.intent);
		explain.hidden = !(run.status === "failed" || run.status === "interrupted" || heldProposal);
	}
}

// loadPipeline renders a pipeline run as an ordered list of step runs, refreshed live over the
// pipeline's event stream while it is active.
async function loadPipeline(pipelineId) {
	const run = await getJSON("/runs/" + pipelineId);
	const stepData = await getJSON("/runs/" + pipelineId + "/steps");
	renderHeader(run);
	renderSteps(stepData.steps || []);
	setStatus("");
	if (!isTerminal(run.status)) {
		openPipelineStream(pipelineId);
	}
}

// openPipelineStream refreshes the header and step list as step events arrive, coalescing bursts
// into one refresh, and settles on the final state at the end signal.
function openPipelineStream(pipelineId) {
	const indicator = document.getElementById("live-indicator");
	const source = new EventSource(streamURL("/runs/" + pipelineId + "/stream"));
	// The browser retries the stream on its own, so an error only flips the indicator.
	source.onopen = () => { if (indicator) indicator.textContent = "live"; };
	source.onerror = () => { if (indicator) indicator.textContent = "reconnecting"; };
	let pending = null;
	const refresh = async () => {
		pending = null;
		try {
			const run = await getJSON("/runs/" + pipelineId);
			const stepData = await getJSON("/runs/" + pipelineId + "/steps");
			renderHeader(run);
			renderSteps(stepData.steps || []);
		} catch (_) { /* keep the last view on a refresh failure */ }
	};
	source.addEventListener("event", () => {
		if (!pending) {
			pending = setTimeout(refresh, 250);
		}
	});
	source.addEventListener("end", () => {
		source.close();
		refresh();
	});
}

// renderSteps lists a pipeline's step runs in order with status and playbook.
function renderSteps(steps) {
	const panel = document.getElementById("steps-panel");
	const list = document.getElementById("steps");
	list.innerHTML = "";
	if (!steps.length) {
		panel.hidden = true;
		return;
	}
	for (const s of steps) {
		const idx = (s.step_index !== undefined && s.step_index !== null) ? Number(s.step_index) + 1 : "?";
		const row = document.createElement("a");
		row.className = "shard-row";
		row.href = "/ui/runs/" + s.id;
		row.appendChild(badge(s.status));
		const label = document.createElement("span");
		label.className = "shard-label";
		let text = idx + ". " + (s.step_name || "step");
		const detail = toolLabel(s);
		if (detail) {
			text += "  ·  " + detail;
		}
		if (s.attempt) {
			text += "  ·  retry " + s.attempt;
		}
		label.textContent = text;
		row.appendChild(label);
		list.appendChild(row);
	}
	panel.hidden = false;
}

// loadSingle renders a normal run and streams it live while it is active.
async function loadSingle(run) {
	const events = await loadAllEvents(run.id);
	detailState = { runId: run.id, run, events };
	detailState.lastSeq = lastSeq(detailState.events);
	renderDetail();
	setStatus("");
	if (!isTerminal(run.status)) {
		openStream(run.id, detailState.lastSeq);
	}
}

// loadParent renders a split run by merging every shard's events into one matrix. While the parent
// is active the merged grid fills in live from the parent's event stream, which carries every
// shard's events.
async function loadParent(parentId) {
	const run = await getJSON("/runs/" + parentId);
	const shardData = await getJSON("/runs/" + parentId + "/shards");
	const shards = shardData.shards || [];
	const perShard = await Promise.all(shards.map((s) =>
		loadAllEvents(s.id).catch(() => [])));

	detailState = { runId: parentId, run, events: [].concat.apply([], perShard) };
	renderDetail();
	renderShards(shards);
	setStatus("");

	if (!isTerminal(run.status)) {
		openParentStream(parentId);
	}
}

// openParentStream applies shard events to the merged matrix as they arrive. A stats event means a
// shard finished, so the shard list refreshes, and the end signal settles the final state.
function openParentStream(parentId) {
	const indicator = document.getElementById("live-indicator");
	const source = new EventSource(streamURL("/runs/" + parentId + "/stream"));
	// The browser retries the stream on its own, so an error only flips the indicator.
	source.onopen = () => { if (indicator) indicator.textContent = "live"; };
	source.onerror = () => { if (indicator) indicator.textContent = "reconnecting"; };
	const refreshShards = async () => {
		try {
			const shardData = await getJSON("/runs/" + parentId + "/shards");
			renderShards(shardData.shards || []);
		} catch (_) { /* keep the last shard list on a refresh failure */ }
	};
	source.addEventListener("event", (e) => {
		try {
			const ev = JSON.parse(e.data);
			detailState.events.push(ev);
			applyLiveEvent(ev);
			if (ev.type === "stats") {
				refreshShards();
			}
		} catch (_) { /* ignore a malformed event */ }
	});
	source.addEventListener("end", async () => {
		source.close();
		try {
			detailState.run = await getJSON("/runs/" + parentId);
			renderDetail();
		} catch (_) { /* keep the last header on refresh failure */ }
		refreshShards();
	});
}

// renderShards lists the shard runs of a split with status and host count.
function renderShards(shards) {
	const panel = document.getElementById("shards-panel");
	const list = document.getElementById("shards");
	list.innerHTML = "";
	if (!shards.length) {
		panel.hidden = true;
		return;
	}
	for (const s of shards) {
		const hostCount = s.limit ? s.limit.split(",").length : 0;
		const idx = (s.shard_index !== undefined && s.shard_index !== null) ? s.shard_index : "?";
		const row = document.createElement("a");
		row.className = "shard-row";
		row.href = "/ui/runs/" + s.id;
		row.appendChild(badge(s.status));
		const label = document.createElement("span");
		label.className = "shard-label";
		label.textContent = "Shard " + idx + "  ·  " + hostCount + " host" + (hostCount === 1 ? "" : "s");
		row.appendChild(label);
		list.appendChild(row);
	}
	panel.hidden = false;
}

// isTerminal reports whether a run status is final.
function isTerminal(status) {
	return status === "succeeded" || status === "failed" ||
		status === "canceled" || status === "interrupted";
}

// renderDetail redraws the header, matrix, and timeline from the current state.
function renderDetail() {
	renderHeader(detailState.run);
	detailState.model = buildModel(detailState.events);
	renderMatrix(detailState.model);
	renderTimeline(detailState.model);
}

// openStream subscribes to the run's live output and applies events, logs, and the end signal.
// It resumes after afterSeq so history is never re-sent, and skips any event at or before the
// cursor in case a reconnect replays one.
function openStream(runId, afterSeq) {
	const indicator = document.getElementById("live-indicator");
	if (indicator) indicator.hidden = false;

	const path = "/runs/" + runId + "/stream" + (afterSeq ? "?after=" + afterSeq : "");
	const source = new EventSource(streamURL(path));
	// The browser retries the stream on its own, so an error only flips the indicator.
	source.onopen = () => { if (indicator) indicator.textContent = "live"; };
	source.onerror = () => { if (indicator) indicator.textContent = "reconnecting"; };
	source.addEventListener("event", (e) => {
		try {
			const ev = JSON.parse(e.data);
			if (ev.seq && ev.seq <= (detailState.lastSeq || 0)) return;
			detailState.events.push(ev);
			if (ev.seq) detailState.lastSeq = ev.seq;
			applyLiveEvent(ev);
		} catch (_) { /* ignore a malformed event */ }
	});
	source.addEventListener("log", (e) => {
		try { appendLog(JSON.parse(e.data)); } catch (_) { /* ignore a malformed chunk */ }
	});
	source.addEventListener("end", async () => {
		source.close();
		if (indicator) indicator.hidden = true;
		try {
			detailState.run = await getJSON("/runs/" + runId);
			renderHeader(detailState.run);
		} catch (_) { /* keep the last header on refresh failure */ }
	});
}

// logCap bounds how many characters the live log pane keeps, so a long run cannot grow it
// without bound. The tail is what matters live; the full log is available on the run itself.
const logCap = 262144;

// appendLog adds a chunk to the live log pane, following the tail only when the view was already
// near the bottom, so a reader scrolled up is not yanked back down. It appends a text node
// rather than rebuilding the whole string, which keeps a long stream from getting quadratic, and
// trims the pane back to the cap when it grows past it.
function appendLog(chunk) {
	const pre = document.getElementById("log");
	const nearBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
	pre.appendChild(document.createTextNode(chunk));
	detailState.logLen = (detailState.logLen || 0) + chunk.length;
	if (detailState.logLen > logCap * 2) {
		pre.textContent = pre.textContent.slice(-logCap);
		detailState.logLen = pre.textContent.length;
	}
	document.getElementById("log-panel").hidden = false;
	if (nearBottom) pre.scrollTop = pre.scrollHeight;
}

// renderHeader fills the run header fields.
function renderHeader(run) {
	const el = document.getElementById("run-header");
	el.innerHTML = "";
	el.appendChild(field("Status", null, badge(run.status)));
	el.appendChild(field("Run", shortId(run.id), null, run.id));
	if (!run.tool || run.tool === "ansible") {
		el.appendChild(field("Playbook", baseName(run.playbook) || (run.playbook || ""), null, run.playbook || ""));
	} else {
		el.appendChild(field("Tool", null, toolBadgeEl(run)));
		el.appendChild(field(run.tool === "terraform" || run.tool === "opentofu" ? "Directory" : "Command",
			toolLabel(run), null, run.command || ""));
	}
	if (run.dry_run) {
		el.appendChild(field("Mode", "dry run"));
	}
	if (run.proposed_from) {
		const link = document.createElement("a");
		link.href = "/ui/runs/" + run.proposed_from;
		link.textContent = shortId(run.proposed_from);
		link.title = run.proposed_from;
		el.appendChild(field("Proposed from drift check", null, link));
		if (run.limit) {
			el.appendChild(field("Limited to", run.limit));
		}
	}
	if (run.intent) {
		el.appendChild(field("Proposed from request", run.intent));
		if (run.limit) {
			el.appendChild(field("Limited to", run.limit));
		}
	}
	if (run.inventory) {
		el.appendChild(field("Inventory", baseName(run.inventory), null, run.inventory));
	}
	if (run.shard_count) {
		el.appendChild(field("Shards", String(run.shard_count)));
	}
	if (run.exit_code !== undefined && run.exit_code !== null) {
		el.appendChild(field("Exit", String(run.exit_code)));
	}
	el.appendChild(field("Duration", fmtDuration(run.started_at, run.ended_at)));
	el.hidden = false;
	updateActions(run);
}

// field builds a labeled field, using node when provided otherwise a text value.
function field(label, value, node, title) {
	const f = document.createElement("div");
	f.className = "field";
	const l = document.createElement("span");
	l.className = "label";
	l.textContent = label;
	f.appendChild(l);
	if (node) {
		f.appendChild(node);
	} else {
		const v = document.createElement("span");
		v.className = "value";
		v.textContent = value;
		if (title) v.title = title;
		f.appendChild(v);
	}
	return f;
}

// buildModel derives the matrix and timeline model from the event stream.
function buildModel(events) {
	const tasks = [];
	const taskSeen = new Set();
	const taskStart = {};
	const hosts = new Set();
	const cells = {};
	let lastTime = null;
	let statsTime = null;

	for (const e of events) {
		const t = e.time ? new Date(e.time).getTime() : null;
		if (t && (lastTime === null || t > lastTime)) lastTime = t;
		if (e.type === "task_start") {
			addTask(tasks, taskSeen, e.task);
			if (taskStart[e.task] === undefined && t) taskStart[e.task] = t;
		} else if (e.type && e.type.indexOf("runner_") === 0) {
			const outcome = e.type === "runner_ok"
				? (e.changed ? "changed" : "ok")
				: e.type.slice("runner_".length);
			hosts.add(e.host);
			if (!cells[e.host]) cells[e.host] = {};
			cells[e.host][e.task] = {
				outcome,
				message: e.message,
				stdout: e.stdout,
				stderr: e.stderr,
				rc: e.rc,
				diff: e.diff,
				truncated: e.truncated,
			};
			addTask(tasks, taskSeen, e.task);
		} else if (e.type === "stats") {
			statsTime = t;
			if (e.stats) for (const h of Object.keys(e.stats)) hosts.add(h);
		}
	}
	return {
		tasks, taskSeen, taskStart, cells,
		hosts: Array.from(hosts).sort(), hostSet: hosts,
		lastTime, statsTime, end: statsTime || lastTime,
	};
}

// applyEvent folds one live event into an existing model, the same way buildModel folds it on
// a full pass. It returns whether the grid gained a host or task, which needs a structural
// rebuild, along with the host and task the event touched so a cell update can target one cell.
function applyEvent(model, e) {
	const t = e.time ? new Date(e.time).getTime() : null;
	if (t && (model.lastTime === null || t > model.lastTime)) model.lastTime = t;
	let structural = false;
	if (e.type === "task_start") {
		if (!model.taskSeen.has(e.task)) structural = true;
		addTask(model.tasks, model.taskSeen, e.task);
		if (model.taskStart[e.task] === undefined && t) model.taskStart[e.task] = t;
	} else if (e.type && e.type.indexOf("runner_") === 0) {
		const outcome = e.type === "runner_ok"
			? (e.changed ? "changed" : "ok")
			: e.type.slice("runner_".length);
		if (!model.hostSet.has(e.host)) {
			model.hostSet.add(e.host);
			model.hosts = Array.from(model.hostSet).sort();
			structural = true;
		}
		if (!model.taskSeen.has(e.task)) structural = true;
		addTask(model.tasks, model.taskSeen, e.task);
		if (!model.cells[e.host]) model.cells[e.host] = {};
		model.cells[e.host][e.task] = {
			outcome, message: e.message, stdout: e.stdout, stderr: e.stderr,
			rc: e.rc, diff: e.diff, truncated: e.truncated,
		};
	} else if (e.type === "stats") {
		model.statsTime = t;
		if (e.stats) {
			for (const h of Object.keys(e.stats)) {
				if (!model.hostSet.has(h)) {
					model.hostSet.add(h);
					model.hosts = Array.from(model.hostSet).sort();
					structural = true;
				}
			}
		}
	}
	model.end = model.statsTime || model.lastTime;
	return { structural, host: e.host, task: e.task };
}

// addTask records a task name once, preserving first seen order.
function addTask(tasks, seen, task) {
	if (task && !seen.has(task)) { seen.add(task); tasks.push(task); }
}

// renderMatrix draws the host by task outcome grid.
// matrixCap returns the configured host matrix cell limit from the page, or zero for no limit.
function matrixCap() {
	const v = parseInt(document.body.dataset.matrixCap || "0", 10);
	return isNaN(v) ? 0 : v;
}

// renderMatrixTooLarge shows a notice in place of the grid when the host matrix has more cells
// than the configured cap. The timeline and summary still render, and the grid returns once the
// view is narrowed to fewer hosts or tasks or an individual shard is opened.
function renderMatrixTooLarge(hostCount, taskCount, cellCount, cap) {
	const table = document.getElementById("matrix");
	const tbody = document.createElement("tbody");
	const tr = document.createElement("tr");
	const cell = document.createElement("td");
	cell.className = "matrix-too-large";
	cell.textContent = "Host matrix is " + hostCount + " × " + taskCount + " = " +
		cellCount.toLocaleString() + " cells, over the display cap of " + cap.toLocaleString() +
		". Filter to fewer hosts or tasks, or open a shard, to see the grid.";
	tr.appendChild(cell);
	tbody.appendChild(tr);
	table.appendChild(tbody);
	renderMatrixSummary(hostCount, taskCount, {});
	document.getElementById("matrix-panel").hidden = false;
}

function renderMatrix(model) {
	const { tasks, hosts, cells } = model;
	const table = document.getElementById("matrix");
	table.innerHTML = "";
	detailState.cellIndex = {};
	detailState.counts = {};
	detailState.overCap = false;
	if (hosts.length === 0 || tasks.length === 0) {
		renderMatrixSummary(hosts.length, tasks.length, detailState.counts);
		return;
	}

	const cap = matrixCap();
	const cellCount = hosts.length * tasks.length;
	if (cap > 0 && cellCount > cap) {
		detailState.overCap = true;
		renderMatrixTooLarge(hosts.length, tasks.length, cellCount, cap);
		return;
	}

	const thead = document.createElement("thead");
	const htr = document.createElement("tr");
	const corner = document.createElement("th");
	corner.className = "corner";
	corner.textContent = "host \\ task";
	htr.appendChild(corner);
	tasks.forEach((task, ci) => {
		const th = document.createElement("th");
		th.textContent = task;
		th.dataset.ci = ci;
		htr.appendChild(th);
	});
	thead.appendChild(htr);
	table.appendChild(thead);

	const tbody = document.createElement("tbody");
	hosts.forEach((host, ri) => {
		const tr = document.createElement("tr");
		const rowTh = document.createElement("th");
		const hostLink = document.createElement("a");
		hostLink.href = "/ui/hosts/" + encodeURIComponent(host);
		hostLink.textContent = host;
		hostLink.dataset.tip = "Open this host's history";
		rowTh.appendChild(hostLink);
		rowTh.dataset.ri = ri;
		tr.appendChild(rowTh);
		tasks.forEach((task, ci) => {
			const info = cells[host] && cells[host][task];
			const outcome = info ? info.outcome : "none";
			detailState.counts[outcome] = (detailState.counts[outcome] || 0) + 1;
			const cell = document.createElement("td");
			const div = document.createElement("div");
			div.className = "cell " + outcome;
			div.title = host + " / " + task + ": " + outcome;
			div.dataset.host = host;
			div.dataset.task = task;
			div.dataset.ri = ri;
			div.dataset.ci = ci;
			div.dataset.outcome = outcome;
			if (!detailState.cellIndex[host]) detailState.cellIndex[host] = {};
			detailState.cellIndex[host][task] = div;
			cell.appendChild(div);
			tr.appendChild(cell);
		});
		tbody.appendChild(tr);
	});
	table.appendChild(tbody);
	wireMatrixDelegation(table);
	renderMatrixSummary(hosts.length, tasks.length, detailState.counts);
	document.getElementById("matrix-panel").hidden = false;
}

// wireMatrixDelegation attaches one set of listeners to the matrix table. Hover lights the
// cell's row and column by their index, and a click opens the cell's drill. It runs once: the
// listeners sit on the table, so they survive the body being refilled and never scale with the
// number of cells.
function wireMatrixDelegation(table) {
	if (table.dataset.wired) return;
	table.dataset.wired = "1";
	const clear = () => {
		table.querySelectorAll(".hi, .row-hi, .col-hi").forEach((el) =>
			el.classList.remove("hi", "row-hi", "col-hi"));
	};
	table.addEventListener("mouseover", (e) => {
		const div = e.target.closest(".cell");
		if (!div) return;
		clear();
		table.querySelectorAll('[data-ri="' + div.dataset.ri + '"]').forEach((el) =>
			el.classList.add(el.classList.contains("cell") ? "row-hi" : "hi"));
		table.querySelectorAll('[data-ci="' + div.dataset.ci + '"]').forEach((el) =>
			el.classList.add(el.classList.contains("cell") ? "col-hi" : "hi"));
	});
	table.addEventListener("mouseleave", clear);
	table.addEventListener("click", (e) => {
		const div = e.target.closest(".cell");
		if (!div) return;
		const model = detailState.model;
		const info = model && model.cells[div.dataset.host] && model.cells[div.dataset.host][div.dataset.task];
		if (info) showDrill(Object.assign({ host: div.dataset.host, task: div.dataset.task }, info));
	});
}

// updateCell repaints one matrix cell from the model after a live event and adjusts the outcome
// rollup, leaving the rest of the grid untouched. It returns false when the cell is not present,
// which means the caller needs a structural rebuild instead.
function updateCell(host, task) {
	const div = detailState.cellIndex[host] && detailState.cellIndex[host][task];
	if (!div) return false;
	const cells = detailState.model.cells;
	const info = cells[host] && cells[host][task];
	const outcome = info ? info.outcome : "none";
	const prev = div.dataset.outcome;
	if (prev !== outcome) {
		detailState.counts[prev] = (detailState.counts[prev] || 1) - 1;
		detailState.counts[outcome] = (detailState.counts[outcome] || 0) + 1;
	}
	div.className = "cell " + outcome;
	div.dataset.outcome = outcome;
	div.title = host + " / " + task + ": " + outcome;
	renderMatrixSummary(detailState.model.hosts.length, detailState.model.tasks.length, detailState.counts);
	return true;
}

// updateTimelineBar recolors one task's timeline bar to its current worst outcome after a live
// event, without rebuilding the timeline.
function updateTimelineBar(task) {
	const bar = detailState.tlBars && detailState.tlBars.get(task);
	if (!bar) return;
	const m = detailState.model;
	bar.className = "tl-bar " + worstOutcome(task, m.cells, m.hosts);
}

// applyLiveEvent folds a streamed event into the live model and repaints only what changed: the
// whole grid when a host or task first appears, otherwise a single cell and its timeline bar.
function applyLiveEvent(ev) {
	if (!detailState.model) {
		renderDetail();
		return;
	}
	const change = applyEvent(detailState.model, ev);
	if (change.structural) {
		renderMatrix(detailState.model);
		renderTimeline(detailState.model);
	} else if (!detailState.overCap && change.host && change.task) {
		if (!updateCell(change.host, change.task)) {
			renderMatrix(detailState.model);
			renderTimeline(detailState.model);
			return;
		}
		updateTimelineBar(change.task);
	}
}

// renderMatrixSummary shows the matrix size and a per-outcome rollup above the grid.
function renderMatrixSummary(hostCount, taskCount, counts) {
	const el = document.getElementById("matrix-summary");
	if (!el) return;
	el.innerHTML = "";
	const scope = document.createElement("span");
	scope.className = "muted";
	scope.textContent = hostCount + (hostCount === 1 ? " host" : " hosts") + " × " +
		taskCount + (taskCount === 1 ? " task" : " tasks");
	el.appendChild(scope);
	for (const o of ["ok", "changed", "failed", "unreachable", "skipped"]) {
		if (!counts[o]) continue;
		const chip = document.createElement("span");
		chip.className = "roll " + o;
		chip.textContent = counts[o] + " " + o;
		el.appendChild(chip);
	}
}

// renderTimeline draws a bar per task scaled to the run span.
function renderTimeline(model) {
	const { tasks, taskStart, cells, hosts, end } = model;
	const container = document.getElementById("timeline");
	container.innerHTML = "";
	detailState.tlBars = new Map();
	const ordered = tasks.filter(t => taskStart[t] !== undefined).sort((a, b) => taskStart[a] - taskStart[b]);
	if (ordered.length === 0) return;

	const t0 = taskStart[ordered[0]];
	const tEnd = end || taskStart[ordered[ordered.length - 1]];
	const span = Math.max(tEnd - t0, 1);

	for (let i = 0; i < ordered.length; i++) {
		const task = ordered[i];
		const start = taskStart[task];
		const stop = (i + 1 < ordered.length) ? taskStart[ordered[i + 1]] : tEnd;
		const dur = Math.max(stop - start, 0);
		const outcome = worstOutcome(task, cells, hosts);

		const row = document.createElement("div");
		row.className = "tl-row";
		const name = document.createElement("div");
		name.className = "tl-name";
		name.textContent = task;
		name.title = task;
		const track = document.createElement("div");
		track.className = "tl-track";
		const bar = document.createElement("div");
		bar.className = "tl-bar " + outcome;
		detailState.tlBars.set(task, bar);
		const leftPct = Math.min(Math.max(((start - t0) / span) * 100, 0), 99);
		const widthPct = Math.min(Math.max((dur / span) * 100, 1), 100 - leftPct);
		bar.style.left = leftPct + "%";
		bar.style.width = widthPct + "%";
		bar.addEventListener("click", () => showDrill({ task, outcome, duration: fmtMs(dur) }));
		track.appendChild(bar);
		const durEl = document.createElement("div");
		durEl.className = "tl-dur";
		durEl.textContent = fmtMs(dur);
		row.appendChild(name);
		row.appendChild(track);
		row.appendChild(durEl);
		container.appendChild(row);
	}
	document.getElementById("timeline-panel").hidden = false;
}

// worstOutcome returns the most severe outcome for a task across hosts, mapped to a bar class.
function worstOutcome(task, cells, hosts) {
	let worst = null;
	let rank = -1;
	for (const host of hosts) {
		const info = cells[host] && cells[host][task];
		if (!info) continue;
		const r = OUTCOME_RANK[info.outcome] === undefined ? 0 : OUTCOME_RANK[info.outcome];
		if (r > rank) { rank = r; worst = info.outcome; }
	}
	if (worst === null) return "skipped";
	if (worst === "unreachable") return "failed";
	return worst;
}

// showDrill opens the side panel with details for a cell or bar.
function showDrill(info) {
	const body = document.getElementById("drill-body");
	body.innerHTML = "";
	const h = document.createElement("h3");
	h.textContent = info.host ? (info.host + " / " + info.task) : info.task;
	if (info.outcome) {
		const b = document.createElement("span");
		b.className = "chip " + info.outcome;
		b.textContent = info.outcome;
		h.appendChild(document.createTextNode(" "));
		h.appendChild(b);
	}
	body.appendChild(h);

	if (info.duration) body.appendChild(drillField("Duration", info.duration));
	if (info.rc !== undefined && info.rc !== null) {
		body.appendChild(drillField("Return code", String(info.rc)));
	}
	if (info.message) body.appendChild(drillBlock("Message", info.message));
	if (info.stdout) body.appendChild(drillBlock("Stdout", info.stdout));
	if (info.stderr) body.appendChild(drillBlock("Stderr", info.stderr));
	if (info.diff) body.appendChild(drillBlock("Diff", info.diff));
	if (info.truncated) {
		const note = document.createElement("div");
		note.className = "drill-note";
		note.textContent = "Output truncated.";
		body.appendChild(note);
	}

	document.getElementById("drill").hidden = false;
}

// drillBlock builds a labeled monospace block for multi line output.
function drillBlock(label, value) {
	const f = document.createElement("div");
	f.className = "field";
	const l = document.createElement("div");
	l.className = "label";
	l.textContent = label;
	const pre = document.createElement("pre");
	pre.className = "drill-pre";
	pre.textContent = value;
	f.appendChild(l);
	f.appendChild(pre);
	return f;
}

// drillField builds a labeled value in the drill panel.
function drillField(label, value) {
	const f = document.createElement("div");
	f.className = "field";
	const l = document.createElement("div");
	l.className = "label";
	l.textContent = label;
	const v = document.createElement("div");
	v.className = "value";
	v.textContent = value;
	f.appendChild(l);
	f.appendChild(v);
	return f;
}

// ensureDrill returns the shared inspect panel's body, creating the slide-in panel on pages that do
// not declare it so any resource list can reuse the run-detail drawer.
function ensureDrill() {
	let drill = document.getElementById("drill");
	if (!drill) {
		drill = document.createElement("aside");
		drill.id = "drill";
		drill.className = "drill";
		drill.hidden = true;
		const close = document.createElement("button");
		close.className = "drill-close";
		close.id = "drill-close";
		close.setAttribute("aria-label", "Close");
		close.innerHTML = "&times;";
		close.addEventListener("click", () => { drill.hidden = true; });
		const body = document.createElement("div");
		body.id = "drill-body";
		drill.appendChild(close);
		drill.appendChild(body);
		document.body.appendChild(drill);
	}
	return document.getElementById("drill-body");
}

// inspectDrawer opens the shared panel with a title and a list of fields. A field marked block
// renders as a monospace block, for multi line values such as inventory content. Empty fields are
// skipped so the panel stays terse.
function inspectDrawer(title, fields) {
	const body = ensureDrill();
	body.innerHTML = "";
	const h = document.createElement("h3");
	h.textContent = title;
	body.appendChild(h);
	for (const f of fields) {
		if (f.value === undefined || f.value === null || f.value === "") continue;
		body.appendChild(f.block ? drillBlock(f.label, f.value) : drillField(f.label, f.value));
	}
	document.getElementById("drill").hidden = false;
}

// inspectable marks a table row as clickable and opens the inspect drawer for it on click.
function inspectable(tr, title, fields) {
	tr.classList.add("row-inspect");
	tr.tabIndex = 0;
	tr.setAttribute("role", "button");
	const open = () => inspectDrawer(title, fields);
	tr.addEventListener("click", open);
	tr.addEventListener("keydown", (e) => {
		if (e.key === "Enter" || e.key === " ") { e.preventDefault(); open(); }
	});
}
