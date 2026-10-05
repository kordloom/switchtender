// REASON_LIMIT is the most characters a reason or a correction holds, the server's cap.
const REASON_LIMIT = 1000;

// REASON_LABEL and REASON_NOTE are the words beside every reason field. A reason is permanent audit
// data, so the field says so before anybody types into it.
const REASON_LABEL = "Stored as audit evidence. Don't include secrets or personal data.";
const REASON_NOTE = "Masked for known secrets before storage. Treat it as permanent audit data.";

// decisionRun is the run the detail page is showing, kept so its Approve and Reject buttons know
// whether the rule that held it requires a reason.
let decisionRun = null;

// rememberDecisionRun records the run the detail page shows.
function rememberDecisionRun(run) {
	decisionRun = run;
}

// reasonDialog returns the shared reason dialog, building it on first use. One dialog serves an
// approval, a denial, and a correction, each with its own title and action.
function reasonDialog() {
	let modal = document.getElementById("reason-modal");
	if (!modal) {
		modal = document.createElement("div");
		modal.id = "reason-modal";
		modal.className = "modal";
		modal.hidden = true;
		modal.innerHTML = '<div class="modal-card"><div class="modal-head">' +
			'<h2 id="reason-title"></h2>' +
			'<button type="button" class="modal-close" id="reason-close" aria-label="Close">×</button></div>' +
			'<label class="field-label" for="reason-text"><span id="reason-label"></span>' +
			'<textarea id="reason-text" class="input" rows="4"></textarea></label>' +
			'<p class="muted" id="reason-note"></p>' +
			'<p class="muted" id="reason-count"></p>' +
			'<p id="reason-required" hidden></p>' +
			'<div id="reason-masked-box" hidden><p id="reason-masked-intro"></p>' +
			'<pre class="log view-code" id="reason-masked"></pre></div>' +
			'<p class="muted" id="reason-status"></p>' +
			'<div class="launch-actions"><button type="button" class="button primary" id="reason-go"></button>' +
			'<button type="button" class="button" id="reason-cancel">Cancel</button></div></div>';
		document.body.appendChild(modal);
		const field = document.getElementById("reason-text");
		field.setAttribute("maxlength", String(REASON_LIMIT));
		field.setAttribute("aria-describedby", "reason-note reason-count");
		field.addEventListener("input", () => updateReasonCount());
		document.getElementById("reason-label").textContent = REASON_LABEL;
		document.getElementById("reason-note").textContent = REASON_NOTE;
	}
	return modal;
}

// updateReasonCount shows how much of the cap the reason uses.
function updateReasonCount() {
	const field = document.getElementById("reason-text");
	const count = document.getElementById("reason-count");
	if (!field || !count) return;
	count.textContent = [...field.value].length + " of " + REASON_LIMIT + " characters";
}

// askReason opens the reason dialog and resolves with the text entered, the empty string for none,
// or null when the person cancels. A required reason keeps the action disabled until there is text,
// and says which rule asked for it.
function askReason(opts) {
	const modal = reasonDialog();
	document.getElementById("reason-title").textContent = opts.title;
	const field = document.getElementById("reason-text");
	field.value = "";
	field.disabled = false;
	field.placeholder = opts.required ? "Required" : "Optional";
	const required = document.getElementById("reason-required");
	required.hidden = !opts.required;
	required.textContent = opts.required
		? "A reason is required for this decision" + (opts.requiredBy ? " by " + opts.requiredBy : "") + "."
		: "";
	document.getElementById("reason-masked-box").hidden = true;
	document.getElementById("reason-status").textContent = "";
	const go = document.getElementById("reason-go");
	go.textContent = opts.action;
	updateReasonCount();
	const sync = () => { go.disabled = !!opts.required && field.value.trim() === ""; };
	sync();
	field.oninput = () => { updateReasonCount(); sync(); };
	return new Promise((resolve) => {
		const finish = (value) => {
			modal.hidden = true;
			go.onclick = null;
			document.getElementById("reason-cancel").onclick = null;
			document.getElementById("reason-close").onclick = null;
			resolve(value);
		};
		go.onclick = () => finish(field.value);
		document.getElementById("reason-cancel").onclick = () => finish(null);
		document.getElementById("reason-close").onclick = () => finish(null);
		modal.hidden = false;
		if (field.focus) field.focus();
	});
}

