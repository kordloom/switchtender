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
	] },
	{ label: "Access", items: [
		{ key: "credentials", href: "/ui/credentials", label: "Credentials", desc: "Secrets and keys", admin: true },
		{ key: "users", href: "/ui/users", label: "Users", desc: "Accounts and roles", admin: true },
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
	credentials: "credentials", users: "users", docs: "docs",
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
	credentials: '<rect x="5" y="11" width="14" height="10" rx="2"/><path d="M8 11V7a4 4 0 0 1 8 0v4"/>',
	users: '<path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2"/><circle cx="9" cy="7" r="4"/><path d="M23 21v-2a4 4 0 0 0-3-3.87M16 3.13a4 4 0 0 1 0 7.75"/>',
	docs: '<path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/><path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>',
};

document.addEventListener("DOMContentLoaded", () => {
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
	}
	buildNav();
	if (isReadOnly()) applyReadOnly();
	setInterval(refreshRelTimes, 20000);
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

// wireLaunchForm hooks the launch panel up to POST /runs and fills the credential picker.
function wireLaunchForm() {
	const form = document.getElementById("launch-form");
	if (!form) return;
	fillCredentialPicker();
	fillSelect(document.getElementById("launch-project"), "/projects", "projects", (p) => p.name);
	fillSelect(document.getElementById("launch-inventory-id"), "/inventories", "inventories",
		(i) => i.name);
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("launch-status");
		const payload = {
			playbook: document.getElementById("launch-playbook").value.trim(),
			inventory: document.getElementById("launch-inventory").value.trim(),
		};
		const projectID = document.getElementById("launch-project").value;
		if (projectID) payload.project_id = projectID;
		const inventoryID = document.getElementById("launch-inventory-id").value;
		if (inventoryID) {
			payload.inventory_id = inventoryID;
			delete payload.inventory;
		}
		const queue = document.getElementById("launch-queue").value.trim();
		if (queue) payload.queue = queue;
		const shards = parseInt(document.getElementById("launch-shards").value, 10);
		if (shards >= 2) payload.shards = shards;
		const picked = Array.from(document.getElementById("launch-credentials").selectedOptions)
			.map((o) => o.value);
		if (picked.length) payload.credential_ids = picked;
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

// wireCredentialForm hooks the add credential form up to POST /credentials.
function wireCredentialForm() {
	const form = document.getElementById("cred-form");
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("cred-status");
		const payload = {
			name: document.getElementById("cred-name").value.trim(),
			kind: document.getElementById("cred-kind").value,
			secret: document.getElementById("cred-secret").value,
		};
		try {
			await postAction("/credentials", payload);
			document.getElementById("cred-name").value = "";
			document.getElementById("cred-secret").value = "";
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

// wireTemplateForm hooks the add template form up to POST /templates.
function wireTemplateForm() {
	fillSelect(document.getElementById("tpl-project"), "/projects", "projects", (p) => p.name);
	fillSelect(document.getElementById("tpl-credentials"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	document.getElementById("template-form").addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("tpl-status");
		const payload = {
			name: document.getElementById("tpl-name").value.trim(),
			project_id: document.getElementById("tpl-project").value,
			playbook: document.getElementById("tpl-playbook").value.trim(),
			inventory: document.getElementById("tpl-inventory").value.trim(),
		};
		const shards = parseInt(document.getElementById("tpl-shards").value, 10);
		if (shards >= 2) payload.shards = shards;
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
		try {
			await postAction("/templates", payload);
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
// so the next save issues a PUT rather than a create.
function openInventoryEdit(inv) {
	const form = document.getElementById("inventory-form");
	form.dataset.editId = inv.id;
	document.getElementById("inv-name").value = inv.name;
	document.getElementById("inv-content").value = inv.content || "";
	document.getElementById("inv-status").textContent = "";
	setModalTitle("inventory", "Edit inventory");
	document.getElementById("inventory-modal").hidden = false;
}

// wireInventoryForm hooks the inventory dialog up to POST /inventories for a new record and PUT
// /inventories/{id} when editing. The New button resets the dialog to add mode.
function wireInventoryForm() {
	const form = document.getElementById("inventory-form");
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("inv-name").value = "";
		document.getElementById("inv-content").value = "";
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
			content: document.getElementById("inv-content").value,
		};
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
	renderRecentRuns(runs.slice(0, 6));
	renderFleetSnapshot(hosts.slice(0, 6));
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

// renderFleetSnapshot lists the top hosts by recent activity, each a link to its history.
function renderFleetSnapshot(hosts) {
	const el = document.getElementById("fleet-snap");
	el.innerHTML = "";
	if (!hosts.length) { el.appendChild(emptyLine("No host history yet.")); return; }
	for (const h of hosts) {
		const row = document.createElement("a");
		row.className = "ov-row";
		row.href = "/ui/hosts/" + encodeURIComponent(h.host);
		const name = document.createElement("span");
		name.className = "ov-row-name mono";
		name.textContent = h.host;
		row.appendChild(name);
		const chip = document.createElement("span");
		chip.className = h.flaky ? "chip flaky" : "chip none";
		chip.textContent = h.flaky ? "flaky" : "steady";
		row.appendChild(chip);
		row.appendChild(outcomeChip(h.last_outcome));
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

// loadRuns populates the run history table.
async function loadRuns() {
	const tbody = document.getElementById("runs");
	const table = document.querySelector("table.runs");
	setStatus("");
	showSkeletonRows(tbody, 6, 5);
	table.hidden = false;
	try {
		const data = await getJSON("/runs");
		const runs = data.runs || [];
		tbody.innerHTML = "";
		if (runs.length === 0) { table.hidden = true; showEmpty("No runs yet."); return; }
		renderSummary(runs);
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

			const pbCell = td(baseName(r.playbook) || (r.playbook || ""));
			pbCell.title = r.playbook || "";
			tr.appendChild(pbCell);

			tr.appendChild(tdTime(r.started_at || r.created_at));

			tr.appendChild(td(fmtDuration(r.started_at, r.ended_at)));
			tbody.appendChild(tr);
		}
	} catch (e) {
		tbody.innerHTML = "";
		table.hidden = true;
		setStatus("Failed to load runs: " + e.message);
	}
}

// renderSummary draws the at-a-glance stat cards above the run history.
function renderSummary(runs) {
	const counts = { total: runs.length, succeeded: 0, failed: 0, active: 0 };
	for (const r of runs) {
		if (r.status === "succeeded") {
			counts.succeeded++;
		} else if (r.status === "failed") {
			counts.failed++;
		} else if (r.status === "running" || r.status === "pending") {
			counts.active++;
		}
	}
	const el = document.getElementById("summary");
	el.innerHTML = "";
	el.appendChild(statCard(counts.total, "Total runs", ""));
	el.appendChild(statCard(counts.succeeded, "Succeeded", "ok"));
	el.appendChild(statCard(counts.failed, "Failed", "failed"));
	el.appendChild(statCard(counts.active, "Active", "running"));
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
	span.textContent = status;
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
	return token ? path + "?access_token=" + encodeURIComponent(token) : path;
}

// loadLogin wires both sign in forms: account login mints a session token, and the raw token
// form verifies a pasted token against the API.
function loadLogin() {
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

// wireUserForm hooks the add user form up to POST /users.
function wireUserForm() {
	document.getElementById("user-form").addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("user-status");
		try {
			await postAction("/users", {
				username: document.getElementById("user-name").value.trim(),
				password: document.getElementById("user-password").value,
				role: document.getElementById("user-role").value,
			});
			document.getElementById("user-name").value = "";
			document.getElementById("user-password").value = "";
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
			tr.appendChild(deleteCell("/users/" + u.id, "user " + u.username, tr));
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
}

// updateActions shows cancel while the run is active and retry on a finished split that did not
// fully succeed.
function updateActions(run) {
	const cancel = document.getElementById("cancel-run");
	const retry = document.getElementById("retry-run");
	if (!cancel || !retry) return;
	if (isReadOnly()) {
		cancel.hidden = true;
		retry.hidden = true;
		return;
	}
	cancel.hidden = isTerminal(run.status);
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
	const ev = await getJSON("/runs/" + run.id + "/events");
	detailState = { runId: run.id, run, events: ev.events || [] };
	renderDetail();
	setStatus("");
	if (!isTerminal(run.status)) {
		openStream(run.id);
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
		getJSON("/runs/" + s.id + "/events").then((r) => r.events || []).catch(() => [])));

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
			renderDetail();
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
	const model = buildModel(detailState.events);
	renderMatrix(model);
	renderTimeline(model);
}

// openStream subscribes to the run's live output and applies events, logs, and the end signal.
function openStream(runId) {
	const indicator = document.getElementById("live-indicator");
	if (indicator) indicator.hidden = false;

	const source = new EventSource(streamURL("/runs/" + runId + "/stream"));
	source.addEventListener("event", (e) => {
		try {
			detailState.events.push(JSON.parse(e.data));
			renderDetail();
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

// appendLog adds a chunk to the live log pane and keeps it scrolled to the end.
function appendLog(chunk) {
	const pre = document.getElementById("log");
	pre.textContent += chunk;
	document.getElementById("log-panel").hidden = false;
	pre.scrollTop = pre.scrollHeight;
}

// renderHeader fills the run header fields.
function renderHeader(run) {
	const el = document.getElementById("run-header");
	el.innerHTML = "";
	el.appendChild(field("Status", null, badge(run.status)));
	el.appendChild(field("Run", shortId(run.id), null, run.id));
	el.appendChild(field("Playbook", baseName(run.playbook) || (run.playbook || ""), null, run.playbook || ""));
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
	return { tasks, hosts: Array.from(hosts).sort(), cells, taskStart, end: statsTime || lastTime };
}

// addTask records a task name once, preserving first seen order.
function addTask(tasks, seen, task) {
	if (task && !seen.has(task)) { seen.add(task); tasks.push(task); }
}

// renderMatrix draws the host by task outcome grid.
function renderMatrix(model) {
	const { tasks, hosts, cells } = model;
	const table = document.getElementById("matrix");
	table.innerHTML = "";
	if (hosts.length === 0 || tasks.length === 0) return;

	const thead = document.createElement("thead");
	const htr = document.createElement("tr");
	const corner = document.createElement("th");
	corner.className = "corner";
	corner.textContent = "host \\ task";
	htr.appendChild(corner);
	const taskThs = [];
	for (const task of tasks) {
		const th = document.createElement("th");
		th.textContent = task;
		htr.appendChild(th);
		taskThs.push(th);
	}
	thead.appendChild(htr);
	table.appendChild(thead);

	const counts = {};
	const colCells = tasks.map(() => []);
	const tbody = document.createElement("tbody");
	for (const host of hosts) {
		const tr = document.createElement("tr");
		const rowTh = document.createElement("th");
		rowTh.textContent = host;
		tr.appendChild(rowTh);
		const rowCells = [];
		tasks.forEach((task, ci) => {
			const info = cells[host] && cells[host][task];
			const outcome = info ? info.outcome : "none";
			counts[outcome] = (counts[outcome] || 0) + 1;
			const cell = document.createElement("td");
			const div = document.createElement("div");
			div.className = "cell " + outcome;
			div.title = host + " / " + task + ": " + outcome;
			colCells[ci].push(div);
			rowCells.push(div);
			// Trace a cell across a wide matrix: light up its row and column, headers included.
			div.addEventListener("mouseenter", () => {
				rowTh.classList.add("hi");
				taskThs[ci].classList.add("hi");
				rowCells.forEach((c) => c.classList.add("row-hi"));
				colCells[ci].forEach((c) => c.classList.add("col-hi"));
			});
			div.addEventListener("mouseleave", () => {
				rowTh.classList.remove("hi");
				taskThs[ci].classList.remove("hi");
				rowCells.forEach((c) => c.classList.remove("row-hi"));
				colCells[ci].forEach((c) => c.classList.remove("col-hi"));
			});
			if (info) {
				div.addEventListener("click", () => showDrill(Object.assign({ host, task }, info)));
			}
			cell.appendChild(div);
			tr.appendChild(cell);
		});
		tbody.appendChild(tr);
	}
	table.appendChild(tbody);
	renderMatrixSummary(hosts.length, tasks.length, counts);
	document.getElementById("matrix-panel").hidden = false;
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
