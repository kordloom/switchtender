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
			if (e.task && taskStart[e.task] === undefined && t) taskStart[e.task] = t;
		// A result with no host or no task has no cell in a host by task grid, so it stays out of
		// the model entirely. Its time still counts toward the run's span.
		} else if (e.type && e.type.indexOf("runner_") === 0 && e.host && e.task) {
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
			if (e.stats) for (const h of Object.keys(e.stats)) if (h) hosts.add(h);
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
// Structural is set only when a host or task was really inserted, so a stream of events the model
// cannot place, a result with no task name above all, never forces a rebuild per event.
function applyEvent(model, e) {
	const t = e.time ? new Date(e.time).getTime() : null;
	if (t && (model.lastTime === null || t > model.lastTime)) model.lastTime = t;
	let structural = false;
	if (e.type === "task_start") {
		if (addTask(model.tasks, model.taskSeen, e.task)) structural = true;
		if (e.task && model.taskStart[e.task] === undefined && t) model.taskStart[e.task] = t;
	} else if (e.type && e.type.indexOf("runner_") === 0 && e.host && e.task) {
		const outcome = e.type === "runner_ok"
			? (e.changed ? "changed" : "ok")
			: e.type.slice("runner_".length);
		if (addHost(model, e.host)) structural = true;
		if (addTask(model.tasks, model.taskSeen, e.task)) structural = true;
		if (!model.cells[e.host]) model.cells[e.host] = {};
		model.cells[e.host][e.task] = {
			outcome, message: e.message, stdout: e.stdout, stderr: e.stderr,
			rc: e.rc, diff: e.diff, truncated: e.truncated,
		};
	} else if (e.type === "stats") {
		model.statsTime = t;
		if (e.stats) {
			for (const h of Object.keys(e.stats)) {
				if (addHost(model, h)) structural = true;
			}
		}
	}
	model.end = model.statsTime || model.lastTime;
	return { structural, host: e.host, task: e.task };
}

// addTask records a task name once, preserving first seen order. It reports whether the name was
// new, which is what makes the grid taller.
function addTask(tasks, seen, task) {
	if (!task || seen.has(task)) return false;
	seen.add(task);
	tasks.push(task);
	return true;
}

// addHost records a host once and keeps the row order sorted. It reports whether the host was new,
// which is what makes the grid wider.
function addHost(model, host) {
	if (!host || model.hostSet.has(host)) return false;
	model.hostSet.add(host);
	model.hosts = Array.from(model.hostSet).sort();
	return true;
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
	// Small runs get roomy cells; the dense grid returns past a few hundred cells so a real fleet
	// still fits a screen. A six-host demo run used to draw postage stamps in half-empty panels.
	const matrixEl = document.getElementById("matrix");
	if (matrixEl) matrixEl.classList.toggle("dense", cellCount > 240);
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
	// firstStop is the cell that holds the grid's tab stop until the arrows move it.
	let firstStop = null;
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
			// The grid is the result of the run, and it used to be readable only by eye and reachable
			// only by mouse: a bare div whose outcome lived in a background color. Each cell now names
			// itself and its outcome, announces that it opens something, and takes focus. A cell with
			// no result is not a control, so it says what it is and stays out of the tab order.
			div.setAttribute("role", outcome === "none" ? "presentation" : "button");
			div.setAttribute("aria-label", outcome === "none"
				? host + ", " + task + ": not run"
				: host + ", " + task + ": " + outcome + ". Opens the detail.");
			// One tab stop for the whole grid, the first cell that has a result: the arrows move it.
			// A thousand-cell matrix must not put a thousand stops in the tab order.
			div.tabIndex = outcome !== "none" && !firstStop ? 0 : -1;
			if (outcome !== "none" && !firstStop) firstStop = div;
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
		openCellDrill(div);
	});
	// A grid is walked with the arrow keys and opened with Enter or Space, which is how a keyboard
	// reaches a cell at all: the mouse handlers above were the only way in.
	table.addEventListener("keydown", (e) => {
		const div = e.target.closest && e.target.closest(".cell");
		if (!div) return;
		if (e.key === "Enter" || e.key === " " || e.key === "Spacebar") {
			e.preventDefault();
			openCellDrill(div);
			return;
		}
		const step = MATRIX_STEPS[e.key];
		if (!step) return;
		e.preventDefault();
		moveCellFocus(table, div, step[0], step[1]);
	});
}

// MATRIX_STEPS maps an arrow key to the row and column it moves the grid focus by.
const MATRIX_STEPS = {
	ArrowRight: [0, 1], ArrowLeft: [0, -1], ArrowDown: [1, 0], ArrowUp: [-1, 0],
};

// openCellDrill opens the detail for one matrix cell, whether a click or a key asked for it. A cell
// with no result has nothing to show and opens nothing.
function openCellDrill(div) {
	const model = detailState && detailState.model;
	const info = model && model.cells[div.dataset.host] && model.cells[div.dataset.host][div.dataset.task];
	if (info) showDrill(Object.assign({ host: div.dataset.host, task: div.dataset.task }, info));
}

