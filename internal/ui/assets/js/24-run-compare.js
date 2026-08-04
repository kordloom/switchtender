// The compare page holds one run against its baseline: verdict per host, timing per task, and
// the headline deltas. Every cell is built from text nodes, since host and task names come from
// inventories and playbooks this server did not author.

// COMPARE_VERDICTS maps a verdict to its badge class and the words the page uses for it.
const COMPARE_VERDICTS = {
	broke: { cls: "failed", label: "Broke" },
	still_failing: { cls: "failed", label: "Still failing" },
	removed: { cls: "canceled", label: "Not in this run" },
	added: { cls: "running", label: "New in this run" },
	recovered: { cls: "succeeded", label: "Recovered" },
	ok: { cls: "succeeded", label: "OK" },
};

// compareSeconds renders a task duration, with a dash for a run that lacked the task.
function compareSeconds(v) {
	return v < 0 ? "-" : v.toFixed(2) + "s";
}

// compareWorst renders a host's outcome words for one side of the pair.
function compareWorst(side) {
	return side ? side.worst : "-";
}

// renderCompareSummary fills the stat strip from the comparison totals.
function renderCompareSummary(c) {
	const box = document.getElementById("compare-summary");
	if (!box) return;
	box.innerHTML = "";
	const card = (value, label, cls) => {
		const el = document.createElement("div");
		el.className = "stat-card";
		const v = document.createElement("span");
		v.className = "stat-value" + (cls ? " " + cls : "");
		v.textContent = value;
		const l = document.createElement("span");
		l.className = "stat-label";
		l.textContent = label;
		el.appendChild(v);
		el.appendChild(l);
		box.appendChild(el);
	};
	card(String(c.totals.broke), "Broke", c.totals.broke ? "bad" : "");
	card(String(c.totals.recovered), "Recovered", c.totals.recovered ? "good" : "");
	card(String(c.totals.still_failing), "Still failing", c.totals.still_failing ? "bad" : "");
	card(String(c.totals.ok), "OK", "");
	card(String(c.totals.added + c.totals.removed), "Hosts came or went", "");
	if (typeof c.duration_delta_seconds === "number") {
		const d = c.duration_delta_seconds;
		card((d >= 0 ? "+" : "") + d.toFixed(1) + "s", "Duration vs baseline", d > 0 ? "bad" : "good");
	}
	box.hidden = false;
}

// renderCompareHeader names the two runs with links, and warns when they came from different
// sources, where a host-by-host reading stops being apples to apples.
function renderCompareHeader(c) {
	const head = document.getElementById("compare-header");
	if (!head) return;
	head.innerHTML = "";
	const link = (r, words) => {
		const a = document.createElement("a");
		a.href = "/ui/runs/" + encodeURIComponent(r.id);
		a.textContent = r.id;
		head.appendChild(document.createTextNode(words));
		head.appendChild(a);
		head.appendChild(document.createTextNode(" (" + r.status + ")"));
	};
	link(c.a, "Comparing ");
	link(c.b, " against baseline ");
	head.appendChild(document.createTextNode("."));
	if (!c.same_source) {
		const warn = document.createElement("span");
		warn.className = "muted";
		warn.textContent = " These runs came from different sources; read host verdicts loosely.";
		head.appendChild(warn);
	}
}

// renderCompare fills the whole page from one comparison document.
function renderCompare(c) {
	renderCompareHeader(c);
	renderCompareSummary(c);

	const hosts = document.getElementById("compare-hosts");
	hosts.innerHTML = "";
	for (const h of c.hosts) {
		const row = document.createElement("tr");
		const hostCell = document.createElement("td");
		const hostLink = document.createElement("a");
		hostLink.href = "/ui/hosts/" + encodeURIComponent(h.host);
		hostLink.textContent = h.host;
		hostCell.appendChild(hostLink);
		row.appendChild(hostCell);
		const verdict = COMPARE_VERDICTS[h.verdict] || { cls: "", label: h.verdict };
		const badgeCell = document.createElement("td");
		const badge = document.createElement("span");
		badge.className = "badge " + verdict.cls;
		badge.textContent = verdict.label;
		badgeCell.appendChild(badge);
		row.appendChild(badgeCell);
		row.appendChild(td(compareWorst(h.a)));
		row.appendChild(td(compareWorst(h.b)));
		row.appendChild(td(String((h.a ? h.a.failures : 0)) + " / " + String(h.b ? h.b.failures : 0)));
		row.appendChild(td(String((h.a ? h.a.changed : 0)) + " / " + String(h.b ? h.b.changed : 0)));
		hosts.appendChild(row);
	}
	document.getElementById("compare-hosts-section").hidden = c.hosts.length === 0;

	const tasks = document.getElementById("compare-tasks");
	tasks.innerHTML = "";
	for (const tk of c.tasks) {
		const row = document.createElement("tr");
		row.appendChild(td(tk.task));
		row.appendChild(td(compareSeconds(tk.a_seconds)));
		row.appendChild(td(compareSeconds(tk.b_seconds)));
		const delta = document.createElement("td");
		if (tk.a_seconds < 0 || tk.b_seconds < 0) {
			delta.textContent = "-";
		} else {
			delta.textContent = (tk.delta_seconds >= 0 ? "+" : "") + tk.delta_seconds.toFixed(2) + "s";
			if (tk.delta_seconds > 0) delta.className = "bad";
			if (tk.delta_seconds < 0) delta.className = "good";
		}
		row.appendChild(delta);
		tasks.appendChild(row);
	}
	document.getElementById("compare-tasks-section").hidden = c.tasks.length === 0;
}

// loadCompare fetches the comparison named by the page URL and renders it.
async function loadCompare() {
	const runId = document.body.dataset.runId;
	const status = document.getElementById("status");
	const withRun = new URLSearchParams(window.location.search).get("with") || "prev";
	try {
		const c = await getJSON("/runs/" + encodeURIComponent(runId) +
			"/compare?with=" + encodeURIComponent(withRun));
		status.hidden = true;
		renderCompare(c);
	} catch (e) {
		status.textContent = "Comparison unavailable: " + e.message +
			". A run needs an earlier run of the same template to compare against.";
	}
}

if (document.body.dataset.page === "compare") {
	loadCompare();
}
