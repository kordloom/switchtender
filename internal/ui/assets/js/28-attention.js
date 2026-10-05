// ATTENTION_BLOCKERS are the four answers to "what is stopping this?", in the order an item's main
// blocker is picked: work in flight on a worker that vanished, a queue nothing serves, a decision a
// person owes, and work waiting behind other work. Each carries its card label, the tip the card
// shows, and the color class its count takes when it is not zero.
const ATTENTION_BLOCKERS = [
	{ key: "worker_lost", label: "Worker lost", cls: "failed",
		tip: "Work whose worker stopped reporting. The lease sweep reclaims it on its own" },
	{ key: "no_worker", label: "No worker available", cls: "failed",
		tip: "Work queued where no connected worker serves its queue" },
	{ key: "approval_needed", label: "Approval needed", cls: "changed",
		tip: "Held runs and workflows waiting at an approval step" },
	{ key: "blocked", label: "Blocked", cls: "changed",
		tip: "Work waiting behind another run or a full worker past its threshold" },
];

// ATTENTION_REFRESH_MS is how often the panel reads the server again while the overview is open, so
// a countdown or a waiting time does not sit frozen at the moment the page loaded.
const ATTENTION_REFRESH_MS = 15000;

// attentionState holds the panel's filter, its last answer, and its refresh timer. The filter
// starts from the page address, so an alert's link opens the list it names.
const attentionState = { blocker: "", data: null, timer: null, started: false };

// attentionBlocker returns the blocker named by key.
function attentionBlocker(key) {
	return ATTENTION_BLOCKERS.find((b) => b.key === key) || { key, label: key, cls: "", tip: "" };
}

// startAttention reads the filter from the address, loads the panel, and refreshes it on a timer.
// It runs once per page, and a second call only reloads.
function startAttention() {
	if (!attentionState.started) {
		attentionState.started = true;
		const wanted = new URLSearchParams(location.search).get("attention") || "";
		if (ATTENTION_BLOCKERS.some((b) => b.key === wanted)) attentionState.blocker = wanted;
		attentionState.timer = setInterval(loadAttention, ATTENTION_REFRESH_MS);
	}
	return loadAttention();
}

// loadAttention reads what needs attention and draws the panel. A failure to read it says so in the
// panel rather than over the rest of the overview, which loaded on its own.
async function loadAttention() {
	const panel = document.getElementById("attention-panel");
	if (!panel) return;
	let data;
	try {
		data = await getJSON("/attention");
	} catch (err) {
		panel.hidden = false;
		const list = document.getElementById("attn-list");
		if (list) {
			list.textContent = "";
			list.appendChild(emptyLine("Could not read what needs attention: " + err.message));
		}
		return;
	}
	attentionState.data = data || {};
	renderAttention(attentionState.data);
}

// renderAttention draws the four counts and the list beneath them. An answer missing either part
// draws it empty rather than failing, since a server can answer with an empty body.
function renderAttention(answer) {
	const panel = document.getElementById("attention-panel");
	if (!panel) return;
	const data = answer || {};
	panel.hidden = false;
	renderAttentionCounts(data.counts || {});
	renderAttentionList(data.items || []);
}

// renderAttentionCounts draws one card per blocker. Each card is a button that narrows the list to
// that blocker, and pressing the pressed one shows everything again.
function renderAttentionCounts(counts) {
	const el = document.getElementById("attn-counts");
	if (!el) return;
	el.textContent = "";
	for (const b of ATTENTION_BLOCKERS) {
		const n = Number(counts[b.key]) || 0;
		const card = statCard(n, b.label, n ? b.cls : "");
		card.classList.add("attn-count");
		card.dataset.blocker = b.key;
		card.dataset.tip = b.tip;
		card.setAttribute("role", "button");
		card.setAttribute("tabindex", "0");
		card.setAttribute("aria-pressed", attentionState.blocker === b.key ? "true" : "false");
		const choose = () => {
			attentionState.blocker = attentionState.blocker === b.key ? "" : b.key;
			renderAttention(attentionState.data || { counts, items: [] });
		};
		card.addEventListener("click", choose);
		card.addEventListener("keydown", (e) => {
			if (e.key === "Enter" || e.key === " ") {
				e.preventDefault();
				choose();
			}
		});
		el.appendChild(card);
	}
}

// renderAttentionList draws the items, narrowed to the chosen blocker.
function renderAttentionList(items) {
	const list = document.getElementById("attn-list");
	const filter = document.getElementById("attn-filter");
	if (!list) return;
	list.textContent = "";
	const shown = attentionState.blocker
		? items.filter((it) => it.blocker === attentionState.blocker)
		: items;
	if (filter) {
		filter.hidden = !attentionState.blocker;
		filter.textContent = attentionState.blocker
			? "Showing " + attentionBlocker(attentionState.blocker).label.toLowerCase() +
				". Press the card again to show everything."
			: "";
	}
	if (!items.length) {
		list.appendChild(emptyLine("Nothing is waiting on a person, a worker, or another run."));
		return;
	}
	if (!shown.length) {
		list.appendChild(emptyLine("Nothing here right now."));
		return;
	}
	for (const it of shown) list.appendChild(attentionItem(it));
}

