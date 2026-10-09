// openFileViewer shows one file from a project's checkout, read only, with a copy control. It is
// the destination for playbook names throughout the interface.
async function openFileViewer(projectID, path) {
	let overlay = document.getElementById("file-modal");
	if (!overlay) {
		overlay = document.createElement("div");
		overlay.id = "file-modal";
		overlay.className = "modal";
		overlay.hidden = true;
		overlay.innerHTML = '<div class="modal-card wide"><div class="modal-head">' +
			'<h2 id="file-title" class="mono"></h2>' +
			'<button type="button" class="modal-close" aria-label="Close">\u00d7</button></div>' +
			'<div id="file-note" class="muted file-note"></div>' +
			'<pre class="log file-body" id="file-body"></pre>' +
			'<div class="drill-actions" id="file-actions"></div></div>';
		document.body.appendChild(overlay);
		overlay.addEventListener("mousedown", (e) => { if (e.target === overlay) overlay.hidden = true; });
		overlay.querySelector(".modal-close").addEventListener("click", () => { overlay.hidden = true; });
		document.addEventListener("keydown", (e) => { if (e.key === "Escape") overlay.hidden = true; });
	}
	const title = document.getElementById("file-title");
	const note = document.getElementById("file-note");
	const body = document.getElementById("file-body");
	const actions = document.getElementById("file-actions");
	title.textContent = path;
	note.textContent = "Loading.";
	body.textContent = "";
	actions.innerHTML = "";
	overlay.hidden = false;
	try {
		const file = await getJSON("/projects/" + encodeURIComponent(projectID) +
			"/file?path=" + encodeURIComponent(path));
		const size = file.size >= 1024 ? Math.round(file.size / 1024) + " KB"
			: file.size + " " + plural(file.size, "byte", "bytes");
		if (file.binary) {
			note.textContent = size + ", binary. Nothing to show.";
		} else {
			note.textContent = size + (file.truncated ? ", showing the first 512 KB." : "") +
				" Read only, from the project's cached checkout.";
			body.textContent = file.content;
			actions.appendChild(Object.assign(document.createElement("button"), {
				type: "button", className: "button", textContent: "Copy",
				onclick: async () => { try { await navigator.clipboard.writeText(file.content); } catch { /* denied */ } },
			}));
			const dl = document.createElement("button");
			dl.type = "button";
			dl.className = "button";
			dl.textContent = "Download";
			dl.addEventListener("click", () =>
				downloadBlob(path.split("/").pop(), "text/plain", file.content));
			actions.appendChild(dl);
		}
	} catch (err) {
		note.textContent = "Could not open this file: " + err.message;
	}
}

// playbookCellEl renders a run or template's playbook as a link into the file viewer when the
// object came from a project, and as plain text otherwise, since only a project has a checkout.
function playbookCellEl(r, text) {
	const cell = td("", "col-playbook");
	// An auto-layout table ignores max-width on the cell itself, so the constraint lives on an
	// inner element and the full value stays on the cell's title.
	const inner = document.createElement("span");
	inner.className = "clamp";
	cell.appendChild(inner);
	const label = text !== undefined ? text : toolLabel(r);
	const path = r.playbook || "";
	if (r.project_id && path && (!r.tool || r.tool === "ansible")) {
		const link = document.createElement("button");
		link.type = "button";
		link.className = "linkish";
		link.textContent = label;
		link.dataset.tip = "View " + path + " from the project checkout";
		link.addEventListener("click", (e) => {
			e.preventDefault();
			e.stopPropagation();
			openFileViewer(r.project_id, path);
		});
		inner.appendChild(link);
	} else {
		inner.textContent = label;
	}
	cell.title = path || r.command || "";
	return cell;
}

