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
		renderActivity(runs);
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

// activityBuckets lays the activity chart's columns out and counts runs into them. Each column
// carries the real Date it starts and ends at, so the window a bar drills into is the same window
// the bar was counted from. Recovering that window by parsing the column key back would not be:
// the columns are cut on local calendar hours and days, and a parsed key is read as either UTC or
// local depending on its shape, which puts the drill-down out by the viewer's UTC offset.
// Returns null when there is nothing worth drawing.
function activityBuckets(runs, now) {
	const times = runs.map((r) => new Date(r.created_at)).filter((d) => !isNaN(d));
	if (!times.length) return null;
	// A fresh install has every run inside an hour, where fourteen day columns would be thirteen
	// empty ones. Pick the bucket that actually spans the data.
	const oldest = Math.min(...times.map((d) => d.getTime()));
	const hourly = now.getTime() - oldest < 36 * 3600 * 1000;
	const days = [];
	const byKey = {};
	const count = hourly ? 12 : 14;
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
		const day = byKey[activityKey(at, hourly)];
		if (!day) continue;
		counted++;
		if (r.status === "succeeded") day.succeeded++;
		else if (r.status === "failed") day.failed++;
		else day.other++;
	}
	if (!counted) return null;
	const max = Math.max(1, ...days.map((d) => d.succeeded + d.failed + d.other));
	return { hourly, days, max };
}

// renderActivity draws a stacked daily bar chart of run outcomes over the last two weeks, from
// the runs the overview already fetched. Bars carry tips; statuses use the reserved colors.
function renderActivity(runs) {
	const panel = document.getElementById("activity-panel");
	const el = document.getElementById("activity");
	if (!panel || !el || !runs.length) return;
	const model = activityBuckets(runs, new Date());
	if (!model) return;
	el.innerHTML = "";
	for (const day of model.days) {
		const total = day.succeeded + day.failed + day.other;
		const col = document.createElement("a");
		col.className = "activity-col";
		col.href = "/ui/runs?after=" + encodeURIComponent(day.start.toISOString()) +
			"&before=" + encodeURIComponent(day.end.toISOString());
		col.dataset.tip = day.label + ": " + day.succeeded + " succeeded, " + day.failed + " failed" +
			(day.other ? ", " + day.other + " other" : "") + ". Open these runs";
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
		col.appendChild(bar);
		const lab = document.createElement("span");
		lab.className = "activity-label";
		lab.textContent = model.hourly ? day.label : (day.label.split(" ")[1] || day.label);
		col.appendChild(lab);
		el.appendChild(col);
	}
	const note = document.getElementById("activity-note");
	if (note) {
		note.textContent = model.hourly ? "Runs per hour, last 12 hours" : "Runs per day, last 14 days";
		if (runs.length >= 200) note.textContent += ", from the latest 200";
	}
	panel.hidden = false;
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

