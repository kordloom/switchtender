"use strict";

// OUTCOME_RANK orders outcomes from least to most severe for rollups.
const OUTCOME_RANK = { skipped: 0, ok: 1, changed: 2, unreachable: 3, failed: 4 };

// NAV_GROUPS defines the drawer navigation, grouped by concern. Items marked admin are hidden from
// signed in non-admins; the server still enforces the real policy.
const NAV_GROUPS = [
	{ label: "Execution", items: [
		{ key: "overview", href: "/ui/", label: "Overview", desc: "At a glance" },
		{ key: "runs", href: "/ui/runs", label: "Runs", desc: "Every playbook execution" },
		{ key: "fleet", href: "/ui/fleet", label: "Fleet health", desc: "Flaky host detection" },
		{ key: "tasks", href: "/ui/tasks", label: "Task trends", desc: "Duration trends per task" },
		{ key: "workers", href: "/ui/workers", label: "Workers", desc: "Executor fleet status" },
	] },
	{ label: "Automation", items: [
		{ key: "projects", href: "/ui/projects", label: "Projects", desc: "Git-sourced playbooks", admin: true },
		{ key: "inventories", href: "/ui/inventories", label: "Inventories", desc: "Stored host inventories", admin: true },
		{ key: "sources", href: "/ui/sources", label: "Sources", desc: "Dynamic inventory sync", admin: true },
		{ key: "templates", href: "/ui/templates", label: "Templates", desc: "Saved launch presets" },
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
	tasks: "tasks", workers: "workers", projects: "projects", inventories: "inventories",
	sources: "sources", jobtemplates: "templates", schedules: "schedules",
	migrate: "migrate", credentials: "credentials", users: "users", audit: "audit",
	policies: "policies", docs: "docs",
};

// NAV_ICONS holds the inline SVG body for each nav key, stroked in the current color.
const NAV_ICONS = {
	overview: '<rect x="3" y="3" width="7" height="7" rx="1"/><rect x="14" y="3" width="7" height="7" rx="1"/><rect x="14" y="14" width="7" height="7" rx="1"/><rect x="3" y="14" width="7" height="7" rx="1"/>',
	runs: '<circle cx="12" cy="12" r="9"/><polygon points="10 8 16 12 10 16"/>',
	fleet: '<path d="M3 12h4l2 6 4-12 2 6h6"/>',
	tasks: '<path d="M3 17l6-6 4 4 8-8"/><path d="M17 7h4v4"/>',
	workers: '<rect x="3" y="4" width="18" height="7" rx="1"/><rect x="3" y="13" width="18" height="7" rx="1"/><line x1="7" y1="7.5" x2="7.01" y2="7.5"/><line x1="7" y1="16.5" x2="7.01" y2="16.5"/>',
	projects: '<path d="M3 7a2 2 0 0 1 2-2h4l2 2h8a2 2 0 0 1 2 2v8a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2z"/>',
	inventories: '<line x1="8" y1="6" x2="21" y2="6"/><line x1="8" y1="12" x2="21" y2="12"/><line x1="8" y1="18" x2="21" y2="18"/><line x1="3" y1="6" x2="3.01" y2="6"/><line x1="3" y1="12" x2="3.01" y2="12"/><line x1="3" y1="18" x2="3.01" y2="18"/>',
	sources: '<path d="M23 4v6h-6"/><path d="M20.49 15a9 9 0 1 1-2.12-9.36L23 10"/>',
	templates: '<polygon points="12 2 2 7 12 12 22 7 12 2"/><polyline points="2 17 12 22 22 17"/><polyline points="2 12 12 17 22 12"/>',
	schedules: '<circle cx="12" cy="12" r="9"/><polyline points="12 7 12 12 15 14"/>',
	migrate: '<path d="M4 17v2a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2v-2"/><polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/>',
	credentials: '<rect x="5" y="11" width="14" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
	users: '<path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>',
	policies: '<path d="M12 3l7 3v5c0 4.5-3 7.5-7 9-4-1.5-7-4.5-7-9V6z"/><polyline points="9 12 11 14 15 10"/>',
	docs: '<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>',
};

// mountTopbar adds docs and repository links to the top bar on every page, so the guides and the
// source are one click away from anywhere in the product.
function mountTopbar() {
	const bar = document.querySelector(".topbar");
	if (!bar || bar.querySelector(".topbar-links")) return;
	const nav = document.createElement("nav");
	nav.className = "topbar-links";
	const tour = document.createElement("button");
	tour.type = "button";
	tour.className = "topbar-link tour-start";
	tour.textContent = "Tour";
	tour.addEventListener("click", () => {
		if (document.body.dataset.page === "overview") {
			startTour();
		} else {
			sessionStorage.setItem("ym_tour_replay", "1");
			window.location.assign("/ui/");
		}
	});
	nav.appendChild(tour);
	const docs = document.createElement("a");
	docs.href = "/ui/docs";
	docs.className = "topbar-link";
	docs.textContent = "Docs";
	nav.appendChild(docs);
	const gh = document.createElement("a");
	gh.href = "https://github.com/dcadolph/yardmaster";
	gh.className = "topbar-link topbar-icon";
	gh.target = "_blank";
	gh.rel = "noopener";
	gh.title = "View on GitHub";
	gh.setAttribute("aria-label", "View on GitHub");
	gh.innerHTML = '<svg viewBox="0 0 16 16" width="18" height="18" fill="currentColor" aria-hidden="true"><path d="M8 0C3.58 0 0 3.58 0 8c0 3.54 2.29 6.53 5.47 7.59.4.07.55-.17.55-.38 0-.19-.01-.82-.02-1.49-2.01.44-2.53-.49-2.69-.94-.09-.23-.48-.94-.82-1.13-.28-.15-.68-.52-.01-.53.63-.01 1.08.58 1.23.82.72 1.21 1.87.87 2.33.66.07-.52.28-.87.51-1.07-1.78-.2-3.64-.89-3.64-3.95 0-.87.31-1.59.82-2.15-.08-.2-.36-1.02.08-2.12 0 0 .67-.21 2.2.82a7.6 7.6 0 012-.27c.68 0 1.36.09 2 .27 1.53-1.04 2.2-.82 2.2-.82.44 1.1.16 1.92.08 2.12.51.56.82 1.27.82 2.15 0 3.07-1.87 3.75-3.65 3.95.29.25.54.73.54 1.48 0 1.07-.01 1.93-.01 2.2 0 .21.15.46.55.38A8.01 8.01 0 0016 8c0-4.42-3.58-8-8-8z"/></svg>';
	nav.appendChild(gh);
	bar.appendChild(nav);
}

// LIST_PAGES are the pages whose main table is a searchable list.
// LIST_PAGES get the client-side row filter. The runs page is excluded because it searches on the
// server, across every run rather than only the loaded page.
const LIST_PAGES = ["jobtemplates", "credentials", "projects", "inventories",
	"sources", "schedules", "users", "workers", "fleet", "tasks", "host", "policies"];

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

// TOUR_STEPS drives the guided tour. Each step spotlights one element on the overview and explains a
// capability; a step with no selector shows a centered card. The tour is the first-visit welcome and
// replays from the Tour link in the top bar.
const TOUR_STEPS = [
	{
		title: "Welcome to Yardmaster",
		body: "One binary runs Ansible, Bash, Terraform, and Python, with no Kubernetes. Here is the sixty-second tour.",
	},
	{
		sel: ".page-head .button.primary",
		title: "Launch any tool",
		body: "Start a run with Ansible, Bash, Terraform, or Python, each with a dry run, and mix them in a single pipeline.",
	},
	{
		sel: ".panel-runs",
		title: "Watch every run",
		body: "Runs stream live here, with a host matrix, sharded splits, and multi-step pipelines all in one place.",
	},
	{
		sel: ".migrate-callout",
		title: "Bring your work with you",
		body: "Moving off AWX or Semaphore? Import projects, inventories, templates, and schedules in a few clicks.",
	},
	{
		sel: ".tile-search",
		title: "Find anything fast",
		body: "This search filters instantly, and every list in Yardmaster is searchable the same way.",
	},
	{
		sel: ".nav-toggle",
		title: "The rest of the yard",
		body: "Job templates, credentials with external secrets, schedules, and fleet analytics all live in this menu.",
	},
	{
		title: "You are set",
		body: "Explore the demo freely; nothing here can be broken. Replay this tour anytime from Tour in the top bar.",
	},
];

// tourState holds the running tour's overlay elements and current step, or null when no tour is open.
let tourState = null;

// mountTour starts the tour automatically on a first visit to the overview, recording a flag so it
// shows once, and honors a replay requested from another page by the Tour link.
function mountTour() {
	if (document.body.dataset.page !== "overview") return;
	if (sessionStorage.getItem("ym_tour_replay")) {
		sessionStorage.removeItem("ym_tour_replay");
		window.setTimeout(startTour, 300);
		return;
	}
	if (localStorage.getItem("ym_tour_done")) return;
	window.setTimeout(startTour, 400);
}

// startTour builds the spotlight overlay and shows the first step. Calling it while a tour runs
// restarts it from the top.
function startTour() {
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
		'<button type="button" class="button tour-skip">Skip</button>' +
		'<button type="button" class="button tour-back">Back</button>' +
		'<button type="button" class="button primary tour-next">Next</button>' +
		"</div></div>";
	document.body.appendChild(blocker);
	document.body.appendChild(hole);
	document.body.appendChild(pop);

	tourState = { step: 0, blocker, hole, pop };
	pop.querySelector(".tour-skip").addEventListener("click", () => endTour(true));
	pop.querySelector(".tour-back").addEventListener("click", () => moveTour(-1));
	pop.querySelector(".tour-next").addEventListener("click", () => moveTour(1));
	document.addEventListener("keydown", tourKey);
	window.addEventListener("resize", tourReflow);
	window.addEventListener("scroll", tourReflow, true);
	showTourStep();
}

// moveTour advances or rewinds the tour, ending it when Next is pressed on the last step.
function moveTour(delta) {
	if (!tourState) return;
	const next = tourState.step + delta;
	if (next >= TOUR_STEPS.length) {
		endTour(true);
		return;
	}
	if (next < 0) return;
	tourState.step = next;
	showTourStep();
}

// showTourStep fills the popover for the current step, scrolls its target into view, positions the
// spotlight, and focuses Next so the keyboard drives the tour.
function showTourStep() {
	if (!tourState) return;
	const step = TOUR_STEPS[tourState.step];
	const { pop } = tourState;
	pop.querySelector(".tour-title").textContent = step.title;
	pop.querySelector(".tour-text").textContent = step.body;
	pop.querySelector(".tour-count").textContent = (tourState.step + 1) + " / " + TOUR_STEPS.length;
	pop.querySelector(".tour-back").hidden = tourState.step === 0;
	const isLast = tourState.step === TOUR_STEPS.length - 1;
	pop.querySelector(".tour-next").textContent = isLast ? "Explore" : "Next";
	pop.querySelector(".tour-skip").hidden = isLast;

	const el = step.sel ? document.querySelector(step.sel) : null;
	if (el) el.scrollIntoView({ block: "center", inline: "nearest" });
	renderTourPosition();
	pop.querySelector(".tour-next").focus();
}

// renderTourPosition places the spotlight and popover for the current step without scrolling, so it
// is safe to call on scroll and resize.
function renderTourPosition() {
	if (!tourState) return;
	const step = TOUR_STEPS[tourState.step];
	const el = step.sel ? document.querySelector(step.sel) : null;
	if (el) {
		placeTourAt(el.getBoundingClientRect());
	} else {
		placeTourCentered();
	}
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

// tourKey drives the tour from the keyboard: Escape ends it, arrows and Enter move between steps.
function tourKey(e) {
	if (!tourState) return;
	if (e.key === "Escape") {
		endTour(true);
	} else if (e.key === "ArrowRight" || e.key === "Enter") {
		e.preventDefault();
		moveTour(1);
	} else if (e.key === "ArrowLeft") {
		e.preventDefault();
		moveTour(-1);
	}
}

// endTour tears down the overlay and its listeners. When completed is true it records that the tour
// has been seen so it does not auto-start again.
function endTour(completed) {
	if (completed) localStorage.setItem("ym_tour_done", "1");
	if (!tourState) return;
	document.removeEventListener("keydown", tourKey);
	window.removeEventListener("resize", tourReflow);
	window.removeEventListener("scroll", tourReflow, true);
	tourState.blocker.remove();
	tourState.hole.remove();
	tourState.pop.remove();
	tourState = null;
}

document.addEventListener("DOMContentLoaded", () => {
	consumeSSOFragment();
	mountTopbar();
	if (LIST_PAGES.includes(document.body.dataset.page)) mountListFilter();
	const close = document.getElementById("drill-close");
	if (close) {
		close.addEventListener("click", () => { document.getElementById("drill").hidden = true; });
	}
	const page = document.body.dataset.page;
	if (page === "overview") {
		loadOverview();
	} else if (page === "runs") {
		wireModal("launch");
		if (!isReadOnly()) wireLaunchForm();
		wireRunsSearch();
		loadRuns();
	} else if (page === "detail") {
		loadDetail(document.body.dataset.runId);
	} else if (page === "fleet") {
		loadFleet();
	} else if (page === "host") {
		loadHost(document.body.dataset.host);
	} else if (page === "tasks") {
		loadTasks();
	} else if (page === "schedules") {
		loadSchedules();
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
		banner.textContent = "Read-only demo. Browse the data freely; changes are disabled.";
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
}

// buildNav injects the menu toggle and the slide-in drawer on every page but sign in, highlighting
// the current page and hiding admin links from non-admins. The toggle opens and closes the drawer.
function buildNav() {
	if (document.body.dataset.page === "login") return;
	const topbar = document.querySelector(".topbar");
	if (!topbar) return;

	const role = localStorage.getItem("ym_role");
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
		drawer.appendChild(g);
	}

	document.body.appendChild(backdrop);
	document.body.appendChild(drawer);

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

// apiToken returns the stored API token, empty when the server runs open.
function apiToken() {
	return localStorage.getItem("ym_token") || "";
}

// consumeSSOFragment stores the session token handed back in the URL fragment after single
// sign-on, then strips it from the address bar so it is not left in history or copied by accident.
function consumeSSOFragment() {
	if (!location.hash || location.hash.indexOf("access_token=") === -1) return;
	const params = new URLSearchParams(location.hash.slice(1));
	const token = params.get("access_token");
	if (!token) return;
	localStorage.setItem("ym_token", token);
	if (params.get("role")) localStorage.setItem("ym_role", params.get("role"));
	if (params.get("user")) localStorage.setItem("ym_user", params.get("user"));
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

// requireLogin sends the browser to the sign in page, remembering where it was.
function requireLogin() {
	if (document.body.dataset.page === "login") return;
	sessionStorage.setItem("ym_return", location.pathname);
	location.href = "/ui/login";
}

// getJSON fetches and decodes a JSON endpoint, redirecting to sign in on a 401.
async function getJSON(url) {
	const res = await fetch(url, { headers: authHeaders() });
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	if (!res.ok) {
		throw new Error(url + " returned " + res.status);
	}
	return res.json();
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
async function loadAudit() {
	try {
		const data = await getJSON("/audit?limit=500");
		const entries = data.entries || [];
		if (entries.length === 0) {
			showEmpty("No audit entries yet. Mutations are recorded here.");
			return;
		}
		const tbody = document.getElementById("audit");
		for (const e of entries) {
			const tr = document.createElement("tr");
			tr.appendChild(td(String(e.seq)));
			tr.appendChild(tdTime(e.at));
			tr.appendChild(td(e.actor || "-"));
			tr.appendChild(td(e.method, "mono"));
			tr.appendChild(td(e.path, "mono"));
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
	const close = () => { modal.hidden = true; };
	openBtn.addEventListener("click", () => { modal.hidden = false; });
	const closeBtn = document.getElementById(name + "-close");
	if (closeBtn) closeBtn.addEventListener("click", close);
	modal.addEventListener("click", (e) => { if (e.target === modal) close(); });
	document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !modal.hidden) close(); });
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
		if (tool === "terraform") commandInput.placeholder = "working directory, e.g. infra";
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
	document.getElementById("cred-status").textContent = "";
	setModalTitle("cred", "Edit credential");
	document.getElementById("cred-modal").hidden = false;
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
		gsm: '{"project":"my-project","secret":"ci-token","version":"latest"}',
	};
	source.addEventListener("change", () => {
		document.getElementById("cred-secret").placeholder = sourcePlaceholders[source.value] || secPlaceholder;
	});
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("cred-name").value = "";
		document.getElementById("cred-source").value = "local";
		const sec = document.getElementById("cred-secret");
		sec.value = "";
		sec.required = true;
		sec.placeholder = secPlaceholder;
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
					const res = await fetch("/credentials/" + c.id, {
						method: "DELETE", headers: authHeaders(),
					});
					if (!res.ok) throw new Error("HTTP " + res.status);
					tr.remove();
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
	sub.textContent = "Set a secret on each to make it usable. Imported credentials arrive this way.";
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
			tr.appendChild(td(p.repo_url, "mono"));
			tr.appendChild(td(p.branch || "default", "mono"));
			tr.appendChild(tdTime(p.created_at));
			const actions = deleteCell("/projects/" + p.id, "project " + p.name, tr);
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
		const path = "/import/" + format + (apply ? "?apply=true" : "");
		const res = await fetch(path, { method: "POST", headers: authHeaders(), body });
		if (res.status === 401) {
			requireLogin();
			throw new Error("authentication required");
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
		status.textContent = "";
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
// selected tool, so a bash, terraform, python, or go template hides playbook, inventory, and shards.
function syncTemplateTool() {
	const tool = document.getElementById("tpl-tool").value;
	const ansible = tool === "ansible" || tool === "";
	for (const id of ["tpl-field-playbook", "tpl-field-inventory", "tpl-field-shards"]) {
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
			tr.appendChild(td(t.playbook, "mono"));
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
			const delBtn = deleteCell("/templates/" + t.id, "template " + t.name, tr);
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
			const created = await postAction("/templates/" + t.id + "/launch", answers);
			location.href = "/ui/runs/" + created.id;
		} catch (err) {
			document.getElementById("survey-status").textContent = "Launch failed: " + err.message;
		}
	};
}

// deleteCell builds a table cell holding a delete button for a resource.
function deleteCell(path, label, tr) {
	const cell = document.createElement("td");
	const del = document.createElement("button");
	del.className = "button danger";
	del.textContent = "Delete";
	del.addEventListener("click", async (e) => {
		e.preventDefault();
		e.stopPropagation();
		if (!window.confirm("Delete " + label + "?")) return;
		try {
			const res = await fetch(path, { method: "DELETE", headers: authHeaders() });
			if (!res.ok) throw new Error("HTTP " + res.status);
			tr.remove();
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
	for (const id of ["inv-command", "inv-vault-addr", "inv-vault-path", "inv-vault-field",
		"inv-vault-token", "inv-gsm-project", "inv-gsm-secret", "inv-gsm-version", "inv-gsm-token"]) {
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
// sends the pasted content; a command, Vault, or Google Secret Manager source assembles the config
// the API seals, so the operator never hand writes JSON. It throws with a message when a required
// field is missing. On edit the config is never returned, so leaving a source's fields blank keeps
// the stored config.
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
			const actions = deleteCell("/inventories/" + i.id, "inventory " + i.name, tr);
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
			const del = deleteCell("/inventory-sources/" + src.id, "source " + src.name, tr);
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
			const fresh = Date.now() - new Date(w.last_seen).getTime() < 30000;
			const chip = document.createElement("span");
			chip.className = fresh ? "chip ok" : "chip none";
			chip.textContent = fresh ? "alive" : "stale";
			health.appendChild(chip);
			tr.appendChild(health);
			tr.appendChild(td(String(w.active)));
			tr.appendChild(tdTime(w.last_seen));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load workers: " + e.message);
	}
}

// loadOverview draws the home dashboard: headline metrics, recent runs, a fleet snapshot, and the
// jump tiles that navigate to every section.
async function loadOverview() {
	renderJumpTiles();
	wireTileFilter();
	const [runsRes, fleetRes] = await Promise.all([
		getJSON("/runs").catch(() => ({ runs: [] })),
		getJSON("/fleet").catch(() => ({ hosts: [] })),
	]);
	const runs = runsRes.runs || [];
	const hosts = fleetRes.hosts || [];
	renderOverviewMetrics(runs, hosts);
	renderRecentRuns(runs.slice(0, 10));
	setStatus("");
}

// renderOverviewMetrics fills the headline metric strip from the run and fleet data.
function renderOverviewMetrics(runs, hosts) {
	let succeeded = 0;
	let failed = 0;
	for (const r of runs) {
		if (r.status === "succeeded") succeeded++;
		else if (r.status === "failed") failed++;
	}
	const rate = runs.length ? Math.round((succeeded / runs.length) * 100) + "%" : "—";
	const el = document.getElementById("ov-metrics");
	el.innerHTML = "";
	el.appendChild(statCard(runs.length, "Total runs", ""));
	el.appendChild(statCard(rate, "Success rate", ""));
	el.appendChild(statCard(failed, "Failed", failed ? "failed" : ""));
	el.appendChild(statCard(hosts.length, "Hosts tracked", ""));
	el.hidden = false;
}

// renderRecentRuns lists the latest runs, each a link to its detail page.
function renderRecentRuns(runs) {
	const el = document.getElementById("recent");
	el.innerHTML = "";
	if (!runs.length) { el.appendChild(emptyLine("No runs yet.")); return; }
	for (const r of runs) {
		const row = document.createElement("a");
		row.className = "ov-row";
		row.href = "/ui/runs/" + r.id;
		row.appendChild(badge(r.status));
		const name = document.createElement("span");
		name.className = "ov-row-name";
		name.textContent = baseName(r.playbook) || r.id;
		name.title = r.playbook || r.id;
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
		el.appendChild(row);
	}
}

// renderJumpTiles draws the navigation tiles for every section but the overview itself, sorted
// alphabetically, each with an icon chip, a label, and a one-line description.
function renderJumpTiles() {
	const el = document.getElementById("tiles");
	if (!el) return;
	const role = localStorage.getItem("ym_role");
	const showAdmin = !role || role === "admin";
	const items = [];
	for (const group of NAV_GROUPS) {
		for (const it of group.items) {
			if (it.key === "overview") continue;
			if (it.admin && !showAdmin) continue;
			items.push(it);
		}
	}
	items.sort((a, b) => a.label.localeCompare(b.label));

	el.innerHTML = "";
	for (const it of items) {
		const tile = document.createElement("a");
		tile.className = "tile";
		tile.href = it.href;

		const icon = document.createElement("span");
		icon.className = "tile-icon";
		icon.innerHTML = svgIcon(NAV_ICONS[it.key] || "");
		tile.appendChild(icon);

		const text = document.createElement("span");
		text.className = "tile-text";
		const label = document.createElement("span");
		label.className = "tile-label";
		label.textContent = it.label;
		text.appendChild(label);
		if (it.desc) {
			const desc = document.createElement("span");
			desc.className = "tile-desc";
			desc.textContent = it.desc;
			text.appendChild(desc);
		}
		tile.appendChild(text);
		el.appendChild(tile);
	}
}

// wireTileFilter filters the jump tiles as the user types and jumps to the first match on Enter.
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
		for (const tile of el.querySelectorAll(".tile")) {
			const match = tile.textContent.toLowerCase().includes(q);
			tile.hidden = !match;
			if (match) shown++;
		}
		empty.hidden = shown > 0;
	});
	input.addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			const first = el.querySelector(".tile:not([hidden])");
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

// loadRuns populates the run history table.
async function loadRuns() {
	const tbody = document.getElementById("runs");
	const table = document.querySelector("table.runs");
	const sizeEl = document.getElementById("runs-pagesize");
	if (sizeEl) sizeEl.onchange = () => loadRuns();
	setStatus("");
	showSkeletonRows(tbody, 6, 5);
	table.hidden = false;
	try {
		const data = await getJSON("/runs?limit=" + runsPageSize() + "&offset=0&q=" + encodeURIComponent(runsQuery()));
		const runs = data.runs || [];
		tbody.innerHTML = "";
		if (runs.length === 0) {
			table.hidden = true;
			showEmpty(runsQuery() ? "No runs match your search." : "No runs yet.");
			return;
		}
		renderSummary(data.summary || {});
		appendRunRows(tbody, runs);
		wireRunsMore(tbody, runs.length, data.hasMore);
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
	for (const r of runs) {
		const tr = document.createElement("tr");
		tr.addEventListener("click", () => { location.href = "/ui/runs/" + r.id; });
		tr.appendChild(tdBadge(r.status));

		const runCell = td(shortId(r.id), "mono");
		runCell.title = r.id;
		if (r.kind === "split" || r.kind === "pipeline") {
			const tag = document.createElement("span");
			tag.className = "run-kind " + r.kind;
			tag.textContent = r.kind;
			runCell.appendChild(document.createTextNode(" "));
			runCell.appendChild(tag);
		}
		tr.appendChild(runCell);

		const pbCell = td("");
		const badge = toolBadgeEl(r);
		if (badge) {
			pbCell.appendChild(badge);
			pbCell.appendChild(document.createTextNode(" "));
		}
		pbCell.appendChild(document.createTextNode(toolLabel(r)));
		pbCell.title = r.playbook || r.command || "";
		if (r.dry_run) {
			const dry = document.createElement("span");
			dry.className = "run-kind dry";
			dry.textContent = "dry";
			pbCell.appendChild(document.createTextNode(" "));
			pbCell.appendChild(dry);
		}
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
			wireRunsMore(tbody, offset + runs.length, data.hasMore);
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
		const data = await getJSON("/schedules");
		const schedules = data.schedules || [];
		if (schedules.length === 0) {
			showEmpty("No schedules yet. Create one with POST /schedules.");
			return;
		}
		const tbody = document.getElementById("schedules");
		for (const s of schedules) {
			const tr = document.createElement("tr");
			tr.appendChild(td(s.name || "(unnamed)"));
			tr.appendChild(td(s.cron, "mono"));
			tr.appendChild(td(scheduleTarget(s)));

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
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
		setStatus("Failed to load schedules: " + e.message);
	}
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
					const res = await fetch("/policies/" + p.id, { method: "DELETE", headers: authHeaders() });
					if (!res.ok) throw new Error("HTTP " + res.status);
					tr.remove();
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
	document.getElementById("full-log").href = "/runs/" + runId + "/logs";
	wireActions(runId);
	try {
		const run = await getJSON("/runs/" + runId);
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
	const res = await fetch(path, opts);
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
	if (!token) return path;
	const sep = path.includes("?") ? "&" : "?";
	return path + sep + "access_token=" + encodeURIComponent(token);
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
		after = data.nextAfter;
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
			const res = await fetch("/auth/login", {
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
			localStorage.setItem("ym_token", session.token);
			localStorage.setItem("ym_role", session.role);
			localStorage.setItem("ym_user", session.username);
			location.href = sessionStorage.getItem("ym_return") || "/ui/";
		} catch (err) {
			setStatus("Sign in failed: " + err.message);
		}
	});
	document.getElementById("login-form").addEventListener("submit", async (e) => {
		e.preventDefault();
		const token = document.getElementById("token-input").value.trim();
		if (!token) return;
		const res = await fetch("/auth/check", {
			method: "POST", headers: { "Authorization": "Bearer " + token },
		});
		if (res.status === 204) {
			localStorage.setItem("ym_token", token);
			localStorage.removeItem("ym_role");
			localStorage.removeItem("ym_user");
			location.href = sessionStorage.getItem("ym_return") || "/ui/";
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
			const actions = deleteCell("/users/" + u.id, "user " + u.username, tr);
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
	cancel.addEventListener("click", async () => {
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
	retry.addEventListener("click", async () => {
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
}

// updateActions shows cancel while the run is active and retry on a finished split that did not
// fully succeed.
function updateActions(run) {
	const cancel = document.getElementById("cancel-run");
	const retry = document.getElementById("retry-run");
	if (!cancel || !retry) return;
	const approve = document.getElementById("approve-run");
	const reject = document.getElementById("reject-run");
	if (isReadOnly()) {
		cancel.hidden = true;
		retry.hidden = true;
		if (approve) approve.hidden = true;
		if (reject) reject.hidden = true;
		return;
	}
	const held = run.status === "pending_approval";
	cancel.hidden = isTerminal(run.status) || held;
	if (approve) approve.hidden = !held;
	if (reject) reject.hidden = !held;
	const splitParent = (run.kind === "split" || run.shard_count) && !run.parent_id;
	retry.hidden = !(splitParent && isTerminal(run.status) && run.status !== "succeeded");
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
	const source = new EventSource(streamURL("/runs/" + pipelineId + "/stream"));
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
		let text = idx + ". " + (s.step_name || "step") + "  ·  " + (s.playbook || "");
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
	const source = new EventSource(streamURL("/runs/" + parentId + "/stream"));
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

// appendLog adds a chunk to the live log pane and keeps it scrolled to the end. It appends a
// text node rather than rebuilding the whole string, which keeps a long stream from getting
// quadratic, and trims the pane back to the cap when it grows past it.
function appendLog(chunk) {
	const pre = document.getElementById("log");
	pre.appendChild(document.createTextNode(chunk));
	detailState.logLen = (detailState.logLen || 0) + chunk.length;
	if (detailState.logLen > logCap * 2) {
		pre.textContent = pre.textContent.slice(-logCap);
		detailState.logLen = pre.textContent.length;
	}
	document.getElementById("log-panel").hidden = false;
	pre.scrollTop = pre.scrollHeight;
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
		el.appendChild(field(run.tool === "terraform" ? "Directory" : "Command",
			toolLabel(run), null, run.command || ""));
	}
	if (run.dry_run) {
		el.appendChild(field("Mode", "dry run"));
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
		rowTh.textContent = host;
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
	tr.addEventListener("click", () => inspectDrawer(title, fields));
}