// confirmMasked shows the reason as the masker left it and resolves true when the person records
// that text, false when they go back. Nothing is recorded until they choose.
function confirmMasked(masked, action) {
	const modal = reasonDialog();
	document.getElementById("reason-text").disabled = true;
	document.getElementById("reason-required").hidden = true;
	document.getElementById("reason-masked-intro").textContent = "Known secrets were masked " +
		"before storage. This is the text that will be recorded. Nothing has been recorded yet.";
	document.getElementById("reason-masked").textContent = masked;
	document.getElementById("reason-masked-box").hidden = false;
	const go = document.getElementById("reason-go");
	go.textContent = action + " with the masked reason";
	go.disabled = false;
	return new Promise((resolve) => {
		const finish = (value) => {
			modal.hidden = true;
			go.onclick = null;
			document.getElementById("reason-cancel").onclick = null;
			document.getElementById("reason-close").onclick = null;
			resolve(value);
		};
		go.onclick = () => finish(true);
		document.getElementById("reason-cancel").onclick = () => finish(false);
		document.getElementById("reason-close").onclick = () => finish(false);
		modal.hidden = false;
	});
}

// postForAnswer posts a JSON body and returns the status and the decoded answer, so a caller can read
// a refusal the server explains rather than only its message.
async function postForAnswer(path, payload) {
	const res = await fetch(API + path, {
		method: "POST",
		headers: Object.assign({ "Content-Type": "application/json" }, authHeaders()),
		body: JSON.stringify(payload),
	});
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	const body = await res.json().catch(() => ({}));
	return { status: res.status, body };
}

// decideWithReason asks for a reason, posts the decision, and when the masker changed the reason,
// shows the masked text and posts again only once the person confirms it. It resolves with the
// server's answer, or null when the person cancels, and throws with the server's sentence when the
// decision is refused. fields names the body fields, which differ for a correction.
async function decideWithReason(path, base, opts) {
	const reason = await askReason(opts);
	if (reason === null) return null;
	const fields = opts.fields || { text: "reason", masked: "masked_reason" };
	const payload = Object.assign({}, base);
	if (reason.trim() !== "") payload[fields.text] = reason;
	let answer = await postForAnswer(path, payload);
	if (answer.status === 409 && answer.body.masked_reason !== undefined) {
		const ok = await confirmMasked(answer.body.masked_reason, opts.action);
		if (!ok) return null;
		payload[fields.masked] = answer.body.masked_reason;
		answer = await postForAnswer(path, payload);
	}
	if (answer.status < 200 || answer.status >= 300) {
		throw new Error(answer.body.error || ("HTTP " + answer.status));
	}
	return answer.body;
}

// reasonRequired reports whether a decision of the given kind needs a reason under a run's
// requirement: denials asks on a denial, always on both.
function reasonRequired(requirement, approve) {
	return requirement === "always" || (requirement === "denials" && !approve);
}

// decisionsMatter reports whether a run has decisions to show: it waits for one, was decided, or is a
// workflow whose approval steps may have been.
function decisionsMatter(run) {
	return !!run && !run.parent_id && (run.status === "pending_approval" || run.status === "rejected" ||
		!!run.held_by_policy || !!run.approved_spec_digest || run.kind === "pipeline");
}

// loadDecisions draws the run's decisions and their reasons, hiding the panel when the server does
// not let this reader see them: reasons are evidence, readable by an admin or by whoever asked.
async function loadDecisions(runId) {
	const panel = document.getElementById("decisions-panel");
	if (!panel) return;
	let data;
	try {
		data = await getJSON("/runs/" + encodeURIComponent(runId) + "/decisions");
	} catch {
		panel.hidden = true;
		return;
	}
	renderDecisions(runId, data.decisions || []);
}

// renderDecisions draws each decision with its reason and its corrections beneath it as a thread.
// A reason is never edited here: an admin can add a correction, or redact a reason's text as a
// privacy action that the chain records.
function renderDecisions(runId, records) {
	const panel = document.getElementById("decisions-panel");
	const host = document.getElementById("decisions");
	if (!panel || !host) return;
	host.textContent = "";
	const decisions = records.filter((r) => r.kind !== "correction");
	panel.hidden = decisions.length === 0;
	const canWrite = roleAtLeast("admin") && !isReadOnly();
	for (const d of decisions) {
		const entry = document.createElement("div");
		entry.className = "decision-entry";
		entry.dataset.decisionId = d.id;
		entry.appendChild(decisionLine(d));
		for (const c of records.filter((r) => r.kind === "correction" && r.decision_id === d.id)) {
			const thread = decisionLine(c);
			thread.classList.add("decision-correction");
			entry.appendChild(thread);
		}
		if (canWrite) {
			const actions = document.createElement("div");
			actions.className = "drill-actions";
			const correct = document.createElement("button");
			correct.type = "button";
			correct.className = "button";
			correct.textContent = "Add correction";
			correct.addEventListener("click", () => addCorrection(runId, d));
			actions.appendChild(correct);
			for (const r of [d].concat(records.filter((x) => x.kind === "correction" && x.decision_id === d.id))) {
				if (!r.reason || r.reason.redacted) continue;
				const redact = document.createElement("button");
				redact.type = "button";
				redact.className = "button danger";
				redact.textContent = r.kind === "correction" ? "Redact correction" : "Redact reason";
				redact.addEventListener("click", () => redactReason(runId, r));
				actions.appendChild(redact);
			}
			entry.appendChild(actions);
		}
		host.appendChild(entry);
	}
}

