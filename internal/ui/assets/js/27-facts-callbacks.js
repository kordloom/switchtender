// The fact cache and provisioning callbacks: the template dialog's controls for both, the
// one-time display of a minted callback key, and the cached facts an inventory holds.
//
// Both settings work per host of a stored inventory, so the server refuses either on a template
// that names none, and the dialog shows them only for Ansible, the one tool they apply to.

// applyCacheCallbackFields writes the dialog's fact cache and callback controls onto a save
// payload. An edit always states both switches, since the dialog shows them and what it shows is
// what the operator means. A create states only what is on, so a template made without touching
// them reads the way it always did.
function applyCacheCallbackFields(payload, ansible, editing) {
	const cache = document.getElementById("tpl-fact-cache");
	const timeout = document.getElementById("tpl-fact-cache-timeout");
	const callbacks = document.getElementById("tpl-allow-callbacks");
	if (!cache || !callbacks) return;
	const useCache = ansible && cache.checked;
	const allow = ansible && callbacks.checked;
	if (useCache || editing) payload.use_fact_cache = useCache;
	if (useCache) {
		const n = parseInt(timeout ? timeout.value : "", 10);
		payload.fact_cache_timeout = n > 0 ? n : 0;
	}
	if (allow || editing) payload.allow_callbacks = allow;
	const mode = document.getElementById("tpl-callback-limit");
	if (mode && ansible && (editing || mode.value === "replace")) payload.callback_limit = mode.value;
	// The AWX address switch is shown only for a template an import bound, and only then sent.
	const awx = document.getElementById("tpl-awx-callback");
	const awxField = document.getElementById("tpl-field-awx-callback");
	if (awx && awxField && !awxField.hidden && editing) payload.awx_callback = awx.checked;
}

// syncAWXCallbackField shows the AWX callback address switch only while editing an Ansible
// template an import bound to its AWX job template id, since no other template has the address.
function syncAWXCallbackField(ansible) {
	const field = document.getElementById("tpl-field-awx-callback");
	const form = document.getElementById("template-form");
	if (!field || !form) return;
	field.hidden = !ansible || !form.dataset.editId || !form.dataset.awxId;
}

// fillCacheCallbackFields loads a template's fact cache and callback settings into the dialog.
function fillCacheCallbackFields(t) {
	const cache = document.getElementById("tpl-fact-cache");
	const timeout = document.getElementById("tpl-fact-cache-timeout");
	const callbacks = document.getElementById("tpl-allow-callbacks");
	if (cache) cache.checked = !!t.use_fact_cache;
	if (timeout) timeout.value = t.fact_cache_timeout ? String(t.fact_cache_timeout) : "";
	if (callbacks) callbacks.checked = !!t.allow_callbacks;
	const mode = document.getElementById("tpl-callback-limit");
	if (mode) mode.value = t.callback_limit === "replace" ? "replace" : "intersect";
	const form = document.getElementById("template-form");
	if (form) form.dataset.awxId = t.awx_job_template_id ? String(t.awx_job_template_id) : "";
	const awx = document.getElementById("tpl-awx-callback");
	if (awx) awx.checked = !!t.awx_callback;
	const hint = document.getElementById("tpl-awx-callback-hint");
	if (hint && t.awx_job_template_id) {
		hint.textContent = "Answers callbacks on " + awxCallbackURL(t.awx_job_template_id) +
			", the address this template had in AWX, so boot scripts that still call it keep " +
			"working. Move them to this template's own address when you can.";
	}
}

// awxCallbackURL is the AWX-compatible callback address of an AWX job template id, on this server.
function awxCallbackURL(awxID) {
	const origin = (window.location && window.location.origin) || "";
	return origin + "/api/v2/job_templates/" + awxID + "/callback/";
}

// factCacheLabel states a run's fact cache setting the way an approver reads it: that the run uses
// cached facts, and how old a served fact may be. The dossier states it in the same words.
function factCacheLabel(run) {
	const timeout = Number(run.fact_cache_timeout) || 0;
	return timeout > 0 ? "uses cached facts (timeout " + timeout + "s)" : "uses cached facts (no timeout)";
}

