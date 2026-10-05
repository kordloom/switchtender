// NTF_KIND_LABELS names each notification channel kind the way the target dialog offers it.
const NTF_KIND_LABELS = {
	webhook: "Webhook", slack: "Slack", mattermost: "Mattermost", rocketchat: "Rocket.Chat",
	discord: "Discord", teams: "Microsoft Teams", ntfy: "ntfy", pagerduty: "PagerDuty",
	grafana: "Grafana", twilio: "Twilio", email: "Email",
};

// NTF_URL_KINDS, NTF_KEY_KINDS, and NTF_TO_KINDS are the kinds addressed by a URL, the kinds that
// carry a key of their own, and the kinds that name a recipient. They mirror the server's
// validation, so the dialog never builds a target the API refuses for a missing field.
const NTF_URL_KINDS = ["webhook", "slack", "mattermost", "rocketchat", "discord", "teams", "ntfy",
	"grafana"];
const NTF_KEY_KINDS = ["pagerduty", "grafana"];
const NTF_TO_KINDS = ["twilio", "email"];

// NTF_MASK is the mark the server puts where it redacted part of an address. A value carrying it is
// a hint read back from the server, never something to store.
const NTF_MASK = "…";

// NTF_EVENT_LABELS says what each event a target hears means, for the attachment and delivery
// lists.
const NTF_EVENT_LABELS = {
	started: "A run starts", success: "A run succeeds", failure: "A run fails",
	approval: "A run waits for approval",
};

// NTF_OBJECT_LABELS names each kind of object a target attaches to, and NTF_OBJECT_LISTS says
// where each is listed and under which key.
const NTF_OBJECT_LABELS = {
	template: "Template", schedule: "Schedule", project: "Project", org: "Organization",
};
const NTF_OBJECT_LISTS = {
	template: { path: "/templates", key: "templates" },
	schedule: { path: "/schedules", key: "schedules" },
	project: { path: "/projects", key: "projects" },
	org: { path: "/orgs", key: "orgs" },
};

// ntfFields reports which inputs a channel kind takes: an address, a key, and a recipient.
function ntfFields(kind) {
	return {
		url: NTF_URL_KINDS.includes(kind),
		key: NTF_KEY_KINDS.includes(kind),
		to: NTF_TO_KINDS.includes(kind),
	};
}

// ntfPartWords names one secret part of a kind the way a person finds it in the other system: the
// incoming webhook address of a chat channel, a PagerDuty routing key, a Grafana API token.
function ntfPartWords(kind, part) {
	if (part === "key") {
		return kind === "pagerduty" ? "the PagerDuty Events API v2 routing key" : "the Grafana API token";
	}
	switch (kind) {
	case "slack":
	case "mattermost":
	case "rocketchat":
		return "the incoming webhook address";
	case "ntfy":
		return "the topic address";
	case "grafana":
		return "the Grafana instance address";
	default:
		return "the webhook address";
	}
}

// ntfFieldLabel is the label of one input for a kind.
function ntfFieldLabel(kind, part) {
	if (part === "key") return kind === "pagerduty" ? "Routing key" : "API token";
	if (part === "to") return kind === "twilio" ? "Phone number" : "Recipients";
	if (kind === "grafana") return "Grafana address";
	if (kind === "ntfy") return "Topic address";
	return "Webhook address";
}

// ntfPlaceholder is the example an input shows for a kind.
function ntfPlaceholder(kind, part) {
	if (part === "key") return kind === "pagerduty" ? "R0UTINGKEY" : "glsa_...";
	if (part === "to") return kind === "twilio" ? "+15550100" : "ops@example.com, lead@example.com";
	switch (kind) {
	case "slack":
		return "https://hooks.slack.com/services/...";
	case "grafana":
		return "https://grafana.example.com";
	case "ntfy":
		return "https://ntfy.example.com/ops";
	default:
		return "https://hooks.example.com/...";
	}
}

// ntfMissingSentence says what a target waiting for its secret still needs, in one sentence.
function ntfMissingSentence(kind, missing) {
	const parts = (missing || []).map((part) => ntfPartWords(kind, part));
	if (parts.length === 0) return "Waiting for its secret.";
	return "Waiting for " + parts.join(" and ") + ".";
}

// ntfMissingParts returns the parts a target is waiting for, as the server reported them.
function ntfMissingParts(target) {
	const delivery = (target && target.delivery) || {};
	return Array.isArray(delivery.missing) ? delivery.missing : [];
}