// openPromptLaunch opens the launch-with-overrides dialog: survey answers when the template has
// one, then limit, stored inventory, selectable credentials, extra vars, and mode.
async function openPromptLaunch(t) {
	const modal = document.getElementById("prompt-modal");
	if (!modal) return;
	document.getElementById("prompt-title").textContent = "Launch " + t.name + " with overrides";
	document.getElementById("prompt-status").textContent = "";
	document.getElementById("prompt-limit").value = "";
	document.getElementById("prompt-vars").value = "";
	document.getElementById("prompt-labels").value = "";
	document.getElementById("prompt-dry-run").checked = !!t.dry_run;
	surveyFieldsInto(document.getElementById("prompt-survey"), t.survey);

	const invSel = document.getElementById("prompt-inventory");
	invSel.innerHTML = '<option value="">Template default</option>';
	try {
		const data = await getJSON("/inventories");
		for (const inv of data.inventories || []) {
			const opt = document.createElement("option");
			opt.value = inv.id;
			opt.textContent = inv.name;
			invSel.appendChild(opt);
		}
	} catch { /* stored inventories are optional */ }

	const credField = document.getElementById("prompt-field-credentials");
	const credSel = document.getElementById("prompt-credentials");
	credSel.innerHTML = "";
	const selectable = t.selectable_credential_ids || [];
	credField.hidden = !selectable.length;
	if (selectable.length) {
		try {
			const data = await getJSON("/credentials");
			const byID = new Map((data.credentials || []).map((c) => [c.id, c]));
			for (const cid of selectable) {
				const c = byID.get(cid);
				const opt = document.createElement("option");
				opt.value = cid;
				opt.textContent = c ? c.name + " (" + c.kind + ")" : cid;
				credSel.appendChild(opt);
			}
		} catch {
			// A picker that silently vanishes reads as "no credentials to choose", which is the
			// opposite of what happened.
			credField.hidden = false;
			credSel.innerHTML = "";
			const opt = document.createElement("option");
			opt.value = "";
			opt.textContent = "could not load the credential list; the template defaults apply";
			credSel.appendChild(opt);
			credSel.disabled = true;
		}
	}

	openDialog("prompt");
	document.getElementById("prompt-close").onclick = () => closeDialog("prompt");
	const status = document.getElementById("prompt-status");
	const go = document.getElementById("prompt-go");
	go.disabled = false;
	// The launch itself is guarded; the overrides are read and validated before it, so a rejected
	// payload leaves the button usable and only a real launch holds it.
	const launch = guardedSubmit(go, async (payload) => {
		const created = await postAction("/templates/" + t.id + "/launch", payload);
		location.href = "/ui/runs/" + created.id;
	}, (err) => {
		showLaunchFailure(status, err);
	});
	go.onclick = () => {
		const payload = { answers: collectSurveyAnswers(document.getElementById("prompt-survey")) };
		const limit = document.getElementById("prompt-limit").value.trim();
		if (limit) payload.limit = limit;
		if (invSel.value) payload.inventory_id = invSel.value;
		const picked = Array.from(credSel.selectedOptions).map((o) => o.value);
		if (picked.length) payload.credential_ids = picked;
		const varsText = document.getElementById("prompt-vars").value.trim();
		if (varsText) {
			try {
				payload.extra_vars = JSON.parse(varsText);
			} catch (_) {
				status.textContent = "Extra vars must be valid JSON.";
				return;
			}
		}
		const labels = {};
		for (const line of document.getElementById("prompt-labels").value.split("\n")) {
			const [key, ...rest] = line.split("=");
			if (key.trim() && rest.length) labels[key.trim()] = rest.join("=").trim();
		}
		if (Object.keys(labels).length) payload.labels = labels;
		payload.dry_run = document.getElementById("prompt-dry-run").checked;
		launch(payload);
	};
}

// deleteCell builds a table cell holding a delete button for a resource. onDeleted, when given, runs
// after a successful delete has taken the row out, for a list whose rows can change who is reading
// it.
function deleteCell(path, label, tr, emptyMsg, onDeleted) {
	const cell = document.createElement("td");
	const del = document.createElement("button");
	del.className = "button danger";
	del.dataset.mutates = "true";
	del.dataset.tip = "Click to delete this permanently";
	del.textContent = "Delete";
	del.addEventListener("click", async (e) => {
		e.preventDefault();
		e.stopPropagation();
		if (!window.confirm("Delete " + label + "?")) return;
		try {
			await authedDelete(path);
			removeRow(tr, emptyMsg);
			if (onDeleted) await onDeleted();
		} catch (err) {
			setStatus("Delete failed: " + err.message);
		}
	});
	cell.appendChild(del);
	return cell;
}

