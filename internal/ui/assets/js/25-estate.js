// The estate page: what the fleet looked like on a date, and what has moved since.
//
// Two questions over one history, so one page rather than two. An operator asking what changed
// almost always wants the state it changed from in front of them, and splitting that across pages
// means holding one date in your head while reading another.

// estateDateValue reads a date input as an RFC 3339 instant at the end of that day.
//
// A date input gives a day, and the API takes an instant. End of day is the right reading of "as of
// the 3rd": a gather at 14:00 on the 3rd is part of that day, and taking the start would answer
// with the 2nd's estate while displaying the 3rd's date.
function estateDateValue(id) {
	const raw = document.getElementById(id).value;
	if (!raw) return "";
	return new Date(raw + "T23:59:59Z").toISOString();
}

// renderEstateHorizon says how far back the records reach, and warns when the question predates
// them. Without it an empty answer reads as a fleet that did not exist rather than as history that
// does not go back that far, and those are very different things to tell somebody.
function renderEstateHorizon(data) {
	const host = document.getElementById("estate-horizon");
	if (!host) return;
	if (!data.horizon) {
		host.hidden = true;
		return;
	}
	host.hidden = false;
	host.textContent = data.before_history
		? "No readings that far back. This estate's history begins " + fmtTime(data.horizon) +
			", so an empty answer here is the records' limit rather than an empty fleet."
		: "History reaches back to " + fmtTime(data.horizon) + ".";
	host.className = data.before_history ? "warn-note" : "muted";
}

// factsSummary renders a fact set compactly, and a diff's changes as what became what.
function factsSummary(entry) {
	const cell = document.createElement("td");
	cell.className = "mono";
	if (entry.facts && entry.state) {
		// A diff row: name each key with the value at each end, because "kernel changed" is not an
		// answer anybody can act on and "5.15.0 to 6.8.0" is.
		const parts = [];
		for (const [key, change] of Object.entries(entry.facts)) {
			parts.push(key + ": " + (change.from || "absent") + " → " + (change.to || "absent"));
		}
		cell.textContent = parts.join(", ");
		return cell;
	}
	const facts = entry.facts || {};
	cell.textContent = Object.keys(facts).sort().map((k) => k + "=" + facts[k]).join(" ");
	return cell;
}

// loadEstate fills the table, either with the estate at one instant or with what moved between two.
async function loadEstate() {
	const at = estateDateValue("estate-at");
	const from = estateDateValue("estate-from");
	const diffing = from !== "";
	const tbody = document.getElementById("estate");
	const table = document.getElementById("estate-table");
	tbody.textContent = "";
	document.getElementById("estate-col-state").textContent = diffing ? "Change" : "Run";
	try {
		const url = diffing
			? "/estate/diff?from=" + encodeURIComponent(from) + (at ? "&to=" + encodeURIComponent(at) : "")
			: "/estate" + (at ? "?at=" + encodeURIComponent(at) : "");
		const data = await getJSON(url);
		renderEstateHorizon(data);
		const rows = data.hosts || [];
		if (rows.length === 0) {
			showEmpty(diffing
				? "Nothing moved in that window." + (data.unchanged ? " " + data.unchanged + " host(s) sat still." : "")
				: "No hosts were observed at that point. Facts are gathered by a run, so an estate exists once something has run.",
				true);
			table.hidden = true;
			return;
		}
		for (const entry of rows) {
			const tr = document.createElement("tr");
			const hostCell = document.createElement("td");
			hostCell.className = "mono";
			const link = document.createElement("a");
			link.href = "/ui/hosts/" + encodeURIComponent(entry.host);
			link.textContent = hostLabel(entry.host);
			link.title = entry.host;
			hostCell.appendChild(link);
			tr.appendChild(hostCell);

			const second = document.createElement("td");
			if (diffing) {
				const chip = document.createElement("span");
				chip.className = "chip " + (entry.state === "unobserved" ? "skipped"
					: entry.state === "added" ? "ok" : "changed");
				chip.textContent = entry.state;
				if (entry.state === "unobserved") {
					// The distinction that matters: nobody looked, which is not the same as looked
					// and found unchanged. Reading the second as the first is how a fleet quietly
					// stops being monitored.
					chip.title = "Nothing gathered this host in the window, so its state is carried " +
						"forward rather than confirmed. It is not known to have changed, and it is " +
						"not known not to have.";
				}
				second.appendChild(chip);
			} else if (entry.run_id) {
				const runLink = document.createElement("a");
				runLink.href = "/ui/runs/" + encodeURIComponent(entry.run_id);
				runLink.className = "mono";
				runLink.textContent = entry.run_id;
				second.appendChild(runLink);
			}
			tr.appendChild(second);
			tr.appendChild(factsSummary(entry));
			const when = document.createElement("td");
			when.textContent = entry.gathered_at ? fmtTime(entry.gathered_at) : "";
			tr.appendChild(when);
			tbody.appendChild(tr);
		}
		table.hidden = false;
		const bits = [];
		if (data.truncated) bits.push("Showing " + rows.length + " of " + data.total + ".");
		if (diffing && data.unchanged) bits.push(data.unchanged + " host(s) unchanged.");
		// Rows the caller may not read are counted rather than dropped in silence, so a short answer
		// is never mistaken for a quiet estate.
		if (data.withheld) bits.push(data.withheld + " hidden by access.");
		setStatus(bits.join(" "));
	} catch (err) {
		showEmpty("Could not read the estate: " + err.message, true);
	}
}

// wireEstate hooks the controls up and loads the current estate to start.
function wireEstate() {
	const button = document.getElementById("estate-load");
	if (button) button.addEventListener("click", loadEstate);
	loadEstate();
}