// ntfState returns the chip a target's delivery status reads as: its label, its color class, and
// the sentence its hover carries.
function ntfState(target) {
	const delivery = (target && target.delivery) || {};
	const kind = target ? target.kind : "";
	switch (delivery.state) {
	case "needs_secret":
		return { label: "needs secret", cls: "flaky",
			tip: ntfMissingSentence(kind, ntfMissingParts(target)) + " It delivers nothing until then." };
	case "healthy":
		return { label: "healthy", cls: "ok", tip: "Its most recent delivery arrived" };
	case "retrying":
		return { label: "retrying", cls: "warn",
			tip: "A delivery failed and is being retried. " + (delivery.last_error || "") };
	case "failing":
		return { label: "failing", cls: "failed",
			tip: "Its most recent delivery failed" +
				(delivery.last_error ? ": " + delivery.last_error : "") };
	default:
		return { label: "configured", cls: "none", tip: "Nothing has been delivered to it yet" };
	}
}

// ntfIsMasked reports whether a typed address or key should be left out of a save: blank, or the
// masked hint the server handed back. Either means keep what is stored, and sending the hint would
// point the channel at the redaction itself.
function ntfIsMasked(value) {
	return typeof value !== "string" || value.trim() === "" || value.includes(NTF_MASK);
}

// ntfFormPlan says how the dialog presents a kind for a create or an edit: which inputs show, which
// stored parts read as configured, and what a target waiting for its secret still needs. A stored
// address or key never travels back into an input. It is described instead, and kept unless a new
// value is typed. A target waiting for its secret asks only for the parts it is missing, unless its
// kind changes, which asks for everything the new kind takes.
function ntfFormPlan(kind, editing) {
	const fields = ntfFields(kind);
	const plan = {
		url: { show: fields.url, configured: "" },
		key: { show: fields.key, configured: "" },
		to: { show: fields.to },
		missing: [],
	};
	if (!editing || editing.kind !== kind) return plan;
	const missing = editing.needs_secret ? ntfMissingParts(editing) : [];
	plan.missing = missing;
	if (fields.url && !missing.includes("url")) {
		plan.url.configured = "Address: configured" + (editing.url ? " (" + editing.url + ")" : "") +
			". Leave empty to keep it.";
		if (editing.needs_secret) plan.url.show = false;
	}
	if (fields.key && !missing.includes("key") && (editing.key_set || editing.needs_secret)) {
		plan.key.configured = (kind === "pagerduty" ? "Routing key" : "Token") +
			": configured. Leave empty to keep it.";
		if (editing.needs_secret) plan.key.show = false;
	}
	return plan;
}

// ntfPayload builds the body a save sends from what the dialog holds, or returns an error sentence
// when the save cannot go ahead. An address or key is sent only when one was typed, never blank and
// never the masked hint, so a stored secret is kept rather than round-tripped. A create must carry
// every field its kind takes. An edit that changes the kind starts the new kind over, so it needs
// the new kind's address or key, unless the target is still waiting for its secret, which may stay
// waiting.
function ntfPayload(input, editing) {
	const name = (input.name || "").trim();
	if (!name) return { error: "Give the target a name." };
	const kind = input.kind;
	const fields = ntfFields(kind);
	const payload = { name, description: (input.description || "").trim() };
	if (!editing || editing.kind !== kind) payload.kind = kind;
	if (fields.url && !ntfIsMasked(input.url)) payload.url = input.url.trim();
	if (fields.key && !ntfIsMasked(input.key)) payload.key = input.key.trim();
	if (fields.to) payload.to = (input.to || "").trim();
	const label = NTF_KIND_LABELS[kind] || kind;
	const starting = !editing || (editing.kind !== kind && !editing.needs_secret);
	if (starting) {
		if (fields.url && !payload.url) {
			return { error: label + " targets need " + ntfPartWords(kind, "url") + "." };
		}
		if (fields.key && !payload.key) {
			return { error: label + " targets need " + ntfPartWords(kind, "key") + "." };
		}
	}
	if (fields.to && !payload.to) return { error: label + " targets need a recipient." };
	return { payload };
}

// notifyTargets holds the targets the list last drew, by id, so a row's actions open the record
// the row shows.
let notifyTargets = new Map();

// notifySealing records whether the server can seal an address or key, from the last list.
let notifySealing = true;

