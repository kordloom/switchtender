// loadPolicies populates the policy table with edit and delete actions. Each empty criterion shows
// as "any", so a reader sees exactly how wide a rule is.
// heldRunCount returns how many runs are waiting for approval right now, so the policies list
// shows live consequence rather than only configuration.
async function heldRunCount() {
	try {
		const data = await getJSON("/runs?status=pending_approval&limit=200");
		return (data.runs || []).length;
	} catch {
		return 0;
	}
}

async function loadPolicies() {
	const heldCount = await heldRunCount();
	try {
		const invByID = await fillInventorySelect(null);
		const data = await getJSON("/policies");
		const policies = data.policies || [];
		if (policies.length === 0) {
			showEmpty("No policies yet. Add one to require approval for the runs it matches.");
			return;
		}
		const tbody = document.getElementById("policies");
		for (const p of policies) {
			const tr = document.createElement("tr");
			tr.appendChild(td(p.name));
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
			const holding = document.createElement("td");
			if (heldCount > 0) {
				const link = document.createElement("a");
				link.href = "/ui/runs?q=" + encodeURIComponent("status:pending_approval");
				link.textContent = heldCount === 1 ? "1 run waiting" : heldCount + " runs waiting";
				link.dataset.tip = "Click to see the runs held for approval";
				holding.appendChild(link);
			} else {
				holding.textContent = "nothing waiting";
				holding.className = "muted";
				holding.dataset.tip = "No run is currently held for approval";
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
					const res = await fetch(API + "/policies/" + p.id, { method: "DELETE", headers: authHeaders() });
					if (!res.ok) throw new Error("HTTP " + res.status);
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
// copyButton returns a small clipboard control that copies text and confirms with a checkmark.
function copyButton(text, tip) {
	const btn = document.createElement("button");
	btn.type = "button";
	btn.className = "copy-btn";
	btn.dataset.tip = tip;
	btn.setAttribute("aria-label", tip);
	const glyph = '<rect x="9" y="9" width="11" height="11" rx="2"/><path d="M5 15V5a2 2 0 0 1 2-2h10"/>';
	btn.innerHTML = svgIcon(glyph);
	btn.addEventListener("click", async () => {
		try { await navigator.clipboard.writeText(text); } catch { return; }
		btn.innerHTML = svgIcon('<polyline points="20 6 9 17 4 12"/>');
		btn.classList.add("copied");
		window.setTimeout(() => {
			btn.innerHTML = svgIcon(glyph);
			btn.classList.remove("copied");
		}, 1200);
	});
	return btn;
}

let detailState = null;

// fetchAuthed requests one of the run's own byte streams with the token in an Authorization header
// and returns the response, sending the browser to sign in on a 401.
async function fetchAuthed(path) {
	const res = await fetch(API + path, { headers: authHeaders() });
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	if (!res.ok) throw new Error("HTTP " + res.status);
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
				if (!window.open(url, "_blank", "noopener")) {
					downloadBlob("switchtender-" + runId + ".log", "text/plain", text);
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
	if (exportEvidence) {
		exportEvidence.dataset.tip =
			"Click to download this run's evidence dossier, one self-contained document for an auditor";
		exportEvidence.addEventListener("click", async (e) => {
			e.preventDefault();
			try {
				const text = await (await fetchAuthed("/runs/" + runId + "/evidence")).text();
				downloadBlob("switchtender-" + runId + "-evidence.html", "text/html", text);
			} catch (err) {
				setStatus("Could not export the evidence: " + err.message);
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

// loadDetail loads one run and dispatches to the split or single render path.
async function loadDetail(runId) {
	wireRunDownloads(runId);
	const auditLink = document.getElementById("audit-link");
	if (auditLink) {
		auditLink.href = "/ui/audit?q=" + encodeURIComponent(runId);
		auditLink.dataset.tip = "Click to see every audited change that mentions this run";
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
	window.setInterval(() => {
		for (const el of document.querySelectorAll(".value.ticking")) {
			el.textContent = fmtDuration(el.dataset.started, new Date().toISOString());
		}
	}, 1000);
	try {
		const run = await getJSON("/runs/" + runId);
		const rerun = document.getElementById("rerun-run");
		// A rejected run, and one canceled before it ever started, are decisions not to run it. The
		// API refuses to replay either, so the button is not offered for them.
		const decided = run.status === "rejected" ||
			(run.status === "canceled" && !run.started_at);
		if (rerun && !run.parent_id && run.kind !== "pipeline" && !decided) {
			rerun.hidden = false;
			rerun.dataset.tip = "Click to start a fresh run with this exact spec";
			if (isReadOnly()) {
				rerun.disabled = true;
				rerun.dataset.tip = "Disabled in the demo";
			} else {
				rerun.addEventListener("click", async () => {
					rerun.disabled = true;
					try {
						const created = await postAction("/runs/" + runId + "/rerun");
						location.href = "/ui/runs/" + created.id;
					} catch (err) {
						setStatus("Rerun failed: " + err.message);
						rerun.disabled = false;
					}
				});
			}
		}
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
		setStatus("Failed to load run: " + e.message);
	}
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
		throw new Error(body.error || ("HTTP " + res.status));
	}
	return body;
}

// streamURL appends the stored token to a stream path, since EventSource cannot set headers. It is
// for that one case only: a URL built here must be opened and dropped, never written into an href,
// where the credential would sit in the DOM and travel with anything the reader copies or shares.
function streamURL(path) {
	const token = apiToken();
	if (!token) return API + path;
	const sep = path.includes("?") ? "&" : "?";
	return API + path + sep + "access_token=" + encodeURIComponent(token);
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
		after = data.next_after;
	}
	return events;
}

// loadLogin wires both sign in forms: account login mints a session token, and the raw token
// form verifies a pasted token against the API.
function loadLogin() {
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
				setStatus("Sign in failed. Check the username and password.");
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
			location.href = sessionStorage.getItem("st_return") || "/ui/";
			return;
		}
		setStatus("That token was not accepted.");
	});
}

