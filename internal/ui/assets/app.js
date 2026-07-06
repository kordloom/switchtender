"use strict";

// OUTCOME_RANK orders outcomes from least to most severe for rollups.
const OUTCOME_RANK = { skipped: 0, ok: 1, changed: 2, unreachable: 3, failed: 4 };

document.addEventListener("DOMContentLoaded", () => {
	const close = document.getElementById("drill-close");
	if (close) {
		close.addEventListener("click", () => { document.getElementById("drill").hidden = true; });
	}
	const page = document.body.dataset.page;
	if (page === "index") {
		wireLaunchForm();
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
		wireCredentialForm();
		loadCredentials();
	} else if (page === "projects") {
		wireProjectForm();
		loadProjects();
	} else if (page === "jobtemplates") {
		wireTemplateForm();
		loadTemplates();
	}
});

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
	if (msg) { el.textContent = msg; el.hidden = false; } else { el.hidden = true; }
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

// wireLaunchForm hooks the launch panel up to POST /runs and fills the credential picker.
function wireLaunchForm() {
	const form = document.getElementById("launch-form");
	if (!form) return;
	fillCredentialPicker();
	fillSelect(document.getElementById("launch-project"), "/projects", "projects", (p) => p.name);
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("launch-status");
		const payload = {
			playbook: document.getElementById("launch-playbook").value.trim(),
			inventory: document.getElementById("launch-inventory").value.trim(),
		};
		const projectID = document.getElementById("launch-project").value;
		if (projectID) payload.project_id = projectID;
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
			setStatus("No credentials yet.");
			return;
		}
		const tbody = document.getElementById("credentials");
		for (const c of creds) {
			const tr = document.createElement("tr");
			tr.appendChild(td(c.name));
			tr.appendChild(td(c.kind, "mono"));
			tr.appendChild(td(fmtTime(c.created_at)));
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

// wireProjectForm hooks the add project form up to POST /projects.
function wireProjectForm() {
	fillSelect(document.getElementById("project-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	document.getElementById("project-form").addEventListener("submit", async (e) => {
		e.preventDefault();
		const status = document.getElementById("project-status");
		try {
			await postAction("/projects", {
				name: document.getElementById("project-name").value.trim(),
				repo_url: document.getElementById("project-repo").value.trim(),
				branch: document.getElementById("project-branch").value.trim(),
				credential_id: document.getElementById("project-credential").value,
			});
			status.textContent = "Saved.";
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
			setStatus("No projects yet.");
			return;
		}
		const tbody = document.getElementById("projects");
		for (const p of projects) {
			const tr = document.createElement("tr");
			tr.appendChild(td(p.name));
			tr.appendChild(td(p.repo_url, "mono"));
			tr.appendChild(td(p.branch || "default", "mono"));
			tr.appendChild(td(fmtTime(p.created_at)));
			tr.appendChild(deleteCell("/projects/" + p.id, "project " + p.name, tr));
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
		try {
			await postAction("/templates", payload);
			status.textContent = "Saved.";
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
			setStatus("No templates yet.");
			return;
		}
		const tbody = document.getElementById("templates");
		for (const t of templates) {
			const tr = document.createElement("tr");
			tr.appendChild(td(t.name));
			tr.appendChild(td(t.playbook, "mono"));
			tr.appendChild(td(String(t.shards || 1)));
			tr.appendChild(td(fmtTime(t.created_at)));
			const actions = document.createElement("td");
			const launch = document.createElement("button");
			launch.className = "button primary";
			launch.textContent = "Launch";
			launch.addEventListener("click", async (e) => {
				e.preventDefault();
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

// deleteCell builds a table cell holding a delete button for a resource.
function deleteCell(path, label, tr) {
	const cell = document.createElement("td");
	const del = document.createElement("button");
	del.className = "button danger";
	del.textContent = "Delete";
	del.addEventListener("click", async (e) => {
		e.preventDefault();
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

// loadRuns populates the run history table.
async function loadRuns() {
	try {
		const data = await getJSON("/runs");
		const runs = data.runs || [];
		if (runs.length === 0) { setStatus("No runs yet."); return; }
		renderSummary(runs);
		const tbody = document.getElementById("runs");
		for (const r of runs) {
			const tr = document.createElement("tr");
			tr.addEventListener("click", () => { location.href = "/ui/runs/" + r.id; });
			tr.appendChild(tdBadge(r.status));
			tr.appendChild(td(r.id, "mono"));
			tr.appendChild(td(r.playbook || ""));
			tr.appendChild(td(fmtTime(r.started_at || r.created_at)));
			tr.appendChild(td(fmtDuration(r.started_at, r.ended_at)));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
	} catch (e) {
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
			setStatus("No host history yet. Run a playbook to build fleet health.");
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
			tr.appendChild(td(fmtTime(h.last_run)));
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
			setStatus("No history for this host yet.");
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
			tr.appendChild(td(fmtTime(r.ran_at)));
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
			setStatus("No task history yet. Run a playbook to build trends.");
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
			tr.appendChild(td(fmtTime(t.last_run)));
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
			setStatus("No schedules yet. Create one with POST /schedules.");
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
async function postAction(path, payload) {
	const opts = { method: "POST", headers: authHeaders() };
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

// loadLogin wires the sign in form: verify the token against the API, store it, and return.
function loadLogin() {
	const form = document.getElementById("login-form");
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		const token = document.getElementById("token-input").value.trim();
		if (!token) return;
		const res = await fetch("/auth/check", {
			method: "POST", headers: { "Authorization": "Bearer " + token },
		});
		if (res.status === 204) {
			localStorage.setItem("ym_token", token);
			location.href = sessionStorage.getItem("ym_return") || "/ui/";
			return;
		}
		setStatus("That token was not accepted.");
	});
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
	el.appendChild(field("Run", run.id));
	el.appendChild(field("Playbook", run.playbook || ""));
	el.appendChild(field("Inventory", run.inventory || ""));
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
function field(label, value, node) {
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
	for (const task of tasks) {
		const th = document.createElement("th");
		th.textContent = task;
		htr.appendChild(th);
	}
	thead.appendChild(htr);
	table.appendChild(thead);

	const tbody = document.createElement("tbody");
	for (const host of hosts) {
		const tr = document.createElement("tr");
		const th = document.createElement("th");
		th.textContent = host;
		tr.appendChild(th);
		for (const task of tasks) {
			const info = cells[host] && cells[host][task];
			const outcome = info ? info.outcome : "none";
			const cell = document.createElement("td");
			const div = document.createElement("div");
			div.className = "cell " + outcome;
			div.title = host + " / " + task + ": " + outcome;
			if (info) {
				div.addEventListener("click", () => showDrill(Object.assign({ host, task }, info)));
			}
			cell.appendChild(div);
			tr.appendChild(cell);
		}
		tbody.appendChild(tr);
	}
	table.appendChild(tbody);
	document.getElementById("matrix-panel").hidden = false;
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
	body.appendChild(h);

	if (info.host) body.appendChild(drillField("Host", info.host));
	if (info.task) body.appendChild(drillField("Task", info.task));
	if (info.outcome) body.appendChild(drillField("Outcome", info.outcome));
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