// notifyEditing is the target the dialog is editing, null while it creates one.
let notifyEditing = null;

// notifyDetailID is the target the detail panel shows, empty while it is closed.
let notifyDetailID = "";

// notifyObjectNames caches each attachable kind's objects by id, filled the first time a kind is
// needed. A kind this session cannot read maps to null.
const notifyObjectNames = new Map();

// syncNotifyFields shows the inputs the selected kind takes, labeled for it, and describes what an
// edited target already holds.
function syncNotifyFields() {
	const kindSelect = document.getElementById("notify-kind");
	if (!kindSelect) return;
	const kind = kindSelect.value;
	const plan = ntfFormPlan(kind, notifyEditing);
	for (const part of ["url", "key"]) {
		const field = document.getElementById("notify-" + part + "-field");
		const input = document.getElementById("notify-" + part);
		const state = document.getElementById("notify-" + part + "-state");
		if (!field || !input) continue;
		const show = plan[part].show || Boolean(plan[part].configured);
		field.hidden = !show;
		input.hidden = !plan[part].show;
		input.placeholder = ntfPlaceholder(kind, part);
		if (field.firstChild && field.firstChild.nodeType === 3) {
			field.firstChild.nodeValue = ntfFieldLabel(kind, part);
		}
		if (state) {
			state.textContent = plan[part].configured;
			state.hidden = !plan[part].configured;
		}
	}
	const to = document.getElementById("notify-to-field");
	const toInput = document.getElementById("notify-to");
	if (to && toInput) {
		to.hidden = !plan.to.show;
		toInput.placeholder = ntfPlaceholder(kind, "to");
		if (to.firstChild && to.firstChild.nodeType === 3) {
			to.firstChild.nodeValue = ntfFieldLabel(kind, "to");
		}
	}
	const missing = document.getElementById("notify-missing");
	if (missing) {
		missing.hidden = plan.missing.length === 0;
		missing.textContent = plan.missing.length
			? ntfMissingSentence(kind, plan.missing) + " Enter it to finish this target. " +
				"Everything else it has is kept."
			: "";
	}
	const hint = document.getElementById("notify-kind-hint");
	if (hint) {
		hint.textContent = kind === "twilio" || kind === "email"
			? "Sent through the server's own " + (kind === "twilio" ? "Twilio account" : "mail transport") +
				", so the target names only who receives it."
			: "";
	}
	const sealing = document.getElementById("notify-secret-hint");
	if (sealing && !notifySealing) {
		sealing.textContent = "This server has no encryption key, so an address or key cannot be " +
			"saved until SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT are set on it.";
	}
}

// resetNotifyForm returns the dialog to creating a new target.
function resetNotifyForm() {
	const form = document.getElementById("notify-form");
	if (!form) return;
	notifyEditing = null;
	delete form.dataset.editId;
	for (const id of ["notify-name", "notify-description", "notify-url", "notify-key", "notify-to"]) {
		const el = document.getElementById(id);
		if (el) el.value = "";
	}
	const kind = document.getElementById("notify-kind");
	if (kind) kind.value = "slack";
	const status = document.getElementById("notify-status");
	if (status) status.textContent = "";
	setModalTitle("notify", "Add a notification target");
	syncNotifyFields();
}

// openNotifyEdit fills the dialog with a stored target and opens it. The address and key inputs
// start empty whatever is stored: the server never returns them, and the dialog says what is held
// instead of putting a hint where a value would go.
function openNotifyEdit(target) {
	const form = document.getElementById("notify-form");
	if (!form) return;
	notifyEditing = target;
	form.dataset.editId = target.id;
	document.getElementById("notify-name").value = target.name || "";
	document.getElementById("notify-description").value = target.description || "";
	document.getElementById("notify-kind").value = target.kind;
	document.getElementById("notify-url").value = "";
	document.getElementById("notify-key").value = "";
	document.getElementById("notify-to").value = target.to || "";
	document.getElementById("notify-status").textContent = "";
	setModalTitle("notify", target.needs_secret ? "Finish " + target.name : "Edit " + target.name);
	syncNotifyFields();
	const modal = document.getElementById("notify-modal");
	if (modal) modal.hidden = false;
}