// moveCellFocus moves the grid's single tab stop by one row or column and focuses it, stopping at the
// edges rather than wrapping, so arrowing along a row does not silently jump to another host.
function moveCellFocus(table, from, dRow, dCol) {
	const ri = parseInt(from.dataset.ri, 10) + dRow;
	const ci = parseInt(from.dataset.ci, 10) + dCol;
	const next = table.querySelector('.cell[data-ri="' + ri + '"][data-ci="' + ci + '"]');
	if (!next) return;
	from.tabIndex = -1;
	next.tabIndex = 0;
	next.focus();
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
	// The name has to follow the color, or a screen reader keeps reading the outcome the cell had
	// when the page loaded while the grid shows the one it has now.
	div.setAttribute("role", outcome === "none" ? "presentation" : "button");
	div.setAttribute("aria-label", outcome === "none"
		? host + ", " + task + ": not run"
		: host + ", " + task + ": " + outcome + ". Opens the detail.");
	if (outcome !== "none" && div.tabIndex !== 0) div.tabIndex = -1;
	renderMatrixSummary(detailState.model.hosts.length, detailState.model.tasks.length, detailState.counts);
	return true;
}

// updateTimelineBar recolors one task's timeline bar to its current worst outcome after a live
// event, without rebuilding the timeline.
function updateTimelineBar(task) {
	const bar = detailState.tlBars && detailState.tlBars.get(task);
	if (!bar) return;
	const m = detailState.model;
	const outcome = worstOutcome(task, m.cells, m.hosts);
	bar.className = "tl-bar " + outcome;
	// The label carries the outcome, so recoloring without relabeling leaves a bar that is read as
	// one thing and shown as another. The duration is not recomputed here because the bar's width and
	// the row's duration column are not either; only the outcome changed.
	const label = bar.getAttribute("aria-label") || "";
	const tail = label.indexOf(", ");
	bar.setAttribute("aria-label", tail === -1
		? task + ": " + outcome
		: task + ": " + outcome + label.slice(tail));
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
		// The grid's shape changed, so it is rebuilt rather than patched. The redraw is coalesced
		// because hosts and tasks arrive in bursts and each rebuild costs hosts times tasks.
		scheduleGrid();
	} else if (!detailState.overCap && change.host && change.task) {
		if (!updateCell(change.host, change.task)) {
			scheduleGrid();
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
		// Each bar is one task's slice of the run and opens its detail, so it says what it is, how it
		// ended, and how long it took, and it takes focus. There are as many bars as tasks, an order of
		// magnitude fewer than cells, so each one being its own tab stop is a short walk rather than a
		// trap.
		bar.setAttribute("role", "button");
		bar.tabIndex = 0;
		bar.setAttribute("aria-label", task + ": " + outcome + ", " + fmtMs(dur) + ". Opens the detail.");
		detailState.tlBars.set(task, bar);
		const leftPct = Math.min(Math.max(((start - t0) / span) * 100, 0), 99);
		const widthPct = Math.min(Math.max((dur / span) * 100, 1), 100 - leftPct);
		bar.style.left = leftPct + "%";
		bar.style.width = widthPct + "%";
		const open = () => showDrill({ task, outcome, duration: fmtMs(dur) });
		bar.addEventListener("click", open);
		bar.addEventListener("keydown", (e) => {
			if (e.key !== "Enter" && e.key !== " " && e.key !== "Spacebar") return;
			e.preventDefault();
			open();
		});
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
	const body = ensureDrill();
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
	// A failure that carried nothing but its return code explains itself in the run log, so the
	// pane says so and takes the reader there instead of ending the story at a bare number.
	if (info.outcome === "failed" && !info.message && !info.stdout && !info.stderr) {
		const note = document.createElement("div");
		note.className = "drill-note";
		note.textContent = "This task reported only its return code. The full run log usually carries the reason. ";
		const jump = document.createElement("button");
		jump.type = "button";
		jump.className = "linkish";
		jump.textContent = "Open the log";
		jump.addEventListener("click", () => {
			closeDrill();
			const panel = document.getElementById("log-panel");
			if (panel) {
				panel.hidden = false;
				if (panel.scrollIntoView) panel.scrollIntoView();
			}
		});
		note.appendChild(jump);
		body.appendChild(note);
	}

	const panel = document.getElementById("drill");
	panel.hidden = false;
	document.getElementById("drill-backdrop").hidden = false;
	// The element that opened the panel is remembered so closing can hand focus back to it. Without
	// that, closing left focus nowhere and the next Tab started over at the top of the document.
	drillOpener = document.activeElement;
	// Focus moves into the panel, since that is where everything the reader can now act on lives. The
	// close button is the safe landing: it is present in every drill, and it is the way out.
	const first = panel.querySelector(".drill-close");
	if (first && first.focus) first.focus();
}

// drillOpener is what had focus when the inspect panel opened, so closing can return it.
let drillOpener = null;

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

// drillField builds a labeled value in the drill panel, with an optional inline copy control.
function drillField(label, value, copy) {
	const f = document.createElement("div");
	f.className = "field";
	const l = document.createElement("div");
	l.className = "label";
	l.textContent = label;
	const v = document.createElement("div");
	v.className = "value";
	v.textContent = value;
	if (copy) v.appendChild(copyButton(String(value), "Copy " + label.toLowerCase()));
	f.appendChild(l);
	f.appendChild(v);
	return f;
}

// ensureDrill returns the shared inspect panel's body, creating the panel and its backdrop on
// pages that do not declare them, and wiring the exits exactly once either way. The wiring used to
// live only in the creation branch, so the run detail page, which declares the panel in its
// template, opened a drawer with no backdrop and no Escape: the exact dead end a failed run's
// RETURN CODE pane became on a phone. Every drill now closes the same three ways on every page:
// the close button, a click on the backdrop, and Escape.
function ensureDrill() {
	let backdrop = document.getElementById("drill-backdrop");
	if (!backdrop) {
		backdrop = document.createElement("div");
		backdrop.id = "drill-backdrop";
		backdrop.className = "drill-backdrop";
		backdrop.hidden = true;
		document.body.appendChild(backdrop);
	}
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
		const body = document.createElement("div");
		body.id = "drill-body";
		drill.appendChild(close);
		drill.appendChild(body);
		document.body.appendChild(drill);
	}
	// The panel sits over the page, so it says what it is, whether it came from the template or was
	// built here: a screen reader is otherwise not told the page changed under the reader, and a
	// keyboard user has no idea the controls they want are now a page of tab stops away.
	drill.setAttribute("role", "dialog");
	drill.setAttribute("aria-modal", "true");
	if (!drill.getAttribute("aria-label")) drill.setAttribute("aria-label", "Task detail");
	if (!drill.dataset.exitsWired) {
		drill.dataset.exitsWired = "true";
		backdrop.addEventListener("click", closeDrill);
		const close = document.getElementById("drill-close");
		if (close) close.addEventListener("click", closeDrill);
		document.addEventListener("keydown", (e) => {
			if (e.key === "Escape" && !drill.hidden) closeDrill();
		});
	}
	return document.getElementById("drill-body");
}

