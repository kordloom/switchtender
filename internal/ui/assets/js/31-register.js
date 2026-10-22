// The change register page: every change in the period with who asked for it, the decision over it,
// and how it ended, drawn on screen from the same register the Evidence pack downloads.
//
// The rows come from GET /v1/audit/register?format=json, which the server builds with the one
// function that also renders the downloaded document, so this page and the file an auditor receives
// cannot list different changes or describe one differently. Nothing here computes a row or a
// sentence: the page draws what the server built, and the filter, facets, pager, sort and export
// are the ones every list page uses.

// REGISTER_VERDICTS maps the register's verdict class to the chip the app draws for it and the word
// the chip says.
const REGISTER_VERDICTS = {
	verified: { cls: "ok", label: "Verified" },
	unanchored: { cls: "warn", label: "Unanchored" },
	broken: { cls: "failed", label: "Broken" },
};

// registerOutcomeChip draws a row's outcome the way the document colors it: succeeded is good,
// failed is bad, and anything else, such as a run still waiting for a decision, is a warning.
function registerOutcomeChip(outcome) {
	const chip = document.createElement("span");
	const cls = outcome === "succeeded" ? "ok" : outcome === "failed" ? "failed" : "warn";
	chip.className = "chip " + cls;
	chip.textContent = outcome || "";
	return chip;
}

// registerMark appends one warning chip to a cell, after a space, the way the document marks a dry
// run and a dry run that was not change free.
function registerMark(cell, label) {
	const chip = document.createElement("span");
	chip.className = "chip warn";
	chip.textContent = label;
	cell.append(" ", chip);
}

// registerRow builds one table row from a register row, in the document's column order and with the
// document's words for an empty value.
function registerRow(row) {
	const tr = document.createElement("tr");
	tr.appendChild(td(row.when || ""));

	const run = document.createElement("td");
	run.className = "mono";
	if (row.run) {
		const link = document.createElement("a");
		link.href = "/ui/runs/" + encodeURIComponent(row.run);
		link.textContent = row.run;
		run.appendChild(link);
	}
	tr.appendChild(run);

	const change = td(row.change || "", "mono");
	if (row.dry_run) registerMark(change, "dry run");
	if (row.not_change_free) registerMark(change, "not change free");
	tr.appendChild(change);

	tr.appendChild(row.actor ? td(row.actor) : td("none recorded", "muted"));
	tr.appendChild(td(row.source || ""));
	tr.appendChild(td(row.risk || ""));
	tr.appendChild(td(row.held || "—"));

	const decision = td(row.decision || "—");
	if (row.decision) {
		// The chain entry that recorded the decision, so a reader can redeem it against the chain.
		const seq = document.createElement("span");
		seq.className = "mono";
		seq.textContent = "#" + (row.decision_seq || 0);
		decision.append(" ", seq);
	}
	tr.appendChild(decision);

	const outcome = document.createElement("td");
	outcome.appendChild(registerOutcomeChip(row.outcome));
	tr.appendChild(outcome);
	return tr;
}

// showRegisterNotice shows a notice the register carries in the element with the given id, or hides
// that element when the register carries none.
function showRegisterNotice(id, text) {
	const el = document.getElementById(id);
	if (!el) return;
	el.textContent = text || "";
	el.hidden = !text;
}

// renderRegisterHead draws everything above the table: the period, the chain's verdict, the notices
// a pruned or truncated register carries, and the four tallies the document leads with. A period
// whose runs retention removed lists nothing and still says so, which is why this is drawn before
// the rows and whether or not there are any.
function renderRegisterHead(reg) {
	const period = document.getElementById("register-period");
	if (period && reg.from && reg.to) {
		period.textContent = "Period: " + reg.from + " to " + reg.to + ".";
		period.hidden = false;
	}
	const chain = document.getElementById("register-chain");
	const verdict = REGISTER_VERDICTS[reg.status];
	if (chain && verdict && reg.status_text) {
		chain.textContent = "";
		const chip = document.createElement("span");
		chip.className = "chip " + verdict.cls;
		chip.textContent = verdict.label;
		chain.append(chip, " ", reg.status_text);
		chain.hidden = false;
	}
	showRegisterNotice("register-pruned", reg.pruned_text);
	showRegisterNotice("register-truncated", reg.truncated ? reg.truncated_text : "");
	const summary = document.getElementById("register-summary");
	if (summary) {
		summary.textContent = "";
		summary.appendChild(statCard(reg.total || 0, reg.truncated ? "Changes shown" : "Changes", ""));
		summary.appendChild(statCard(reg.approved || 0, "Approved", "ok"));
		summary.appendChild(statCard(reg.rejected || 0, "Rejected", ""));
		summary.appendChild(statCard(reg.failed || 0, "Failed", "failed"));
		summary.hidden = false;
	}
}

// renderRegisterForAdminsOnly explains the page to a session below admin, which the server refuses
// the register to. The download is hidden with it, since it reads the same refused register.
function renderRegisterForAdminsOnly() {
	const download = document.getElementById("register-download");
	if (download) download.hidden = true;
	showEmpty("The change register is readable by admins. A run you launched carries its own " +
		"evidence and signed receipt on its page.");
}

// renderRegisterRefusal states why there is no register to show. A licensing refusal carries the
// pricing link and becomes the same offer the Audit page makes. A refusal without it is about role,
// and is never shown as an offer. Anything else is reported as the failure it is.
function renderRegisterRefusal(err) {
	const table = document.getElementById("register-table");
	if (table) table.hidden = true;
	if (err && err.status === 403) {
		const message = String(err.message || "");
		if (message.includes(PRICING_URL)) {
			const download = document.getElementById("register-download");
			if (download) download.hidden = true;
			renderLicensedFeature(message);
		} else {
			renderRegisterForAdminsOnly();
		}
		return;
	}
	setStatus("Could not read the change register: " + (err && err.message ? err.message : "no answer"));
}

// wireRegister hooks the Download document button to the same download the Evidence pack button
// makes, so the file saved here is the document whose rows the page shows.
function wireRegister() {
	const download = document.getElementById("register-download");
	if (download) download.addEventListener("click", () => downloadEvidencePack(download));
}

// loadRegister reads the period's register as data through the session's own credential and draws
// it. getJSON carries the header token, sends a dead session to sign in, and keeps the server's
// licensing sentence on a 403 while turning a bare role refusal into the role sentence. A body
// without rows is read as an empty period, so a register that lists nothing says so.
async function loadRegister() {
	const table = document.getElementById("register-table");
	const tbody = document.getElementById("register-rows");
	if (!table || !tbody) return;
	try {
		const reg = await getJSON("/audit/register?format=json");
		if (!reg || typeof reg !== "object") throw new Error("the server answered with no register");
		renderRegisterHead(reg);
		const rows = Array.isArray(reg.rows) ? reg.rows : [];
		tbody.textContent = "";
		if (rows.length === 0) {
			table.hidden = true;
			showEmpty("The register lists no changes in this period.");
			return;
		}
		for (const row of rows) tbody.appendChild(registerRow(row));
		table.hidden = false;
		setStatus("");
		showListControls();
	} catch (err) {
		renderRegisterRefusal(err);
	}
}