// wireNotifyForm hooks the dialog up to POST /notifications for a new target and PUT
// /notifications/{id} for an edit, and hides the create button from a session that cannot use it.
function wireNotifyForm() {
	const form = document.getElementById("notify-form");
	if (!form) return;
	const open = document.getElementById("notify-open");
	if (open) {
		if (!roleAtLeast("admin")) open.hidden = true;
		open.addEventListener("click", resetNotifyForm);
	}
	document.getElementById("notify-kind").addEventListener("change", syncNotifyFields);
	syncNotifyFields();
	const submitBtn = form.querySelector('button[type="submit"]');
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("notify-status");
		const built = ntfPayload({
			name: document.getElementById("notify-name").value,
			description: document.getElementById("notify-description").value,
			kind: document.getElementById("notify-kind").value,
			url: document.getElementById("notify-url").value,
			key: document.getElementById("notify-key").value,
			to: document.getElementById("notify-to").value,
		}, notifyEditing);
		if (built.error) {
			status.textContent = built.error;
			return;
		}
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			const editId = form.dataset.editId;
			if (editId) {
				await postAction("/notifications/" + encodeURIComponent(editId), built.payload, "PUT");
			} else {
				await postAction("/notifications", built.payload);
			}
			// The typed secrets leave the inputs once the server holds them.
			document.getElementById("notify-url").value = "";
			document.getElementById("notify-key").value = "";
			status.textContent = "Saved.";
			closeModal("notify");
			loadNotifications();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// renderNotificationsForViewers explains the page to a session that cannot read targets, rather than
// asking the server for a list it refuses.
function renderNotificationsForViewers() {
	const open = document.getElementById("notify-open");
	if (open) open.hidden = true;
	showEmpty("Notification targets are readable by operators and admins. A run's own page shows " +
		"what its targets were told.");
}

// renderNotifySealingNotice says once above the list that this server cannot seal an address or a
// key, so a target that needs one cannot be saved, while the ones it holds stay listed.
function renderNotifySealingNotice() {
	const table = document.getElementById("notify-table");
	if (!table) return;
	const anchor = table.closest(".list-scroll") || table;
	if (!anchor.parentNode || anchor.parentNode.querySelector(".seal-notice")) return;
	const note = document.createElement("div");
	note.className = "ro-banner seal-notice";
	note.textContent = "This server has no encryption key, so saving a target's address or key " +
		"needs SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT set on the server. Email " +
		"and Twilio targets carry no secret and can still be saved.";
	anchor.parentNode.insertBefore(note, anchor);
}

// loadNotifications fills the target list: each target's channel, the hint of where it posts or who
// it reaches, and its delivery status, with the targets still waiting for a secret gathered above.
async function loadNotifications() {
	if (!roleAtLeast("operator")) {
		renderNotificationsForViewers();
		return;
	}
	const table = document.getElementById("notify-table");
	try {
		const data = await getJSON("/notifications");
		const targets = Array.isArray(data.notifications) ? data.notifications : [];
		notifySealing = data.sealing !== false;
		if (!notifySealing) renderNotifySealingNotice();
		notifyTargets = new Map(targets.map((t) => [t.id, t]));
		renderNotifyNeeds(targets);
		if (targets.length === 0) {
			if (table) table.hidden = true;
			showEmpty("No notification targets yet. Add one, or import an AWX export, and attach it " +
				"to the templates, schedules, projects, or organizations whose runs it should hear about.");
			return;
		}
		const tbody = document.getElementById("notifications");
		tbody.textContent = "";
		for (const target of targets) tbody.appendChild(notifyRow(target));
		setStatus("");
		if (table) table.hidden = false;
		showListControls();
		if (notifyDetailID && notifyTargets.has(notifyDetailID)) {
			renderNotifyDetailState(notifyTargets.get(notifyDetailID));
		}
	} catch (e) {
		setStatus("Failed to load notification targets: " + e.message);
	}
}

// notifyRow builds one target's row.
function notifyRow(target) {
	const tr = document.createElement("tr");
	tr.dataset.notifyId = target.id;
	const name = td(target.name || target.id);
	if (target.description) name.dataset.tip = target.description;
	tr.appendChild(name);
	const kind = td("");
	const kindChip = document.createElement("span");
	kindChip.className = "cred-kind";
	kindChip.textContent = NTF_KIND_LABELS[target.kind] || target.kind;
	kind.appendChild(kindChip);
	tr.appendChild(kind);
	const address = td(notifyAddress(target));
	if (address.textContent) address.dataset.tip = address.textContent;
	tr.appendChild(address);
	const state = ntfState(target);
	const stateCell = td("");
	const chip = document.createElement("span");
	chip.className = "chip " + state.cls;
	chip.textContent = state.label;
	chip.dataset.tip = state.tip;
	stateCell.appendChild(chip);
	tr.appendChild(stateCell);
	const delivery = target.delivery || {};
	tr.appendChild(tdTime(delivery.last_delivered_at, "never"));
	tr.appendChild(tdTime(target.created_at));
	const actions = document.createElement("td");
	actions.className = "col-actions";
	const view = document.createElement("button");
	view.type = "button";
	view.className = "button";
	view.textContent = "Open";
	view.dataset.tip = "Click to see what this target is attached to and what it was told";
	view.addEventListener("click", (e) => {
		e.preventDefault();
		openNotifyDetail(target.id);
	});
	actions.appendChild(view);
	if (roleAtLeast("admin")) {
		actions.appendChild(document.createTextNode(" "));
		actions.appendChild(editButton(() => openNotifyEdit(notifyTargets.get(target.id) || target),
			target.needs_secret ? "Click to enter the secret this target is waiting for"
				: "Click to edit this target"));
		actions.appendChild(document.createTextNode(" "));
		const del = document.createElement("button");
		del.type = "button";
		del.className = "button danger";
		del.dataset.mutates = "true";
		del.dataset.tip = "Click to delete this target and its attachments";
		del.textContent = "Delete";
		del.addEventListener("click", async (e) => {
			e.preventDefault();
			if (!window.confirm("Delete notification target " + (target.name || target.id) + "?")) return;
			try {
				await authedDelete("/notifications/" + encodeURIComponent(target.id));
				notifyTargets.delete(target.id);
				if (notifyDetailID === target.id) closeNotifyDetail();
				removeRow(tr, "No notification targets yet.");
				renderNotifyNeeds([...notifyTargets.values()]);
			} catch (err) {
				setStatus("Delete failed: " + err.message);
			}
		});
		actions.appendChild(del);
	}
	tr.appendChild(actions);
	return tr;
}

// notifyAddress is what a row shows of where a target delivers: the masked hint of its address, its
// recipient, or that the address is still to be entered.
function notifyAddress(target) {
	const fields = ntfFields(target.kind);
	if (fields.to) return target.to || "";
	if (target.needs_secret && ntfMissingParts(target).includes("url")) return "not entered yet";
	return target.url || "";
}

// renderNotifyNeeds lists the targets an import left waiting for a secret, each with what it is
// waiting for and a way to finish it, above the main list.
function renderNotifyNeeds(targets) {
	const panel = document.getElementById("notify-needs");
	if (!panel) return;
	panel.textContent = "";
	const pending = targets.filter((t) => t.needs_secret);
	panel.hidden = pending.length === 0;
	if (!pending.length) return;
	const head = document.createElement("div");
	head.className = "cred-needs-head";
	const title = document.createElement("strong");
	title.textContent = pending.length === 1
		? "1 target is waiting for its secret"
		: pending.length + " targets are waiting for their secrets";
	const sub = document.createElement("span");
	sub.className = "cred-needs-sub";
	sub.textContent = "An import keeps everything a target has except the secret the export held " +
		"back. Each delivers nothing until its secret is entered.";
	head.appendChild(title);
	head.appendChild(sub);
	panel.appendChild(head);
	const list = document.createElement("div");
	list.className = "cred-needs-list";
	for (const target of pending) {
		const row = document.createElement("div");
		row.className = "cred-needs-row";
		row.dataset.notifyId = target.id;
		const meta = document.createElement("div");
		meta.className = "cred-needs-meta";
		const name = document.createElement("span");
		name.className = "cred-needs-name";
		name.textContent = target.name || target.id;
		const kind = document.createElement("span");
		kind.className = "cred-needs-kind";
		kind.textContent = NTF_KIND_LABELS[target.kind] || target.kind;
		meta.appendChild(name);
		meta.appendChild(kind);
		const what = document.createElement("span");
		what.className = "muted";
		what.textContent = ntfMissingSentence(target.kind, ntfMissingParts(target));
		row.appendChild(meta);
		row.appendChild(what);
		if (roleAtLeast("admin")) {
			const finish = document.createElement("button");
			finish.type = "button";
			finish.className = "button primary";
			finish.textContent = "Finish";
			finish.dataset.tip = "Click to enter the secret this target is waiting for";
			finish.disabled = isReadOnly();
			finish.addEventListener("click", () => openNotifyEdit(notifyTargets.get(target.id) || target));
			row.appendChild(finish);
		}
		list.appendChild(row);
	}
	panel.appendChild(list);
}

// wireNotifyDetail hooks up the detail panel's close button, its attach form, and the object picker
// that follows the chosen kind of object.
function wireNotifyDetail() {
	const close = document.getElementById("notify-detail-close");
	if (close) close.addEventListener("click", closeNotifyDetail);
	const kind = document.getElementById("notify-attach-kind");
	if (kind) kind.addEventListener("change", () => fillNotifyObjects(kind.value));
	const form = document.getElementById("notify-attach-form");
	if (!form) return;
	if (!roleAtLeast("operator")) form.hidden = true;
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight || !notifyDetailID) return;
		const status = document.getElementById("notify-attach-form-status");
		const objectID = document.getElementById("notify-attach-object").value;
		if (!objectID) {
			status.textContent = "Pick the object to attach the target to.";
			return;
		}
		inFlight = true;
		try {
			await postAction("/notifications/" + encodeURIComponent(notifyDetailID) + "/attachments", {
				object_kind: document.getElementById("notify-attach-kind").value,
				object_id: objectID,
				event: document.getElementById("notify-attach-event").value,
			});
			status.textContent = "Attached.";
			loadNotifyAttachments(notifyDetailID);
		} catch (err) {
			status.textContent = "Attach failed: " + err.message;
		} finally {
			inFlight = false;
		}
	});
}

