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

// runsFilterParams reads the status, tool, and order dropdowns plus any date window from the URL
// into query parameters, so the server filters the whole run history, not just the loaded page.
function runsFilterParams() {
	let params = "";
	for (const id of ["runs-status", "runs-tool", "runs-order"]) {
		const el = document.getElementById(id);
		if (el && el.value) params += "&" + id.replace("runs-", "") + "=" + encodeURIComponent(el.value);
	}
	const url = new URLSearchParams(location.search);
	for (const key of ["after", "before"]) {
		const v = url.get(key);
		if (v) params += "&" + key + "=" + encodeURIComponent(v);
	}
	return params;
}

// mountRunsWindowChip shows which day the runs list is scoped to, with one click to clear it.
function mountRunsWindowChip() {
	const url = new URLSearchParams(location.search);
	const after = url.get("after");
	if (!after) return;
	const bar = document.querySelector(".runs-toolbar");
	if (!bar || bar.querySelector(".window-chip")) return;
	const chip = document.createElement("span");
	chip.className = "window-chip";
	const when = new Date(after);
	chip.appendChild(document.createTextNode(
		isNaN(when) ? "Filtered window" : when.toLocaleDateString(undefined, { month: "short", day: "numeric", year: "numeric" })));
	const clear = document.createElement("a");
	clear.href = "/ui/runs";
	clear.textContent = "\u00d7";
	clear.setAttribute("aria-label", "Clear the date filter");
	clear.dataset.tip = "Show every run again";
	chip.appendChild(clear);
	bar.appendChild(chip);
}

// wireRunsFilters reloads the table when a filter or order dropdown changes.
function wireRunsFilters() {
	for (const id of ["runs-status", "runs-tool", "runs-order"]) {
		const el = document.getElementById(id);
		if (el) el.addEventListener("change", loadRuns);
	}
}

// wireRunsAutoRefresh keeps the table honest while runs are moving: a run shown running used to
// stay running on screen forever, because nothing ever re-read the list. The poll only fires when
// the tab is visible, only while a non-terminal chip is on screen, and never after Load more has
// grown the table past one page, so it cannot throw away rows someone paged in.
function wireRunsAutoRefresh() {
	window.setInterval(() => {
		if (document.hidden) return;
		const tbody = document.getElementById("runs");
		if (!tbody) return;
		if (tbody.querySelectorAll("tr").length > runsPageSize()) return;
		if (!tbody.querySelector(".badge.running, .badge.pending, .badge.pending_approval")) return;
		loadRuns();
	}, 15000);
}

// wireRunsSearch reloads the runs table from the server as the search box changes, debounced so a
// burst of keystrokes issues one request. The server searches every run, not just the loaded page.
function wireRunsSearch() {
	const el = document.getElementById("runs-search");
	if (!el) return;
	// A link that lands here carrying ?q= promised a filtered view: the held-runs count, a label
	// chip, a user's fired-runs link. Seeding the box keeps that promise; dropping it showed the
	// unfiltered list and made every such link a small lie.
	const preset = new URLSearchParams(location.search).get("q");
	if (preset && !el.value) el.value = preset;
	let timer;
	el.addEventListener("input", () => {
		clearTimeout(timer);
		timer = setTimeout(loadRuns, 250);
	});
}

// runsFiltered reports whether a search, a status or tool filter, or a date window narrows the
// table. An empty result then speaks about the query, not the instance, so the controls that
// created it have to stay on screen to be revised.
function runsFiltered() {
	if (runsQuery()) return true;
	for (const id of ["runs-status", "runs-tool"]) {
		const el = document.getElementById(id);
		if (el && el.value) return true;
	}
	const url = new URLSearchParams(location.search);
	return !!(url.get("after") || url.get("before"));
}

// runsLoadGen counts run-table loads so a slow response from an earlier search or page size cannot
// overwrite the table after a newer load has already rendered.
let runsLoadGen = 0;

// runsPageURL builds the request for one page of run history: the page size and offset, the search
// box, and every active filter. Every page comes from this one function, so a later page is drawn
// from the same filtered result set its offset counts within. Building the Load more request
// separately dropped the filters from it, which appended unrelated runs to a filtered table and
// counted the offset against a different set of rows.
function runsPageURL(offset) {
	return "/runs?limit=" + runsPageSize() + "&offset=" + offset +
		"&q=" + encodeURIComponent(runsQuery()) + runsFilterParams();
}