// attentionItem builds one item: its main blocker, what it is, how long it has been in that
// blocker, its other conditions as badges and how they interact, why it is stuck, who can act,
// and what happens next. There is deliberately no requeue control: putting a run back by hand could
// start it a second time on a worker that is only cut off rather than gone, and the lease sweep's
// reclaim is the safe version of that move.
function attentionItem(it) {
	const main = it.main || {};
	const row = document.createElement("div");
	row.className = "attn-item";
	row.dataset.blocker = it.blocker;
	row.dataset.key = it.key;

	const head = document.createElement("div");
	head.className = "attn-item-head";
	const label = document.createElement("span");
	label.className = "attn-main " + attentionBlocker(it.blocker).cls;
	label.textContent = attentionBlocker(it.blocker).label;
	head.appendChild(label);
	const name = document.createElement("a");
	name.className = "attn-name";
	name.textContent = it.name || it.run_id || it.schedule_id || it.key;
	name.href = it.kind === "schedule"
		? "/ui/schedules"
		: "/ui/runs/" + encodeURIComponent(it.run_id || "");
	head.appendChild(name);
	if (it.kind && it.kind !== "run") {
		const kind = document.createElement("span");
		kind.className = "attn-kind muted";
		kind.textContent = it.kind;
		head.appendChild(kind);
	}
	const since = document.createElement("span");
	since.className = "attn-since";
	since.textContent = "In this state for " + attentionWaited(it.since);
	since.title = fmtTime(it.since);
	head.appendChild(since);
	if (it.alerting) {
		const alert = document.createElement("span");
		alert.className = "attn-alerting";
		alert.textContent = "Alerting";
		alert.title = "Past its alert threshold of " + fmtMs((it.alert_after_seconds || 0) * 1000);
		head.appendChild(alert);
	}
	row.appendChild(head);

	const badges = attentionBadges(it);
	if (badges.childNodes.length) row.appendChild(badges);

	const reason = document.createElement("p");
	reason.className = "attn-reason";
	reason.textContent = main.reason || "";
	row.appendChild(reason);
	if (it.interaction) {
		const inter = document.createElement("p");
		inter.className = "attn-interaction muted";
		inter.textContent = it.interaction;
		row.appendChild(inter);
	}
	row.appendChild(attentionFacts(it));
	return row;
}

// attentionWaited renders how long an item has been in its current blocker.
function attentionWaited(since) {
	const ms = Date.now() - new Date(since).getTime();
	if (!isFinite(ms) || ms < 0) return "0s";
	if (ms < 60000) return Math.round(ms / 1000) + "s";
	return fmtMs(ms);
}

// attentionBadges builds the item's badges: whether an approval is a whole run or a workflow step
// and, for a step, whether the rest of the workflow is running or paused, then one badge per other
// condition the item carries.
function attentionBadges(it) {
	const wrap = document.createElement("div");
	wrap.className = "attn-badges";
	const ap = (it.main || {}).approval;
	if (ap) {
		const scope = document.createElement("span");
		scope.className = "attn-badge scope";
		if (ap.scope === "workflow_step") {
			scope.textContent = "Workflow step, " + (ap.workflow_state === "other_branches_running"
				? "other branches running" : "workflow paused");
		} else {
			scope.textContent = ap.proposed_from ? "Run, from a plan" : "Run";
		}
		wrap.appendChild(scope);
	}
	for (const b of it.badges || []) {
		const badge = document.createElement("span");
		badge.className = "attn-badge " + attentionBlocker(b.blocker).cls;
		badge.dataset.blocker = b.blocker;
		badge.textContent = attentionBlocker(b.blocker).label + (b.step ? ": " + b.step : "");
		badge.title = b.reason || "";
		wrap.appendChild(badge);
	}
	return wrap;
}

// attentionFacts builds the item's facts: who can act and what happens next, and the details its
// blocker has, such as the queue and how many workers serve it, the reclaim countdown, the run
// holding it, or what each answer to an approval runs.
function attentionFacts(it) {
	const main = it.main || {};
	const dl = document.createElement("dl");
	dl.className = "attn-facts";
	const fact = (term, value) => {
		if (value === undefined || value === null || value === "") return;
		const dt = document.createElement("dt");
		dt.textContent = term;
		const dd = document.createElement("dd");
		if (typeof value === "object") dd.appendChild(value);
		else dd.textContent = value;
		dl.appendChild(dt);
		dl.appendChild(dd);
	};
	fact("Who can act", main.who_can_act);
	const ap = main.approval;
	if (ap) {
		fact("On approve", ap.on_approve);
		fact("On deny", ap.on_deny);
		if (ap.expires_at) fact("Times out", fmtTime(ap.expires_at));
	} else {
		fact("Next", main.next);
	}
	if (main.queue !== undefined && main.queue !== null) {
		const queue = main.queue === "" ? "the default queue" : main.queue;
		fact("Queue", queue + ", " + (main.eligible_workers || 0) + " " +
			plural(main.eligible_workers || 0, "eligible worker", "eligible workers") + " connected");
	}
	if (main.worker) fact("Worker", main.worker + ", last reported " + relTime(main.last_seen));
	if (main.reclaim_at) fact("Reclaim", attentionReclaim(main.reclaim_at));
	if (main.holder_run_id) {
		const link = document.createElement("a");
		link.href = "/ui/runs/" + encodeURIComponent(main.holder_run_id);
		link.textContent = main.holder_run_id;
		fact("Held by run", link);
	}
	if (it.alert_after_seconds) {
		fact("Alert", it.alerting
			? "Alerting, past " + fmtMs(it.alert_after_seconds * 1000)
			: "Alerts after " + fmtMs(it.alert_after_seconds * 1000) + " in this state");
	}
	return dl;
}

// attentionReclaim renders the automatic reclaim countdown.
function attentionReclaim(at) {
	const ms = new Date(at).getTime() - Date.now();
	if (!isFinite(ms)) return "";
	if (ms > 0) return "Automatically, in " + Math.ceil(ms / 1000) + "s";
	return "Due now, at the next lease sweep";
}