// openNotifyDetail shows one target's attachments and recent deliveries below the list.
function openNotifyDetail(id) {
	const panel = document.getElementById("notify-detail");
	const target = notifyTargets.get(id);
	if (!panel || !target) return;
	notifyDetailID = id;
	document.getElementById("notify-detail-title").textContent = target.name || target.id;
	renderNotifyDetailState(target);
	const status = document.getElementById("notify-attach-form-status");
	if (status) status.textContent = "";
	panel.hidden = false;
	fillNotifyObjects(document.getElementById("notify-attach-kind").value);
	loadNotifyAttachments(id);
	loadNotifyDeliveries(id);
}

// closeNotifyDetail hides the detail panel.
function closeNotifyDetail() {
	notifyDetailID = "";
	const panel = document.getElementById("notify-detail");
	if (panel) panel.hidden = true;
}

// renderNotifyDetailState says, in a sentence above the panel's lists, where the target stands.
function renderNotifyDetailState(target) {
	const line = document.getElementById("notify-detail-state");
	if (!line) return;
	line.textContent = ntfStateSentence(target);
}

// ntfStateSentence says where a target stands: waiting for its secret, failing and since when,
// being retried, delivering, or not yet asked to deliver anything.
function ntfStateSentence(target) {
	const delivery = (target && target.delivery) || {};
	const failed = delivery.failed || 0;
	const recentFailures = failed
		? " " + failed + (failed === 1 ? " delivery" : " deliveries") + " failed in the last seven days."
		: "";
	switch (delivery.state) {
	case "needs_secret":
		return ntfMissingSentence(target.kind, ntfMissingParts(target)) +
			" It delivers nothing until then.";
	case "failing":
		return "Failing. The latest delivery failed" +
			(delivery.last_failed_at ? " " + relTime(delivery.last_failed_at) : "") +
			(delivery.last_error ? ": " + delivery.last_error : "") + "." + recentFailures;
	case "retrying":
		return "Retrying a delivery that failed" +
			(delivery.last_error ? ": " + delivery.last_error : "") + "." + recentFailures;
	case "healthy":
		return "Delivering. The latest delivery arrived" +
			(delivery.last_delivered_at ? " " + relTime(delivery.last_delivered_at) : "") + "." +
			recentFailures;
	default:
		return "Nothing has been delivered to it yet.";
	}
}

