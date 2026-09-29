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

async function loadAudit() {
	try {
		const data = await getJSON("/audit?limit=500");
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
			tr.appendChild(auditChangeCell(e.method, e.path));
			tr.appendChild(td(e.method, "mono"));
			tr.appendChild(auditPathCell(e.path));
			tr.appendChild(td((e.hash || "").slice(0, 12), "mono"));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load the audit trail: " + e.message);
	}
}

// wireAudit hooks the verify and export buttons. Verify recomputes the chain and shows a badge;
// export downloads the signed snapshot for offline verification.
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
	const exp = document.getElementById("audit-export");
	if (exp) {
		exp.addEventListener("click", async () => {
			try {
				const data = await getJSON("/audit/export");
				const blob = new Blob([JSON.stringify(data, null, 2)], { type: "application/json" });
				const url = URL.createObjectURL(blob);
				const a = document.createElement("a");
				a.href = url;
				a.download = "audit-export.json";
				a.click();
				URL.revokeObjectURL(url);
			} catch (err) {
				setStatus("Export failed: " + err.message);
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

