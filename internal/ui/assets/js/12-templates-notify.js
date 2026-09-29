// openTemplateEdit fills the template dialog with an existing record and switches it to edit mode.
// The dialog does not expose inventory_id, so it is carried through the form dataset to avoid
// dropping a stored inventory reference on save.
// NOTIFY_KINDS lists every selectable per-template channel. The first seven take a URL; the rest
// carry a routing key, a Grafana url and token, or a recipient, and the row shows the right field.
const NOTIFY_KINDS = [
	"webhook", "slack", "mattermost", "rocketchat", "discord", "teams", "ntfy",
	"pagerduty", "grafana", "twilio", "email",
];

// notifyNeedsURL reports whether a channel kind is addressed by a URL.
function notifyNeedsURL(kind) {
	return ["webhook", "slack", "mattermost", "rocketchat", "discord", "teams", "ntfy", "grafana"]
		.includes(kind);
}

// notifyRow appends one notification target row to the template dialog. The fields it shows follow
// the channel kind, mirroring the server's own validation: a URL channel shows a URL, pagerduty a
// routing key, grafana a URL and a token, and twilio or email a recipient.
function notifyRow(target) {
	const rows = document.getElementById("tpl-notify-rows");
	if (!rows) return;
	const row = document.createElement("div");
	row.className = "notify-row";
	const kind = document.createElement("select");
	kind.className = "input";
	for (const k of NOTIFY_KINDS) {
		const opt = document.createElement("option");
		opt.value = k;
		opt.textContent = k;
		kind.appendChild(opt);
	}
	kind.value = (target && target.kind) || "slack";

	const fields = document.createElement("div");
	fields.className = "notify-fields";
	const url = document.createElement("input");
	url.className = "input mono notify-url";
	url.value = (target && target.url) || "";
	const key = document.createElement("input");
	key.className = "input mono notify-key";
	key.value = (target && target.key) || "";
	const to = document.createElement("input");
	to.className = "input mono notify-to";
	to.value = (target && target.to) || "";
	fields.appendChild(url);
	fields.appendChild(key);
	fields.appendChild(to);

	// applyKind shows only the inputs the selected channel needs and labels them, so a row cannot be
	// built that the API will reject for a missing field.
	const applyKind = () => {
		const k = kind.value;
		const needsKey = k === "pagerduty" || k === "grafana";
		const needsTo = k === "twilio" || k === "email";
		url.style.display = notifyNeedsURL(k) ? "" : "none";
		key.style.display = needsKey ? "" : "none";
		to.style.display = needsTo ? "" : "none";
		url.placeholder = k === "grafana" ? "https://grafana.example/" : "https://hooks.example/...";
		key.placeholder = k === "pagerduty" ? "routing key" : "api token";
		to.placeholder = k === "twilio" ? "+15550100" : "ops@example.com";
	};
	kind.addEventListener("change", applyKind);
	applyKind();

	const fail = document.createElement("label");
	fail.className = "check-label notify-fail";
	const cb = document.createElement("input");
	cb.type = "checkbox";
	cb.checked = !!(target && target.on_failure);
	fail.appendChild(cb);
	fail.appendChild(document.createTextNode(" Failure only"));
	const del = document.createElement("button");
	del.type = "button";
	del.className = "modal-close";
	del.setAttribute("aria-label", "Remove notification");
	del.textContent = "\u00d7";
	del.addEventListener("click", () => row.remove());
	row.appendChild(kind);
	row.appendChild(fields);
	row.appendChild(fail);
	row.appendChild(del);
	rows.appendChild(row);
}

// collectNotifyTargets reads the dialog's notification rows into API targets, keeping only the field
// each channel uses and skipping a row the operator left blank.
function collectNotifyTargets() {
	const out = [];
	for (const row of document.querySelectorAll("#tpl-notify-rows .notify-row")) {
		const kind = row.querySelector("select").value;
		const url = row.querySelector(".notify-url").value.trim();
		const key = row.querySelector(".notify-key").value.trim();
		const to = row.querySelector(".notify-to").value.trim();
		const target = { kind };
		if (kind === "pagerduty") {
			if (!key) continue;
			target.key = key;
		} else if (kind === "grafana") {
			if (!url || !key) continue;
			target.url = url;
			target.key = key;
		} else if (kind === "twilio" || kind === "email") {
			if (!to) continue;
			target.to = to;
		} else {
			if (!url) continue;
			target.url = url;
		}
		if (row.querySelector("input[type=checkbox]").checked) target.on_failure = true;
		out.push(target);
	}
	return out;
}