// notifyObjects returns one kind's objects by id, reading the kind's list the first time it is
// needed. A list this session cannot read resolves to null, and names fall back to ids.
async function notifyObjects(kind) {
	if (notifyObjectNames.has(kind)) return notifyObjectNames.get(kind);
	const spec = NTF_OBJECT_LISTS[kind];
	let byID = null;
	if (spec) {
		try {
			const data = await getJSON(spec.path);
			const list = Array.isArray(data[spec.key]) ? data[spec.key] : [];
			byID = new Map(list.map((o) => [o.id, o.name || o.id]));
		} catch {
			byID = null;
		}
	}
	notifyObjectNames.set(kind, byID);
	return byID;
}

// fillNotifyObjects fills the attach form's object picker with the chosen kind's objects.
async function fillNotifyObjects(kind) {
	const select = document.getElementById("notify-attach-object");
	if (!select) return;
	const objects = await notifyObjects(kind);
	select.textContent = "";
	if (!objects || objects.size === 0) {
		const opt = document.createElement("option");
		opt.value = "";
		opt.disabled = true;
		opt.selected = true;
		opt.textContent = objects ? "None to attach to" : "Not readable with this role";
		select.appendChild(opt);
		return;
	}
	const sorted = [...objects.entries()].sort((a, b) => a[1].localeCompare(b[1]));
	for (const [id, name] of sorted) {
		const opt = document.createElement("option");
		opt.value = id;
		opt.textContent = name;
		select.appendChild(opt);
	}
	select.value = sorted[0][0];
}

