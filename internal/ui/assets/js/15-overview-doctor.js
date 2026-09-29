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
	// With no AI provider the box would look usable and fail on the first description. Show the
	// off state plainly instead, the same way the overview's ask panel does.
	if (aiOff()) {
		input.disabled = true;
		input.placeholder = "Advisory AI is off on this server";
		go.disabled = true;
		const note = panel.querySelector(".propose-note");
		const off = aiOffNoticeEl(
			"Advisory AI is off on this server. Turn it on to propose a run from a description, held for approval.");
		if (note) note.replaceWith(off);
		else panel.appendChild(off);
		return;
	}
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
		// Each half fails alone: one refused endpoint used to blank the whole dashboard, runs,
		// fleet, and all, when the other half had answered fine.
		const [runsRes, fleetRes] = await Promise.allSettled([
			getJSON("/runs"),
			getJSON("/fleet"),
		]);
		if (runsRes.status === "rejected" && fleetRes.status === "rejected") {
			throw runsRes.reason;
		}
		const runs = runsRes.status === "fulfilled" ? (runsRes.value.runs || []) : [];
		const hosts = fleetRes.status === "fulfilled" ? (fleetRes.value.hosts || []) : [];
		renderOverviewMetrics(runs, hosts);
		renderActivity(runs);
		renderRecentRuns(runs.slice(0, 8));
		renderFleetSnapshot(hosts);
		setStatus(runsRes.status === "rejected" ? "Could not load runs: " + runsRes.reason.message
			: fleetRes.status === "rejected" ? "Could not load fleet health: " + fleetRes.reason.message
			: "");
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

// activityState holds the chart's view controls so a re-render keeps the fetched runs and only
// re-reads the knobs. The window and chart choice persist, so an operator's dashboard opens the way
// they left it; the filter is intentionally not persisted, since a stale query would hide data on
// the next visit with no visible cause.
const activityState = {
	runs: [],
	windowH: Number(localStorage.getItem("st_activity_window")) || 12,
	chart: localStorage.getItem("st_activity_chart") === "line" ? "line" : "bars",
	filter: "",
	built: false,
};

// ACTIVITY_WINDOWS are the selectable spans, in hours, with the label shown on each pill and the
// phrase used in the note under the chart.
const ACTIVITY_WINDOWS = [
	{ h: 6, label: "6h", phrase: "6 hours" },
	{ h: 12, label: "12h", phrase: "12 hours" },
	{ h: 24, label: "24h", phrase: "24 hours" },
	{ h: 168, label: "7d", phrase: "7 days" },
	{ h: 336, label: "14d", phrase: "14 days" },
	{ h: 720, label: "30d", phrase: "30 days" },
];

// activityKey names the local calendar hour or day a time falls in. The key is built from local
// fields rather than sliced out of an ISO string, so a run and the column it belongs to agree in
// the timezone the operator is reading the page in.
function activityKey(at, hourly) {
	const pad = (n) => String(n).padStart(2, "0");
	const day = at.getFullYear() + "-" + pad(at.getMonth() + 1) + "-" + pad(at.getDate());
	return hourly ? day + "T" + pad(at.getHours()) : day;
}

// activityBucketEnd is the instant a bucket stops, the exclusive end of its drill-down window. It
// steps the local calendar field rather than adding a fixed span, so the hour a clock change
// lengthens or shortens still ends where the next one starts.
function activityBucketEnd(start, hourly) {
	return hourly
		? new Date(start.getFullYear(), start.getMonth(), start.getDate(), start.getHours() + 1)
		: new Date(start.getFullYear(), start.getMonth(), start.getDate() + 1);
}

// activityHaystack is the lowercased text a filter query is tested against: status, playbook or
// name, inventory, actor, source, labels, and any hosts, so "failed", "deploy-bot", "schedule", or
// "prod" each narrow the chart. The run list carries no per-host breakdown, so a host query only
// matches where an install populates hosts on the run; the label stays off host for that reason.
function activityHaystack(r) {
	const parts = [r.status, r.playbook, r.inventory, r.id, r.source, r.actor];
	if (typeof toolLabel === "function") parts.push(toolLabel(r));
	if (Array.isArray(r.hosts)) parts.push(...r.hosts);
	if (r.labels) for (const k in r.labels) parts.push(k, r.labels[k]);
	return parts.filter(Boolean).join(" ").toLowerCase();
}

