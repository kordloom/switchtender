// openScheduleEdit fills the schedule dialog with an existing record and switches it to edit mode.
// wireCronPreview shows the next firings for the cadence as it is typed, whether a cron spec or a
// recurrence rule, so a schedule is verifiable before saving.
function wireCronPreview() {
	const input = document.getElementById("schedule-cron");
	const out = document.getElementById("cron-preview");
	if (!input || !out) return;
	const zoneEl = document.getElementById("schedule-timezone");
	const ruleEl = document.getElementById("schedule-rrule");
	const kindEl = document.getElementById("schedule-kind");
	const springEl = document.getElementById("schedule-spring-forward");
	const gapEl = document.getElementById("cron-preview-gap");
	let timer = 0;
	const update = async () => {
		const asRule = scheduleKind() === "rrule";
		const spec = (asRule ? (ruleEl ? ruleEl.value : "") : input.value).trim();
		if (!spec) { out.textContent = ""; renderSpringGap(gapEl, null); return; }
		try {
			// The zone was never sent, so a schedule the operator had just set to America/New_York
			// previewed in UTC and the times under the box were the wrong times. The server has
			// always accepted this parameter; only the caller omitted it.
			const zone = zoneEl ? zoneEl.value.trim() : "";
			// The setting changes where a time the clocks skip fires, so the preview asks with it and
			// shows the times the schedule will really fire at.
			const spring = springEl ? springEl.value : "";
			const data = await getJSON("/schedules/preview?" + (asRule ? "rrule=" : "cron=") +
				encodeURIComponent(spec) + (zone ? "&timezone=" + encodeURIComponent(zone) : "") +
				(spring ? "&spring_forward=" + encodeURIComponent(spring) : ""));
			// Rendered on the schedule's own clock and labeled with it. Showing a New York schedule
			// in the reader's local zone is a different wrong answer: right instant, wrong clock face.
			// A recurrence names its zone on its DTSTART, and the server says which one it read.
			const shown = zone || data.timezone || "UTC";
			const fmt = { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" };
			// A recurrence is the form people reach for when the dates are irregular, so it shows
			// all five fires the server returns: three of a last-Friday rule do not show the year.
			const times = (data.next || []).slice(0, asRule ? 5 : 3).map((t) => {
				try {
					return new Date(t).toLocaleString(undefined, Object.assign({ timeZone: shown }, fmt));
				} catch {
					// An unknown zone is the operator's typo, not a reason to show nothing.
					return new Date(t).toLocaleString(undefined, fmt);
				}
			});
			// A rule bounded by COUNT or UNTIL may have fewer fires left than asked for, and a list
			// that simply ends reads as a rule that repeats. It says so.
			const tail = data.finished ? ", then it stops" : "";
			out.textContent = times.length
				? "Next: " + times.join("  ·  ") + "  (" + shown + ")" + tail
				: "";
			out.classList.remove("error-text");
			renderSpringGap(gapEl, data.spring_gap);
		} catch (err) {
			// A recurrence spans several lines, so the server's refusal, which names the part at
			// fault, is the only way to find it. A cron expression is one line and its message says
			// nothing a reader can use.
			out.textContent = asRule
				? "Invalid recurrence rule: " + ((err && err.message) || "it could not be read")
				: "Invalid cron expression";
			out.classList.add("error-text");
			renderSpringGap(gapEl, null);
		}
	};
	const later = () => {
		window.clearTimeout(timer);
		timer = window.setTimeout(update, 350);
	};
	input.addEventListener("input", later);
	if (ruleEl) ruleEl.addEventListener("input", later);
	// Switching between the two forms re-asks, or the preview keeps describing the other one.
	if (kindEl) kindEl.addEventListener("change", update);
	// Changing the zone has to re-ask, or the preview keeps showing the previous zone's times.
	if (zoneEl) {
		zoneEl.addEventListener("change", update);
		zoneEl.addEventListener("input", later);
	}
	// So does the setting, since it moves the one night a year a time does not exist.
	if (springEl) springEl.addEventListener("change", update);
	update();
}

// SPRING_FORWARD_LABELS names each spring-forward setting the way the dialog's choices do.
const SPRING_FORWARD_LABELS = {
	jump: "run when the clock jumps",
	later: "run after the clock change",
	skip: "skip that day",
};

// syncSpringForwardDefault names the default the dialog's first choice stands for, which depends on
// the cadence: a cron expression runs when the clock jumps, a recurrence rule after the clock
// change.
function syncSpringForwardDefault() {
	const el = document.getElementById("schedule-spring-forward");
	if (!el || !el.options || !el.options.length) return;
	const fallback = scheduleKind() === "rrule" ? "later" : "jump";
	el.options[0].textContent = "Default for this cadence (" + SPRING_FORWARD_LABELS[fallback] + ")";
}

// springGapText says what the schedule does on the next night the clocks go forward over a time it
// names: the date, the readings the jump erases, and where it fires that night or that it skips.
// The times are on the clock of the zone that jumps, which is the schedule's own.
function springGapText(gap) {
	const zone = gap.zone || "UTC";
	const onZone = (t, fmt) => {
		try {
			return new Date(t).toLocaleString(undefined, Object.assign({ timeZone: zone }, fmt));
		} catch {
			return new Date(t).toLocaleString(undefined, fmt);
		}
	};
	const day = onZone(gap.transition, { weekday: "short", month: "short", day: "numeric" });
	let text = "Daylight saving: on " + day + " the clock jumps from " + gap.from + " to " + gap.to +
		" (" + zone + "), so a time this schedule names does not exist that night.";
	const times = (gap.fires || []).map((t) => onZone(t, { hour: "2-digit", minute: "2-digit" }));
	let list = times.join(", ");
	if (gap.more_fires) list += ", and " + gap.more_fires + " more";
	if (gap.setting === "skip") {
		text += " It is skipped.";
		if (times.length) text += " Still firing in that hour: " + list + ".";
		return text;
	}
	const how = SPRING_FORWARD_LABELS[gap.setting] || "";
	if (times.length) {
		text += " It fires at " + list + (how ? " (" + how + ")" : "") + ".";
	}
	return text;
}

// renderSpringGap shows the daylight-saving note under the preview, or hides it when the server
// described no such night.
function renderSpringGap(el, gap) {
	if (!el) return;
	if (!gap) {
		el.hidden = true;
		el.textContent = "";
		return;
	}
	el.textContent = springGapText(gap);
	el.hidden = false;
}

// scheduleKind reports which cadence form the schedule dialog is set to: "cron" or "rrule".
function scheduleKind() {
	const el = document.getElementById("schedule-kind");
	return el && el.value === "rrule" ? "rrule" : "cron";
}

// setScheduleKind switches the schedule dialog between a cron expression and a recurrence rule. The
// cron box is required only while it is the form in use, or a rule could never be saved.
function setScheduleKind(kind) {
	const asRule = kind === "rrule";
	const el = document.getElementById("schedule-kind");
	if (el) el.value = asRule ? "rrule" : "cron";
	const cronField = document.getElementById("schedule-cron-field");
	const ruleField = document.getElementById("schedule-rrule-field");
	if (cronField) cronField.hidden = asRule;
	if (ruleField) ruleField.hidden = !asRule;
	const cron = document.getElementById("schedule-cron");
	if (cron) cron.required = !asRule;
	syncSpringForwardDefault();
}

function openScheduleEdit(s) {
	const form = document.getElementById("schedule-form");
	form.dataset.editId = s.id;
	document.getElementById("schedule-name").value = s.name || "";
	document.getElementById("schedule-cron").value = s.cron || "";
	// A schedule imported from AWX often carries a recurrence rule rather than a cron expression, and
	// a dialog that showed an empty cron box for it invited saving one over the rule.
	document.getElementById("schedule-rrule").value = s.rrule || "";
	setScheduleKind(s.rrule ? "rrule" : "cron");
	document.getElementById("schedule-template").value = s.template_id || "";
	scheduleInventoryPreview(s.template_id || "");
	// A schedule the interface did not create carries a zone and, for an imported crontab line, a
	// direct target instead of a template. The dialog knew about neither, so opening one of the
	// hundreds an import produces and pressing Save either moved when it fires or was refused for
	// having no template it never had.
	document.getElementById("schedule-timezone").value = s.timezone || "";
	// The setting is shown as stored, so saving the dialog keeps it rather than resetting it.
	const springEl = document.getElementById("schedule-spring-forward");
	if (springEl) springEl.value = s.spring_forward || "";
	document.getElementById("schedule-playbook").value = s.playbook || "";
	document.getElementById("schedule-inventory").value = s.inventory || "";
	// A pipeline or split schedule is a graph the dialog cannot express, so the fields it does show
	// stay read-only rather than offering an edit that would flatten it into a single run.
	const inline = document.getElementById("schedule-inline");
	inline.open = !s.template_id;
	const graph = (s.steps && s.steps.length > 0) || s.shards > 1;
	// The notice says the cadence is editable, but Save was still refused for having no template or
	// playbook, which a graph schedule never has, and typing a playbook to satisfy the check saved
	// a value the scheduler ignores. Every schedule a crontab import creates is a graph, so the
	// headline import produced hundreds of scheduables nobody could edit. The target inputs lock
	// and the check steps aside; the server preserves the steps and shards on update.
	form.dataset.graph = graph ? "1" : "";
	for (const id of ["schedule-template", "schedule-playbook", "schedule-inventory"]) {
		document.getElementById(id).disabled = graph;
	}
	setScheduleGraphNotice(graph ? s : null);
	document.getElementById("schedule-status").textContent = "";
	setModalTitle("schedule", "Edit schedule");
	document.getElementById("schedule-modal").hidden = false;
}

// wireScheduleForm hooks the schedule dialog up to POST /schedules for a new schedule and PUT
// /schedules/{id} when editing. The New button resets the dialog to add mode.
function wireScheduleForm() {
	const form = document.getElementById("schedule-form");
	fillTemplateSelect(document.getElementById("schedule-template"));
	fillZoneList(document.getElementById("tz-list"));
	const kindEl = document.getElementById("schedule-kind");
	if (kindEl) kindEl.addEventListener("change", () => setScheduleKind(kindEl.value));
	syncSpringForwardDefault();
	const tplEl = document.getElementById("schedule-template");
	if (tplEl) tplEl.addEventListener("change", () => scheduleInventoryPreview(tplEl.value));
	const resetToCreate = () => {
		delete form.dataset.editId;
		form.dataset.graph = "";
		for (const id of ["schedule-template", "schedule-playbook", "schedule-inventory"]) {
			document.getElementById(id).disabled = false;
		}
		document.getElementById("schedule-name").value = "";
		document.getElementById("schedule-cron").value = "";
		document.getElementById("schedule-rrule").value = "";
		setScheduleKind("cron");
		document.getElementById("schedule-template").value = "";
		scheduleInventoryPreview("");
		document.getElementById("schedule-timezone").value = "";
		const springEl = document.getElementById("schedule-spring-forward");
		if (springEl) springEl.value = "";
		document.getElementById("schedule-playbook").value = "";
		document.getElementById("schedule-inventory").value = "";
		document.getElementById("schedule-inline").open = false;
		setScheduleGraphNotice(null);
		document.getElementById("schedule-status").textContent = "";
		setModalTitle("schedule", "Add a schedule");
	};
	const openBtn = document.getElementById("schedule-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still posting, so a fast double click on Save
	// stores the schedule once rather than twice. A modal save stays on the page, so the button is
	// re-enabled once the request settles either way, leaving the dialog usable for the next schedule.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("schedule-status");
		const editId = form.dataset.editId;
		const isGraph = form.dataset.graph === "1";
		const templateID = document.getElementById("schedule-template").value;
		const playbook = document.getElementById("schedule-playbook").value.trim();
		if (!isGraph && !templateID && !playbook) {
			status.textContent = "Pick a template, or fill in a playbook or command to run directly.";
			return;
		}
		if (!isGraph && templateID && playbook) {
			// Both would leave which one fires up to the server's precedence rules, which is not a
			// thing to guess at when the answer decides what runs on real hosts.
			status.textContent = "Pick a template or a direct target, not both.";
			return;
		}
		// Every field the API knows is sent, filled or empty, because the update handler rebuilds the
		// schedule whole: an omitted timezone used to move when an imported schedule fires, and an
		// omitted target used to leave it firing nothing.
		// Only the form in use is sent; the other goes as empty, since the server refuses a schedule
		// that carries both, and switching an edit from one to the other has to clear the old one.
		const asRule = scheduleKind() === "rrule";
		const cronText = document.getElementById("schedule-cron").value.trim();
		const ruleText = document.getElementById("schedule-rrule").value.trim();
		if (asRule && !ruleText) {
			status.textContent = "Write a recurrence rule, or switch the cadence back to cron.";
			return;
		}
		const payload = {
			name: document.getElementById("schedule-name").value.trim(),
			cron: asRule ? "" : cronText,
			rrule: asRule ? ruleText : "",
			timezone: document.getElementById("schedule-timezone").value.trim(),
			template_id: templateID,
			playbook: playbook,
			inventory: document.getElementById("schedule-inventory").value.trim(),
		};
		// Sent whatever it says, because the dialog shows the stored setting: an empty value is the
		// default the dialog displays, and an edit has to be able to return a schedule to it.
		const springEl = document.getElementById("schedule-spring-forward");
		if (springEl) payload.spring_forward = springEl.value;
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/schedules/" + editId, payload, "PUT");
			} else {
				await postAction("/schedules", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("schedule");
			document.getElementById("schedules").innerHTML = "";
			loadSchedules();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// schedulePreviewSeq numbers the inventory previews the schedule dialog asks for, so a slow answer
// for a template no longer selected cannot overwrite the answer for the one that is.
let schedulePreviewSeq = 0;

// scheduleInventoryPreview says, under the template picker, what the chosen template's inventory
// matches right now when it is a smart or constructed one. A schedule over a composed inventory that
// matches nothing still fires on time and records each fire as skipped, so the dialog warns before
// Save rather than leaving it to a week of skipped fires. It is best effort and never blocks Save:
// any failure leaves the hint empty.
async function scheduleInventoryPreview(templateID) {
	const out = document.getElementById("schedule-inventory-preview");
	if (!out) return;
	const seq = ++schedulePreviewSeq;
	out.hidden = true;
	out.textContent = "";
	out.className = "field-hint";
	if (!templateID) return;
	try {
		const tpls = await getJSON("/templates");
		const tpl = (tpls.templates || []).find((t) => t.id === templateID);
		if (!tpl || !tpl.inventory_id) return;
		const invs = await getJSON("/inventories");
		const inv = (invs.inventories || []).find((i) => i.id === tpl.inventory_id);
		if (!inv || !inv.kind) return;
		const res = await postAction("/inventories/" + encodeURIComponent(inv.id) + "/preview");
		if (seq !== schedulePreviewSeq) return;
		const count = typeof res.count === "number" ? res.count : (res.hosts || []).length;
		const what = "Its " + inv.kind + " inventory \u201c" + inv.name + "\u201d";
		if (count > 0) {
			out.textContent = what + " matches " + count + " " + plural(count, "host", "hosts") +
				" right now.";
		} else {
			out.className = "warn-note";
			out.textContent = what + " matches no hosts right now. Each fire is recorded as " +
				"skipped, not failed, until it matches a host.";
		}
		out.hidden = false;
	} catch (_) {
		// Best effort: a preview that cannot be read says nothing rather than something wrong.
	}
}

// setScheduleGraphNotice warns, when a schedule fires a pipeline or a split, that the dialog shows
// only its cadence. Saving does not touch the graph, but a reader looking at a form with one playbook
// field would reasonably conclude the schedule runs one playbook.
function setScheduleGraphNotice(s) {
	const el = document.getElementById("schedule-graph-notice");
	if (!el) return;
	if (!s) {
		el.hidden = true;
		el.textContent = "";
		return;
	}
	el.hidden = false;
	el.textContent = s.steps && s.steps.length
		? "This schedule fires a pipeline of " + s.steps.length + " " +
			plural(s.steps.length, "step", "steps") + ". Its cadence is editable here; the steps are not."
		: "This schedule fires a split across " + s.shards +
			" shards. Its cadence is editable here; the split is not.";
}

// fillZoneList offers the browser's own zone and the common ones, so the field can be typed or
// picked. The list is a convenience: any IANA name the server accepts may be typed.
function fillZoneList(list) {
	if (!list) return;
	const here = (Intl.DateTimeFormat().resolvedOptions() || {}).timeZone || "";
	const zones = [here, "UTC", "America/New_York", "America/Chicago", "America/Denver",
		"America/Los_Angeles", "Europe/London", "Europe/Berlin", "Asia/Tokyo", "Australia/Sydney"];
	const seen = new Set();
	for (const z of zones) {
		if (!z || seen.has(z)) continue;
		seen.add(z);
		const opt = document.createElement("option");
		opt.value = z;
		list.appendChild(opt);
	}
}

// scheduleTarget describes what a schedule fires.
function scheduleTarget(s) {
	if (s.steps && s.steps.length) {
		return "pipeline, " + s.steps.length + " " + plural(s.steps.length, "step", "steps");
	}
	if (s.shards) {
		return "split x" + s.shards + "  " + (s.playbook || "");
	}
	return s.playbook || "";
}

// fillInventorySelect loads stored inventories into a select and returns an id to name map, so the
// policy table can show an inventory name instead of an id. It is best effort: a load failure just
// leaves the picker with its Any option.
async function fillInventorySelect(select) {
	const byID = {};
	try {
		const data = await getJSON("/inventories");
		for (const inv of data.inventories || []) {
			byID[inv.id] = inv.name;
			if (select) {
				const opt = document.createElement("option");
				opt.value = inv.id;
				opt.textContent = inv.name;
				select.appendChild(opt);
			}
		}
	} catch (_) { /* inventories disabled or unauthorized; picker keeps only Any */ }
	return byID;
}

// anyCell returns a table cell showing a muted "any", used where a policy criterion is empty and so
// matches every value.
function anyCell() {
	const cell = document.createElement("td");
	const span = document.createElement("span");
	span.className = "muted";
	span.textContent = "any";
	cell.appendChild(span);
	return cell;
}

// openPolicyEdit fills the policy dialog with an existing rule and switches it to edit mode, so a
// saved policy is changed in place rather than deleted and recreated.
function openPolicyEdit(p) {
	const form = document.getElementById("policy-form");
	form.dataset.editId = p.id;
	document.getElementById("policy-name").value = p.name;
	document.getElementById("policy-tool").value = p.tool || "";
	document.getElementById("policy-command").value = p.command_contains || "";
	document.getElementById("policy-inventory").value = p.inventory_id || "";
	document.getElementById("policy-queue").value = p.queue || "";
	// An exemption reads back as one. Mapped to the default, an edit saved an exemption as a rule
	// holding the very runs it was written to let through.
	document.getElementById("policy-effect").value =
		p.effect === "deny" || p.effect === "exempt" ? p.effect : "";
	document.getElementById("policy-actor-kind").value = p.actor_kind || "";
	document.getElementById("policy-actor").value = p.actor || "";
	document.getElementById("policy-min-risk").value = p.min_risk || "";
	document.getElementById("policy-max-destroy").value =
		(p.max_destroy !== undefined && p.max_destroy !== null && p.max_destroy >= 0) ? String(p.max_destroy) : "";
	document.getElementById("policy-exclude-dry").checked = !!p.exclude_dry_run;
	document.getElementById("policy-distinct-approver").checked = !!p.require_distinct_approver;
	const reasonField = document.getElementById("policy-require-reason");
	if (reasonField) reasonField.value = p.require_reason || "";
	document.getElementById("policy-status").textContent = "";
	setModalTitle("policy", "Edit policy");
	document.getElementById("policy-modal").hidden = false;
}

// ADVANCED_POLICY_FIELDS are the five inputs whose use makes a rule Team, matching exactly what
// policy.Advanced() tests: a deny effect, a risk floor, distinct-approver separation of duties, and
// either form of actor scoping. Marking them is not decoration. A Community reader filled the
// dialog, pressed Save, and met a 403 explaining the tier after composing the whole rule, which is
// the same shape as the evidence-pack refusal and just as avoidable.
const ADVANCED_POLICY_FIELDS = [
	["policy-effect", "A rule that denies outright, rather than holding for a person, is Team. " +
		"An exemption from the default agent hold is Community."],
	["policy-actor-kind", "Scoping a rule to who is asking, such as agents as a class, is Team."],
	["policy-actor", "Scoping a rule to one named actor is Team."],
	["policy-min-risk", "A risk floor, so a rule applies only above a grade, is Team."],
	["policy-distinct-approver", "Requiring a different person to approve than asked is Team."],
];

// markPolicyTiers tags each advanced field's label, so the gate is read from the control rather
// than met as a refusal after the rule is composed.
function markPolicyTiers() {
	for (const [id, why] of ADVANCED_POLICY_FIELDS) {
		const el = document.getElementById(id);
		if (!el) continue;
		const label = el.closest(".field-label") || el.parentElement;
		markTier(label, "Team", why);
	}
}

// wirePolicyForm hooks the policy dialog up to POST /policies for a new rule and PUT /policies/{id}
// when editing. The New button resets the dialog to add mode.
function wirePolicyForm() {
	const form = document.getElementById("policy-form");
	fillInventorySelect(document.getElementById("policy-inventory"));
	markPolicyTiers();
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("policy-name").value = "";
		document.getElementById("policy-tool").value = "";
		document.getElementById("policy-command").value = "";
		document.getElementById("policy-inventory").value = "";
		document.getElementById("policy-queue").value = "";
		document.getElementById("policy-effect").value = "";
		document.getElementById("policy-actor-kind").value = "";
		document.getElementById("policy-actor").value = "";
		document.getElementById("policy-min-risk").value = "";
		document.getElementById("policy-max-destroy").value = "";
		document.getElementById("policy-exclude-dry").checked = false;
		document.getElementById("policy-distinct-approver").checked = false;
		const reasonField = document.getElementById("policy-require-reason");
		if (reasonField) reasonField.value = "";
		document.getElementById("policy-status").textContent = "";
		setModalTitle("policy", "Add a policy");
	};
	const openBtn = document.getElementById("policy-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still posting, so a fast double click on Save
	// stores the policy once rather than twice. A modal save stays on the page, so the button is
	// re-enabled once the request settles either way, leaving the dialog usable for the next policy.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("policy-status");
		const editId = form.dataset.editId;
		// Every field the API knows is carried, filled or empty. Sending only the filled ones
		// made an edit a silent downgrade: the update handler rebuilds the policy whole, so a
		// deny rule saved from a dialog that did not know about effect came back as an approval
		// rule with no warning.
		const payload = {
			name: document.getElementById("policy-name").value.trim(),
			tool: document.getElementById("policy-tool").value,
			command_contains: document.getElementById("policy-command").value.trim(),
			inventory_id: document.getElementById("policy-inventory").value,
			queue: document.getElementById("policy-queue").value.trim(),
			effect: document.getElementById("policy-effect").value,
			actor_kind: document.getElementById("policy-actor-kind").value,
			actor: document.getElementById("policy-actor").value.trim(),
			min_risk: document.getElementById("policy-min-risk").value,
			exclude_dry_run: document.getElementById("policy-exclude-dry").checked,
			require_distinct_approver: document.getElementById("policy-distinct-approver").checked,
			// Sent filled or empty, for the reason every other field is: the update rebuilds the rule.
			require_reason: (document.getElementById("policy-require-reason") || {}).value || "",
		};
		const maxDestroy = document.getElementById("policy-max-destroy").value.trim();
		if (maxDestroy !== "") {
			payload.max_destroy = parseInt(maxDestroy, 10);
		}
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/policies/" + editId, payload, "PUT");
			} else {
				await postAction("/policies", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("policy");
			document.getElementById("policies").innerHTML = "";
			loadPolicies();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

