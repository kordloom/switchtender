// auditCollections maps the first path segment to what one of its members is called, so a recorded
// request reads as the change it made rather than as the route that made it.
const auditCollections = {
	runs: "run", templates: "template", projects: "project", inventories: "inventory",
	credentials: "credential", schedules: "schedule", users: "user", policies: "approval policy",
	"inventory-sources": "inventory source", pipelines: "pipeline", grants: "grant",
	teams: "team", orgs: "organization", tokens: "API token", triggers: "webhook trigger",
	hooks: "webhook", restore: "restore", backup: "backup", import: "import",
};

// auditVerbs maps an HTTP method to what it did, in the words somebody reading an audit uses.
const auditVerbs = { POST: "Created", PUT: "Updated", PATCH: "Updated", DELETE: "Deleted" };

// auditActions names the trailing path segments that are the change itself rather than a field of
// one, so a release reads as a release instead of as an update to the run it released.
const auditActions = {
	approve: "Approved", reject: "Rejected", cancel: "Canceled", stop: "Stopped",
	rerun: "Reran", retry: "Retried", launch: "Launched", trigger: "Triggered",
	"rotate-secret": "Rotated the secret on", reconcile: "Reconciled drift on",
};

// auditCLI names what a command-line change did. These arrive with a CLI method and a /cli path.
const auditCLI = {
	restore: "Restored this install from a backup",
	backup: "Took a backup",
	"audit/anchor": "Anchored the audit chain",
	"import/apply": "Applied an import",
};