// activityBuckets lays the chart's columns out on local calendar hours or days and counts runs into
// them. Each column carries the real Date it starts and ends at, so the window a bar drills into is
// the window the bar was counted from; recovering that window by reparsing the key would not be, as
// a key is read as UTC or local depending on its shape. With an opts.windowH the caller pins the
// span and the granularity, hourly up to a day and daily beyond, and an empty window still returns a
// drawable model so the axis holds while a filter matches nothing. Without opts it keeps the older
// auto behavior, twelve hourly columns for fresh data and fourteen daily ones for older, returning
// null when there is nothing worth drawing. opts.filter narrows the counted runs.
function activityBuckets(runs, now, opts) {
	let hourly, count;
	if (opts && opts.windowH) {
		hourly = opts.windowH <= 24;
		count = hourly ? opts.windowH : Math.round(opts.windowH / 24);
	} else {
		const times = runs.map((r) => new Date(r.created_at)).filter((d) => !isNaN(d));
		if (!times.length) return null;
		const oldest = Math.min(...times.map((d) => d.getTime()));
		hourly = now.getTime() - oldest < 36 * 3600 * 1000;
		count = hourly ? 12 : 14;
	}
	const q = opts && opts.filter ? opts.filter.trim().toLowerCase() : "";
	const days = [];
	const byKey = {};
	for (let i = count - 1; i >= 0; i--) {
		const start = hourly
			? new Date(now.getFullYear(), now.getMonth(), now.getDate(), now.getHours() - i)
			: new Date(now.getFullYear(), now.getMonth(), now.getDate() - i);
		const day = {
			key: activityKey(start, hourly),
			start,
			end: activityBucketEnd(start, hourly),
			label: hourly
				? start.toLocaleTimeString(undefined, { hour: "numeric" })
				: start.toLocaleDateString(undefined, { month: "short", day: "numeric" }),
			succeeded: 0, failed: 0, other: 0,
		};
		days.push(day);
		byKey[day.key] = day;
	}
	let counted = 0;
	for (const r of runs) {
		const at = r.created_at && new Date(r.created_at);
		if (!at || isNaN(at)) continue;
		if (q && !activityHaystack(r).includes(q)) continue;
		const day = byKey[activityKey(at, hourly)];
		if (!day) continue;
		counted++;
		if (r.status === "succeeded") day.succeeded++;
		else if (r.status === "failed") day.failed++;
		else day.other++;
	}
	if (!counted && !(opts && opts.windowH)) return null;
	const max = Math.max(1, ...days.map((d) => d.succeeded + d.failed + d.other));
	return { hourly, days, max, matched: counted };
}

// drillHref is the runs-list link a column or point opens: the runs created inside that column's
// exact span, so a bar and its drill-down agree on the window.
function drillHref(day) {
	return "/ui/runs?after=" + encodeURIComponent(day.start.toISOString()) +
		"&before=" + encodeURIComponent(day.end.toISOString());
}

// colTip is the hover text a column carries: its label, the outcome split, and the invitation to
// open those runs.
function colTip(day) {
	const total = day.succeeded + day.failed + day.other;
	if (!total) return day.label + ": no runs";
	return day.label + ": " + day.succeeded + " succeeded, " + day.failed + " failed" +
		(day.other ? ", " + day.other + " other" : "") + ". Open these runs";
}