// loadNotifyAttachments lists what a target is attached to, by the name of each object where this
// session can read it, with a way to detach each.
async function loadNotifyAttachments(id) {
	const status = document.getElementById("notify-attach-status");
	const table = document.getElementById("notify-attach-table");
	const tbody = document.getElementById("notify-attachments");
	if (!status || !table || !tbody) return;
	try {
		const data = await getJSON("/notifications/" + encodeURIComponent(id) + "/attachments");
		const list = Array.isArray(data.attachments) ? data.attachments : [];
		if (id !== notifyDetailID) return;
		tbody.textContent = "";
		if (!list.length) {
			table.hidden = true;
			status.textContent = "Attached to nothing yet, so it hears about no run.";
			status.hidden = false;
			return;
		}
		for (const a of list) {
			const objects = await notifyObjects(a.object_kind);
			const name = objects && objects.get(a.object_id);
			const tr = document.createElement("tr");
			tr.appendChild(td(NTF_OBJECT_LABELS[a.object_kind] || a.object_kind));
			const nameCell = td(name || a.object_id, name ? "" : "mono");
			if (name) nameCell.dataset.tip = a.object_id;
			tr.appendChild(nameCell);
			tr.appendChild(td(NTF_EVENT_LABELS[a.event] || a.event));
			const actions = document.createElement("td");
			actions.className = "col-actions";
			if (roleAtLeast("operator")) {
				const detach = document.createElement("button");
				detach.type = "button";
				detach.className = "button";
				detach.dataset.mutates = "true";
				detach.dataset.tip = "Click to stop telling this target about that object's runs";
				detach.textContent = "Detach";
				detach.addEventListener("click", async (e) => {
					e.preventDefault();
					try {
						await authedDelete("/notifications/" + encodeURIComponent(id) + "/attachments/" +
							encodeURIComponent(a.id));
						loadNotifyAttachments(id);
					} catch (err) {
						status.textContent = "Detach failed: " + err.message;
						status.hidden = false;
					}
				});
				actions.appendChild(detach);
			}
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		status.textContent = "";
		status.hidden = true;
		table.hidden = false;
	} catch (err) {
		table.hidden = true;
		status.textContent = "Could not read the attachments: " + err.message;
		status.hidden = false;
	}
}

// ntfDeliveryBadge builds the badge a delivery's status reads as, a failed one in the failure color.
function ntfDeliveryBadge(status) {
	const span = document.createElement("span");
	const cls = { delivered: "succeeded", failed: "failed", pending: "pending", skipped: "canceled" };
	span.className = "badge " + (cls[status] || "pending");
	span.textContent = status || "pending";
	return span;
}

// ntfEventText renders a delivery's event: what it was, its place in the run's order, and the
// workflow step it came from when it came from one.
function ntfEventText(d) {
	let text = (NTF_EVENT_LABELS[d.event] || d.event) + " #" + d.seq;
	if (d.branch) text += ", step " + d.branch;
	return text;
}

// ntfDeliveryDetail says what became of a delivery beyond its status: why it failed or was skipped,
// the note it carried, or when it is tried next.
function ntfDeliveryDetail(d) {
	const parts = [];
	if (d.last_error) parts.push(d.last_error);
	if (d.note) parts.push(d.note);
	if (d.status === "pending" && d.attempts > 0 && d.next_attempt_at) {
		parts.push("Next attempt " + relTime(d.next_attempt_at) + ".");
	}
	return parts.join(" ");
}

// loadNotifyDeliveries lists a target's recent deliveries, newest first, each linked to its run, a
// failed one standing out from the rest.
async function loadNotifyDeliveries(id) {
	const status = document.getElementById("notify-delivery-status");
	const table = document.getElementById("notify-delivery-table");
	const tbody = document.getElementById("notify-deliveries");
	if (!status || !table || !tbody) return;
	try {
		const data = await getJSON("/notifications/" + encodeURIComponent(id) + "/deliveries");
		const list = Array.isArray(data.deliveries) ? data.deliveries : [];
		if (id !== notifyDetailID) return;
		tbody.textContent = "";
		if (!list.length) {
			table.hidden = true;
			status.textContent = "Nothing has been sent to it yet.";
			status.hidden = false;
			return;
		}
		for (const d of list) {
			const tr = document.createElement("tr");
			if (d.status === "failed") tr.className = "notify-failed";
			const run = document.createElement("td");
			const link = document.createElement("a");
			link.href = "/ui/runs/" + encodeURIComponent(d.run_id);
			link.textContent = d.run_id;
			link.className = "mono";
			run.appendChild(link);
			tr.appendChild(run);
			tr.appendChild(td(ntfEventText(d)));
			const badgeCell = td("");
			badgeCell.appendChild(ntfDeliveryBadge(d.status));
			tr.appendChild(badgeCell);
			tr.appendChild(td(String(d.attempts || 0)));
			tr.appendChild(td(ntfDeliveryDetail(d)));
			tr.appendChild(tdTime(d.created_at));
			tbody.appendChild(tr);
		}
		status.textContent = "";
		status.hidden = true;
		table.hidden = false;
	} catch (err) {
		table.hidden = true;
		status.textContent = "Could not read the deliveries: " + err.message;
		status.hidden = false;
	}
}

// loadRunNotifications fills the run page's notification section with what the run's named targets
// were told, in the run's order: each event, each target, and whether it arrived. A run no target
// heard about, and a session that cannot read the record, leave the section hidden rather than
// putting an empty box or an error on the run's page.
async function loadRunNotifications(runID) {
	const host = document.getElementById("run-notifications");
	if (!host || !runID) return;
	let list;
	try {
		const data = await getJSON("/runs/" + encodeURIComponent(runID) + "/notifications");
		list = Array.isArray(data.deliveries) ? data.deliveries : [];
	} catch {
		host.hidden = true;
		return;
	}
	host.textContent = "";
	host.hidden = list.length === 0;
	if (!list.length) return;
	const head = document.createElement("div");
	head.className = "panel-head";
	const title = document.createElement("h2");
	title.textContent = "Notifications";
	head.appendChild(title);
	const failed = list.filter((d) => d.status === "failed").length;
	if (failed) {
		const chip = document.createElement("span");
		chip.className = "chip failed";
		chip.textContent = failed === 1 ? "1 failed" : failed + " failed";
		head.appendChild(chip);
	}
	host.appendChild(head);
	const scroll = document.createElement("div");
	scroll.className = "list-scroll";
	const table = document.createElement("table");
	table.className = "runs run-notify-table";
	const thead = document.createElement("thead");
	const hr = document.createElement("tr");
	for (const label of ["Target", "Event", "Status", "Attempts", "Detail"]) {
		const th = document.createElement("th");
		th.textContent = label;
		hr.appendChild(th);
	}
	thead.appendChild(hr);
	table.appendChild(thead);
	const tbody = document.createElement("tbody");
	for (const d of list) {
		const tr = document.createElement("tr");
		if (d.status === "failed") tr.className = "notify-failed";
		const target = td(d.target_name || d.notification_id);
		target.dataset.tip = (NTF_KIND_LABELS[d.target_kind] || d.target_kind || "") +
			(d.notification_id ? " target " + d.notification_id : "");
		tr.appendChild(target);
		tr.appendChild(td(ntfEventText(d)));
		const badgeCell = td("");
		badgeCell.appendChild(ntfDeliveryBadge(d.status));
		tr.appendChild(badgeCell);
		tr.appendChild(td(String(d.attempts || 0)));
		tr.appendChild(td(ntfDeliveryDetail(d)));
		tbody.appendChild(tr);
	}
	table.appendChild(tbody);
	scroll.appendChild(table);
	host.appendChild(scroll);
}