// loadRuns populates the run history table.
async function loadRuns() {
	const tbody = document.getElementById("runs");
	const table = document.querySelector("table.runs");
	const sizeEl = document.getElementById("runs-pagesize");
	if (sizeEl) sizeEl.onchange = () => loadRuns();
	const gen = ++runsLoadGen;
	setStatus("");
	showSkeletonRows(tbody, 6, 9);
	table.hidden = false;
	try {
		const data = await getJSON(runsPageURL(0));
		if (gen !== runsLoadGen) return;
		const runs = data.runs || [];
		tbody.innerHTML = "";
		if (runs.length === 0) {
			table.hidden = true;
			const filtered = runsFiltered();
			showEmpty(filtered ? "No runs match your search." : "No runs yet.", filtered);
			return;
		}
		showListControls();
		renderSummary(data.summary || {});
		appendRunRows(tbody, runs);
		wireRunsMore(tbody, runs.length, data.has_more);
	} catch (e) {
		// A failure from a superseded load says nothing about the table a newer load already drew,
		// so it is dropped the same way a superseded success is. Clearing here wiped good rows and
		// showed an error over them whenever an older request lost the race and then failed.
		if (gen !== runsLoadGen) return;
		tbody.innerHTML = "";
		table.hidden = true;
		setStatus("Failed to load runs: " + e.message);
	}
}

// toolLabel returns a short label for what a run executed: its name where it has one, its playbook
// file, or the most informative line of its script, collapsed and truncated so a long one does not
// stretch the row.
function toolLabel(r) {
	// A saved workflow's identity is its graph, not a playbook it does not have. Without this a
	// stepped template listed with a blank what-it-runs label and read as a broken ansible entry.
	// A named one keeps its name: "Provision and deploy, 3 steps" says what ran, where the count
	// alone said only that something did, and every workflow in the list looked like every other.
	if (r.steps && r.steps.length) {
		const count = r.steps.length + (r.steps.length === 1 ? " step" : " steps");
		return r.playbook ? clipLabel(r.playbook) + ", " + count : "workflow, " + count;
	}
	if (r.playbook) return baseName(r.playbook) || r.playbook;
	// Terraform and OpenTofu are pointed at a directory rather than handed a script, so their
	// command is a path and is named the way a playbook's path is named: by its last segment. The
	// whole path is worse than useless in a column this narrow, and on a demo or a temp checkout it
	// is a scratch location that reads as debris.
	if (DIR_TOOLS[r.tool] && r.command) {
		return clipLabel(baseName(r.command) || r.command);
	}
	return clipLabel(scriptLabel(r.command || ""));
}

// DIR_TOOLS are the tools whose command names a working directory rather than a script to run.
const DIR_TOOLS = { terraform: true, opentofu: true };