// buildActivityControls fills the panel header with the window pills, the bars-or-line toggle, and
// the filter box, once. Every control updates activityState, persists the durable ones, and redraws
// from the runs already in hand, so switching views never refetches.
function buildActivityControls() {
	const host = document.getElementById("activity-controls");
	if (!host || activityState.built) return;
	activityState.built = true;
	host.innerHTML = "";

	const windows = document.createElement("div");
	windows.className = "seg activity-windows";
	windows.setAttribute("role", "group");
	windows.setAttribute("aria-label", "Time window");
	for (const w of ACTIVITY_WINDOWS) {
		const b = document.createElement("button");
		b.type = "button";
		b.className = "seg-btn" + (w.h === activityState.windowH ? " active" : "");
		b.textContent = w.label;
		b.dataset.window = String(w.h);
		b.addEventListener("click", () => {
			activityState.windowH = w.h;
			localStorage.setItem("st_activity_window", String(w.h));
			windows.querySelectorAll(".seg-btn").forEach((x) => x.classList.toggle("active", x === b));
			renderActivityView();
		});
		windows.appendChild(b);
	}

	const toggle = document.createElement("div");
	toggle.className = "seg activity-chart-toggle";
	toggle.setAttribute("role", "group");
	toggle.setAttribute("aria-label", "Chart style");
	for (const t of [
		{ k: "bars", label: "Bars", icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round"><line x1="6" y1="20" x2="6" y2="12"/><line x1="12" y1="20" x2="12" y2="5"/><line x1="18" y1="20" x2="18" y2="14"/></svg>' },
		{ k: "line", label: "Line", icon: '<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><polyline points="3 15 9 9 13 13 21 5"/><circle cx="9" cy="9" r="1.6" fill="currentColor"/><circle cx="13" cy="13" r="1.6" fill="currentColor"/></svg>' },
	]) {
		const b = document.createElement("button");
		b.type = "button";
		b.className = "seg-btn icon" + (t.k === activityState.chart ? " active" : "");
		b.innerHTML = t.icon;
		b.title = t.label + " chart";
		b.setAttribute("aria-label", t.label + " chart");
		b.dataset.chart = t.k;
		b.addEventListener("click", () => {
			activityState.chart = t.k;
			localStorage.setItem("st_activity_chart", t.k);
			toggle.querySelectorAll(".seg-btn").forEach((x) => x.classList.toggle("active", x === b));
			renderActivityView();
		});
		toggle.appendChild(b);
	}

	const filter = document.createElement("input");
	filter.type = "search";
	filter.className = "input activity-filter";
	filter.placeholder = "Filter runs";
	filter.setAttribute("aria-label", "Filter activity by name, status, actor, or label");
	filter.value = activityState.filter;
	let t = null;
	filter.addEventListener("input", () => {
		clearTimeout(t);
		t = setTimeout(() => {
			activityState.filter = filter.value;
			renderActivityView();
		}, 140);
	});

	host.appendChild(windows);
	host.appendChild(toggle);
	host.appendChild(filter);
}

// renderActivity stores the runs the overview fetched and draws the current view. A fresh install
// with no runs leaves the panel hidden rather than showing an empty frame; once there are runs the
// controls build once and later calls redraw without rebuilding them.
function renderActivity(runs) {
	const panel = document.getElementById("activity-panel");
	if (!panel) return;
	activityState.runs = runs || [];
	if (!activityState.runs.length) { panel.hidden = true; return; }
	buildActivityControls();
	panel.hidden = false;
	renderActivityView();
}

// loadActivityPage draws the full-page activity view: the windowed chart, an outcome breakdown that
// tracks the window, and a CSV export of whatever window and filter are showing. It shares the chart
// with the overview, so the same controls and drawing serve both.
async function loadActivityPage() {
	const status = document.getElementById("status");
	try {
		const res = await getJSON("/runs");
		applyActivityURLParams();
		renderActivity((res && res.runs) || []);
		wireActivityExport();
		wireActivityShare();
		if (status) status.hidden = true;
	} catch (e) {
		if (status) status.textContent = "Could not load activity: " + e.message;
	}
}

// updateActivitySummary fills the activity page's headline cards from the current model, so the
// totals track the chosen window and filter. It is a no-op on any page without the summary element,
// which is every page but the activity detail one.
function updateActivitySummary(model) {
	const el = document.getElementById("activity-summary");
	if (!el) return;
	let s = 0, f = 0, o = 0;
	for (const d of model.days) { s += d.succeeded; f += d.failed; o += d.other; }
	const total = s + f + o;
	const rate = total ? Math.round((s / total) * 100) + "%" : "-";
	el.innerHTML = "";
	el.appendChild(statCard(total, "Runs in window", ""));
	el.appendChild(statCard(rate, "Success rate", ""));
	el.appendChild(statCard(f, "Failed", f ? "failed" : ""));
	el.appendChild(statCard(o, "Other", ""));
	el.hidden = false;
}

// wireActivityExport hooks the Export CSV button to dump the current window's buckets.
function wireActivityExport() {
	const btn = document.getElementById("activity-export");
	if (btn && !btn.dataset.wired) { btn.dataset.wired = "1"; btn.addEventListener("click", exportActivityCSV); }
}

// exportActivityCSV writes the current window and filter's buckets as CSV: one row per column, with
// its start, label, and outcome counts, so the chart on screen leaves with the operator as data.
function exportActivityCSV() {
	const model = activityBuckets(activityState.runs, new Date(),
		{ windowH: activityState.windowH, filter: activityState.filter });
	const rows = [["bucket_start", "label", "succeeded", "failed", "other", "total"]];
	for (const d of model.days) {
		rows.push([d.start.toISOString(), d.label, d.succeeded, d.failed, d.other,
			d.succeeded + d.failed + d.other]);
	}
	const csv = rows.map((r) => r.map(csvCell).join(",")).join("\n");
	const day = new Date().toISOString().slice(0, 10);
	downloadBlob("switchtender-activity-" + day + ".csv", "text/csv", csv);
}

// renderActivityView reads the current window, chart style, and filter, models the runs, and draws
// either the stacked bars or the area-and-dots line into the chart element, then updates the note.
function renderActivityView() {
	const el = document.getElementById("activity");
	if (!el) return;
	const model = activityBuckets(activityState.runs, new Date(),
		{ windowH: activityState.windowH, filter: activityState.filter });
	updateActivitySummary(model);
	el.classList.toggle("as-line", activityState.chart === "line");
	if (activityState.chart === "line") renderActivitySvg(el, model);
	else renderActivityBars(el, model);

	const note = document.getElementById("activity-note");
	if (note) {
		const w = ACTIVITY_WINDOWS.find((x) => x.h === activityState.windowH);
		let text = "Runs per " + (model.hourly ? "hour" : "day") + ", last " + (w ? w.phrase : activityState.windowH + "h");
		const total = activityState.runs.length;
		if (activityState.filter.trim()) text += " · " + model.matched + " of " + total + " match";
		else if (total >= 200) text += " · from the latest 200 runs";
		note.textContent = text;
	}
	syncActivityURL();
}

// renderActivityBars draws the stacked outcome columns, each a link into the runs of its span, with
// the reserved outcome colors and a hover tip.
function renderActivityBars(el, model) {
	el.innerHTML = "";
	for (const day of model.days) {
		const total = day.succeeded + day.failed + day.other;
		const a = document.createElement("a");
		a.className = "activity-col";
		a.href = drillHref(day);
		a.dataset.tip = colTip(day);
		const bar = document.createElement("div");
		bar.className = "activity-bar";
		for (const part of [
			{ n: day.other, cls: "other" },
			{ n: day.failed, cls: "failed" },
			{ n: day.succeeded, cls: "succeeded" },
		]) {
			if (!part.n) continue;
			const seg = document.createElement("div");
			seg.className = "activity-seg " + part.cls;
			seg.style.height = Math.max(3, Math.round((part.n / model.max) * 64)) + "px";
			bar.appendChild(seg);
		}
		if (!total) bar.appendChild(Object.assign(document.createElement("div"), { className: "activity-seg empty" }));
		a.appendChild(bar);
		const lab = document.createElement("span");
		lab.className = "activity-label";
		lab.textContent = day.label;
		a.appendChild(lab);
		el.appendChild(a);
	}
	thinActivityLabels(el, model.days.length);
}

// isActivityPage reports whether the current page is the full activity view, so the URL-syncing
// share helpers stay inert on the overview, which carries the same chart but a different address.
function isActivityPage() {
	return !!(document.body && document.body.dataset && document.body.dataset.page === "activity");
}

// activityShareURL builds the link that reproduces the current window and filter. Absolute for the
// clipboard, path-only for the address bar.
function activityShareURL(absolute) {
	const p = new URLSearchParams();
	p.set("window", String(activityState.windowH));
	if (activityState.filter.trim()) p.set("filter", activityState.filter.trim());
	const path = "/ui/activity?" + p.toString();
	return absolute ? (location.origin + path) : path;
}

// applyActivityURLParams seeds the window and filter from a shared link, so opening
// /ui/activity?window=24&filter=failed lands on exactly that view. An unknown window is ignored so a
// hand-edited link cannot wedge the chart.
function applyActivityURLParams() {
	const q = new URLSearchParams(location.search || "");
	const w = Number(q.get("window"));
	if (ACTIVITY_WINDOWS.some((x) => x.h === w)) activityState.windowH = w;
	const f = q.get("filter");
	if (f != null) activityState.filter = f;
}

// syncActivityURL rewrites the address bar to match the view, without a history entry, so the Share
// button and a browser copy of the URL both carry the window and filter on screen. Only on the
// activity page, where the URL is meant to describe the view.
function syncActivityURL() {
	if (!isActivityPage()) return;
	try {
		history.replaceState(null, "", activityShareURL(false));
	} catch (e) {
		// A sandboxed history is not fatal; the Share button still composes the link on demand.
	}
}

// wireActivityShare copies a link to the current view to the clipboard, falling back to putting it in
// the address bar when the clipboard is blocked, so there is always a link to hand to someone.
function wireActivityShare() {
	const btn = document.getElementById("activity-share");
	if (!btn || btn.dataset.wired) return;
	btn.dataset.wired = "1";
	const label = btn.querySelector(".share-label");
	btn.addEventListener("click", async () => {
		try {
			await navigator.clipboard.writeText(activityShareURL(true));
			if (label) {
				const prev = label.textContent;
				label.textContent = "Link copied";
				setTimeout(() => { label.textContent = prev; }, 1600);
			}
		} catch (e) {
			syncActivityURL();
			if (label) {
				const prev = label.textContent;
				label.textContent = "Link in address bar";
				setTimeout(() => { label.textContent = prev; }, 1600);
			}
		}
	});
}

// SVGNS is the namespace the line chart's elements are created in.
const SVGNS = "http://www.w3.org/2000/svg";

// svgEl makes a namespaced SVG element with attributes, the small helper the line chart leans on.
function svgEl(name, attrs) {
	const e = document.createElementNS(SVGNS, name);
	for (const k in attrs) e.setAttribute(k, attrs[k]);
	return e;
}

// renderActivitySvg draws the continuous view: a soft gradient area under the total line and a
// failed line over it, both with dots, on a faint baseline grid. Each dot is a link into its span's
// runs, so the line drills down the same way the bars do. The viewBox matches the element's measured
// pixel size, so x and y scale together and the dots stay round.
function renderActivitySvg(el, model) {
	el.innerHTML = "";
	const W = Math.max(320, Math.round(el.clientWidth) || 1000), H = 190, padX = 14, padTop = 16, padBot = 12;
	const days = model.days;
	const n = days.length;
	const innerW = W - padX * 2;
	const x = (i) => n <= 1 ? W / 2 : padX + (innerW * i) / (n - 1);
	const y = (v) => padTop + (H - padTop - padBot) * (1 - v / model.max);

	const svg = svgEl("svg", { viewBox: "0 0 " + W + " " + H, class: "activity-svg", role: "img", "aria-label": "Runs over time" });

	const defs = svgEl("defs", {});
	const grad = svgEl("linearGradient", { id: "actArea", x1: "0", y1: "0", x2: "0", y2: "1" });
	grad.appendChild(svgEl("stop", { offset: "0", "stop-color": "var(--ok)", "stop-opacity": "0.34" }));
	grad.appendChild(svgEl("stop", { offset: "1", "stop-color": "var(--ok)", "stop-opacity": "0" }));
	defs.appendChild(grad);
	svg.appendChild(defs);

	for (let g = 0; g <= 3; g++) {
		const gy = padTop + (H - padTop - padBot) * (g / 3);
		svg.appendChild(svgEl("line", { x1: padX, y1: gy, x2: W - padX, y2: gy, class: "act-grid" }));
	}

	const totalPts = days.map((d, i) => [x(i), y(d.succeeded + d.failed + d.other)]);
	const failPts = days.map((d, i) => [x(i), y(d.failed)]);
	const line = (pts) => pts.map((p, i) => (i ? "L" : "M") + p[0].toFixed(1) + " " + p[1].toFixed(1)).join(" ");

	const areaD = line(totalPts) + " L " + x(n - 1).toFixed(1) + " " + y(0).toFixed(1) + " L " + x(0).toFixed(1) + " " + y(0).toFixed(1) + " Z";
	svg.appendChild(svgEl("path", { d: areaD, fill: "url(#actArea)", stroke: "none" }));
	svg.appendChild(svgEl("path", { d: line(totalPts), class: "act-line ok" }));
	if (days.some((d) => d.failed)) svg.appendChild(svgEl("path", { d: line(failPts), class: "act-line fail" }));

	days.forEach((d, i) => {
		const total = d.succeeded + d.failed + d.other;
		const gx = x(i), gy = y(total);
		const a = svgEl("a", { href: drillHref(d), class: "act-dot-hit" });
		a.dataset.tip = colTip(d);
		a.appendChild(svgEl("circle", { cx: gx, cy: gy, r: 14, fill: "transparent" }));
		a.appendChild(svgEl("circle", { cx: gx, cy: gy, r: total ? 3.5 : 2.2, class: "act-dot" + (total ? "" : " zero") }));
		if (d.failed) a.appendChild(svgEl("circle", { cx: gx, cy: y(d.failed), r: 3, class: "act-dot fail" }));
		svg.appendChild(a);
	});

	el.appendChild(svg);

	const axis = document.createElement("div");
	axis.className = "activity-axis";
	days.forEach((d) => {
		const s = document.createElement("span");
		s.textContent = d.label;
		axis.appendChild(s);
	});
	el.appendChild(axis);
	thinActivityLabels(axis, n);
}

// activityRedrawOnResize keeps the measured-width line chart correct when the window changes. Bars
// reflow on their own, so only the line view needs the redraw, and only where it is drawn.
let activityResizeTimer = null;
if (typeof window !== "undefined" && window.addEventListener) {
	window.addEventListener("resize", () => {
		if (activityState.chart !== "line" || !activityState.runs.length) return;
		if (!document.getElementById("activity")) return;
		clearTimeout(activityResizeTimer);
		activityResizeTimer = setTimeout(renderActivityView, 160);
	});
}

// thinActivityLabels hides all but roughly eight evenly spaced axis labels, so a 24-column window
// shows a readable handful rather than an overlapping smear. Works on either the bar columns or the
// line chart's axis row, whichever child list it is handed.
function thinActivityLabels(container, n) {
	const labels = container.classList && container.classList.contains("activity-axis")
		? container.children
		: container.querySelectorAll(".activity-label");
	const step = Math.ceil(n / 8);
	if (step <= 1) return;
	Array.prototype.forEach.call(labels, (lab, i) => {
		if (i % step !== 0 && i !== n - 1) lab.classList.add("axis-thin");
	});
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
			? "Recent outcomes alternate between pass and fail. Worth a look."
			: "Recent outcomes are stable";
		row.appendChild(chip);
		const go = document.createElement("span");
		go.className = "ov-row-go";
		go.innerHTML = svgIcon('<polyline points="9 18 15 12 9 6"/>');
		row.appendChild(go);
		el.appendChild(row);
	}
}

