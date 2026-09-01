"use strict";

// A stored flat theme is stamped before anything renders, so a forced theme never flashes the
// signature default. A ?theme= query parameter, named by theme, picks and persists one, so a
// themed link is shareable. The script sits at the end of body, so document.body exists.
(function () {
	let theme = null;
	try { theme = localStorage.getItem("st_theme"); } catch { /* storage may be unavailable */ }
	const param = new URLSearchParams(location.search).get("theme");
	if (param) {
		const byName = {
			loom: "signature", linen: "light", ink: "dark",
			// The first published names still resolve, so links already shared keep working.
			stitch: "signature", kord: "light", seal: "dark",
		};
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

// NAV_GROUPS defines the drawer navigation, grouped by concern. Items marked admin or operator
// are hidden from roles below that level; the server still enforces the real policy.
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
		{ key: "sources", href: "/ui/sources", label: "Sources", desc: "Dynamic inventory sync", operator: true },
		{ key: "templates", href: "/ui/templates", label: "Templates", desc: "Saved launch presets" },
		{ key: "workflows", href: "/ui/workflows", label: "Workflow", desc: "Visual pipeline builder" },
		{ key: "schedules", href: "/ui/schedules", label: "Schedules", desc: "Cron-driven runs", operator: true },
		{ key: "migrate", href: "/ui/migrate", label: "Migrate", desc: "Import from AWX, Semaphore, Rundeck, or Jenkins", admin: true },
	] },
	{ label: "Access", items: [
		{ key: "credentials", href: "/ui/credentials", label: "Credentials", desc: "Secrets and keys", admin: true },
		{ key: "users", href: "/ui/users", label: "Users", desc: "Accounts and roles", admin: true },
		{ key: "audit", href: "/ui/audit", label: "Audit", desc: "Tamper-evident change log", admin: true },
		{ key: "policies", href: "/ui/policies", label: "Policies", desc: "Approval rules", admin: true },
		{ key: "doctor", href: "/ui/doctor", label: "Doctor", desc: "Reference health checks", admin: true },
	] },
	{ label: "Help", items: [
		{ key: "docs", href: "/ui/docs", label: "Docs", desc: "Guides and reference" },
	] },
];


// hostLabel compacts a path-shaped host key for display. A dry-run plan records drift keyed on
// its working directory, so a Terraform root sits beside inventory hosts in fleet and drift
// views; the full path is the row's identity, but as a label it reads as noise and crushes the
// column, so only the last two segments show. Inventory hostnames carry no slash and pass
// through untouched. Callers put the full key in the row's title so hover still tells the truth.
function hostLabel(host) {
	if (!host || host.indexOf("/") === -1) return host;
	const parts = host.split("/").filter(Boolean);
	if (parts.length <= 2) return host;
	return parts.slice(-2).join("/");
}
