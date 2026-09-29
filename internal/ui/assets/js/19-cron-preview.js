// openScheduleEdit fills the schedule dialog with an existing record and switches it to edit mode.
// wireCronPreview shows the next firings for the cron spec as it is typed, so a schedule is
// verifiable before saving.
function wireCronPreview() {
	const input = document.getElementById("schedule-cron");
	const out = document.getElementById("cron-preview");
	if (!input || !out) return;
	let timer = 0;
	const update = async () => {
		const spec = input.value.trim();
		if (!spec) { out.textContent = ""; return; }
		try {
			const data = await getJSON("/schedules/preview?cron=" + encodeURIComponent(spec));
			const times = (data.next || []).slice(0, 3).map((t) =>
				new Date(t).toLocaleString(undefined, { weekday: "short", month: "short", day: "numeric", hour: "2-digit", minute: "2-digit" }));
			out.textContent = times.length ? "Next: " + times.join("  ·  ") : "";
			out.classList.remove("error-text");
		} catch {
			out.textContent = "Invalid cron expression";
			out.classList.add("error-text");
		}
	};
	input.addEventListener("input", () => {
		window.clearTimeout(timer);
		timer = window.setTimeout(update, 350);
	});
	update();
}

function openScheduleEdit(s) {
	const form = document.getElementById("schedule-form");
	form.dataset.editId = s.id;
	document.getElementById("schedule-name").value = s.name || "";
	document.getElementById("schedule-cron").value = s.cron || "";
	document.getElementById("schedule-template").value = s.template_id || "";
	// A schedule the interface did not create carries a zone and, for an imported crontab line, a
	// direct target instead of a template. The dialog knew about neither, so opening one of the
	// hundreds an import produces and pressing Save either moved when it fires or was refused for
	// having no template it never had.
	document.getElementById("schedule-timezone").value = s.timezone || "";
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
	const resetToCreate = () => {
		delete form.dataset.editId;
		form.dataset.graph = "";
		for (const id of ["schedule-template", "schedule-playbook", "schedule-inventory"]) {
			document.getElementById(id).disabled = false;
		}
		document.getElementById("schedule-name").value = "";
		document.getElementById("schedule-cron").value = "";
		document.getElementById("schedule-template").value = "";
		document.getElementById("schedule-timezone").value = "";
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
		const payload = {
			name: document.getElementById("schedule-name").value.trim(),
			cron: document.getElementById("schedule-cron").value.trim(),
			timezone: document.getElementById("schedule-timezone").value.trim(),
			template_id: templateID,
			playbook: playbook,
			inventory: document.getElementById("schedule-inventory").value.trim(),
		};
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
		? "This schedule fires a pipeline of " + s.steps.length +
			" steps. Its cadence is editable here; the steps are not."
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
		return "pipeline, " + s.steps.length + " steps";
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
	document.getElementById("policy-effect").value = p.effect === "deny" ? "deny" : "";
	document.getElementById("policy-actor-kind").value = p.actor_kind || "";
	document.getElementById("policy-actor").value = p.actor || "";
	document.getElementById("policy-min-risk").value = p.min_risk || "";
	document.getElementById("policy-max-destroy").value =
		(p.max_destroy !== undefined && p.max_destroy !== null && p.max_destroy >= 0) ? String(p.max_destroy) : "";
	document.getElementById("policy-exclude-dry").checked = !!p.exclude_dry_run;
	document.getElementById("policy-distinct-approver").checked = !!p.require_distinct_approver;
	document.getElementById("policy-status").textContent = "";
	setModalTitle("policy", "Edit policy");
	document.getElementById("policy-modal").hidden = false;
}

// wirePolicyForm hooks the policy dialog up to POST /policies for a new rule and PUT /policies/{id}
// when editing. The New button resets the dialog to add mode.
function wirePolicyForm() {
	const form = document.getElementById("policy-form");
	fillInventorySelect(document.getElementById("policy-inventory"));
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

