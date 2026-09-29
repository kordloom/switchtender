async function loadInventories() {
	try {
		const data = await getJSON("/inventories");
		const inventories = data.inventories || [];
		if (inventories.length === 0) {
			showEmpty("No inventories yet.");
			return;
		}
		const tbody = document.getElementById("inventories");
		for (const i of inventories) {
			const tr = document.createElement("tr");
			tr.appendChild(td(i.name));
			const parsed = parseInventory(i.content);
			const fmt = td("");
			const fmtChip = document.createElement("span");
			fmtChip.className = "tool-badge";
			fmtChip.textContent = parsed.format;
			fmtChip.dataset.tip = parsed.format === "yaml"
				? "YAML inventory, read by Ansible's YAML plugin"
				: "INI inventory: groups in brackets, one host per line";
			fmt.appendChild(fmtChip);
			tr.appendChild(fmt);
			const count = parsed.hosts.length;
			const hostsCell = td(parsed.format === "yaml" ? "\u2014" : String(count));
			hostsCell.dataset.tip = parsed.format === "yaml"
				? "Host count is not estimated for YAML inventories"
				: "Counted from the stored content";
			tr.appendChild(hostsCell);
			const groupsCell = td(parsed.format === "yaml" ? "\u2014" : String(parsed.groups.length));
			groupsCell.dataset.tip = parsed.groups.length
				? "Groups: " + parsed.groups.join(", ")
				: "No groups declared, so every host is in the implicit all group";
			tr.appendChild(groupsCell);
			tr.appendChild(tdTime(i.created_at));
			const actions = deleteCell("/inventories/" + i.id, "inventory " + i.name, tr, "No inventories yet.");
			actions.insertBefore(editButton(() => openInventoryEdit(i), "Click to edit this inventory's hosts and groups"), actions.firstChild);
			tr.appendChild(actions);
			inspectable(tr, i.name, [
				{ label: "Format", value: parsed.format === "yaml" ? "YAML" : "INI" },
				{ label: "Hosts", value: parsed.format === "yaml" ? "not estimated for YAML" : parsed.hosts.join(", ") },
				{ label: "Groups", value: parsed.groups.join(", ") },
				{ label: "Size", value: (String(i.content || "").length) + " bytes" },
				{ label: "Created", value: fmtTime(i.created_at) },
				{ label: "ID", value: i.id, copy: true },
				{ label: "Content", value: i.content, block: true },
			], [
				{ label: "Edit", primary: true, mutates: true, tip: "Click to edit this inventory", onClick: () => { closeDrill(); openInventoryEdit(i); } },
				{ label: "Copy content", tip: "Copy the inventory to the clipboard", onClick: async () => {
					try { await navigator.clipboard.writeText(i.content || ""); } catch { /* denied */ }
				} },
				{ label: "Download", tip: "Download the inventory as a file", onClick: () =>
					downloadBlob(i.name.replace(/\s+/g, "-") + ".ini", "text/plain", i.content || "") },
			]);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load inventories: " + e.message);
	}
}

// openSourceEdit fills the source dialog with an existing record and switches it to edit mode so
// the next save issues a PUT rather than a create.
function openSourceEdit(src) {
	const form = document.getElementById("source-form");
	form.dataset.editId = src.id;
	document.getElementById("src-name").value = src.name;
	document.getElementById("src-source").value = src.source;
	document.getElementById("src-credential").value = src.credential_id || "";
	document.getElementById("src-project").value = src.project_id || "";
	document.getElementById("src-status").textContent = "";
	setModalTitle("source", "Edit source");
	document.getElementById("source-modal").hidden = false;
}

// wireSourceForm hooks the source dialog up to POST /inventory-sources for a new record and PUT
// /inventory-sources/{id} when editing. The New button resets the dialog to add mode.
function wireSourceForm() {
	fillSelect(document.getElementById("src-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	fillSelect(document.getElementById("src-project"), "/projects", "projects", (p) => p.name);
	const form = document.getElementById("source-form");
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("src-name").value = "";
		document.getElementById("src-source").value = "";
		document.getElementById("src-credential").value = "";
		document.getElementById("src-project").value = "";
		document.getElementById("src-status").textContent = "";
		setModalTitle("source", "Add a source");
	};
	const openBtn = document.getElementById("source-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still posting, so a fast double click on Save
	// stores the source once rather than twice. A modal save stays on the page, so the button is
	// re-enabled once the request settles either way, leaving the dialog usable for the next source.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("src-status");
		const editId = form.dataset.editId;
		const payload = {
			name: document.getElementById("src-name").value.trim(),
			source: document.getElementById("src-source").value.trim(),
			credential_id: document.getElementById("src-credential").value,
			project_id: document.getElementById("src-project").value,
		};
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/inventory-sources/" + editId, payload, "PUT");
			} else {
				await postAction("/inventory-sources", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("source");
			document.getElementById("sources").innerHTML = "";
			loadSources();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// loadSources populates the source table with refresh and delete actions.
async function loadSources() {
	try {
		const data = await getJSON("/inventory-sources");
		const sources = data.sources || [];
		if (sources.length === 0) {
			showEmpty("No inventory sources yet.");
			return;
		}
		const tbody = document.getElementById("sources");
		// The inventory each source maintains, so a row names its destination rather than an id.
		let invByID = new Map();
		try {
			const inv = await getJSON("/inventories");
			invByID = new Map((inv.inventories || []).map((i) => [i.id, i]));
		} catch { /* names fall back to ids */ }
		for (const src of sources) {
			const tr = document.createElement("tr");
			tr.appendChild(td(src.name));
			tr.appendChild(td(src.source, "mono"));
			const target = td("");
			if (src.inventory_id) {
				const link = document.createElement("a");
				link.href = "/ui/inventories";
				link.textContent = (invByID.get(src.inventory_id) || {}).name || shortId(src.inventory_id);
				link.dataset.tip = "This source refreshes that stored inventory. Open inventories";
				target.appendChild(link);
			} else {
				target.textContent = "\u2014";
			}
			tr.appendChild(target);
			const cadence = td("");
			const every = src.sync_interval_seconds
				? "every " + fmtInterval(src.sync_interval_seconds)
				: "on every launch";
			cadence.textContent = src.update_on_launch ? "Before launch, " + every : every;
			cadence.dataset.tip = src.update_on_launch
				? "Refreshed before a run targeting this inventory, and on the interval"
				: "Refreshed on the interval only, not before a launch";
			tr.appendChild(cadence);
			tr.appendChild(tdTime(src.synced_at, "never"));
			const state = document.createElement("td");
			const chip = document.createElement("span");
			chip.className = src.last_error ? "chip failed" : (src.synced_at ? "chip ok" : "chip none");
			chip.textContent = src.last_error ? "error" : (src.synced_at ? "synced" : "pending");
			if (src.last_error) chip.title = src.last_error;
			state.appendChild(chip);
			// A failure's reason belongs where a person and an export can both read it, not only
			// under a hover nothing on a phone can reach.
			if (src.last_error) {
				state.dataset.export = "error: " + src.last_error;
				const why = document.createElement("div");
				why.className = "muted";
				why.textContent = src.last_error;
				state.appendChild(why);
			}
			tr.appendChild(state);
			const actions = document.createElement("td");
			const refresh = document.createElement("button");
			refresh.className = "button primary";
			refresh.dataset.mutates = "true";
			refresh.dataset.tip = "Click to re-run this source and refresh its inventory now";
			refresh.textContent = "Refresh";
			refresh.addEventListener("click", async (e) => {
				e.preventDefault();
				refresh.disabled = true;
				try {
					await postAction("/inventory-sources/" + src.id + "/refresh");
					document.getElementById("sources").innerHTML = "";
					loadSources();
				} catch (err) {
					setStatus("Refresh failed: " + err.message);
					refresh.disabled = false;
				}
			});
			actions.appendChild(refresh);
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(editButton(() => openSourceEdit(src), "Click to edit this source's plugin and credentials"));
			actions.appendChild(document.createTextNode(" "));
			const del = deleteCell("/inventory-sources/" + src.id, "source " + src.name, tr,
				"No inventory sources yet.");
			actions.appendChild(del.firstChild);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load sources: " + e.message);
	}
}

// loadWorkers populates the executor table, marking anyone silent past the lease window stale.
async function loadWorkers() {
	try {
		const data = await getJSON("/workers");
		const workers = data.workers || [];
		if (workers.length === 0) {
			showEmpty("No executors seen yet. Run something.");
			return;
		}
		const tbody = document.getElementById("workers");
		for (const w of workers) {
			const tr = document.createElement("tr");
			tr.appendChild(td(w.owner, "mono"));
			const health = document.createElement("td");
			// An executor with no active leases has nothing to renew, so silence means idle, not
			// broken. Stale is reserved for a held lease whose renewals have stopped.
			const fresh = Date.now() - new Date(w.last_seen).getTime() < 30000;
			const chip = document.createElement("span");
			if (w.active > 0 && fresh) {
				chip.className = "chip ok";
				chip.textContent = "active";
			} else if (w.active > 0) {
				chip.className = "chip failed";
				chip.textContent = "stale";
				chip.title = "Holds runs but has stopped renewing its lease.";
				health.dataset.export = "stale: holds runs but has stopped renewing its lease";
				const why = document.createElement("div");
				why.className = "muted";
				why.textContent = "holds runs, lease renewals stopped";
				health.appendChild(why);
			} else {
				chip.className = "chip none";
				chip.textContent = "idle";
			}
			health.appendChild(chip);
			tr.appendChild(health);
			tr.appendChild(td(String(w.active)));
			tr.appendChild(td(String(w.completed || 0)));
			tr.appendChild(td(String(w.failed || 0)));
			tr.appendChild(tdTime(w.last_seen));
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load workers: " + e.message);
	}
}

// wireAsk hooks the fleet question box up to the ask endpoint. Advisory only: the answer comes
// from run, health, and drift metadata the viewer can already see, and asking changes nothing.
// explainReadOnly gives every dimmed table action a tooltip in read-only mode, so a disabled
// Launch or Delete reads as policy, not breakage.
function explainReadOnly() {
	if (!isReadOnly()) return;
	document.addEventListener("mouseover", (e) => {
		const b = e.target.closest && e.target.closest("table .button");
		if (b && !b.title) b.title = "Disabled in this read-only demo. Self-host to use it.";
	});
}

function wireAsk() {
	const go = document.getElementById("ask-go");
	const input = document.getElementById("ask-input");
	if (!go || !input) return;
	// Without a usable provider the input is theater. The read-only demo swaps the whole block
	// for one teaser line pointing at the guide.
	if (isReadOnly()) {
		const block = document.getElementById("ask-panel");
		if (block) {
			block.innerHTML = "";
			const title = document.createElement("h3");
			title.className = "ask-title";
			title.textContent = "Ask about your fleet";
			block.appendChild(title);
			const teaser = document.createElement("p");
			teaser.className = "ask-teaser muted";
			teaser.textContent = "Ask questions about your fleet and get advisory answers grounded in run, health, and drift data. Available when you self-host with an AI provider, including local Ollama. ";
			const link = document.createElement("a");
			link.href = "/ui/docs/ai";
			link.className = "link-arrow";
			link.textContent = "How Advisory AI works";
			teaser.appendChild(link);
			block.appendChild(teaser);
		}
		return;
	}
	// When no AI provider is configured the panel would look usable and then fail on the first
	// question with a line most people miss. Make the off state plain up front instead.
	const panel = document.getElementById("ask-panel");
	if (panel && aiOff()) {
		renderAskDisabled(panel, input, go);
		return;
	}
	go.addEventListener("click", askFleet);
	input.addEventListener("keydown", (e) => {
		if (e.key === "Enter") {
			e.preventDefault();
			askFleet();
		}
	});
}

// renderAskDisabled turns the ask panel into an obvious off state: the input and button are
// disabled and a clear notice explains that advisory AI is not configured, with a link to enable
// it. This replaces a subtle after-the-click message that people missed.
function renderAskDisabled(panel, input, go) {
	input.disabled = true;
	input.value = "";
	input.placeholder = "Advisory AI is off on this server";
	go.disabled = true;
	const note = panel.querySelector(".ask-note");
	const off = aiOffNoticeEl(
		"Advisory AI is off on this server. Turn it on to ask questions grounded in your run, health, and drift data.");
	if (note) note.replaceWith(off);
	else panel.appendChild(off);
}

// askFleet posts the question and renders the answer, keeping the button disabled while one
// question is in flight.
async function askFleet() {
	const input = document.getElementById("ask-input");
	const go = document.getElementById("ask-go");
	const status = document.getElementById("ask-status");
	const answer = document.getElementById("ask-answer");
	const question = input.value.trim();
	if (!question) {
		status.textContent = "Type a question first.";
		status.hidden = false;
		return;
	}
	go.disabled = true;
	answer.hidden = true;
	status.textContent = "Thinking.";
	status.hidden = false;
	try {
		const res = await fetch(API + "/ai/ask", {
			method: "POST",
			headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
			body: JSON.stringify({ question }),
		});
		if (res.status === 401) {
			requireLogin();
			return;
		}
		if (res.status === 404) {
			status.textContent = "AI is not enabled on this server.";
			return;
		}
		const data = await res.json().catch(() => ({}));
		if (!res.ok) throw new Error(data.error || "HTTP " + res.status);
		answer.textContent = data.answer || "";
		answer.hidden = false;
		status.hidden = true;
	} catch (err) {
		status.textContent = "Could not answer: " + err.message;
	} finally {
		go.disabled = false;
	}
}