// closeDrill hides the inspect panel and its backdrop, returning focus to whatever opened it.
function closeDrill() {
	const drill = document.getElementById("drill");
	const backdrop = document.getElementById("drill-backdrop");
	if (drill) drill.hidden = true;
	if (backdrop) backdrop.hidden = true;
	if (drillOpener && drillOpener.focus) drillOpener.focus();
	drillOpener = null;
}

// inspectDrawer opens the shared panel with a title and a list of fields. A field marked block
// renders as a monospace block, for multi line values such as inventory content. Empty fields are
// skipped so the panel stays terse.
function inspectDrawer(title, fields, actions) {
	const body = ensureDrill();
	body.innerHTML = "";
	const h = document.createElement("h3");
	h.textContent = title;
	body.appendChild(h);
	for (const f of fields) {
		if (f.value === undefined || f.value === null || f.value === "") continue;
		body.appendChild(f.block ? drillBlock(f.label, f.value) : drillField(f.label, f.value, f.copy));
	}
	if (actions && actions.length) {
		const row = document.createElement("div");
		row.className = "drill-actions";
		for (const a of actions) {
			if (a.href) {
				const link = document.createElement("a");
				link.className = "button";
				link.href = a.href;
				if (a.external) { link.target = "_blank"; link.rel = "noopener"; }
				link.textContent = a.label;
				if (a.tip) link.dataset.tip = a.tip;
				row.appendChild(link);
				continue;
			}
			const btn = document.createElement("button");
			btn.type = "button";
			btn.className = "button" + (a.primary ? " primary" : "");
			btn.textContent = a.label;
			if (a.tip) btn.dataset.tip = a.tip;
			if (a.mutates) btn.dataset.mutates = "true";
			btn.addEventListener("click", a.onClick);
			row.appendChild(btn);
		}
		body.appendChild(row);
	}
	document.getElementById("drill").hidden = false;
	document.getElementById("drill-backdrop").hidden = false;
}

// inspectable marks a table row as clickable and opens the inspect drawer for it on click.
function inspectable(tr, title, fields, actions) {
	tr.classList.add("row-inspect");
	tr.tabIndex = 0;
	tr.setAttribute("role", "button");
	const open = (e) => {
		// A click on a row action such as Edit or Delete is not a request to inspect.
		if (e && e.target && e.target.closest && e.target.closest("button, a")) return;
		inspectDrawer(title, fields, actions);
	};
	tr.addEventListener("click", open);
	tr.addEventListener("keydown", (e) => {
		if ((e.key === "Enter" || e.key === " ") && e.target === tr) { e.preventDefault(); open(); }
	});
}
