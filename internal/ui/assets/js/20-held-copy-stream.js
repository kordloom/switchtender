// loadPolicies populates the policy table with edit and delete actions. Each empty criterion shows
// as "any", so a reader sees exactly how wide a rule is.
// heldRuns returns the runs waiting for approval right now, so the policies list shows live
// consequence rather than only configuration, attributed per rule through held_by_policy rather
// than one global count stamped on every row.
async function heldRuns() {
	try {
		const data = await getJSON("/runs?status=pending_approval&limit=200");
		return data.runs || [];
	} catch {
		return [];
	}
}

// heldByRule counts the held runs a specific rule is holding. A run records the rule that held it
// by the name that rule carried at the hold, falling back to its id.
function heldByRule(held, p) {
	let n = 0;
	for (const r of held) {
		if (!r.held_by_policy) continue;
		// A Rego policy records its reasons and bundle after its name, so its holds are counted
		// by that prefix rather than by an exact name.
		if (r.held_by_policy === p.name || r.held_by_policy === p.id ||
			(p.rego && r.held_by_policy.startsWith(p.name + " ("))) n++;
	}
	return n;
}

// CRITERIA_COLUMNS is how many criteria columns follow the effect in the policies table: who, risk,
// tool, command, inventory, queue, destroy threshold, dry runs, and separation of duties.
const CRITERIA_COLUMNS = 9;

// appendRegoCells fills the criteria columns for a Rego policy. Its package decides everything, so
// each column says so rather than reading "any", which would describe a rule matching every run.
function appendRegoCells(tr, p) {
	const rego = p.rego || {};
	const digest = (rego.sha256 || "").slice(0, 12);
	for (let i = 0; i < CRITERIA_COLUMNS; i++) {
		const cell = document.createElement("td");
		const span = document.createElement("span");
		span.className = i === 0 ? "mono" : "muted";
		span.textContent = i === 0 ? (rego.package || "rego") : "in rego";
		span.dataset.tip = "Decided by Rego package " + (rego.package || "") +
			", bundle sha256:" + digest;
		cell.appendChild(span);
		tr.appendChild(cell);
	}
}

// regoEffectTip says what a Rego policy does to a run: what its warn rule does, which the policy file
// sets, and how long one evaluation may run before the submission is refused.
function regoEffectTip(rego) {
	const warn = rego.warn === "note"
		? "Its warnings are recorded on the run without holding it."
		: "Its warnings hold the run for approval.";
	const limit = rego.timeout ? " Each evaluation may run for " + rego.timeout + "." : "";
	return "Denies or holds a run as its Rego package decides. " + warn + limit;
}