function openTemplateEdit(t) {
	const form = document.getElementById("template-form");
	form.dataset.editId = t.id;
	form.dataset.inventoryId = t.inventory_id || "";
	document.getElementById("tpl-name").value = t.name;
	document.getElementById("tpl-project").value = t.project_id || "";
	document.getElementById("tpl-playbook").value = t.playbook;
	document.getElementById("tpl-inventory").value = t.inventory || "";
	document.getElementById("tpl-shards").value = t.shards ? String(t.shards) : "";
	document.getElementById("tpl-queue").value = t.queue || "";
	document.getElementById("tpl-timeout").value = t.timeout ? String(t.timeout) : "";
	document.getElementById("tpl-image").value = t.image || "";
	document.getElementById("tpl-pull-credential").value = t.pull_credential_id || "";
	const chosen = new Set(t.credential_ids || []);
	for (const opt of document.getElementById("tpl-credentials").options) {
		opt.selected = chosen.has(opt.value);
	}
	document.getElementById("tpl-vars").value = t.extra_vars ? JSON.stringify(t.extra_vars, null, 2) : "";
	document.getElementById("tpl-survey").value =
		(t.survey && t.survey.length) ? JSON.stringify(t.survey, null, 2) : "";
	document.getElementById("tpl-tool").value = t.tool || "ansible";
	document.getElementById("tpl-command").value = t.command || "";
	document.getElementById("tpl-dry-run").checked = !!t.dry_run;
	document.getElementById("tpl-confirm-launch").checked = !!t.confirm_on_launch;
	const notifyRows = document.getElementById("tpl-notify-rows");
	if (notifyRows) {
		notifyRows.innerHTML = "";
		for (const target of t.notifications || []) notifyRow(target);
	}
	syncTemplateTool();
	document.getElementById("tpl-status").textContent = "";
	setModalTitle("template", "Edit template");
	document.getElementById("template-modal").hidden = false;
}

