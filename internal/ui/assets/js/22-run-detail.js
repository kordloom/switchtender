// wireActions hooks up the cancel and retry buttons for the run being viewed.
function wireActions(runId) {
	const cancel = document.getElementById("cancel-run");
	if (cancel) cancel.addEventListener("click", async () => {
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
	if (retry) retry.addEventListener("click", async () => {
		retry.disabled = true;
		try {
			const created = await postAction("/runs/" + runId + "/retry");
			window.location.href = "/ui/runs/" + created.id;
		} catch (e) {
			setStatus("Retry failed: " + e.message);
			retry.disabled = false;
		}
	});
	const approve = document.getElementById("approve-run");
	if (approve) {
		approve.addEventListener("click", async () => {
			approve.disabled = true;
			try {
				await postAction("/runs/" + runId + "/approve");
				location.reload();
			} catch (e) {
				setStatus("Approve failed: " + e.message);
				approve.disabled = false;
			}
		});
	}
	const reject = document.getElementById("reject-run");
	if (reject) {
		reject.addEventListener("click", async () => {
			const reason = window.prompt("Reason for rejecting (optional):");
			if (reason === null) return;
			reject.disabled = true;
			try {
				await postAction("/runs/" + runId + "/reject", { reason });
				location.reload();
			} catch (e) {
				setStatus("Reject failed: " + e.message);
				reject.disabled = false;
			}
		});
	}
	const explain = document.getElementById("explain-run");
	if (explain) {
		explain.addEventListener("click", async () => {
			const panel = document.getElementById("explain-panel");
			const bodyEl = document.getElementById("explain-body");
			if (panel) panel.hidden = false;
			// With no AI provider the click answers immediately with the standard off notice
			// instead of a round trip that fails with a line most people miss.
			if (aiOff()) {
				if (bodyEl) {
					bodyEl.textContent = "";
					bodyEl.appendChild(aiOffNoticeEl(
						"Advisory AI is off on this server. Turn it on to get a failure diagnosis grounded in this run."));
				}
				return;
			}
			if (bodyEl) bodyEl.textContent = "Reading the run…";
			explain.disabled = true;
			try {
				const res = await postAction("/runs/" + runId + "/explain");
				if (bodyEl) bodyEl.textContent = res.explanation || "No explanation was returned.";
			} catch (e) {
				if (bodyEl) {
					bodyEl.textContent = e.message === "ai is not enabled"
						? "AI triage is not enabled on this server."
						: "Could not explain the run: " + e.message;
				}
			} finally {
				explain.disabled = false;
			}
		});
	}
}

// updateActions shows cancel while the run is active and retry on a finished split that did not
// fully succeed.
function updateActions(run) {
	const cancel = document.getElementById("cancel-run");
	const retry = document.getElementById("retry-run");
	if (!cancel || !retry) return;
	const approve = document.getElementById("approve-run");
	const reject = document.getElementById("reject-run");
	const explain = document.getElementById("explain-run");
	if (isReadOnly()) {
		cancel.hidden = true;
		retry.hidden = true;
		if (approve) approve.hidden = true;
		if (reject) reject.hidden = true;
		if (explain) explain.hidden = true;
		return;
	}
	const held = run.status === "pending_approval";
	// A held run is still cancelable, and the API has always allowed it: CancelPending accepts an
	// unclaimed run in pending_approval. Hiding the button left the person who submitted the run with
	// no way to stop it, because rejecting is an admin decision while canceling your own run is
	// operator work. They had to ask an approver to reject something nobody wanted decided.
	// Each control also honors the session's role: the server refuses a viewer's cancel and an
	// operator's approve anyway, and a button whose only future is a 403 reads as breakage.
	cancel.hidden = isTerminal(run.status) || !roleAtLeast("operator");
	if (approve) approve.hidden = !held || !roleAtLeast("admin");
	if (reject) reject.hidden = !held || !roleAtLeast("admin");
	const splitParent = (run.kind === "split" || run.shard_count) && !run.parent_id;
	retry.hidden = !(splitParent && isTerminal(run.status) && run.status !== "succeeded") ||
		!roleAtLeast("operator");
	if (explain) {
		const heldProposal = held && (run.proposed_from || run.intent);
		explain.hidden = !(run.status === "failed" || run.status === "interrupted" || heldProposal) ||
			!roleAtLeast("operator");
	}
}

// riskBadge renders a run's graded blast radius. The server computes the grade from the run's tool,
// command, and how wide it targets; it is advisory, so it reads as information rather than as a
// verdict the approver has to argue with.
function riskBadge(risk) {
	const span = document.createElement("span");
	span.className = "risk risk-" + (risk.level || "low");
	span.textContent = risk.level || "low";
	if (risk.reasons && risk.reasons.length) {
		span.dataset.tip = risk.reasons.join("\n");
	}
	return span;
}

// renderRiskCallout spells out why a held run is graded as it is, in the place the decision is made.
// A tooltip is enough for a run that is only being read, but an approver deciding whether to let a
// change through should not have to hover to find out that it destroys infrastructure. It shows only
// while the run is held, and disappears once the decision is taken.
function renderRiskCallout(run) {
	const host = document.getElementById("risk-callout");
	if (!host) return;
	const risk = run.risk;
	if (!risk || run.status !== "pending_approval") {
		host.hidden = true;
		host.textContent = "";
		return;
	}
	host.textContent = "";
	host.className = "risk-callout risk-" + (risk.level || "low");
	const head = document.createElement("div");
	head.className = "risk-callout-head";
	const label = document.createElement("strong");
	label.textContent = run.held_by_policy
		? "Held for approval by \"" + run.held_by_policy + "\""
		: "Held for approval";
	head.appendChild(label);
	head.appendChild(riskBadge(risk));
	host.appendChild(head);
	const why = document.createElement("ul");
	why.className = "risk-reasons";
	for (const reason of risk.reasons || []) {
		const li = document.createElement("li");
		li.textContent = reason;
		why.appendChild(li);
	}
	if (why.children.length) host.appendChild(why);
	host.hidden = false;
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

// headerTimer coalesces live header refreshes, so a burst of events refreshes the status,
// duration, and action row once every few seconds rather than once per event.
let headerTimer = null;

// scheduleHeaderRefresh re-reads the run and redraws its header while it executes. Without it the
// header rendered once at load and then froze: a run that moved from pending to running to failed
// kept its first badge and its stale action buttons until the end signal, or forever if the end
// was missed.
function scheduleHeaderRefresh(runId) {
	if (headerTimer !== null) return;
	headerTimer = window.setTimeout(async () => {
		headerTimer = null;
		try {
			const run = await getJSON("/runs/" + runId);
			if (!detailState || detailState.runId !== runId) return;
			detailState.run = run;
			renderHeader(run);
		} catch (_) { /* keep the last header on a refresh failure */ }
	}, 3000);
}

// streamIndicator wires a stream's lifecycle into the live indicator. The browser retries a
// dropped stream on its own, so an error normally only flips the label to reconnecting. But a
// stream the browser has given up on, an expired token answered with 401 or a response that is
// not a stream at all, fires its error with the source already closed and never retries: saying
// "reconnecting" forever was a promise nothing was keeping. That state now says the live view is
// gone and offers the reload that actually resumes it.
function streamIndicator(source, onReconnect) {
	const indicator = document.getElementById("live-indicator");
	if (!indicator) return;
	indicator.hidden = false;
	let dropped = false;
	source.onopen = () => {
		indicator.textContent = "live";
		// A stream that resumes after a drop missed whatever was sent meanwhile. The caller says
		// how to catch up, since only it knows whether a cursor already covers the gap.
		if (dropped && onReconnect) onReconnect();
		dropped = false;
	};
	source.onerror = () => {
		if (source.readyState === 2) {
			indicator.textContent = "";
			const link = document.createElement("a");
			link.href = location.href;
			link.textContent = "live view lost, reload to resume";
			indicator.appendChild(link);
			return;
		}
		dropped = true;
		indicator.textContent = "reconnecting";
	};
}

// openPipelineStream refreshes the header and step list as step events arrive, coalescing bursts
// into one refresh, and settles on the final state at the end signal.
function openPipelineStream(pipelineId) {
	const source = new EventSource(streamURL("/runs/" + pipelineId + "/stream"));
	streamIndicator(source);
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
		let text = idx + ". " + (s.step_name || "step");
		const detail = toolLabel(s);
		if (detail) {
			text += "  ·  " + detail;
		}
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
	const events = await loadAllEvents(run.id);
	detailState = { runId: run.id, run, events };
	detailState.lastSeq = lastSeq(detailState.events);
	renderDetail();
	setStatus("");
	if (!isTerminal(run.status)) {
		openStream(run.id, detailState.lastSeq);
	}
}

// loadParent renders a split run by merging every shard's events into one matrix. While the parent
// is active the merged grid fills in live from the parent's event stream, which carries every
// shard's events.
async function loadParent(parentId) {
	const run = await getJSON("/runs/" + parentId);
	const shardData = await getJSON("/runs/" + parentId + "/shards");
	const shards = shardData.shards || [];

	detailState = { runId: parentId, run, events: await mergedShardEvents(shards) };
	renderDetail();
	renderShards(shards);
	setStatus("");

	if (!isTerminal(run.status)) {
		openParentStream(parentId);
	}
}

// mergedShardEvents reads every shard's event history and merges it into one list for the matrix.
// A shard whose events cannot be read contributes nothing rather than failing the whole view.
async function mergedShardEvents(shards) {
	const perShard = await Promise.all(shards.map((s) => loadAllEvents(s.id).catch(() => [])));
	return [].concat.apply([], perShard);
}

// reconcileParent redraws the merged matrix from a fresh read of every shard, discarding the folded
// model so the new events are folded from scratch. Anything the live view missed is recovered here.
async function reconcileParent(parentId) {
	const shardData = await getJSON("/runs/" + parentId + "/shards");
	const shards = shardData.shards || [];
	const events = await mergedShardEvents(shards);
	if (!detailState || detailState.runId !== parentId) return;
	detailState.events = events;
	detailState.model = null;
	renderDetail();
	renderShards(shards);
}

// openParentStream applies shard events to the merged matrix as they arrive. A stats event means a
// shard finished, so the shard list refreshes, and the end signal settles the final state.
//
// The stream opens without a resume cursor, unlike a single run's. A shard's events are stored under
// the shard, and each shard's history was read through its own endpoint, so the highest sequence
// seen during those reads is a cursor in a shard's space and means nothing to the parent's. There is
// no parent-space cursor to derive from what the page has, so instead the end signal re-reads every
// shard and refolds the matrix. That closes the window between the per-shard reads and the stream
// connecting: whatever the live view missed is on the page by the time the run is finished.
function openParentStream(parentId) {
	const source = new EventSource(streamURL("/runs/" + parentId + "/stream"));
	// The parent stream has no resume cursor, so a reconnect re-reads every shard whole.
	streamIndicator(source, () => { reconcileParent(parentId).catch(() => {}); });
	const refreshShards = async () => {
		try {
			const shardData = await getJSON("/runs/" + parentId + "/shards");
			renderShards(shardData.shards || []);
		} catch (_) { /* keep the last shard list on a refresh failure */ }
	};
	source.addEventListener("event", (e) => {
		try {
			const ev = JSON.parse(e.data);
			applyLiveEvent(ev);
			scheduleHeaderRefresh(parentId);
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
		try {
			await reconcileParent(parentId);
		} catch (_) {
			// A failed reconcile leaves the live view as it stands, which is still the shard list.
			refreshShards();
		}
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
		status === "canceled" || status === "interrupted" || status === "rejected";
}

// renderDetail redraws the header, matrix, and timeline from the current state. The model is folded
// from the loaded events once and is authoritative from then on, so the raw array is released rather
// than kept alongside a structure that already holds everything read from it. Live events are applied
// to the model in place, which is what keeps a long run from growing the tab without bound.
function renderDetail() {
	renderHeader(detailState.run);
	if (!detailState.model) {
		detailState.model = buildModel(detailState.events || []);
		detailState.events = null;
	}
	renderMatrix(detailState.model);
	renderTimeline(detailState.model);
}

// GRID_COALESCE_MS is how long a burst of structural events is allowed to gather before the grid is
// redrawn. Adding a host or a task changes the shape of the matrix, so it cannot be patched in place
// the way a single cell can; redrawing per event costs hosts times tasks each time, and a run that
// discovers a hundred hosts in a second would spend the burst rebuilding the same grid. One frame's
// worth of delay is imperceptible and collapses the burst into a single redraw.
const GRID_COALESCE_MS = 120;

// gridTimer holds the pending coalesced redraw, or null when none is scheduled.
let gridTimer = null;

// scheduleGrid redraws the matrix and the timeline once the current burst of structural changes
// settles. Repeated calls inside the window collapse into one redraw of the model's latest state.
function scheduleGrid() {
	if (gridTimer !== null) return;
	gridTimer = window.setTimeout(() => {
		gridTimer = null;
		if (!detailState || !detailState.model) return;
		renderMatrix(detailState.model);
		renderTimeline(detailState.model);
	}, GRID_COALESCE_MS);
}

// runStreamPath builds a run's live stream path carrying the resume cursor. The cursor is always
// sent, zero included: the server treats the parameter's presence as authoritative and a stream
// opened without it starts at the current end of the event log. Dropping a zero cursor therefore
// lost every event stored between the history fetch and the stream connecting, which on a run that
// starts fast is its first tasks. Sending zero replays from the start instead, and the caller's
// sequence guard discards whatever it already has.
function runStreamPath(runId, afterSeq) {
	return "/runs/" + runId + "/stream?after=" + (afterSeq || 0);
}

// openStream subscribes to the run's live output and applies events, logs, and the end signal.
// It resumes after afterSeq so history is never re-sent, and skips any event at or before the
// cursor in case a reconnect replays one.
function openStream(runId, afterSeq) {
	const source = new EventSource(streamURL(runStreamPath(runId, afterSeq)));
	streamIndicator(source);
	source.addEventListener("event", (e) => {
		try {
			const ev = JSON.parse(e.data);
			if (ev.seq && ev.seq <= (detailState.lastSeq || 0)) return;
			if (ev.seq) detailState.lastSeq = ev.seq;
			applyLiveEvent(ev);
			scheduleHeaderRefresh(runId);
		} catch (_) { /* ignore a malformed event */ }
	});
	source.addEventListener("log", (e) => {
		try { appendLog(JSON.parse(e.data)); } catch (_) { /* ignore a malformed chunk */ }
	});
	source.addEventListener("end", async () => {
		source.close();
		const indicator = document.getElementById("live-indicator");
		if (indicator) indicator.hidden = true;
		try {
			detailState.run = await getJSON("/runs/" + runId);
			renderHeader(detailState.run);
		} catch (_) { /* keep the last header on refresh failure */ }
	});
}

// logCap bounds how many characters the live log pane keeps, so a long run cannot grow it
// without bound. The tail is what matters live; the full log is available on the run itself.
const logCap = 262144;

// appendLog adds a chunk to the live log pane, following the tail only when the view was already
// near the bottom, so a reader scrolled up is not yanked back down. It appends a text node
// rather than rebuilding the whole string, which keeps a long stream from getting quadratic, and
// trims the pane back to the cap when it grows past it.
function appendLog(chunk) {
	const pre = document.getElementById("log");
	detailState.logRaw = ((detailState.logRaw || "") + chunk).slice(-logCap * 2);
	if (detailState.logFilter) {
		renderLogView();
		document.getElementById("log-panel").hidden = false;
		return;
	}
	const nearBottom = pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40;
	pre.appendChild(document.createTextNode(chunk));
	detailState.logLen = (detailState.logLen || 0) + chunk.length;
	if (detailState.logLen > logCap * 2) {
		pre.textContent = pre.textContent.slice(-logCap);
		detailState.logLen = pre.textContent.length;
	}
	document.getElementById("log-panel").hidden = false;
	if (nearBottom) pre.scrollTop = pre.scrollHeight;
}

// renderLogView draws the log pane from the raw buffer, filtered when a query is set.
function renderLogView() {
	const pre = document.getElementById("log");
	const q = (detailState.logFilter || "").toLowerCase();
	if (!q) {
		pre.textContent = detailState.logRaw || "";
		detailState.logLen = pre.textContent.length;
		pre.scrollTop = pre.scrollHeight;
		return;
	}
	const lines = (detailState.logRaw || "").split("\n").filter((l) => l.toLowerCase().includes(q));
	pre.textContent = lines.length ? lines.join("\n") : "No log lines match.";
}

// wireLogFilter filters the live log pane by substring as you type.
function wireLogFilter() {
	const input = document.getElementById("log-filter");
	if (!input) return;
	input.addEventListener("input", () => {
		if (!detailState) return;
		if (detailState.logRaw === undefined) {
			detailState.logRaw = document.getElementById("log").textContent;
		}
		detailState.logFilter = input.value.trim();
		renderLogView();
	});
}

// renderWarningCallout shows a degradation the run survived, such as event capture being
// unavailable. A run that finished green with an empty matrix looks like one that did nothing, so
// the reason belongs on the page rather than only in the server log.
function renderWarningCallout(run) {
	const host = document.getElementById("run-warning");
	if (!host) return;
	if (!run.warning) {
		host.hidden = true;
		host.textContent = "";
		return;
	}
	host.textContent = "";
	const head = document.createElement("div");
	head.className = "risk-callout-head";
	const label = document.createElement("strong");
	label.textContent = "Warning";
	head.appendChild(label);
	host.appendChild(head);
	const why = document.createElement("div");
	why.className = "muted";
	why.textContent = run.warning;
	host.appendChild(why);
	host.hidden = false;
}

// renderHeader fills the run header fields.
function renderHeader(run) {
	const el = document.getElementById("run-header");
	el.innerHTML = "";
	el.appendChild(field("Status", null, badge(run.status)));
	const runField = field("Run", shortId(run.id), null, run.id);
	runField.querySelector(".value").appendChild(copyButton(run.id, "Copy the full run id"));
	el.appendChild(runField);
	if (!run.tool || run.tool === "ansible") {
		const pb = field("Playbook", baseName(run.playbook) || (run.playbook || ""), null, run.playbook || "");
		const value = pb.querySelector(".value");
		if (run.project_id && run.playbook) {
			value.textContent = "";
			const link = document.createElement("button");
			link.type = "button";
			link.className = "linkish";
			link.textContent = baseName(run.playbook) || run.playbook;
			link.dataset.tip = "View " + run.playbook + " from the project checkout";
			link.addEventListener("click", () => openFileViewer(run.project_id, run.playbook));
			value.appendChild(link);
		}
		if (run.playbook) value.appendChild(copyButton(run.playbook, "Copy the playbook path"));
		el.appendChild(pb);
	} else {
		el.appendChild(field("Tool", null, toolBadgeEl(run)));
		el.appendChild(field(run.tool === "terraform" || run.tool === "opentofu" ? "Directory" : "Command",
			toolLabel(run), null, run.command || ""));
	}
	if (run.dry_run) {
		el.appendChild(field("Mode", "dry run"));
	}
	if (run.risk) {
		el.appendChild(field("Risk", null, riskBadge(run.risk)));
	}
	renderRiskCallout(run);
	renderWarningCallout(run);
	if (run.actor) {
		const who = field("Requested by",
			run.actor + (run.actor_type === "agent" ? " (agent)" : ""), null, run.actor);
		if (run.actor_type === "agent") {
			who.querySelector(".value").dataset.tip =
				"An AI agent's token submitted this run, on a named human's behalf. The chain " +
				"commits that attribution.";
		}
		el.appendChild(who);
	}
	if (run.approved_spec_digest) {
		const bound = field("Approved spec", shortId(run.approved_spec_digest), null,
			run.approved_spec_digest);
		bound.querySelector(".value").dataset.tip =
			"The digest of the exact spec the approver released. The decision entry on the chain " +
			"commits it, and the executor refuses a spec that no longer matches.";
		bound.querySelector(".value").appendChild(
			copyButton(run.approved_spec_digest, "Copy the approved spec digest"));
		el.appendChild(bound);
	}
	if (run.source) {
		const origin = originCellEl(run);
		origin.className = "";
		el.appendChild(field("Origin", null, origin));
	}
	if (run.rerun_of) {
		const link = document.createElement("a");
		link.href = "/ui/runs/" + run.rerun_of;
		link.textContent = shortId(run.rerun_of);
		link.title = run.rerun_of;
		link.dataset.tip = "Open the run this replayed";
		el.appendChild(field("Rerun of", null, link));
	}
	if (run.labels && Object.keys(run.labels).length) {
		const wrap = document.createElement("span");
		wrap.className = "value label-wrap";
		labelChipsInto(wrap, run.labels);
		el.appendChild(field("Labels", null, wrap));
	}
	if (run.proposed_from) {
		const link = document.createElement("a");
		link.href = "/ui/runs/" + run.proposed_from;
		link.textContent = shortId(run.proposed_from);
		link.title = run.proposed_from;
		el.appendChild(field("Proposed from drift check", null, link));
		if (run.limit) {
			el.appendChild(field("Limited to", run.limit));
		}
	}
	if (run.intent) {
		el.appendChild(field("Proposed from request", run.intent));
		if (run.limit) {
			el.appendChild(field("Limited to", run.limit));
		}
	}
	if (run.inventory) {
		const inv = field("Inventory", baseName(run.inventory), null, run.inventory);
		inv.querySelector(".value").appendChild(copyButton(run.inventory, "Copy the inventory path"));
		el.appendChild(inv);
	}
	if (run.shard_count) {
		el.appendChild(field("Shards", String(run.shard_count)));
	}
	if (run.claimed_by) {
		const worker = document.createElement("a");
		worker.href = "/ui/workers";
		worker.textContent = run.claimed_by;
		worker.dataset.tip = "The executor that claimed this run. Open workers";
		el.appendChild(field("Worker", null, worker));
	}
	if (run.exit_code !== undefined && run.exit_code !== null) {
		el.appendChild(field("Exit", String(run.exit_code)));
	}
	if (run.status === "running" && run.started_at) {
		const dur = field("Duration", fmtDuration(run.started_at, new Date().toISOString()));
		const val = dur.querySelector(".value");
		val.classList.add("ticking");
		val.dataset.started = run.started_at;
		el.appendChild(dur);
	} else {
		el.appendChild(field("Duration", fmtDuration(run.started_at, run.ended_at)));
	}
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