// factCacheNote explains, where an approver decides, why a run that serves cached facts can act on
// a host differently from one that gathers them, or returns null for a run without the cache.
function factCacheNote(run) {
	if (!run.use_fact_cache) return null;
	const note = document.createElement("p");
	note.className = "risk-facts";
	note.textContent = "This run " + factCacheLabel(run) + ". The play reads the facts earlier runs " +
		"gathered instead of gathering them, so it can act on a host as it was when they were cached. " +
		"Approving binds this setting, and the run is refused if it changes afterward.";
	return note;
}

// callbackURL is the address a host posts its key to, on this server.
function callbackURL(templateID) {
	const origin = (window.location && window.location.origin) || "";
	return origin + API + "/templates/" + templateID + "/callback";
}

// callbackCurl is the command a host runs at boot to call back, the same shape AWX documents.
function callbackCurl(templateID, key) {
	return "curl -k -f -i -H 'Content-Type: application/json' -X POST \\\n" +
		"  -d '{\"host_config_key\": \"" + key + "\"}' \\\n  " + callbackURL(templateID);
}

// cacheCallbackRows adds the fact cache and callback lines to a template's read-only view.
function cacheCallbackRows(addRow, t) {
	if (t.use_fact_cache) {
		addRow("Fact cache", t.fact_cache_timeout
			? "on, facts older than " + t.fact_cache_timeout + "s are gathered again" : "on");
	}
	if (t.allow_callbacks) {
		addRow("Callbacks", t.host_config_key_set ? "on, key set" : "on, no key minted yet");
		addRow("Callback URL", callbackURL(t.id));
		if (t.limit) {
			addRow("Callback limit", t.callback_limit === "replace"
				? "replaced by the calling host, as AWX does"
				: "kept: a calling host outside " + t.limit + " is refused");
		}
	}
	if (t.awx_job_template_id) {
		addRow("AWX callback URL", awxCallbackURL(t.awx_job_template_id) +
			(t.awx_callback && t.allow_callbacks ? "" : " (off)"));
		addRow("Last called through AWX", t.awx_callback_called_at
			? fmtTime(t.awx_callback_called_at) : "never since the import");
	}
}

// appendCallbackAction puts the mint or rotate control under a template's view, for an admin,
// when the template accepts callbacks. The key is shown once, so rotating is the only way to see
// one again.
function appendCallbackAction(container, after, t, onDone) {
	const old = container.querySelector(".callback-actions");
	if (old) old.remove();
	if (!t.allow_callbacks || !roleAtLeast("admin")) return;
	const btn = document.createElement("button");
	btn.type = "button";
	btn.className = "button";
	btn.dataset.mutates = "true";
	btn.textContent = t.host_config_key_set ? "Rotate callback key" : "Mint callback key";
	btn.dataset.tip = t.host_config_key_set
		? "Mint a new key. Hosts holding the old one are refused from now on."
		: "Mint the key hosts present to call back. It is shown once.";
	btn.addEventListener("click", async () => {
		btn.disabled = true;
		try {
			if (onDone) onDone();
			await mintCallbackKey(t);
		} catch (err) {
			setStatus("Could not mint a callback key: " + err.message);
		} finally {
			btn.disabled = false;
		}
	});
	const row = document.createElement("div");
	row.className = "drill-actions callback-actions";
	row.appendChild(btn);
	container.insertBefore(row, after ? after.nextSibling : null);
}

// mintCallbackKey mints or rotates a template's callback key and shows it once.
async function mintCallbackKey(t) {
	const minted = await postAction("/templates/" + t.id + "/callback-key");
	showCallbackKey(t, minted.host_config_key);
	return minted;
}