// wireTemplateForm hooks the template dialog up to POST /templates for a new record and PUT
// /templates/{id} when editing. The New button resets the dialog to add mode.
function wireTemplateForm() {
	const notifyAdd = document.getElementById("tpl-notify-add");
	if (notifyAdd) notifyAdd.addEventListener("click", () => notifyRow());
	fillSelect(document.getElementById("tpl-project"), "/projects", "projects", (p) => p.name);
	fillSelect(document.getElementById("tpl-credentials"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	fillSelect(document.getElementById("tpl-pull-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	const form = document.getElementById("template-form");
	document.getElementById("tpl-tool").addEventListener("change", syncTemplateTool);
	syncTemplateTool();
	const resetToCreate = () => {
		delete form.dataset.editId;
		delete form.dataset.inventoryId;
		form.reset();
		for (const opt of document.getElementById("tpl-credentials").options) opt.selected = false;
		syncTemplateTool();
		document.getElementById("tpl-status").textContent = "";
		setModalTitle("template", "Add a template");
	};
	const openBtn = document.getElementById("template-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still posting, so a fast double click on Save
	// stores the template once rather than twice. A modal save stays on the page, so the button is
	// re-enabled once the request settles either way, leaving the dialog usable for the next template.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("tpl-status");
		const tool = document.getElementById("tpl-tool").value;
		const payload = {
			name: document.getElementById("tpl-name").value.trim(),
			project_id: document.getElementById("tpl-project").value,
		};
		if (tool && tool !== "ansible") {
			payload.tool = tool;
			payload.command = document.getElementById("tpl-command").value.trim();
		} else {
			payload.playbook = document.getElementById("tpl-playbook").value.trim();
			payload.inventory = document.getElementById("tpl-inventory").value.trim();
			const shards = parseInt(document.getElementById("tpl-shards").value, 10);
			if (shards >= 2) payload.shards = shards;
			const image = document.getElementById("tpl-image").value.trim();
			if (image) {
				payload.image = image;
				const pull = document.getElementById("tpl-pull-credential").value;
				if (pull) payload.pull_credential_id = pull;
			}
		}
		if (document.getElementById("tpl-dry-run").checked) payload.dry_run = true;
		if (document.getElementById("tpl-confirm-launch").checked) payload.confirm_on_launch = true;
		const tqueue = document.getElementById("tpl-queue").value.trim();
		if (tqueue) payload.queue = tqueue;
		const ttimeout = parseInt(document.getElementById("tpl-timeout").value, 10);
		if (ttimeout > 0) payload.timeout = ttimeout;
		const picked = Array.from(document.getElementById("tpl-credentials").selectedOptions)
			.map((o) => o.value);
		if (picked.length) payload.credential_ids = picked;
		const varsText = document.getElementById("tpl-vars").value.trim();
		if (varsText) {
			try {
				payload.extra_vars = JSON.parse(varsText);
			} catch (_) {
				status.textContent = "Extra vars must be valid JSON.";
				return;
			}
		}
		const surveyText = document.getElementById("tpl-survey").value.trim();
		if (surveyText) {
			try {
				payload.survey = JSON.parse(surveyText);
			} catch (_) {
				status.textContent = "Survey must be a valid JSON array.";
				return;
			}
		}
		payload.notifications = collectNotifyTargets();
		const editId = form.dataset.editId;
		if (editId && form.dataset.inventoryId) payload.inventory_id = form.dataset.inventoryId;
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/templates/" + editId, payload, "PUT");
			} else {
				await postAction("/templates", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("template");
			document.getElementById("templates").innerHTML = "";
			loadTemplates();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// loadTemplates populates the template table with launch and delete actions.
async function loadTemplates() {
	try {
		const data = await getJSON("/templates");
		const templates = data.templates || [];
		if (templates.length === 0) {
			showEmpty("No templates yet.");
			return;
		}
		const tbody = document.getElementById("templates");
		for (const t of templates) {
			const tr = document.createElement("tr");
			tr.appendChild(td(t.name));
			tr.appendChild(typeCellEl(t));
			// The playbook cell opens a read-only view of everything the template runs.
			const whatCell = td("", "mono");
			const view = document.createElement("button");
			view.type = "button";
			view.className = "linkish mono";
			view.textContent = toolLabel(t);
			view.dataset.tip = "View what this template runs";
			view.addEventListener("click", (e) => { e.preventDefault(); openTemplateView(t); });
			whatCell.appendChild(view);
			tr.appendChild(whatCell);
			tr.appendChild(td(String(t.shards || 1)));
			tr.appendChild(tdTime(t.created_at));
			const actions = document.createElement("td");
			actions.appendChild(launchSplitButton(t));
			actions.appendChild(document.createTextNode(" "));
			const history = document.createElement("a");
			history.className = "button";
			history.href = "/ui/runs?q=" + encodeURIComponent("from:" + t.id);
			history.textContent = "History";
			history.dataset.tip = "See every run this template has produced";
			actions.appendChild(history);
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(editButton(() => openTemplateEdit(t), "Click to edit this template's playbook, credentials, and settings"));
			actions.appendChild(document.createTextNode(" "));
			const delBtn = deleteCell("/templates/" + t.id, "template " + t.name, tr, "No templates yet.");
			actions.appendChild(delBtn.firstChild);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load templates: " + e.message);
	}
}

// launchSplitButton builds the template's launch control: a primary Launch that runs the saved
// preset, joined to a caret that opens the one advanced option, launching with per-run overrides.
// Two ways to launch used to sit apart with a history button between them and the advanced one
// shown only as an icon; grouping them into one split control makes the common path obvious and
// keeps the advanced path one click away.
function launchSplitButton(t) {
	const wrap = document.createElement("span");
	wrap.className = "split-btn";

	const main = document.createElement("button");
	main.className = "button primary split-main";
	main.dataset.mutates = "true";
	main.dataset.tip = t.confirm_on_launch
		? "Review and confirm before this template launches"
		: "Launch this template with its saved settings";
	main.textContent = "Launch";
	main.addEventListener("click", async (e) => {
		e.preventDefault();
		closeLaunchMenu();
		// A confirm-on-launch template never fires on one click: the overrides dialog is the
		// confirmation, and it carries the survey fields too, so it outranks the survey path.
		if (t.confirm_on_launch) {
			openPromptLaunch(t);
			return;
		}
		if (t.survey && t.survey.length) {
			openSurvey(t);
			return;
		}
		main.disabled = true;
		try {
			const created = await postAction("/templates/" + t.id + "/launch");
			location.href = "/ui/runs/" + created.id;
		} catch (err) {
			setStatus("Launch failed: " + err.message);
			main.disabled = false;
		}
	});

	const caret = document.createElement("button");
	caret.className = "button primary split-caret";
	caret.setAttribute("aria-label", "More launch options");
	caret.setAttribute("aria-haspopup", "true");
	caret.setAttribute("aria-expanded", "false");
	caret.innerHTML = svgIcon('<polyline points="6 9 12 15 18 9"/>');
	caret.addEventListener("click", (e) => {
		e.preventDefault();
		e.stopPropagation();
		// The menu floats on document.body, so this caret's own open state is tracked globally, not
		// found under the wrap. Clicking the caret whose menu is open closes it; any other opens.
		if (launchMenuCaret === caret) {
			closeLaunchMenu();
		} else {
			openLaunchMenu(wrap, caret, t);
		}
	});

	wrap.appendChild(main);
	wrap.appendChild(caret);
	return wrap;
}

// launchMenuCaret is the caret whose menu is currently open, so closing can reset its state. The
// menu itself is attached to document.body, not the caret, so a table row painted afterward can
// never cover it and no scroll container can clip it. Only one launch menu is ever open.
let launchMenuCaret = null;

// openLaunchMenu shows the split button's one advanced option, anchored under the caret with fixed
// positioning so it floats above the table instead of behind the next row. It closes on an outside
// click or Escape.
function openLaunchMenu(wrap, caret, t) {
	closeLaunchMenu();
	const menu = document.createElement("div");
	menu.className = "launch-menu";
	const item = document.createElement("button");
	item.type = "button";
	item.className = "launch-menu-item";
	item.innerHTML = '<span class="launch-menu-title"></span><span class="launch-menu-desc muted"></span>';
	item.querySelector(".launch-menu-title").textContent = "Launch with overrides…";
	item.querySelector(".launch-menu-desc").textContent = "Change host limit, inventory, credentials, or vars for this run only";
	item.addEventListener("click", () => { closeLaunchMenu(); openPromptLaunch(t); });
	menu.appendChild(item);
	document.body.appendChild(menu);

	// Anchor to the caret in viewport coordinates and keep the menu on screen at the right edge,
	// where the actions column sits.
	const rect = caret.getBoundingClientRect();
	const vw = window.innerWidth || 9999;
	menu.style.position = "fixed";
	menu.style.top = (rect.bottom + 6) + "px";
	menu.style.left = Math.max(8, Math.min(rect.left, vw - menu.offsetWidth - 8)) + "px";

	launchMenuCaret = caret;
	caret.setAttribute("aria-expanded", "true");
	item.focus();
	document.addEventListener("click", launchMenuOutside);
	document.addEventListener("keydown", launchMenuKey);
	window.addEventListener("resize", closeLaunchMenu);
	window.addEventListener("scroll", closeLaunchMenu, true);
}

// closeLaunchMenu removes any open launch menu and its listeners.
function closeLaunchMenu() {
	const menu = document.querySelector(".launch-menu");
	if (menu) menu.remove();
	if (launchMenuCaret) {
		launchMenuCaret.setAttribute("aria-expanded", "false");
		launchMenuCaret = null;
	}
	document.removeEventListener("click", launchMenuOutside);
	document.removeEventListener("keydown", launchMenuKey);
	window.removeEventListener("resize", closeLaunchMenu);
	window.removeEventListener("scroll", closeLaunchMenu, true);
}

// launchMenuOutside closes the menu when a click lands outside both the split button and the menu.
function launchMenuOutside(e) {
	if (!e.target.closest(".split-btn") && !e.target.closest(".launch-menu")) closeLaunchMenu();
}

// launchMenuKey closes the menu on Escape, returning focus to the caret.
function launchMenuKey(e) {
	if (e.key === "Escape") {
		const caret = launchMenuCaret;
		closeLaunchMenu();
		if (caret) caret.focus();
	}
}

// openTemplateView shows everything a template runs in a read-only dialog: its full command or
// playbook, tool, shards, and the rest, since list cells truncate.
function openTemplateView(t) {
	let overlay = document.getElementById("view-modal");
	if (!overlay) {
		overlay = document.createElement("div");
		overlay.id = "view-modal";
		overlay.className = "modal";
		overlay.hidden = true;
		overlay.innerHTML = '<div class="modal-card wide"><div class="modal-head"><h2 id="view-title"></h2>' +
			'<button type="button" class="modal-close" aria-label="Close">\u00d7</button></div>' +
			'<div class="view-rows" id="view-rows"></div><pre class="log view-code" id="view-code"></pre></div>';
		document.body.appendChild(overlay);
		overlay.addEventListener("mousedown", (e) => { if (e.target === overlay) overlay.hidden = true; });
		overlay.querySelector(".modal-close").addEventListener("click", () => { overlay.hidden = true; });
		document.addEventListener("keydown", (e) => { if (e.key === "Escape") overlay.hidden = true; });
	}
	document.getElementById("view-title").textContent = t.name;
	const rows = document.getElementById("view-rows");
	rows.innerHTML = "";
	const addRow = (k, v) => {
		if (!v && v !== 0) return;
		const key = document.createElement("span");
		key.className = "view-k";
		key.textContent = k;
		const val = document.createElement("span");
		val.className = "view-v";
		val.textContent = String(v);
		rows.appendChild(key);
		rows.appendChild(val);
	};
	addRow("Tool", (t.tool || "ansible"));
	addRow("Playbook", t.playbook);
	addRow("Inventory", t.inventory);
	addRow("Shards", t.shards && t.shards > 1 ? t.shards : "");
	addRow("Limit", t.limit);
	addRow("Timeout", t.timeout ? t.timeout + "s" : "");
	addRow("Created", t.created_at ? fmtTime(t.created_at) : "");
	const code = document.getElementById("view-code");
	code.hidden = !t.command;
	if (t.command) code.textContent = t.command;
	if (t.project_id && t.playbook && (!t.tool || t.tool === "ansible")) {
		const open = document.createElement("button");
		open.type = "button";
		open.className = "button";
		open.textContent = "View playbook";
		open.dataset.tip = "Open " + t.playbook + " from the project checkout";
		open.addEventListener("click", () => { overlay.hidden = true; openFileViewer(t.project_id, t.playbook); });
		const row = document.createElement("div");
		row.className = "drill-actions";
		row.appendChild(open);
		rows.parentNode.insertBefore(row, code.nextSibling);
	}
	overlay.hidden = false;
}

// openSurvey renders a template's survey as a form and launches with the collected answers.
// surveyFieldsInto renders a template's survey fields into a container.
function surveyFieldsInto(container, survey) {
	container.innerHTML = "";
	for (const f of survey || []) {
		const label = document.createElement("label");
		label.className = "field-label";
		label.textContent = (f.label || f.var) + (f.required ? " *" : "");
		let input;
		if (f.type === "choice") {
			input = document.createElement("select");
			for (const c of f.choices || []) {
				const opt = document.createElement("option");
				opt.value = c;
				opt.textContent = c;
				input.appendChild(opt);
			}
		} else if (f.type === "bool") {
			input = document.createElement("select");
			for (const v of ["false", "true"]) {
				const opt = document.createElement("option");
				opt.value = v;
				opt.textContent = v;
				input.appendChild(opt);
			}
		} else {
			input = document.createElement("input");
			input.type = f.type === "int" ? "number" : "text";
		}
		input.className = "input";
		input.dataset.var = f.var;
		input.dataset.type = f.type || "text";
		if (f.default !== undefined && f.default !== null) input.value = f.default;
		label.appendChild(input);
		container.appendChild(label);
	}
}

// collectSurveyAnswers reads typed answers back out of a survey container.
function collectSurveyAnswers(container) {
	const answers = {};
	for (const el of container.querySelectorAll("[data-var]")) {
		const raw = el.value;
		if (raw === "") continue;
		if (el.dataset.type === "int") answers[el.dataset.var] = parseInt(raw, 10);
		else if (el.dataset.type === "bool") answers[el.dataset.var] = raw === "true";
		else answers[el.dataset.var] = raw;
	}
	return answers;
}

function openSurvey(t) {
	const modal = document.getElementById("survey-modal");
	const form = document.getElementById("survey-form");
	document.getElementById("survey-title").textContent = "Launch " + t.name;
	document.getElementById("survey-status").textContent = "";
	surveyFieldsInto(form, t.survey);
	modal.hidden = false;

	document.getElementById("survey-cancel").onclick = () => { modal.hidden = true; };
	const go = document.getElementById("survey-go");
	go.disabled = false;
	go.onclick = guardedSubmit(go, async () => {
		const created = await postAction("/templates/" + t.id + "/launch",
			{ answers: collectSurveyAnswers(form) });
		location.href = "/ui/runs/" + created.id;
	}, (err) => {
		document.getElementById("survey-status").textContent = "Launch failed: " + err.message;
	});
}

// openProjectFiles lists a project's cached checkout and opens any file in the viewer, so a
// playbook can be found by browsing rather than by knowing its path.
async function openProjectFiles(project) {
	let overlay = document.getElementById("tree-modal");
	if (!overlay) {
		overlay = document.createElement("div");
		overlay.id = "tree-modal";
		overlay.className = "modal";
		overlay.hidden = true;
		overlay.innerHTML = '<div class="modal-card wide"><div class="modal-head">' +
			'<h2 id="tree-title"></h2>' +
			'<button type="button" class="modal-close" aria-label="Close">\u00d7</button></div>' +
			'<input id="tree-filter" class="input" type="search" placeholder="Filter files" aria-label="Filter files">' +
			'<div id="tree-note" class="muted file-note"></div>' +
			'<div id="tree-list" class="tree-list"></div></div>';
		document.body.appendChild(overlay);
		overlay.addEventListener("mousedown", (e) => { if (e.target === overlay) overlay.hidden = true; });
		overlay.querySelector(".modal-close").addEventListener("click", () => { overlay.hidden = true; });
		document.addEventListener("keydown", (e) => { if (e.key === "Escape") overlay.hidden = true; });
	}
	document.getElementById("tree-title").textContent = project.name;
	const note = document.getElementById("tree-note");
	const list = document.getElementById("tree-list");
	const filter = document.getElementById("tree-filter");
	filter.value = "";
	note.textContent = "Loading the checkout.";
	list.innerHTML = "";
	overlay.hidden = false;
	try {
		const data = await getJSON("/projects/" + encodeURIComponent(project.id) + "/files");
		const files = data.files || [];
		note.textContent = files.length
			? files.length + " files in the cached checkout. Click one to view it."
			: "The checkout is empty.";
		for (const f of files) {
			const row = document.createElement("button");
			row.type = "button";
			row.className = "tree-row";
			row.dataset.path = f.path;
			row.dataset.tip = "Click to view this file";
			const name = document.createElement("span");
			name.className = "mono";
			name.textContent = f.path;
			const size = document.createElement("span");
			size.className = "tree-size";
			size.textContent = f.size >= 1024 ? Math.round(f.size / 1024) + " KB" : f.size + " B";
			row.appendChild(name);
			row.appendChild(size);
			row.addEventListener("click", () => { overlay.hidden = true; openFileViewer(project.id, f.path); });
			list.appendChild(row);
		}
		filter.oninput = () => {
			const q = filter.value.trim().toLowerCase();
			for (const row of list.querySelectorAll(".tree-row")) {
				row.hidden = q !== "" && !row.dataset.path.toLowerCase().includes(q);
			}
		};
	} catch (err) {
		note.textContent = "Could not list this project: " + err.message;
	}
}

