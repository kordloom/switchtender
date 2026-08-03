// describeCron renders the common cron shapes in words. An expression it does not recognize is
// reported as a custom schedule rather than guessed at.
function describeCron(spec) {
	const parts = String(spec || "").trim().split(/\s+/);
	if (parts.length !== 5) return "Custom schedule";
	const [min, hour, dom, mon, dow] = parts;
	const days = { "0": "Sunday", "1": "Monday", "2": "Tuesday", "3": "Wednesday", "4": "Thursday", "5": "Friday", "6": "Saturday", "7": "Sunday" };
	const at = (h, m) => {
		const hh = parseInt(h, 10);
		const mm = String(parseInt(m, 10)).padStart(2, "0");
		if (isNaN(hh)) return "";
		const suffix = hh < 12 ? "am" : "pm";
		const h12 = hh % 12 === 0 ? 12 : hh % 12;
		return h12 + ":" + mm + suffix;
	};
	if (min === "*" && hour === "*") return "Every minute";
	if (hour === "*" && /^\*\/\d+$/.test(min)) return "Every " + min.slice(2) + " minutes";
	if (hour === "*" && /^\d+$/.test(min) && dom === "*" && mon === "*" && dow === "*") {
		return parseInt(min, 10) === 0 ? "Hourly, on the hour" : "Hourly at :" + String(min).padStart(2, "0");
	}
	if (/^\*\/\d+$/.test(hour) && /^\d+$/.test(min)) return "Every " + hour.slice(2) + " hours";
	// The named shapes below all read a clock time, so both fields must be plain numbers: a step
	// or wildcard in either belongs to a cadence no sentence here describes.
	const timed = /^\d+$/.test(min) && /^\d+$/.test(hour);
	if (dom === "*" && mon === "*" && dow === "*" && timed) return "Daily at " + at(hour, min);
	if (dom === "*" && mon === "*" && days[dow] && timed) return days[dow] + "s at " + at(hour, min);
	if (dow === "*" && mon === "*" && /^\d+$/.test(dom) && timed) return "Monthly on day " + dom + " at " + at(hour, min);
	if (dow === "1-5" && timed) return "Weekdays at " + at(hour, min);
	if ((dow === "6,0" || dow === "0,6") && timed) return "Weekends at " + at(hour, min);
	return "Custom schedule";
}

// CRON_MONTHS and CRON_DAYS name the values of the two cron fields that read as words rather than
// numbers, so a breakdown says December rather than 12.
const CRON_MONTHS = ["January", "February", "March", "April", "May", "June", "July", "August",
	"September", "October", "November", "December"];
const CRON_DAYS = ["Sunday", "Monday", "Tuesday", "Wednesday", "Thursday", "Friday", "Saturday"];

// CRON_FIELDS describes each position of a five-field expression: what it is called, what a wildcard
// means there, the unit a step counts in, and the names its values carry when they have any.
const CRON_FIELDS = [
	{ label: "Minute", any: "every minute", unit: "minutes", offset: 0 },
	{ label: "Hour", any: "every hour", unit: "hours", offset: 0 },
	{ label: "Day of month", any: "every day", unit: "days", offset: 0 },
	{ label: "Month", any: "every month", unit: "months", offset: 1, names: CRON_MONTHS },
	{ label: "Day of week", any: "every day", unit: "days", offset: 0, names: CRON_DAYS },
];

// cronValue names one value inside a cron field, using the field's vocabulary where it has one. A
// day-of-week 7 is Sunday, the same as 0, which is how cron itself reads it.
function cronValue(token, field) {
	const n = parseInt(token, 10);
	if (isNaN(n)) return token;
	if (!field.names) return token;
	const idx = n - field.offset;
	return field.names[idx % field.names.length] || token;
}

// describeCronField reads one field of a cron expression in plain words, covering wildcards, steps,
// ranges, and lists. An expression it cannot read comes back verbatim rather than guessed at.
function describeCronField(spec, field) {
	if (spec === "*" || spec === "?") return field.any;
	const step = spec.match(/^(.+)\/(\d+)$/);
	if (step) {
		const every = "every " + step[2] + " " + field.unit;
		if (step[1] === "*") return every;
		const range = step[1].match(/^(\d+)-(\d+)$/);
		if (range) {
			return every + " from " + cronValue(range[1], field) + " to " + cronValue(range[2], field);
		}
		return every + " from " + cronValue(step[1], field);
	}
	const range = spec.match(/^(\d+)-(\d+)$/);
	if (range) return cronValue(range[1], field) + " to " + cronValue(range[2], field);
	if (spec.includes(",")) {
		return spec.split(",").map((part) => describeCronField(part.trim(), field)).join(", ");
	}
	return cronValue(spec, field);
}