// appendCriteriaCells fills the criteria columns for a YAML or API policy, one per criterion.
function appendCriteriaCells(tr, p, invByID) {
	const whoCell = document.createElement("td");
	if (p.actor || p.account) {
		const span = document.createElement("span");
		span.className = "mono";
		span.textContent = p.actor && p.account ? p.actor + " on " + p.account : p.actor || p.account;
		span.dataset.tip = p.actor && p.account
			? "Only this named actor's runs match, on this account"
			: p.actor ? "Only this named actor's runs match"
				: "Only runs asked for under this account match";
		whoCell.appendChild(span);
	} else if (p.actor_kind === "agent" || p.actor_kind === "human") {
		const span = document.createElement("span");
		span.textContent = p.actor_kind === "agent" ? "agents" : "people";
		span.dataset.tip = p.actor_kind === "agent"
			? "Only runs an AI agent submitted match"
			: "Only runs a person submitted match";
		whoCell.appendChild(span);
	} else {
		const span = document.createElement("span");
		span.className = "muted";
		span.textContent = "any";
		whoCell.appendChild(span);
	}
	tr.appendChild(whoCell);
	const riskCell = document.createElement("td");
	if (p.min_risk) {
		const badge = document.createElement("span");
		badge.className = "risk risk-" + p.min_risk;
		badge.textContent = p.min_risk + "+";
		badge.dataset.tip = "Only runs graded at least this risky match";
		riskCell.appendChild(badge);
	} else {
		const span = document.createElement("span");
		span.className = "muted";
		span.textContent = "any";
		riskCell.appendChild(span);
	}
	tr.appendChild(riskCell);
	const toolCell = document.createElement("td");
	if (p.tool) {
		const badge = document.createElement("span");
		badge.className = "tool-badge " + p.tool;
		badge.dataset.tool = p.tool;
		badge.textContent = p.tool;
		toolCell.appendChild(badge);
	} else {
		const span = document.createElement("span");
		span.className = "muted";
		span.textContent = "any";
		toolCell.appendChild(span);
	}
	tr.appendChild(toolCell);
	tr.appendChild(p.command_contains ? td(p.command_contains, "mono") : anyCell());
	tr.appendChild(p.inventory_id ? td(invByID[p.inventory_id] || p.inventory_id) : anyCell());
	tr.appendChild(p.queue ? td(p.queue, "mono") : anyCell());
	const destroyCell = document.createElement("td");
	if (p.max_destroy !== undefined && p.max_destroy !== null && p.max_destroy >= 0) {
		const span = document.createElement("span");
		span.className = "mono";
		span.textContent = "> " + p.max_destroy;
		span.dataset.tip = "A matching apply is held when its plan destroys more than this many resources";
		destroyCell.appendChild(span);
	} else {
		const span = document.createElement("span");
		span.className = "muted";
		span.textContent = "off";
		destroyCell.appendChild(span);
	}
	tr.appendChild(destroyCell);
	const dry = document.createElement("td");
	if (p.exclude_dry_run) {
		const chip = document.createElement("span");
		chip.className = "chip ok";
		chip.textContent = "yes";
		dry.appendChild(chip);
	} else {
		const span = document.createElement("span");
		span.className = "muted";
		span.textContent = "no";
		dry.appendChild(span);
	}
	tr.appendChild(dry);
	// Separation of duties reads at a glance, beside the other thing a rule does to a match.
	const distinct = document.createElement("td");
	if (p.require_distinct_approver) {
		const chip = document.createElement("span");
		chip.className = "chip ok";
		chip.textContent = "required";
		chip.dataset.tip = "The person who asks for a matching run cannot approve it";
		distinct.appendChild(chip);
	} else {
		const span = document.createElement("span");
		span.className = "muted";
		span.textContent = "any approver";
		distinct.appendChild(span);
	}
	// A rule that asks for the approver's reason says so in the same cell, since it is the other
	// thing a rule asks of whoever decides.
	if (p.require_reason) {
		const reason = document.createElement("span");
		reason.className = "chip ok";
		reason.textContent = p.require_reason === "always" ? "reason required" : "reason to deny";
		reason.dataset.tip = p.require_reason === "always"
			? "An approval and a denial must each carry the approver's reason"
			: "A denial must carry the approver's reason";
		distinct.appendChild(document.createTextNode(" "));
		distinct.appendChild(reason);
	}
	tr.appendChild(distinct);
}

