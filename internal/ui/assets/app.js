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
		loadRuns();
	} else if (page === "detail") {
		loadDetail(document.body.dataset.runId);
	}
});

// getJSON fetches and decodes a JSON endpoint.
async function getJSON(url) {
	const res = await fetch(url);
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

// loadRuns populates the run history table.
async function loadRuns() {
	try {
		const data = await getJSON("/runs");
		const runs = data.runs || [];
		if (runs.length === 0) { setStatus("No runs yet."); return; }
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

// detailState holds the current run and its accumulated events for incremental rendering.
let detailState = null;

// loadDetail loads one run, renders it, and opens a live stream when the run is still active.
async function loadDetail(runId) {
	document.getElementById("full-log").href = "/runs/" + runId + "/logs";
	try {
		const [run, ev] = await Promise.all([
			getJSON("/runs/" + runId),
			getJSON("/runs/" + runId + "/events"),
		]);
		detailState = { runId, run, events: ev.events || [] };
		renderDetail();
		setStatus("");
		if (!isTerminal(run.status)) {
			openStream(runId);
		}
	} catch (e) {
		setStatus("Failed to load run: " + e.message);
	}
}

// isTerminal reports whether a run status is final.
function isTerminal(status) {
	return status === "succeeded" || status === "failed" || status === "canceled";
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

	const source = new EventSource("/runs/" + runId + "/stream");
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
	if (run.exit_code !== undefined && run.exit_code !== null) {
		el.appendChild(field("Exit", String(run.exit_code)));
	}
	el.appendChild(field("Duration", fmtDuration(run.started_at, run.ended_at)));
	el.hidden = false;
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
			cells[e.host][e.task] = outcome;
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
			const outcome = (cells[host] && cells[host][task]) || "none";
			const cell = document.createElement("td");
			const div = document.createElement("div");
			div.className = "cell " + outcome;
			div.title = host + " / " + task + ": " + outcome;
			if (outcome !== "none") {
				div.addEventListener("click", () => showDrill({ host, task, outcome }));
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
		bar.style.left = (((start - t0) / span) * 100) + "%";
		bar.style.width = Math.max((dur / span) * 100, 1) + "%";
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
		const o = cells[host] && cells[host][task];
		if (!o) continue;
		const r = OUTCOME_RANK[o] === undefined ? 0 : OUTCOME_RANK[o];
		if (r > rank) { rank = r; worst = o; }
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
	body.appendChild(drillField("Task", info.task));
	if (info.outcome) body.appendChild(drillField("Outcome", info.outcome));
	if (info.duration) body.appendChild(drillField("Duration", info.duration));
	document.getElementById("drill").hidden = false;
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