// showCallbackKey shows a freshly minted key with the command a host runs, in a dialog that says it
// will not be shown again, because the server keeps the key sealed and never returns it.
function showCallbackKey(t, key) {
	let overlay = document.getElementById("callback-key-modal");
	if (!overlay) {
		overlay = document.createElement("div");
		overlay.id = "callback-key-modal";
		overlay.className = "modal";
		overlay.hidden = true;
		overlay.innerHTML = '<div class="modal-card wide"><div class="modal-head">' +
			'<h2 id="callback-key-title"></h2>' +
			'<button type="button" class="modal-close" aria-label="Close">×</button></div>' +
			'<p class="muted" id="callback-key-note"></p>' +
			'<pre class="log view-code" id="callback-key-code"></pre>' +
			'<div class="drill-actions"><button type="button" class="button" id="callback-key-copy">' +
			"Copy command</button></div></div>";
		document.body.appendChild(overlay);
		overlay.querySelector(".modal-close").addEventListener("click", () => { overlay.hidden = true; });
	}
	document.getElementById("callback-key-title").textContent = "Callback key for " + t.name;
	document.getElementById("callback-key-note").textContent = "This key is shown once. Put it in " +
		"the boot script of the hosts in this template's inventory now: it cannot be read back, " +
		"only rotated.";
	const command = callbackCurl(t.id, key);
	document.getElementById("callback-key-code").textContent = command;
	document.getElementById("callback-key-copy").onclick = async () => {
		try { await navigator.clipboard.writeText(command); } catch { /* denied */ }
	};
	overlay.hidden = false;
}

// factCacheReaderRole is the role that may read cached facts on this server: operator, or admin
// when the server restricts reading them to admins.
function factCacheReaderRole() {
	return document.body && document.body.dataset.factCacheAdminOnly === "true" ? "admin" : "operator";
}

// openInventoryFacts lists the hosts an inventory holds cached facts for, and shows one host's
// document when its row is chosen. Values whose keys look like secrets come back masked for anyone
// below admin, so this shows what the server chose to show.
async function openInventoryFacts(inv) {
	let overlay = document.getElementById("facts-modal");
	if (!overlay) {
		overlay = document.createElement("div");
		overlay.id = "facts-modal";
		overlay.className = "modal";
		overlay.hidden = true;
		overlay.innerHTML = '<div class="modal-card wide"><div class="modal-head">' +
			'<h2 id="facts-title"></h2>' +
			'<button type="button" class="modal-close" aria-label="Close">×</button></div>' +
			'<p class="muted" id="facts-status"></p>' +
			'<table class="runs" id="facts-table"><thead><tr><th>Host</th><th>Size</th>' +
			"<th>Gathered</th><th>Run</th></tr></thead><tbody id=\"facts-rows\"></tbody></table>" +
			'<pre class="log view-code" id="facts-doc" hidden></pre></div>';
		document.body.appendChild(overlay);
		overlay.querySelector(".modal-close").addEventListener("click", () => { overlay.hidden = true; });
	}
	document.getElementById("facts-title").textContent = "Cached facts: " + inv.name;
	const status = document.getElementById("facts-status");
	const rows = document.getElementById("facts-rows");
	const doc = document.getElementById("facts-doc");
	rows.innerHTML = "";
	doc.hidden = true;
	doc.textContent = "";
	status.textContent = "Loading cached facts.";
	overlay.hidden = false;
	let data;
	try {
		data = await getJSON("/inventories/" + inv.id + "/facts");
	} catch (err) {
		status.textContent = "Could not load cached facts: " + err.message;
		return;
	}
	const hosts = data.hosts || [];
	status.textContent = hosts.length
		? "Facts a template with the fact cache on gathered for these hosts. Choose a host to read them."
		: "No cached facts yet. A template with the fact cache on keeps them when it runs.";
	for (const h of hosts) {
		const tr = document.createElement("tr");
		const hostCell = td("", "mono");
		const open = document.createElement("button");
		open.type = "button";
		open.className = "linkish mono";
		open.textContent = h.host;
		open.addEventListener("click", async () => {
			try {
				const one = await getJSON("/inventories/" + inv.id + "/facts/" + encodeURIComponent(h.host));
				doc.textContent = JSON.stringify(one.facts, null, 2);
				doc.hidden = false;
			} catch (err) {
				status.textContent = "Could not load the facts for " + h.host + ": " + err.message;
			}
		});
		hostCell.appendChild(open);
		tr.appendChild(hostCell);
		tr.appendChild(td(fmtBytes(h.bytes || 0)));
		tr.appendChild(td(h.modified_at ? fmtTime(h.modified_at) : ""));
		const runCell = td("", "mono");
		if (h.run_id) {
			const link = document.createElement("a");
			link.href = "/ui/runs/" + h.run_id;
			link.textContent = h.run_id;
			runCell.appendChild(link);
		}
		tr.appendChild(runCell);
		rows.appendChild(tr);
	}
}