// auditChange turns a recorded method and path into a sentence.
//
// The chain hashes the actor, the method, and the path, because those are what the server knows for
// certain at the moment it allows a request, and a stored description could drift from what actually
// happened. That makes the raw pair the truth and a poor thing to read: an auditor should not have
// to join two columns and know REST to learn that a template was deleted. The sentence is derived
// here for reading, beside the pair it came from, and never in place of it.
function auditChange(method, path) {
	const clean = String(path || "").replace(/^\/v1/, "").replace(/^\//, "");
	const parts = clean.split("/").filter(Boolean);
	if (parts.length === 0) {
		return "";
	}
	if (parts[0] === "cli") {
		const rest = parts.slice(1).join("/");
		return auditCLI[rest] || "Ran " + rest.replace(/[/-]/g, " ") + " from the command line";
	}
	if (parts[0] === "hooks") {
		return "Fired a webhook trigger";
	}
	// The chain's own entry kinds read as sentences, not as lowercase path fragments: a decision
	// binding a spec, a schedule firing, and a run's committed outcome are the entries an auditor
	// reads first.
	if (method === "DECISION") {
		const verdict = parts[parts.length - 1];
		const runID = parts[1] || "";
		if (verdict === "approved") return "Approved run " + runID + ", binding its spec digest";
		if (verdict === "rejected") return "Rejected run " + runID;
		return "Decided run " + runID;
	}
	if (method === "SCHEDULE") {
		return "Schedule " + (parts[1] || "") + " fired";
	}
	if (method === "RUN") {
		return "Run " + (parts[1] || "") + " finished " +
			String(parts[parts.length - 1] || "").replace(/_/g, " ");
	}
	if (parts[0] === "auth") {
		return parts.includes("acs") || parts.includes("callback") || parts.includes("login")
			? "Signed in" : "Changed authentication";
	}
	const noun = auditCollections[parts[0]] || parts[0].replace(/-/g, " ");
	// A trailing action is the change, not a field of one.
	if (parts.length > 2) {
		const tail = parts[parts.length - 1];
		const action = auditActions[tail];
		return action
			? action + " " + noun + " " + parts[1]
			: tail.replace(/-/g, " ") + " on " + noun + " " + parts[1];
	}
	const verb = auditVerbs[String(method || "").toUpperCase()];
	if (!verb) {
		return String(method || "") + " on " + noun;
	}
	return parts.length === 2 ? verb + " " + noun + " " + parts[1] : verb + " " + noun;
}

// auditChangeCell renders the sentence, linking the object it names to the page that shows it.
function auditChangeCell(method, path) {
	const cell = td("");
	const text = auditChange(method, path);
	const idMatch = text.match(/\b([a-z]{3,4}_[a-z0-9_]+)\b/);
	if (!idMatch) {
		cell.textContent = text;
		return cell;
	}
	const id = idMatch[1];
	const prefix = Object.keys(auditObjectPages).find((k) => id.startsWith(k));
	if (!prefix) {
		cell.textContent = text;
		return cell;
	}
	const before = text.slice(0, idMatch.index);
	const after = text.slice(idMatch.index + id.length);
	cell.appendChild(document.createTextNode(before));
	const a = document.createElement("a");
	a.href = prefix === "run_" ? auditObjectPages[prefix] + id : auditObjectPages[prefix];
	a.textContent = id;
	a.dataset.tip = "Open the " + (auditCollections[String(path).split("/")[2]] || "object")
		+ " this entry changed";
	cell.appendChild(a);
	cell.appendChild(document.createTextNode(after));
	return cell;
}

// loadAudit fills the audit table with the trail, newest first, showing each entry's chain hash.
// auditPathCell renders an audit path with any run reference linked to its run page.
function auditPathCell(path) {
	const cell = td("", "mono");
	for (const part of String(path || "").split(/(run_[a-z0-9]+)/)) {
		if (/^run_[a-z0-9]+$/.test(part)) {
			const a = document.createElement("a");
			a.href = "/ui/runs/" + part;
			a.textContent = part;
			cell.appendChild(a);
		} else if (part) {
			cell.appendChild(document.createTextNode(part));
		}
	}
	return cell;
}

// AUDIT_PAGE is how many entries the audit table asks the server for. The answer carries has_more
// when the trail runs past it, which is the only thing that tells this page it is holding a page
// rather than the whole trail.
const AUDIT_PAGE = 500;

// markTrailTruncated says on the page that the table is one page of the trail, and turns off the
// table exports. A CSV or JSON taken from a cut table is an audit artifact that looks complete and
// says nothing about the changes it left out, so the button is refused rather than answered with a
// partial file. The signed export beside it still covers the whole chain.
function markTrailTruncated(shown) {
	const table = document.querySelector("main.content table.runs");
	if (table && !document.getElementById("audit-truncated")) {
		const notice = document.createElement("div");
		notice.id = "audit-truncated";
		notice.className = "trail-notice";
		notice.textContent = "Showing the " + shown + " most recent entries. The trail holds more "
			+ "than this, so the table exports are off. Use Export signed for the whole chain.";
		table.parentNode.insertBefore(notice, table);
	}
	for (const btn of document.querySelectorAll("button.table-export")) {
		btn.disabled = true;
		btn.dataset.tip = "Off: this table shows only the newest " + shown
			+ " entries, so the export would leave the rest out. Use Export signed.";
	}
}

async function loadAudit() {
	try {
		// A ?q= arrival is a search, usually a run id from the run page's Audit trail button, so
		// the page asks for the server's maximum window instead of its display default: a run
		// older than the default page filtered to an empty table with nothing explaining why.
		const preset = new URLSearchParams(location.search).get("q");
		const data = await getJSON("/audit?limit=" + (preset ? 1000 : AUDIT_PAGE));
		const entries = data.entries || [];
		if (entries.length === 0) {
			showEmpty("No audit entries yet. Every change is recorded here.");
			return;
		}
		const tbody = document.getElementById("audit");
		for (const e of entries) {
			const tr = document.createElement("tr");
			tr.appendChild(td(String(e.seq)));
			tr.appendChild(tdTime(e.at));
			tr.appendChild(td(e.actor || "-"));
			tr.appendChild(td(e.actor_type || "-"));
			tr.appendChild(td(e.on_behalf_of || "-"));
			tr.appendChild(auditChangeCell(e.method, e.path));
			tr.appendChild(td(e.method, "mono"));
			tr.appendChild(auditPathCell(e.path));
			tr.appendChild(td((e.hash || "").slice(0, 12), "mono"));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
		if (data.has_more) {
			markTrailTruncated(entries.length);
			if (preset) {
				const notice = document.getElementById("audit-truncated");
				if (notice) {
					notice.textContent += " A search only covers this window, so older entries " +
						"mentioning \"" + preset + "\" may exist beyond it. A run's receipt " +
						"(switchtender receipt) carries its own chain entries whole.";
				}
			}
		}
	} catch (e) {
		setStatus("Failed to load the audit trail: " + e.message);
	}
}

// wireAudit hooks the audit page's three buttons. Verify recomputes the chain and shows a badge,
// the evidence pack renders the period's change register, and bundle downloads a signed LoomSeal
// bundle anyone can verify offline with an open verifier.
function wireAudit() {
	const badge = document.getElementById("audit-badge");
	const verify = document.getElementById("audit-verify");
	if (verify) {
		verify.addEventListener("click", async () => {
			badge.hidden = false;
			badge.className = "chip none";
			badge.textContent = "Verifying...";
			try {
				const r = await getJSON("/audit/verify");
				if (r.ok) {
					badge.className = "chip ok";
					badge.textContent = "Chain verified: " + r.count + " entries";
				} else {
					badge.className = "chip failed";
					badge.textContent = "Tampered at entry " + r.broke_at;
				}
			} catch (err) {
				badge.className = "chip failed";
				badge.textContent = "Verify failed: " + err.message;
			}
		});
	}
	const reg = document.getElementById("audit-register");
	if (reg) {
		reg.dataset.tip = "Click to download the last 90 days as a change register, the evidence a compliance review samples from";
		reg.addEventListener("click", async () => {
			reg.disabled = true;
			try {
				const text = await (await fetchAuthed("/audit/register")).text();
				downloadBlob("switchtender-change-register.html", "text/html", text);
			} catch (err) {
				setStatus("Could not build the evidence pack: " + err.message);
			} finally {
				reg.disabled = false;
			}
		});
	}
	const bundle = document.getElementById("audit-bundle");
	if (bundle) {
		bundle.dataset.tip = "Download the signed LoomSeal bundle: the whole chain and its anchors in one file, verifiable offline with an open tool and no trust in this server";
		bundle.addEventListener("click", async () => {
			bundle.disabled = true;
			try {
				// The bundle is signed bytes served as-is. It is fetched as raw text and downloaded
				// unchanged, never parsed and re-encoded, because re-encoding would change the bytes the
				// signature covers and a verifier would then reject a bundle this install really signed.
				const res = await fetch(API + "/audit/bundle", { headers: authHeaders() });
				if (res.status === 401) {
					requireLogin();
					return;
				}
				if (!res.ok) {
					let msg = "HTTP " + res.status;
					try {
						const e = await res.json();
						if (e && e.error) msg = e.error;
					} catch (_) { /* a non-JSON error body leaves the status message. */ }
					throw new Error(msg);
				}
				downloadBlob("switchtender-audit.loomseal.json", "application/json", await res.text());
			} catch (err) {
				setStatus("Could not build the bundle: " + err.message);
			} finally {
				bundle.disabled = false;
			}
		});
	}
}

// tdTime builds a table cell showing a relative age with the full timestamp on hover. It carries
// the timestamp so a background timer can keep the age current.
function tdTime(iso, empty) {
	const el = td(iso ? relTime(iso) : (empty || ""));
	if (iso) {
		el.title = fmtTime(iso);
		el.dataset.time = iso;
		el.classList.add("reltime");
	}
	return el;
}

// refreshRelTimes rewrites every relative-age cell so ages stay current without a reload.
function refreshRelTimes() {
	for (const el of document.querySelectorAll(".reltime[data-time]")) {
		el.textContent = relTime(el.dataset.time);
	}
}

// wireModal wires a create dialog: the open button shows it; the close button, a backdrop click, and
// Escape hide it. name is the shared id prefix for the open, modal, and close elements.
function wireModal(name) {
	const openBtn = document.getElementById(name + "-open");
	const modal = document.getElementById(name + "-modal");
	if (!openBtn || !modal) return;
	const card = modal.querySelector(".modal-card");
	if (card) {
		card.setAttribute("role", "dialog");
		card.setAttribute("aria-modal", "true");
	}
	let opener = null;
	const close = () => {
		modal.hidden = true;
		if (opener && opener.focus) opener.focus();
	};
	openBtn.addEventListener("click", () => {
		opener = document.activeElement;
		modal.hidden = false;
		const first = modal.querySelector("input, select, textarea") ||
			modal.querySelector("button:not(.modal-close)");
		if (first) first.focus();
	});
	const closeBtn = document.getElementById(name + "-close");
	if (closeBtn) closeBtn.addEventListener("click", close);
	modal.addEventListener("click", (e) => { if (e.target === modal) close(); });
	document.addEventListener("keydown", (e) => { if (e.key === "Escape" && !modal.hidden) close(); });
	modal.addEventListener("keydown", (e) => {
		if (e.key !== "Tab" || modal.hidden) return;
		const focusable = modal.querySelectorAll(
			"a[href], button:not([disabled]), input:not([disabled]), " +
			"select:not([disabled]), textarea:not([disabled])");
		if (focusable.length === 0) return;
		const first = focusable[0];
		const last = focusable[focusable.length - 1];
		if (e.shiftKey && document.activeElement === first) {
			e.preventDefault();
			last.focus();
		} else if (!e.shiftKey && document.activeElement === last) {
			e.preventDefault();
			first.focus();
		}
	});
}