// decisionLine renders one decision or correction: who, when, and the reason as recorded.
function decisionLine(r) {
	const line = document.createElement("div");
	line.className = "decision-line";
	const head = document.createElement("p");
	const verb = r.kind === "correction" ? "Correction by "
		: (r.verdict === "approved" ? "Approved by " : "Rejected by ");
	head.textContent = verb + r.actor + (r.on_behalf_of && r.on_behalf_of !== r.actor
		? " on behalf of " + r.on_behalf_of : "") + (r.step_run_id ? ", approval step " + r.step_run_id : "");
	const when = document.createElement("span");
	when.className = "muted reltime";
	when.dataset.time = r.at;
	when.textContent = " " + relTime(r.at);
	when.title = fmtTime(r.at);
	head.appendChild(when);
	line.appendChild(head);
	const reason = document.createElement("p");
	reason.className = "decision-reason";
	if (r.reason && r.reason.redacted) {
		reason.classList.add("muted");
		reason.textContent = "Reason redacted (" + String(r.reason.redacted.category).replace(/_/g, " ") +
			") by " + r.reason.redacted.actor + ". Its commitment stays on the chain and cannot be opened.";
	} else if (r.reason) {
		reason.textContent = r.reason.text;
		if (r.reason.masked) {
			const note = document.createElement("span");
			note.className = "muted";
			note.textContent = " (masked for known secrets before storage)";
			reason.appendChild(note);
		}
	} else {
		reason.classList.add("muted");
		reason.textContent = "No reason given.";
	}
	line.appendChild(reason);
	if (r.separation_of_duties) {
		const s = r.separation_of_duties;
		const sod = document.createElement("p");
		sod.className = "muted";
		sod.textContent = "Separation of duties " + (s.required ? "required" : "not required") +
			": the agent's bound account " + s.requester + " counts as the requester, and " + s.decider +
			(s.independent ? " is independent of it." : " is the same account.");
		line.appendChild(sod);
	}
	return line;
}

// addCorrection appends a correction to a decision. The reason it corrects is never edited.
async function addCorrection(runId, d) {
	try {
		const done = await decideWithReason(
			"/runs/" + encodeURIComponent(runId) + "/decisions/" + encodeURIComponent(d.id) + "/corrections",
			{}, { title: "Add a correction", action: "Add correction", required: true,
				fields: { text: "text", masked: "masked_text" } });
		if (done === null) return;
		setStatus("Correction recorded.");
		await loadDecisions(runId);
	} catch (err) {
		setStatus("Could not record the correction: " + err.message);
	}
}

// redactDialog returns the redaction dialog, building it on first use.
function redactDialog() {
	let modal = document.getElementById("redact-modal");
	if (!modal) {
		modal = document.createElement("div");
		modal.id = "redact-modal";
		modal.className = "modal";
		modal.hidden = true;
		modal.innerHTML = '<div class="modal-card"><div class="modal-head">' +
			'<h2>Redact a reason</h2>' +
			'<button type="button" class="modal-close" id="redact-close" aria-label="Close">×</button></div>' +
			'<p id="redact-note"></p>' +
			'<label class="field-label">Why<select id="redact-category" class="input">' +
			'<option value="personal_data">Personal data</option>' +
			'<option value="secret">A secret the masker missed</option>' +
			'<option value="other">Other</option></select></label>' +
			'<div class="launch-actions"><button type="button" class="button danger" id="redact-go">Redact</button>' +
			'<button type="button" class="button" id="redact-cancel">Cancel</button></div></div>';
		document.body.appendChild(modal);
		document.getElementById("redact-note").textContent = "Redaction removes this reason's text " +
			"and the random value that opens its commitment, together. The chain records who redacted " +
			"it, when, and why. The commitment stays on the chain and can never be opened again. " +
			"Receipts already issued keep what they show.";
	}
	return modal;
}

// redactReason asks for a category and redacts the reason on the record, a privacy action the chain
// records rather than an edit.
function redactReason(runId, r) {
	const modal = redactDialog();
	const go = document.getElementById("redact-go");
	const close = () => { modal.hidden = true; };
	document.getElementById("redact-cancel").onclick = close;
	document.getElementById("redact-close").onclick = close;
	go.onclick = async () => {
		go.disabled = true;
		try {
			await postAction("/runs/" + encodeURIComponent(runId) + "/decisions/" +
				encodeURIComponent(r.id) + "/redact",
			{ category: document.getElementById("redact-category").value });
			close();
			setStatus("Reason redacted.");
			await loadDecisions(runId);
		} catch (err) {
			setStatus("Could not redact the reason: " + err.message);
		} finally {
			go.disabled = false;
		}
	};
	modal.hidden = false;
}