// cronBreakdown reads a cron expression field by field, so a reader who does not hold the five
// positions in their head can see which number means what.
function cronBreakdown(spec) {
	const parts = String(spec || "").trim().split(/\s+/);
	if (parts.length !== 5) return "";
	return parts.map((part, i) =>
		CRON_FIELDS[i].label + ": " + describeCronField(part, CRON_FIELDS[i])).join("\n");
}

// cronTip is the hover text for a cron expression: its cadence in one line, then the field-by-field
// reading, so hovering the syntax anywhere in the product explains it.
function cronTip(spec) {
	const breakdown = cronBreakdown(spec);
	if (!breakdown) return "Five fields: minute, hour, day of month, month, day of week";
	return describeCron(spec) + "\n" + breakdown;
}

// wireCronTips explains every cron expression on the page on hover, and keeps explaining the one
// being typed in a form field as it changes.
//
// An input is subscribed once however often this runs. The schedules page rewires after every save,
// but the form input outlives the table it rewires, so a fresh listener each time left one listener
// per save on the same field: after ten saves every keystroke did the same work ten times and wrote
// the same tip ten times. The tip itself is still refreshed on each call, so a rewire after a save
// shows the current value.
function wireCronTips(root) {
	for (const el of (root || document).querySelectorAll("[data-cron]")) {
		el.dataset.tip = cronTip(el.dataset.cron);
	}
	for (const input of (root || document).querySelectorAll("input[data-cron-input]")) {
		const sync = () => { input.dataset.tip = cronTip(input.value.trim()); };
		sync();
		if (input.dataset.cronWired) continue;
		input.dataset.cronWired = "true";
		input.addEventListener("input", sync);
	}
}

// mountHostActions fills the host page's action bar: the things an operator wants to do to one
// host, rather than leaving the page a dead-end table.
function mountHostActions(host) {
	const bar = document.getElementById("host-actions");
	if (!bar) return;
	const add = (label, href, tip) => {
		const a = document.createElement("a");
		a.className = "button";
		a.href = href;
		a.textContent = label;
		a.dataset.tip = tip;
		bar.appendChild(a);
	};
	add("Runs on this host", "/ui/runs?q=" + encodeURIComponent("host:" + host),
		"Click to see every run that touched this host");
	add("Drift", "/ui/drift?q=" + encodeURIComponent(host),
		"Click to see this host's divergence from the desired state");
	add("Fleet health", "/ui/fleet?q=" + encodeURIComponent(host),
		"Click to see this host beside the rest of the fleet");
	const copy = copyButton(host, "Copy this host name");
	copy.className = "button";
	copy.appendChild(document.createTextNode("Copy name"));
	bar.appendChild(copy);
}

// FACT_LABELS names each stored fact for the interface, in the order they read best.
const FACT_LABELS = [
	["fqdn", "Hostname"],
	["distribution", "Distribution"],
	["distribution_version", "Version"],
	["kernel", "Kernel"],
	["architecture", "Architecture"],
	["processor_vcpus", "vCPUs"],
	["memtotal_mb", "Memory"],
	["ip", "Address"],
	["service_mgr", "Service manager"],
	["virtualization_type", "Virtualization"],
	["python_version", "Python"],
];

// loadHostFacts fills the system facts panel from the last run that gathered them. A host that
// has never been gathered says so and explains how to gather, rather than showing a blank card.
async function loadHostFacts(host) {
	const panel = document.getElementById("host-facts");
	const body = document.getElementById("host-facts-body");
	const note = document.getElementById("host-facts-note");
	if (!panel || !body) return;
	try {
		const data = await getJSON("/hosts/" + encodeURIComponent(host) + "/facts");
		const facts = data.facts || {};
		body.innerHTML = "";
		for (const [key, label] of FACT_LABELS) {
			if (!facts[key]) continue;
			const k = document.createElement("span");
			k.className = "view-k";
			k.textContent = label;
			const v = document.createElement("span");
			v.className = "view-v";
			v.textContent = key === "memtotal_mb" ? facts[key] + " MB" : facts[key];
			body.appendChild(k);
			body.appendChild(v);
		}
		if (!body.childElementCount) {
			body.innerHTML = "";
			note.textContent = "The last gather returned nothing recognizable.";
		} else if (note) {
			note.textContent = data.gathered_at
				? "Gathered " + relTime(data.gathered_at)
				: "";
			if (data.run_id) {
				note.appendChild(document.createTextNode(" by "));
				const link = document.createElement("a");
				link.href = "/ui/runs/" + data.run_id;
				link.textContent = shortId(data.run_id);
				link.dataset.tip = "Click to open the run that gathered these facts";
				note.appendChild(link);
			}
		}
		panel.hidden = false;
	} catch {
		// A host with no gather is the ordinary case on a fleet that runs with gather_facts off.
		body.innerHTML = "";
		if (note) {
			note.textContent = "No facts gathered yet. Run a play against this host with " +
				"gather_facts enabled and they will appear here.";
		}
		panel.hidden = false;
	}
}