async function loadPolicies() {
	const held = await heldRuns();
	try {
		const invByID = await fillInventorySelect(null);
		const data = await getJSON("/policies");
		const policies = data.policies || [];
		if (policies.length === 0) {
			showEmpty("No policies yet. A run an agent asks for is held for approval by default. " +
				"Add a rule to hold more, or an exemption to let a routine agent run through.");
			return;
		}
		const tbody = document.getElementById("policies");
		for (const p of policies) {
			const tr = document.createElement("tr");
			tr.appendChild(td(p.name));
			const effectCell = document.createElement("td");
			if (p.rego) {
				const chip = document.createElement("span");
				chip.className = "chip";
				chip.textContent = "rego";
				chip.dataset.tip = regoEffectTip(p.rego);
				effectCell.appendChild(chip);
			} else if (p.effect === "deny") {
				const chip = document.createElement("span");
				chip.className = "chip failed";
				chip.textContent = "deny";
				chip.dataset.tip = "A matching submission is refused outright and never created";
				effectCell.appendChild(chip);
			} else if (p.effect === "exempt") {
				const chip = document.createElement("span");
				chip.className = "chip";
				chip.textContent = "exempt";
				chip.dataset.tip = "A matching run an agent asked for goes ahead without the default " +
					"hold. Any rule that holds or denies it still does";
				effectCell.appendChild(chip);
			} else {
				const span = document.createElement("span");
				span.textContent = "hold";
				span.dataset.tip = "A matching run waits for a person to approve or reject it";
				effectCell.appendChild(span);
			}
			tr.appendChild(effectCell);
			if (p.rego) {
				appendRegoCells(tr, p);
			} else {
				appendCriteriaCells(tr, p, invByID);
			}
			const holding = document.createElement("td");
			const ruleHeld = heldByRule(held, p);
			if (ruleHeld > 0) {
				const link = document.createElement("a");
				// The link names the rule, quoted since rules are named in prose, so the count and
				// its destination finally agree: it used to open every held run on the install.
				link.href = "/ui/runs?q=" +
					encodeURIComponent('held_by:"' + (p.name || p.id) + '" status:pending_approval');
				link.textContent = ruleHeld === 1 ? "1 run waiting" : ruleHeld + " runs waiting";
				link.dataset.tip = "Click to see the runs this rule is holding";
				holding.appendChild(link);
			} else {
				holding.textContent = "nothing waiting";
				holding.className = "muted";
				holding.dataset.tip = "No run is currently held by this rule";
			}
			tr.appendChild(holding);
			tr.appendChild(tdTime(p.created_at));
			const actions = document.createElement("td");
			const del = document.createElement("button");
			del.className = "button danger";
	del.dataset.mutates = "true";
	del.dataset.tip = "Click to delete this permanently";
			del.textContent = "Delete";
			del.addEventListener("click", async (e) => {
				e.preventDefault();
				if (!window.confirm("Delete policy " + p.name + "?")) return;
				try {
					await authedDelete("/policies/" + p.id);
					removeRow(tr, "No policies yet. Add one to require approval for the runs it matches.");
				} catch (err) {
					setStatus("Delete failed: " + err.message);
				}
			});
			actions.appendChild(editButton(() => openPolicyEdit(p)));
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(del);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load policies: " + e.message);
	}
}

// sparkline builds a row of outcome ticks, oldest on the left, newest on the right. When run ids
// ride along, each tick links to its run, so no outcome is a dead end.
function sparkline(recent, runIDs) {
	const wrap = document.createElement("span");
	wrap.className = "spark";
	for (let i = recent.length - 1; i >= 0; i--) {
		const id = runIDs && runIDs[i];
		const tick = document.createElement(id ? "a" : "span");
		tick.className = "tick " + (recent[i] || "none");
		if (id) {
			tick.href = "/ui/runs/" + id;
			tick.dataset.tip = (recent[i] || "run") + ": open this run";
		} else {
			tick.title = recent[i];
		}
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

// fetchAuthed requests one of the run's own byte streams with the token in an Authorization header
// and returns the response, sending the browser to sign in on a 401.
async function fetchAuthed(path) {
	const res = await fetch(API + path, { headers: authHeaders() });
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	if (!res.ok) {
		// Same reason getJSON reads the body: a 403 here is the licensing explanation with its
		// pricing link, and "HTTP 403" threw away both the reason and the one upsell this page
		// exists to make.
		const data = await res.clone().json().catch(() => ({}));
		const err = new Error(data.error || ("the server refused with HTTP " + res.status));
		// A caller that wants to tell a licensing refusal apart from a real failure needs the
		// status. Without it the one upsell the audit page exists to make rendered as "Could not
		// build the evidence pack".
		err.status = res.status;
		throw err;
	}
	return res;
}

// wireRunDownloads points the full log and the event export at the run's own bytes, fetched with
// the token in a header and handed to the browser as a local blob.
//
// The two links used to be href attributes built by streamURL, which carries the bearer token as a
// query parameter because EventSource cannot set headers. An href has no such excuse and three ways
// to leak: the live token sits in the DOM for anything on the page to read, it goes into the
// address bar and the history of whatever tab the link opens, and "copy link address" hands the
// whole credential to whoever the reader pastes it to. A token pasted into a ticket is a token
// someone else can use until it is revoked.
function wireRunDownloads(runId) {
	const fullLog = document.getElementById("full-log");
	if (fullLog) {
		fullLog.dataset.tip = "Click to open the full log in a new tab";
		fullLog.addEventListener("click", async (e) => {
			e.preventDefault();
			try {
				const text = await (await fetchAuthed("/runs/" + runId + "/logs")).text();
				const url = URL.createObjectURL(new Blob([text], { type: "text/plain" }));
				// A blocked popup would otherwise swallow the log silently, so it is saved instead.
				//
				// The handle is what decides, not its truthiness. window.open returns null whenever
				// noopener is set, per the spec, so testing the handle fired the fallback on every
				// successful open too: the tab appeared AND an unrequested .log landed in Downloads,
				// silently, on every click. Opening without noopener keeps a usable handle; the blob
				// URL is same-origin and carries no token, so there is nothing for the opened
				// document to reach back for.
				const opened = window.open(url, "_blank");
				if (!opened) {
					downloadBlob("switchtender-" + runId + ".log", "text/plain", text);
					setStatus("The log opened as a download: your browser blocked the new tab.");
				}
				// The new tab reads the blob synchronously on open, so the handle is released on the
				// next turn rather than held for the life of the page.
				window.setTimeout(() => URL.revokeObjectURL(url), 60000);
			} catch (err) {
				setStatus("Could not open the log: " + err.message);
			}
		});
	}
	const exportEvidence = document.getElementById("export-evidence");
	// The dossier quotes the audit trail, approver identities included, so the server serves it to an
	// admin and to the actor who launched the run. The control starts where the trail itself would
	// show, an admin session or an open instance with no accounts, and offerOwnEvidence adds the
	// launcher once the run is read.
	if (exportEvidence) {
		exportEvidence.hidden = !roleAtLeast("admin");
		exportEvidence.dataset.tip =
			"Click to download this run's evidence dossier, one self-contained document for an auditor";
		exportEvidence.addEventListener("click", async (e) => {
			e.preventDefault();
			try {
				const text = await (await fetchAuthed("/runs/" + runId + "/evidence")).text();
				downloadBlob("switchtender-" + runId + "-evidence.html", "text/html", text);
			} catch (err) {
				// A token session carries no stored role, so the control cannot know in advance
				// that the server will refuse. Saying which rule refused, and retiring the button,
				// beats leaving a control that fails the same way on every click.
				// fetchAuthed attaches the status to the error and puts the server's own sentence
				// in the message, so the status is what to match. Matching a fixed message string
				// made this branch unreachable: the control stayed on screen and failed the same
				// way on every click, which is exactly what it was written to replace.
				if (err.status === 403) {
					exportEvidence.hidden = true;
					setStatus("A run's evidence is readable by an admin or by whoever launched it.");
					return;
				}
				setStatus("Could not export the evidence: " + err.message);
			}
		});
	}
	// The signed receipt is the artifact the whole claim rests on, and it was reachable only from a
	// shell on the server: the run on screen could be read and exported as a dossier, but not turned
	// into the one file a third party checks without trusting this install. It reads the same trail
	// the dossier does, so it shows where the dossier does.
	const receiptBtn = document.getElementById("download-receipt");
	if (receiptBtn) {
		receiptBtn.hidden = !roleAtLeast("admin");
		receiptBtn.dataset.tip =
			"Click to download this run's signed receipt, verifiable offline with switchtender verify";
		receiptBtn.addEventListener("click", async () => {
			receiptBtn.disabled = true;
			try {
				const res = await fetchAuthed("/runs/" + runId + "/receipt");
				const key = res.headers.get("Switchtender-Key-Id") || "";
				downloadBlob("switchtender-" + runId + ".receipt", "application/json", await res.text());
				setStatus(key
					? "Receipt downloaded. Verify it with: switchtender verify the file --pubkey " + key
					: "Receipt downloaded. Verify it with: switchtender verify the file");
			} catch (err) {
				// A run still going, or one the scheduler started before its fire was recorded, has
				// nothing to attest yet. That is the ordinary case, so it reads as a state rather
				// than as a failure.
				// An install that cannot sign is a fact about the install, not a failed download.
				//
				// A shared-database install will not mint a signing key, deliberately: every process
				// has to sign as the same install, so a key made by one pod would be that pod's
				// alone. Such an install serves no receipt at all, and this read "Could not download
				// the receipt", which describes a transient failure and sends the reader to retry.
				// It is a configuration gap with a name and a fix, so it says both, and the control
				// stops offering what this install cannot do.
				if (err.status === 403) {
					receiptBtn.hidden = true;
					setStatus("A run's receipt is readable by an admin or by whoever launched it.");
					return;
				}
				if (String(err.message).includes("no signing identity")) {
					receiptBtn.disabled = true;
					receiptBtn.dataset.tip = "This install has no signing identity, so it cannot " +
						"sign a receipt.";
					setStatus("This install has no signing identity, so it cannot sign a receipt. " +
						"An install that shares one database will not create a key by itself, " +
						"because every process has to sign as the same install: generate one seed " +
						"and set SWITCHTENDER_AUDIT_KEY on every server and worker. The " +
						"configuration guide covers it.");
					return;
				}
				setStatus(err.message === "HTTP 409"
					? "This run has nothing to attest yet: a receipt covers a run that has finished."
					: "Could not download the receipt: " + err.message);
			} finally {
				receiptBtn.disabled = false;
			}
		});
	}
	const exportEvents = document.getElementById("export-events");
	if (exportEvents) {
		exportEvents.dataset.tip = "Click to download every event as newline-delimited JSON";
		exportEvents.addEventListener("click", async (e) => {
			e.preventDefault();
			try {
				const text = await (await fetchAuthed("/runs/" + runId + "/events?download=1")).text();
				downloadBlob("switchtender-" + runId + "-events.ndjson", "application/x-ndjson", text);
			} catch (err) {
				setStatus("Could not export the events: " + err.message);
			}
		});
	}
}

// offerOwnEvidence shows a run's Evidence and receipt controls to the actor who launched it. The
// server serves both to an admin or to that actor, and the page used to draw them for admins only, so
// an operator could not reach the evidence for their own change from the page that shows it.
function offerOwnEvidence(run) {
	if (!run || roleAtLeast("admin") || !signedInAs(run.actor)) return;
	for (const id of ["export-evidence", "download-receipt"]) {
		const el = document.getElementById(id);
		if (el) el.hidden = false;
	}
}

// loadDetail loads one run and dispatches to the split or single render path.
async function loadDetail(runId) {
	wireRunDownloads(runId);
	const auditLink = document.getElementById("audit-link");
	// The audit page is management data even to read, so the link only shows where it would answer.
	if (auditLink && !roleAtLeast("admin")) {
		auditLink.hidden = true;
	} else if (auditLink) {
		// The receipt is added once the run is read, below. The id alone cannot find the run's own
		// creation entry: that entry is written in middleware before the handler runs, so the path
		// it commits is the collection, /v1/runs, without an id the server does not yet know.
		auditLink.href = "/ui/audit?q=" + encodeURIComponent(runId);
		auditLink.dataset.tip = "Click to see this run's chain entries, its creation included";
	}
	const copyLink = document.getElementById("copy-link");
	if (copyLink) {
		copyLink.dataset.tip = "Click to copy a link to this run";
		copyLink.addEventListener("click", async () => {
			try { await navigator.clipboard.writeText(location.href); } catch { return; }
			copyLink.textContent = "Copied";
			window.setTimeout(() => { copyLink.textContent = "Copy link"; }, 1200);
		});
	}
	const exportResults = document.getElementById("export-results");
	if (exportResults) {
		exportResults.dataset.tip = "Click to download this run and its per-host results as JSON";
		exportResults.addEventListener("click", () => {
			if (!detailState || !detailState.run) return;
			// Read the folded model rather than the raw events: it already carries every host, task,
			// and outcome, and it is the only copy the page keeps once a run is loaded.
			const results = {};
			const cells = (detailState.model && detailState.model.cells) || {};
			for (const host of Object.keys(cells)) {
				for (const task of Object.keys(cells[host])) {
					const cell = cells[host][task];
					if (!results[host]) results[host] = {};
					results[host][task] = { outcome: cell.outcome, rc: cell.rc ?? undefined };
				}
			}
			const payload = { run: detailState.run, results, exported_at: new Date().toISOString() };
			downloadBlob("switchtender-" + detailState.runId + ".json", "application/json",
				JSON.stringify(payload, null, 2) + "\n");
		});
	}
	wireActions(runId);
	wireLogFilter();
	wireLogDownload(runId);
	window.setInterval(() => {
		for (const el of document.querySelectorAll(".value.ticking")) {
			el.textContent = fmtDuration(el.dataset.started, new Date().toISOString());
		}
	}, 1000);
	let run = null;
	try {
		run = await getJSON("/runs/" + runId);
		// Filtering the trail by the run id matched only entries written after the run existed, so
		// a run still held for approval matched none of them and its Audit trail button opened an
		// empty table while the header beside it named the very chain entry that created it. The
		// receipt carries that sequence, so the link passes both and the page keeps either.
		if (auditLink && !auditLink.hidden && run.audit_receipt) {
			auditLink.href = "/ui/audit?q=" + encodeURIComponent(runId) +
				"&seq=" + encodeURIComponent(String(run.audit_receipt).split(":")[0]);
		}
		offerRerun(run);
		offerOwnEvidence(run);
		// The decisions and the reasons approvers gave, drawn beside the run without holding up the
		// rest of the page.
		if (decisionsMatter(run)) loadDecisions(runId);
		// A split or pipeline parent has no output of its own; each shard or step carries its log
		// and events. Hiding the links beats serving blanks.
		const isParent = !run.parent_id && (run.kind === "pipeline" || run.kind === "split" || run.shard_count);
		// Looked up here rather than shared with wireRunDownloads: that function owns the click
		// wiring and its own references, and reaching into its scope from here is what broke this
		// page once already.
		const fullLogLink = document.getElementById("full-log");
		const exportEventsLink = document.getElementById("export-events");
		if (fullLogLink && isParent) fullLogLink.hidden = true;
		if (exportEventsLink && isParent) exportEventsLink.hidden = true;
		if (run.kind === "pipeline" && !run.parent_id) {
			await loadPipeline(runId);
		} else if ((run.kind === "split" || run.shard_count) && !run.parent_id) {
			await loadParent(runId);
		} else {
			await loadSingle(run);
		}
	} catch (e) {
		if (run) {
			showDrawFault(run, e);
			return;
		}
		setStatus("Failed to load run: " + e.message);
		// Nothing loaded, so every action on this page acts on nothing. They used to stay enabled:
		// eight live buttons under one grey line, one of which produced no download, no message,
		// and no change at all when clicked, because the state they read was never populated. A
		// run id that does not exist is reachable by an ordinary click, so the page says so and
		// offers the way back rather than a row of controls that cannot work.
		const actions = document.querySelector("main.content .actions");
		if (actions) actions.hidden = true;
		const header = document.getElementById("run-header");
		if (header) header.hidden = true;
		showRunDeadEnd(runId, e.status === 404);
	}
}

// offerRerun shows the Run again button once a run can be replayed. It is called when the page loads
// and again when a run it is watching ends: decided once at load, the button never appeared for a
// run that finished while its page was open, so the one run somebody had just watched was the one
// they could not run again without a reload. The click is wired once however often it is called.
function offerRerun(run) {
	const rerun = document.getElementById("rerun-run");
	// A rejected run, and one canceled before it ever started, are decisions not to run it. The
	// API refuses to replay either, so the button is not offered for them.
	const decided = run.status === "rejected" ||
		(run.status === "canceled" && !run.started_at);
	// The server refuses to replay a run that has not finished, so the button waits for the
	// terminal state instead of offering a click whose only future is a refusal.
	if (!rerun || run.parent_id || run.kind === "pipeline" || decided ||
		!isTerminal(run.status) || !roleAtLeast("operator")) {
		return;
	}
	rerun.hidden = false;
	rerun.dataset.tip = "Click to start a fresh run with this exact spec";
	if (isReadOnly()) {
		rerun.disabled = true;
		rerun.dataset.tip = readOnlyReason();
		return;
	}
	if (rerun.dataset.wired) return;
	rerun.dataset.wired = "1";
	rerun.addEventListener("click", async () => {
		rerun.disabled = true;
		try {
			const created = await postAction("/runs/" + run.id + "/rerun");
			location.href = "/ui/runs/" + created.id;
		} catch (err) {
			setStatus("Rerun failed: " + err.message);
			rerun.disabled = false;
		}
	});
}

// showDrawFault reports a run that was read but could not be fully drawn. The run exists, so it
// must not be called missing, and its actions are what an approver came for: a fault drawing one
// header field used to hide Approve and Reject and tell the approver there was no such run.
function showDrawFault(run, e) {
	const header = document.getElementById("run-header");
	if (header && header.childElementCount) header.hidden = false;
	let message = "Part of this page could not be drawn: " + e.message + ". The run itself is intact.";
	try {
		updateActions(run);
	} catch (err) {
		message += " Its actions could not be drawn either: " + err.message + ".";
	}
	setStatus(message);
}

// showRunDeadEnd explains an unreadable run and offers somewhere to go, since the page it replaces
// has no other exit. Only a run the server says does not exist is called missing. Any other failure
// to read it, a server error or a lost connection, says so instead.
function showRunDeadEnd(runId, missing) {
	if (document.getElementById("run-deadend")) return;
	const host = document.querySelector("main.content");
	if (!host) return;
	const box = document.createElement("div");
	box.id = "run-deadend";
	box.className = "empty";
	const p = document.createElement("p");
	p.textContent = missing
		? "There is no run " + runId + " on this install. A link to it may be stale, or the run " +
			"may have been removed with the history it belonged to."
		: "Run " + runId + " could not be read just now. Reload to try again.";
	box.appendChild(p);
	const ways = document.createElement("p");
	const runs = document.createElement("a");
	runs.href = "/ui/runs";
	runs.textContent = "All runs";
	const audit = document.createElement("a");
	audit.href = "/ui/audit";
	audit.textContent = "the audit trail";
	ways.appendChild(document.createTextNode("Try "));
	ways.appendChild(runs);
	ways.appendChild(document.createTextNode(", or "));
	ways.appendChild(audit);
	ways.appendChild(document.createTextNode(", which records runs that no longer exist."));
	box.appendChild(ways);
	host.appendChild(box);
}

// postAction sends a POST to the API and returns the parsed JSON body, throwing on an error reply.
async function postAction(path, payload, method) {
	const opts = { method: method || "POST", headers: authHeaders() };
	if (payload !== undefined) {
		opts.headers["Content-Type"] = "application/json";
		opts.body = JSON.stringify(payload);
	}
	const res = await fetch(API + path, opts);
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	if (res.status === 204) {
		return {};
	}
	const body = await res.json().catch(() => ({}));
	if (!res.ok) {
		const err = new Error(body.error || ("HTTP " + res.status));
		// A launch refused because its inventory matched no hosts names the page that previews the
		// inventory, so the caller can link to it.
		if (typeof body.preview_url === "string") err.previewURL = body.preview_url;
		throw err;
	}
	return body;
}

// streamURL puts a short-lived ticket on a stream path, since EventSource cannot set headers.
//
// It used to put the reader's own bearer token there. The URL was always opened and dropped, never
// written into an href, but a URL is not private once it leaves the browser: nginx, Traefik, and an
// ALB all log the full request line by default, so a thirty-day session credential ended up in
// access logs and everything downstream of them. A ticket in the same position opens one run, once,
// and dies in thirty seconds.
//
// The ticket is minted over the ordinary header-authenticated route, so nothing about who may watch
// a run changes. When there is no token at all the install is running open and the plain path works.
async function streamURL(path, runId) {
	if (!apiToken()) return API + path;
	const sep = path.includes("?") ? "&" : "?";
	try {
		const res = await postAction("/runs/" + encodeURIComponent(runId) + "/stream-ticket", {});
		if (res && res.ticket) {
			return API + path + sep + "ticket=" + encodeURIComponent(res.ticket);
		}
	} catch (err) {
		// Fall through: the stream will answer 401 and the indicator will show it, which is a
		// clearer failure than opening a stream that silently receives nothing.
	}
	return API + path;
}

// lastSeq returns the highest store sequence among events, or zero when none carry one. It is
// the cursor a live stream resumes from, so the browser never re-receives history it has.
function lastSeq(events) {
	let max = 0;
	for (const e of events) {
		if (e.seq && e.seq > max) max = e.seq;
	}
	return max;
}

// loadAllEvents fetches a run's full event history in bounded pages, following the nextAfter
// cursor, so one request never carries an unbounded log. A split run pages each shard this way.
async function loadAllEvents(runId) {
	const batch = 5000;
	let after = 0;
	const events = [];
	for (;;) {
		const data = await getJSON("/runs/" + runId + "/events?after=" + after + "&limit=" + batch);
		const page = data.events || [];
		for (const e of page) events.push(e);
		if (page.length < batch) break;
		// The cursor has to move, or the next request is the one just answered. A response missing
		// next_after asked for "after=undefined" forever: the run never rendered and the server was
		// asked for the same five thousand events without end. A cursor that cannot advance ends
		// the paging with what has been read rather than looping on it.
		const next = data.next_after;
		if (typeof next !== "number" || !(next > after)) break;
		after = next;
	}
	return events;
}

// offerStoredSession shows the way back when this browser already holds a session, so arriving at
// sign in is not a dead end. A reader who followed a link here, or whose one expired call sent them
// here while the rest of the session still works, had no route back to the page they came from and no
// indication which account the browser was holding.
function offerStoredSession() {
	const back = document.getElementById("signed-in-return");
	if (!back) return;
	if (!apiToken()) {
		back.hidden = true;
		return;
	}
	const name = localStorage.getItem("st_user") || "";
	const label = document.getElementById("signed-in-name");
	if (label) label.textContent = name ? "as " + name : "with a token";
	back.hidden = false;
	const leave = document.getElementById("signed-in-out");
	if (leave) leave.addEventListener("click", () => signOut());
}

// loadLogin wires both sign in forms: account login mints a session token, and the raw token
// form verifies a pasted token against the API.
function loadLogin() {
	offerStoredSession();
	const ssoErr = ssoError();
	if (ssoErr) {
		setStatus(ssoErr);
		history.replaceState(null, "", location.pathname + location.search);
	}
	document.getElementById("account-form").addEventListener("submit", async (e) => {
		e.preventDefault();
		try {
			const res = await fetch(API + "/auth/login", {
				method: "POST",
				headers: { "Content-Type": "application/json" },
				body: JSON.stringify({
					username: document.getElementById("username-input").value.trim(),
					password: document.getElementById("password-input").value,
				}),
			});
			if (!res.ok) {
				// Only a 401 means the credentials were wrong. Reading a database outage or a 500
				// as "check the username and password" sent people to reset passwords that worked.
				setStatus(res.status === 401
					? "Sign in failed. Check the username and password."
					: "Sign in failed: the server answered HTTP " + res.status + ". Try again, and " +
						"check the server log if it persists.");
				return;
			}
			const session = await res.json();
			localStorage.setItem("st_token", session.token);
			localStorage.setItem("st_role", session.role);
			localStorage.setItem("st_user", session.username);
			location.href = sessionStorage.getItem("st_return") || "/ui/";
		} catch (err) {
			setStatus("Sign in failed: " + err.message);
		}
	});
	document.getElementById("login-form").addEventListener("submit", async (e) => {
		e.preventDefault();
		const token = document.getElementById("token-input").value.trim();
		if (!token) return;
		const res = await fetch(API + "/auth/check", {
			method: "POST", headers: { "Authorization": "Bearer " + token },
		});
		if (res.status === 204) {
			localStorage.setItem("st_token", token);
			localStorage.removeItem("st_role");
			localStorage.removeItem("st_user");
			// The server knows who this token is; asking beats guessing. Without the answer every
			// role-gated control treated a token session as admin and drew buttons that could only
			// 403. An older server without the endpoint just leaves the role unknown, as before.
			try {
				const meRes = await fetch(API + "/auth/me", {
					headers: { "Authorization": "Bearer " + token },
				});
				if (meRes.ok) {
					const me = await meRes.json();
					if (me && me.role) localStorage.setItem("st_role", me.role);
					if (me && me.name) localStorage.setItem("st_user", me.name);
				}
			} catch (_) { /* the role stays unknown and the server still enforces */ }
			location.href = sessionStorage.getItem("st_return") || "/ui/";
			return;
		}
		setStatus("That token was not accepted.");
	});
}