// openInventoryEdit fills the inventory dialog with an existing record and switches it to edit mode
// so the next save issues a PUT rather than a create. The source config is never returned, so its
// fields start blank and a blank keeps the stored config.
function openInventoryEdit(inv) {
	const form = document.getElementById("inventory-form");
	form.dataset.editId = inv.id;
	document.getElementById("inv-name").value = inv.name;
	document.getElementById("inv-content").value = inv.content || "";
	document.getElementById("inv-queue").value = inv.queue || "";
	for (const id of ["inv-command", "inv-vault-addr", "inv-vault-path", "inv-vault-field",
		"inv-vault-token", "inv-gsm-project", "inv-gsm-secret", "inv-gsm-version", "inv-gsm-token",
		"inv-aws-secret-id", "inv-aws-region", "inv-aws-access-key", "inv-aws-secret-key",
		"inv-azure-vault", "inv-azure-secret", "inv-azure-tenant", "inv-azure-client",
		"inv-azure-client-secret"]) {
		document.getElementById(id).value = "";
	}
	const sourceSel = document.getElementById("inv-content-source");
	sourceSel.value = inv.content_source || "local";
	sourceSel.dispatchEvent(new Event("change"));
	const ids = inv.credential_ids || [];
	for (const o of document.getElementById("inv-credentials").options) o.selected = ids.includes(o.value);
	document.getElementById("inv-host-filter").value = inv.host_filter || "";
	document.getElementById("inv-source-vars").value = inv.source_vars || "";
	document.getElementById("inv-limit").value = inv.limit || "";
	const inputs = inv.input_inventory_ids || [];
	for (const o of document.getElementById("inv-inputs").options) {
		o.selected = inputs.includes(o.value);
		// An inventory cannot be its own input, so editing one hides it from its own list.
		o.hidden = o.value === inv.id;
	}
	const kindSel = document.getElementById("inv-kind");
	kindSel.value = inv.kind || "";
	kindSel.dispatchEvent(new Event("change"));
	document.getElementById("inv-status").textContent = "";
	setModalTitle("inventory", "Edit inventory");
	document.getElementById("inventory-modal").hidden = false;
}

