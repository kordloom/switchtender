// renderHostSummary turns the host's run history into headline metrics, so its condition reads
// before the table does.
function renderHostSummary(host, runs) {
	const el = document.getElementById("host-summary");
	if (!el) return;
	let failed = 0;
	let changed = 0;
	let busy = 0;
	for (const r of runs) {
		if (r.outcome === "failed" || r.outcome === "unreachable") failed++;
		if (r.changed) changed += r.changed;
		busy += r.duration_seconds || 0;
	}
	const rate = runs.length ? Math.round(((runs.length - failed) / runs.length) * 100) + "%" : "-";
	el.innerHTML = "";
	el.appendChild(statCard(String(runs.length), "Runs recorded", ""));
	el.appendChild(statCard(rate, "Success rate", failed ? "" : "ok"));
	el.appendChild(statCard(String(failed), "Failures", failed ? "failed" : ""));
	el.appendChild(statCard(String(changed), "Tasks changed", changed ? "changed" : ""));
	el.appendChild(statCard(fmtSeconds(busy), "Total busy time", ""));
	el.hidden = false;
}

// fmtInterval renders a sync interval in the largest whole unit that fits.
function fmtInterval(seconds) {
	if (seconds % 3600 === 0) {
		const h = seconds / 3600;
		return h === 1 ? "hour" : h + " hours";
	}
	if (seconds % 60 === 0) {
		const m = seconds / 60;
		return m === 1 ? "minute" : m + " minutes";
	}
	return seconds + " seconds";
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
			sparkCell.appendChild(sparkline(h.recent || [], h.recent_runs || []));
			// The drawing exports as its underlying outcomes, oldest first, not as an empty cell.
			sparkCell.dataset.export = (h.recent || []).join(" ");
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
		showListControls();
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
		showListControls();
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
	mountHostActions(host);
	loadHostFacts(host);
	try {
		const data = await getJSON("/hosts/" + encodeURIComponent(host) + "/runs");
		const runs = data.runs || [];
		renderHostSummary(host, runs);
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
			tr.appendChild(td(String(r.unreachable)));
			tr.appendChild(td(r.duration_seconds ? r.duration_seconds.toFixed(1) + "s" : "0s"));
			tr.appendChild(tdTime(r.ran_at));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
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
			spark.dataset.export = (t.recent || []).join(" ");
			tr.appendChild(spark);
			tr.appendChild(td(String(t.runs)));
			tr.appendChild(td(fmtSeconds(t.avg_seconds)));
			tr.appendChild(td(fmtSeconds(t.last_seconds)));
			tr.appendChild(tdTime(t.last_run));
			const taskActions = document.createElement("td");
			const runsLink = document.createElement("a");
			runsLink.className = "button";
			runsLink.href = "/ui/runs?q=" + encodeURIComponent(t.task);
			runsLink.textContent = "Runs";
			runsLink.dataset.tip = "Click to search runs mentioning this task";
			taskActions.appendChild(runsLink);
			tr.appendChild(taskActions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
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
			// Hovering the expression reads it back in words, field by field, so the syntax explains
			// itself wherever it appears rather than only in the neighboring column.
			const cron = td(s.cron, "mono");
			cron.dataset.cron = s.cron || "";
			tr.appendChild(cron);
			// The zone the cron is read in rides alongside the cadence: two identical expressions in
			// different zones fire at different times, and without it the rows are indistinguishable.
			const cadenceText = s.timezone ? describeCron(s.cron) + " (" + s.timezone + ")"
				: describeCron(s.cron);
			const cadence = td(cadenceText);
			cadence.dataset.cron = s.cron || "";
			tr.appendChild(cadence);
			const target = document.createElement("td");
			if (s.template_id) {
				const tpl = document.createElement("a");
				// The link lands on the named template rather than the whole list: the list honors
				// ?q=, so arriving unfiltered made the reader search again for a name already known.
				const tplName = tplByID[s.template_id];
				tpl.href = tplName ? "/ui/templates?q=" + encodeURIComponent(tplName) : "/ui/templates";
				tpl.textContent = tplName || "template";
				tpl.dataset.tip = tplName ? "Open " + tplName : "Open templates";
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
	del.dataset.mutates = "true";
	del.dataset.tip = "Click to delete this permanently";
			del.textContent = "Delete";
			del.addEventListener("click", async (e) => {
				e.preventDefault();
				if (!window.confirm("Delete schedule " + (s.name || s.id) + "?")) return;
				try {
					await authedDelete("/schedules/" + s.id);
					removeRow(tr, "No schedules yet. Add one to fire a template on a cadence.");
				} catch (err) {
					setStatus("Delete failed: " + err.message);
				}
			});
			// Changing or deleting a schedule is admin work, so the controls only draw for a
			// session that can use them.
			if (roleAtLeast("admin")) {
				actions.appendChild(editButton(() => openScheduleEdit(s), "Click to edit this schedule's cadence and target"));
				actions.appendChild(document.createTextNode(" "));
				actions.appendChild(del);
			}
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
		wireCronTips();
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