// scriptLabel picks the line of a script that describes it.
//
// A script has no filename, so the whole source was collapsed onto one line and cut at 48
// characters. What fits in 48 characters of a script is its preamble, so a Go run listed as
// "package main import ( "encoding/json" "fmt" ) t" and a Python one as "import json want = {" and
// neither said anything about what the run did. Two scripts that differ entirely looked identical.
//
// A leading comment is how people already title a script, so it wins. Failing that, the first line
// that is not ceremony: a shebang, an import, a package or module declaration, or a shell setting
// its own options. Failing that the whole thing, which is no worse than before.
function scriptLabel(command) {
	// A command that fits is shown whole, whatever it is spread across. "terraform apply
	// -auto-approve" written over two lines is one instruction, not a script with a title line, and
	// picking a line out of it would drop half the meaning. Only something too long to show needs a
	// line chosen to stand for it.
	const whole = command.replace(/\s+/g, " ").trim();
	if (whole.length <= 48) return whole;
	const lines = command.split("\n").map((l) => l.trim()).filter(Boolean);
	for (const line of lines) {
		const comment = line.match(/^(?:#|\/\/|--)\s*(.+)$/);
		// A shebang starts with # too and is ceremony, not a title.
		if (comment && !line.startsWith("#!")) return comment[1];
	}
	// A parenthesized import or declaration block is ceremony too, and its contents are just
	// quoted paths: without this a Go program was described as "encoding/json".
	let inBlock = false;
	for (const line of lines) {
		if (inBlock) {
			if (/^\)/.test(line)) inBlock = false;
			continue;
		}
		if (/^(import|var|const|require|use)\s*\($/.test(line)) {
			inBlock = true;
			continue;
		}
		if (/^(#!|import\b|from\b|package\b|use\b|require\b|set\s+-|@echo\b)/.test(line)) continue;
		if (/^[)}\]]/.test(line)) continue;
		return line;
	}
	return command.replace(/\s+/g, " ").trim();
}

// clipLabel collapses whitespace and cuts a label to the width a row can hold.
function clipLabel(text) {
	const one = String(text || "").replace(/\s+/g, " ").trim();
	return one.length > 48 ? one.slice(0, 47) + "…" : one;
}

// KIND_TIPS explains each run kind and tool chip on hover.
const KIND_TIPS = {
	pipeline: "A multi-step pipeline. Each step runs after the one before it and can pass outputs on",
	split: "Split into shards across the inventory, balanced by each host's measured duration",
	dry: "A dry run. Reports what would change without applying anything",
	ansible: "Runs an Ansible playbook",
	bash: "Runs a Bash script",
	terraform: "Runs Terraform",
	opentofu: "Runs OpenTofu",
	python: "Runs a Python script",
	powershell: "Runs a PowerShell script",
	go: "Runs a Go program",
};

// SOURCE_LABELS names each provenance source in the interface.
const SOURCE_LABELS = {
	api: "API", manual: "Manual", template: "Template", schedule: "Schedule",
	rerun: "Rerun", reconcile: "Drift fix", propose: "Proposed",
};

// originCellEl renders what fired a run: a chip naming the source, linked to the object behind
// it when there is one, plus the actor who asked for it.
function originCellEl(r) {
	const cell = td("");
	const source = r.source || (r.proposed_from ? "reconcile" : "");
	if (!source) {
		cell.textContent = "\u2014";
		cell.dataset.tip = "Recorded before run provenance was tracked";
		return cell;
	}
	const label = SOURCE_LABELS[source] || source;
	let chip;
	const href = originHref(r);
	if (href) {
		chip = document.createElement("a");
		chip.href = href;
	} else {
		chip = document.createElement("span");
	}
	chip.className = "origin-chip " + source;
	chip.textContent = label;
	chip.dataset.tip = originTip(r);
	cell.appendChild(chip);
	if (r.actor) {
		const who = document.createElement("span");
		who.className = "origin-actor";
		who.textContent = r.actor;
		cell.appendChild(who);
	}
	return cell;
}

// originHref returns where a run's origin chip navigates, empty when the source has no page.
function originHref(r) {
	switch (r.source) {
	case "template":
		return r.source_id ? "/ui/templates" : "";
	case "schedule":
		return r.source_id ? "/ui/schedules" : "";
	case "rerun":
		return r.rerun_of ? "/ui/runs/" + r.rerun_of : "";
	case "reconcile":
		return r.proposed_from ? "/ui/runs/" + r.proposed_from : "";
	default:
		return "";
	}
}

// originTip explains a run's origin in a sentence.
function originTip(r) {
	switch (r.source) {
	case "template": return "Launched from a saved template. Open templates";
	case "schedule": return "Fired by a schedule on its cron cadence. Open schedules";
	case "rerun": return "Replayed the spec of an earlier run. Open that run";
	case "reconcile": return "Proposed to fix drift found by a check. Open the check";
	case "propose": return "Proposed from a description, held for approval";
	case "api": return "Submitted directly through the API";
	default: return "How this run was started";
	}
}

// labelChipsInto appends a run's labels as key-value chips that filter the list.
function labelChipsInto(cell, labels) {
	for (const key of Object.keys(labels || {}).sort()) {
		cell.appendChild(labelChip(key, labels[key]));
	}
}

// labelChip builds one clickable key=value chip.
function labelChip(key, value) {
	const chip = document.createElement("a");
	chip.className = "label-chip";
	chip.href = "/ui/runs?q=" + encodeURIComponent("label:" + key + "=" + value);
	chip.textContent = key + "=" + value;
	chip.dataset.tip = "Click to show every run labeled " + key + "=" + value;
	return chip;
}

// labelCellEl renders a run's labels in their own column, capped at two chips so every row keeps
// the same height. The rest collapse into a count that expands the row on click.
function labelCellEl(labels) {
	const cell = td("", "col-labels");
	const keys = Object.keys(labels || {}).sort();
	if (!keys.length) {
		cell.textContent = "\u2014";
		return cell;
	}
	const wrap = document.createElement("span");
	wrap.className = "label-wrap";
	const shown = keys.slice(0, 2);
	for (const key of shown) wrap.appendChild(labelChip(key, labels[key]));
	const rest = keys.slice(2);
	if (rest.length) {
		const more = document.createElement("button");
		more.type = "button";
		more.className = "label-chip label-more";
		more.textContent = "+" + rest.length;
		more.dataset.tip = "Click to show " + rest.map((k) => k + "=" + labels[k]).join(", ");
		more.addEventListener("click", (e) => {
			e.preventDefault();
			e.stopPropagation();
			more.remove();
			for (const key of rest) wrap.appendChild(labelChip(key, labels[key]));
		});
		wrap.appendChild(more);
	}
	cell.appendChild(wrap);
	return cell;
}

// typeCellEl fills a table cell with the run's tool chip and any kind tags, so type lives in one
// labeled, aligned column instead of floating beside names.
function typeCellEl(r) {
	const cell = td("");
	// A stepped template names no tool of its own, each step does, so a tool chip here would
	// claim ansible for a graph that may run none. The pipeline tag below is its identity.
	const stepped = !r.tool && r.steps && r.steps.length;
	if (!stepped) {
		const tool = (r.tool || "ansible").toLowerCase();
		const chip = document.createElement("span");
		chip.className = "tool-badge " + tool;
		chip.dataset.tool = tool;
		chip.textContent = tool;
		if (KIND_TIPS[tool]) chip.dataset.tip = KIND_TIPS[tool];
		cell.appendChild(chip);
	}
	for (const kind of [r.kind === "split" ? "split" : "", (r.kind === "pipeline" || stepped) ? "pipeline" : "", r.dry_run ? "dry" : ""]) {
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
	badge.dataset.tool = r.tool;
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
		const openRun = (e) => {
			// A chip in the row is its own link: the origin chip opens templates or schedules, a label
			// chip filters the list. Clicking one used to fire both the chip's navigation and this
			// handler, and the two raced, so clicking a run's origin chip could land on the run or on
			// templates depending on which won. Let an interactive child handle its own click; only a
			// click on the row itself opens the run.
			if (e && e.target.closest("a, button")) return;
			location.href = "/ui/runs/" + r.id;
		};
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
		tr.appendChild(originCellEl(r));

		tr.appendChild(playbookCellEl(r));
		tr.appendChild(labelCellEl(r.labels));

		tr.appendChild(tdTime(r.started_at || r.created_at));
		tr.appendChild(td(fmtDuration(r.started_at, r.ended_at)));
		tbody.appendChild(tr);
	}
}

// runsHasMore remembers whether the server holds more rows than the table shows, which is what
// an export has to know to avoid shipping a partial file that looks whole.
let runsHasMore = false;

// runsExportBound caps how many rows an export pulls in one click, ten server pages, so an export
// cannot ask for an unbounded history. A pull cut at the bound says so instead of staying quiet.
const runsExportBound = 10000;

// runsExportPrepare pulls every remaining page of the current query into the table before an
// export. The export reads the rendered table, so without this it silently carried only the rows
// already scrolled into view: a file that reads as the record and quietly is not. It returns ""
// when the table is complete and "-partial" with a page notice when the bound cut the pull short.
async function runsExportPrepare() {
	const tbody = document.getElementById("runs");
	if (!tbody) return "";
	while (runsHasMore) {
		const offset = tbody.querySelectorAll("tr").length;
		if (offset >= runsExportBound) {
			setStatus("The export carries the first " + offset + " matching runs. Narrow the " +
				"search or the date window to export the rest.");
			return "-partial";
		}
		const data = await getJSON("/runs?limit=1000&offset=" + offset +
			"&q=" + encodeURIComponent(runsQuery()) + runsFilterParams());
		const runs = data.runs || [];
		appendRunRows(tbody, runs);
		runsHasMore = !!data.has_more && runs.length > 0;
	}
	wireRunsMore(tbody, tbody.querySelectorAll("tr").length, runsHasMore);
	return "";
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
		const anchor = table.closest(".list-scroll") || table;
		anchor.parentNode.insertBefore(btn, anchor.nextSibling);
	}
	runsHasMore = !!hasMore;
	btn.hidden = !hasMore;
	btn.onclick = async () => {
		btn.disabled = true;
		try {
			const data = await getJSON(runsPageURL(offset));
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
	countUp(v, value);
	const l = document.createElement("div");
	l.className = "stat-label";
	l.textContent = label;
	card.appendChild(v);
	card.appendChild(l);
	return card;
}

// countUp animates a metric from zero to its value, preserving any suffix such as a percent
// sign. A value that is not a plain number, and a reader who asked for reduced motion, get the
// final text immediately.
function countUp(el, value) {
	const text = String(value);
	const match = text.match(/^(\d[\d,]*)(\D*)$/);
	if (!match || window.matchMedia("(prefers-reduced-motion: reduce)").matches) return;
	const target = parseInt(match[1].replace(/,/g, ""), 10);
	const suffix = match[2] || "";
	if (!Number.isFinite(target) || target === 0) return;
	const duration = 620;
	const start = performance.now();
	el.classList.add("counting");
	const step = (now) => {
		const t = Math.min(1, (now - start) / duration);
		// Ease out cubic, so the count decelerates into its final figure.
		const eased = 1 - Math.pow(1 - t, 3);
		el.textContent = Math.round(target * eased).toLocaleString() + suffix;
		if (t < 1) requestAnimationFrame(step);
		else el.textContent = text;
	};
	requestAnimationFrame(step);
}