// applyInventorySource fills an inventory payload from the selected content source. A local source
// sends the pasted content; a command, Vault, Google Secret Manager, AWS Secrets Manager, or Azure
// Key Vault source assembles the config the API seals, so the operator never hand writes JSON. It
// throws with a message when a required field is missing. On edit the config is never returned, so
// leaving a source's fields blank keeps the stored config.
function applyInventorySource(payload, src, editId) {
	const val = (id) => document.getElementById(id).value.trim();
	if (src === "local") {
		const content = document.getElementById("inv-content").value;
		if (!content) throw new Error("Paste the inventory content, or pick another source.");
		payload.content = content;
		return;
	}
	if (src === "command") {
		const cmd = val("inv-command");
		if (cmd) payload.content_config = cmd;
		else if (!editId) throw new Error("Enter the command that prints the inventory.");
		return;
	}
	if (src === "vault") {
		const addr = val("inv-vault-addr"), path = val("inv-vault-path"), field = val("inv-vault-field");
		const token = val("inv-vault-token");
		if (addr || path || field || token) {
			if (!(addr && path && field)) throw new Error("Vault needs an address, path, and field.");
			const cfg = { addr, path, field };
			if (token) cfg.token = token;
			payload.content_config = JSON.stringify(cfg);
		} else if (!editId) {
			throw new Error("Fill in the Vault address, path, and field.");
		}
		return;
	}
	if (src === "gsm") {
		const project = val("inv-gsm-project"), secret = val("inv-gsm-secret");
		const version = val("inv-gsm-version"), token = val("inv-gsm-token");
		if (project || secret || version || token) {
			if (!(project && secret)) throw new Error("Google Secret Manager needs a project and secret.");
			const cfg = { project, secret };
			if (version) cfg.version = version;
			if (token) cfg.token = token;
			payload.content_config = JSON.stringify(cfg);
		} else if (!editId) {
			throw new Error("Fill in the Google Secret Manager project and secret.");
		}
		return;
	}
	if (src === "aws") {
		const secretId = val("inv-aws-secret-id"), region = val("inv-aws-region");
		const accessKey = val("inv-aws-access-key"), secretKey = val("inv-aws-secret-key");
		if (secretId || region || accessKey || secretKey) {
			if (!secretId) throw new Error("AWS Secrets Manager needs a secret id.");
			const cfg = { secret_id: secretId };
			if (region) cfg.region = region;
			if (accessKey) cfg.access_key_id = accessKey;
			if (secretKey) cfg.secret_access_key = secretKey;
			payload.content_config = JSON.stringify(cfg);
		} else if (!editId) {
			throw new Error("Fill in the AWS Secrets Manager secret id.");
		}
		return;
	}
	if (src === "azure") {
		const vault = val("inv-azure-vault"), secret = val("inv-azure-secret");
		const tenant = val("inv-azure-tenant"), client = val("inv-azure-client");
		const clientSecret = val("inv-azure-client-secret");
		if (vault || secret || tenant || client || clientSecret) {
			if (!(vault && secret)) throw new Error("Azure Key Vault needs a vault and secret.");
			const cfg = { vault, secret };
			if (tenant) cfg.tenant_id = tenant;
			if (client) cfg.client_id = client;
			if (clientSecret) cfg.client_secret = clientSecret;
			payload.content_config = JSON.stringify(cfg);
		} else if (!editId) {
			throw new Error("Fill in the Azure Key Vault vault and secret.");
		}
	}
}

// composedPayload reads the smart or constructed fields of the inventory dialog into the payload the
// API takes, and throws with a message when a required one is missing. A smart inventory is its
// host filter; a constructed one is its inputs, in the order the list shows them, with its plugin
// options and limit when given.
function composedPayload(kind) {
	const payload = { kind };
	if (kind === "smart") {
		const filter = document.getElementById("inv-host-filter").value.trim();
		if (!filter) throw new Error("Write the host filter that picks this inventory's hosts.");
		payload.host_filter = filter;
		return payload;
	}
	const inputs = Array.from(document.getElementById("inv-inputs").selectedOptions).map((o) => o.value);
	if (!inputs.length) throw new Error("Pick at least one input inventory.");
	payload.input_inventory_ids = inputs;
	const vars = document.getElementById("inv-source-vars").value;
	if (vars.trim()) payload.source_vars = vars;
	const limit = document.getElementById("inv-limit").value.trim();
	if (limit) payload.limit = limit;
	return payload;
}

// renderInventoryPreview lists the hosts a preview resolved to, and says where they came from, so
// the person writing a filter sees what it reaches before saving it.
function renderInventoryPreview(result, list, status) {
	list.innerHTML = "";
	const hosts = result.hosts || [];
	for (const h of hosts) {
		const li = document.createElement("li");
		li.textContent = h;
		list.appendChild(li);
	}
	const from = (result.inputs || []).map((i) => i.name);
	const noun = hosts.length === 1 ? "host" : "hosts";
	status.textContent = hosts.length
		? hosts.length + " " + noun + " right now, from " + from.join(", ") + "."
		: "No host matches right now among the inventories you may use.";
	// A smart inventory keeps the hosts and leaves their groups behind, as AWX does, so a play
	// written for a group reaches none of what the preview lists. Said beside the list, where the
	// person writing the filter is looking.
	if (hosts.length && result.kind === "smart") {
		status.textContent += " They arrive without their groups, so a play for hosts: all reaches " +
			"them and a play for a group does not. A constructed inventory keeps or builds groups.";
	}
	status.textContent += inventoryEngineText(result);
}

