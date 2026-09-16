// The changes page: what somebody set out to do, and every run it took.
//
// A run is what an executor produces. A change is what a person means, and it is the thing an
// auditor asks about. This page exists because the two are not the same size: "we rolled out the
// fix" is four runs, a rollback, and a fix-forward, and reading them as six unrelated rows loses
// the only question anybody was asking.

// changeOutcomeChip renders a derived outcome. Mixed gets its own treatment rather than being
// folded into failed, because a change that broke and was then put right is neither, and calling it
// failed describes an outage that was resolved as though it were not.
function changeOutcomeChip(outcome) {
	const chip = document.createElement("span");
	const cls = outcome === "succeeded" ? "ok"
		: outcome === "failed" ? "failed"
		: outcome === "mixed" ? "changed" : "skipped";
	chip.className = "chip " + cls;
	chip.textContent = (outcome || "").replace("_", " ");
	if (outcome === "mixed") {
		chip.dataset.tip = "Some runs failed and others succeeded. A rollback followed by a fix " +
			"reads this way, and it is what really happened.";
	}
	return chip;
}

// renderChangesNote states what the listing could not see.
//
// The index is built by scanning recent runs, because no store can return the distinct values of a
// label. So a change whose every run is older than the scan does not appear here. Saying so is the
// difference between "these are your changes" and "these are the changes we could see".
function renderChangesNote(data) {
	const host = document.getElementById("changes-note");
	if (!host) return;
	if (!data.partial) {
		host.hidden = true;
		return;
	}
	host.hidden = false;
	host.className = "warn-note";
	host.textContent = "This list was built from the " + data.scanned + " most recent runs, so a " +
		"change whose runs are all older than that is not shown. The change register in the audit " +
		"page answers a date range exhaustively.";
}

// loadChanges fills the table, newest change first.
async function loadChanges() {
	const tbody = document.getElementById("changes");
	const table = document.getElementById("changes-table");
	try {
		const data = await getJSON("/changes");
		renderChangesNote(data);
		const rows = data.changes || [];
		if (rows.length === 0) {
			showEmpty("No changes yet. Label a run with a change and every run sharing that label " +
				"reads as one thing here.");
			table.hidden = true;
			return;
		}
		for (const c of rows) {
			const tr = document.createElement("tr");
			const nameCell = document.createElement("td");
			const link = document.createElement("a");
			// Its runs, filtered by the label, which is where the members actually live. A second
			// detail page would show the same rows the runs list already renders better.
			link.href = "/ui/runs?q=" + encodeURIComponent("label:change=" + c.change);
			link.className = "mono";
			link.textContent = c.change;
			nameCell.appendChild(link);
			tr.appendChild(nameCell);

			const outcome = document.createElement("td");
			outcome.appendChild(changeOutcomeChip(c.outcome));
			tr.appendChild(outcome);
			tr.appendChild(td(String(c.total || 0)));
			tr.appendChild(td((c.actors || []).join(", ")));
			tr.appendChild(tdTime(c.opened_at));
			const closed = document.createElement("td");
			// An open change has no close time on purpose: one written while work continues would
			// say it was finished.
			closed.textContent = c.closed_at ? fmtTime(c.closed_at) : "still open";
			if (!c.closed_at) closed.className = "muted";
			tr.appendChild(closed);
			tbody.appendChild(tr);
		}
		table.hidden = false;
		const bits = [];
		if (data.truncated) bits.push("Showing " + rows.length + " of " + data.total + ".");
		setStatus(bits.join(" "));
	} catch (err) {
		showEmpty("Could not read changes: " + err.message, true);
	}
}