// loadDoctor runs the reference checks and lists every finding with a fix link.
async function loadDoctor() {
	try {
		const data = await getJSON("/doctor");
		const findings = data.findings || [];
		const sum = document.getElementById("doctor-summary");
		sum.innerHTML = "";
		sum.appendChild(statCard(String(data.checked_templates), "Templates checked", ""));
		sum.appendChild(statCard(String(data.checked_schedules), "Schedules checked", ""));
		sum.appendChild(statCard(String(data.checked_credentials), "Credentials checked", ""));
		sum.appendChild(statCard(String(findings.length), "Findings", findings.length ? "failed" : "ok"));
		sum.hidden = false;
		if (!findings.length) {
			showEmpty("Everything checks out. Every reference resolves and every schedule can fire.");
			return;
		}
		const tbody = document.getElementById("doctor");
		for (const f of findings) {
			const tr = document.createElement("tr");
			const sev = document.createElement("td");
			const chip = document.createElement("span");
			chip.className = "chip " + (f.severity === "broken" ? "failed" : "flaky");
			chip.textContent = f.severity;
			sev.appendChild(chip);
			tr.appendChild(sev);
			const obj = td("");
			const name = document.createElement("strong");
			name.textContent = f.object_name || f.object_id;
			obj.appendChild(name);
			obj.appendChild(document.createTextNode(" "));
			const kind = document.createElement("span");
			kind.className = "run-kind";
			kind.textContent = f.object_type;
			obj.appendChild(kind);
			obj.title = f.object_id;
			obj.dataset.export = (obj.textContent || "").trim() + " (" + (f.object_id || "") + ")";
			tr.appendChild(obj);
			tr.appendChild(td(f.problem));
			const fix = td("");
			const link = document.createElement("a");
			link.className = "button";
			link.href = f.fix_path;
			link.textContent = "Open";
			link.dataset.tip = "Open the page where this is repaired";
			fix.appendChild(link);
			tr.appendChild(fix);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Doctor failed: " + e.message);
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