// inventoryEngineText says which engine resolved a preview, the way the run's evidence will: the
// native engine needs no Ansible, and an Ansible resolution names its ansible-core.
function inventoryEngineText(result) {
	if (result.engine === "native") return " Resolved by the native engine, without Ansible.";
	if (result.engine === "ansible") {
		return " Resolved by Ansible" + (result.ansible_core ? ", ansible-core " + result.ansible_core : "") + ".";
	}
	return "";
}

// wireInventoryForm hooks the inventory dialog up to POST /inventories for a new record and PUT
// /inventories/{id} when editing. The content source select swaps the stored-content box for the
// fields of a command, Vault, or Google Secret Manager source. The New button resets the dialog to
// add mode.
function wireInventoryForm() {
	const form = document.getElementById("inventory-form");
	const creds = document.getElementById("inv-credentials");
	const sourceSel = document.getElementById("inv-content-source");
	const hint = document.getElementById("inv-source-hint");
	// Derived from the markup rather than listed by hand. The hand-written list missed every AWS and
	// Azure field, so New inventory opened with the previous record's secret id, region, and static
	// access keys still in the form: saving created a second inventory pointing at the first one's
	// secret, and a typed secret access key stayed live in the DOM across what looked like a fresh
	// dialog. A new source family cannot be forgotten this way.
	const sourceFieldEls = () => form.querySelectorAll(
		"[data-source-group] input, [data-source-group] textarea, [data-source-group] select");
	fillSelect(creds, "/credentials", "credentials", (c) => c.name + " (" + c.kind + ")");

	const syncSource = () => {
		const src = sourceSel.value;
		for (const g of form.querySelectorAll("[data-source-group]")) {
			g.hidden = g.id !== "inv-source-" + src;
		}
		hint.hidden = !(form.dataset.editId && src !== "local");
	};
	sourceSel.addEventListener("change", syncSource);

	// The kind decides which half of the dialog applies: content of its own, a host filter, or
	// inputs and plugin options. Only a composed kind has hosts to preview.
	const kindSel = document.getElementById("inv-kind");
	const inputsSel = document.getElementById("inv-inputs");
	const previewBox = document.getElementById("inv-preview-box");
	const previewList = document.getElementById("inv-preview-hosts");
	const previewStatus = document.getElementById("inv-preview-status");
	const clearPreview = () => {
		previewList.innerHTML = "";
		previewStatus.textContent = "";
	};
	const syncKind = () => {
		const kind = kindSel.value;
		for (const g of form.querySelectorAll("[data-kind-group]")) {
			g.hidden = g.id !== "inv-kind-" + (kind || "static");
		}
		previewBox.hidden = !kind;
		clearPreview();
	};
	kindSel.addEventListener("change", syncKind);
	// Only an inventory with hosts of its own can be an input; a composed one is refused by the API.
	(async () => {
		try {
			const data = await getJSON("/inventories");
			for (const inv of data.inventories || []) {
				if (inv.kind) continue;
				const opt = document.createElement("option");
				opt.value = inv.id;
				opt.textContent = inv.name;
				inputsSel.appendChild(opt);
			}
		} catch { /* inventories unavailable; the list stays empty */ }
	})();
	const previewBtn = document.getElementById("inv-preview");
	previewBtn.addEventListener("click", async (e) => {
		e.preventDefault();
		clearPreview();
		let payload;
		try {
			payload = Object.assign({ name: document.getElementById("inv-name").value.trim() },
				composedPayload(kindSel.value));
		} catch (err) {
			previewStatus.textContent = err.message;
			return;
		}
		previewStatus.textContent = "Resolving.";
		try {
			renderInventoryPreview(await postAction("/inventories/preview", payload), previewList,
				previewStatus);
		} catch (err) {
			previewStatus.textContent = "Preview failed: " + err.message;
		}
	});

	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("inv-name").value = "";
		document.getElementById("inv-queue").value = "";
		for (const el of sourceFieldEls()) el.value = "";
		for (const o of creds.options) o.selected = false;
		for (const o of inputsSel.options) {
			o.selected = false;
			o.hidden = false;
		}
		for (const id of ["inv-host-filter", "inv-source-vars", "inv-limit"]) {
			document.getElementById(id).value = "";
		}
		kindSel.value = "";
		syncKind();
		sourceSel.value = "local";
		syncSource();
		document.getElementById("inv-status").textContent = "";
		setModalTitle("inventory", "Add an inventory");
	};
	const openBtn = document.getElementById("inventory-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still posting, so a fast double click on Save
	// stores the inventory once rather than twice. A modal save stays on the page, so the button is
	// re-enabled once the request settles either way, leaving the dialog usable for the next inventory.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("inv-status");
		const editId = form.dataset.editId;
		const kind = kindSel.value;
		const payload = { name: document.getElementById("inv-name").value.trim() };
		try {
			if (kind) {
				Object.assign(payload, composedPayload(kind));
			} else {
				payload.content_source = sourceSel.value;
				applyInventorySource(payload, sourceSel.value, editId);
			}
		} catch (err) {
			status.textContent = err.message;
			return;
		}
		const picked = Array.from(creds.selectedOptions).map((o) => o.value);
		if (picked.length) payload.credential_ids = picked;
		const invQueue = document.getElementById("inv-queue").value.trim();
		if (invQueue) payload.queue = invQueue;
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/inventories/" + editId, payload, "PUT");
			} else {
				await postAction("/inventories", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("inventory");
			document.getElementById("inventories").innerHTML = "";
			loadInventories();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// loadInventories populates the inventory table with delete actions.
// parseInventory reads a stored inventory into its format, host names, and group names. An INI
// inventory is parsed directly; a YAML one is reported as such rather than guessed at, since its
// shape needs a real parser and a wrong count is worse than an honest dash.
function parseInventory(content) {
	const text = String(content || "");
	// A source stores what ansible-inventory --list prints, a JSON document. Read line by line as
	// INI it became hosts named "{" and "\"db\":", a count of ten, and no groups, and an empty
	// document counted one host.
	if (text.trim().startsWith("{")) {
		try {
			return parseInventoryJSON(JSON.parse(text));
		} catch {
			return { format: "json", hosts: [], groups: [] };
		}
	}
	if (/^\s*(---|all\s*:)/m.test(text)) return { format: "yaml", hosts: [], groups: [] };
	const hosts = [];
	const groups = [];
	let skip = false;
	for (const raw of text.split("\n")) {
		const line = raw.trim();
		if (!line || line.startsWith("#") || line.startsWith(";")) continue;
		if (line.startsWith("[")) {
			const name = line.replace(/^\[|\]$/g, "");
			// A vars or children section describes a group rather than listing hosts.
			skip = /:(vars|children)$/.test(name);
			if (!skip) groups.push(name);
			continue;
		}
		if (skip) continue;
		const host = line.split(/\s+/)[0];
		if (host && !hosts.includes(host)) hosts.push(host);
	}
	return { format: "ini", hosts, groups };
}

// parseInventoryJSON reads the ansible-inventory --list shape: every host under _meta.hostvars,
// each group under its own name with its hosts, and all and ungrouped as the implicit groups.
function parseInventoryJSON(doc) {
	const hosts = new Set();
	const groups = [];
	if (!doc || typeof doc !== "object" || Array.isArray(doc)) return { format: "json", hosts: [], groups };
	const hostvars = doc._meta && doc._meta.hostvars;
	if (hostvars && typeof hostvars === "object") {
		for (const name of Object.keys(hostvars)) hosts.add(name);
	}
	for (const [name, group] of Object.entries(doc)) {
		if (name === "_meta") continue;
		if (name !== "all" && name !== "ungrouped") groups.push(name);
		const members = Array.isArray(group) ? group : (group && Array.isArray(group.hosts) ? group.hosts : []);
		for (const h of members) hosts.add(String(h));
	}
	return { format: "json", hosts: [...hosts], groups };
}

