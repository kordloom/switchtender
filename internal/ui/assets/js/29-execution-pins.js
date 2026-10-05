// imagePinned reports whether an image reference names the exact image by digest rather than by a
// tag the registry can move.
function imagePinned(ref) {
	return typeof ref === "string" && ref.includes("@sha256:");
}

// imagePinLabel states how a run's image is pinned, the way an approver reads it: by the digest the
// tag resolved to when the run was submitted, or by a tag alone when the registry did not answer
// then, which the outcome later pins to the digest that was pulled.
function imagePinLabel(run) {
	if (!run || !run.image) return "";
	if (imagePinned(run.image)) {
		const at = run.image.indexOf("@sha256:");
		return "pinned to " + run.image.slice(at + 1, at + 20);
	}
	return "tag not pinned to a digest";
}

// inventorySnapshotLabel states what the inventory snapshot a run executes holds: the hosts it
// names, the hosts a composed inventory resolved to, or that it is a dynamic source whose hosts
// resolve at execution. It returns the empty string for a run with no snapshot.
function inventorySnapshotLabel(run) {
	const snap = run && run.inventory_snapshot;
	if (!snap) return "";
	if (run.inventory_resolution && !snap.dynamic) {
		const hosts = Array.isArray(snap.hosts) ? snap.hosts : [];
		return hosts.length + " host" + (hosts.length === 1 ? "" : "s") + " as resolved at submission";
	}
	if (snap.dynamic) {
		const resolved = Array.isArray(run.resolved_hosts) ? run.resolved_hosts : [];
		return resolved.length
			? "dynamic source, resolved at execution to " + resolved.length + " host" +
				(resolved.length === 1 ? "" : "s")
			: "dynamic source: hosts resolve at execution";
	}
	const hosts = Array.isArray(snap.hosts) ? snap.hosts : [];
	return hosts.length + " host" + (hosts.length === 1 ? "" : "s") + " as submitted";
}

// executionPinsNote explains, where an approver decides, what the approval binds about where and
// against what the run executes: an image held to a tag alone, an inventory snapshot taken when the
// run was submitted, the result a composed inventory resolved to, or a dynamic source that resolves
// only at execution, and the saved plan an apply carries out, with what to do when the tool refuses
// that plan as stale. It returns null when there is nothing to say.
function executionPinsNote(run) {
	const lines = [];
	if (run.image && !imagePinned(run.image)) {
		lines.push("The image " + run.image + " is a tag not pinned to a digest: the registry did not " +
			"answer when the run was submitted, so approving binds the tag, and the outcome records the " +
			"digest that was pulled.");
	}
	const snap = run.inventory_snapshot;
	if (snap && snap.dynamic) {
		lines.push("The inventory is a dynamic source, so its hosts resolve at execution, against the " +
			"live system it describes. Approving binds the source's definition, and the outcome records " +
			"the hosts it resolved to.");
	} else if (snap) {
		const hosts = Array.isArray(snap.hosts) ? snap.hosts : [];
		const named = hosts.length + " host" + (hosts.length === 1 ? "" : "s") +
			(hosts.length ? ": " + hosts.slice(0, 20).join(", ") +
			(hosts.length > 20 ? ", and " + (hosts.length - 20) + " more" : "") : "");
		lines.push(run.inventory_resolution
			? "The run executes the composed inventory as it resolved when the run was submitted, " +
				named + ", with their variables as its inputs held them then. An edit to an input after " +
				"approval, a variable included, does not reach it."
			: "The run executes the inventory as it was submitted, " + named +
				". An edit to the inventory after approval does not reach it.");
	}
	if (run.plan_sha256) {
		lines.push(run.source === "reconcile"
			? "Approving releases the plan the drift check saved, the plan shown with that check, and " +
				"the apply runs that plan rather than planning again. If the infrastructure changes " +
				"before the approval lands, the tool refuses the stale plan and changes nothing, and the " +
				"reconcile has to be proposed again from a new drift check."
			: "Approving releases the saved plan file this apply carries out, the plan its plan run " +
				"made, and the apply runs that plan rather than planning again. If the infrastructure " +
				"changes before the approval lands, the tool refuses the stale plan and changes nothing, " +
				"and the apply has to be submitted again to plan it afresh.");
	}
	if (!lines.length) return null;
	const box = document.createElement("div");
	box.className = "risk-pins";
	for (const text of lines) {
		const p = document.createElement("p");
		p.textContent = text;
		box.appendChild(p);
	}
	return box;
}
