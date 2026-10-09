// FEDKEY_STATES maps a federation signing key's state to the badge it is drawn with and the word a
// reader sees. The badge classes are the run status colors, so signing reads as healthy, pending as
// waiting, retired as idle, and removed as stopped.
const FEDKEY_STATES = {
	pending: { badge: "pending_approval", label: "Pending" },
	signing: { badge: "succeeded", label: "Signing" },
	retired: { badge: "pending", label: "Retired" },
	removed: { badge: "canceled", label: "Removed" },
};

// fedkeyStatus writes a sentence under the signing key heading.
function fedkeyStatus(text) {
	const el = document.getElementById("fedkey-status");
	if (el) el.textContent = text;
}

// fedkeyShortID shortens a key id, a 43 character thumbprint, to something a row can hold, keeping
// the whole id in the cell's tip and in the text a copy takes.
function fedkeyShortID(id) {
	const s = String(id || "");
	return s.length > 16 ? s.slice(0, 12) + "..." : s;
}

// loadFederationKeys fills the signing key section. Federation is off on most installs, where the
// listing answers 404, so the section stays hidden without a word, the way credential types do. An
// install the server said has it off is not asked, since the browser logs the 404 the page would be
// quietly reading.
async function loadFederationKeys() {
	const section = document.getElementById("fedkey-section");
	if (!section) return;
	if (featureOff("federation")) {
		section.hidden = true;
		return;
	}
	let data;
	try {
		data = await getJSON("/federation/keys");
	} catch (err) {
		if (err.status === 404 || err.status === 403) {
			section.hidden = true;
			return;
		}
		section.hidden = false;
		fedkeyStatus("Failed to load the signing keys: " + err.message);
		return;
	}
	renderFederationKeys(data);
	section.hidden = false;
}

// renderFederationKeys draws the key table from a listing or a rotation answer, and sets the
// rotation controls: a normal rotation is offered only when none is under way, and both are drawn
// only for an admin on an install that accepts changes.
function renderFederationKeys(data) {
	const keys = (data && data.keys) || [];
	const tbody = document.getElementById("fedkeys");
	const table = document.getElementById("fedkey-table");
	if (!tbody || !table) return;
	tbody.textContent = "";
	for (const k of keys) {
		const tr = document.createElement("tr");
		const idCell = td(fedkeyShortID(k.id), "mono");
		idCell.dataset.tip = k.id;
		idCell.dataset.keyId = k.id;
		tr.appendChild(idCell);
		const state = FEDKEY_STATES[k.state] || { badge: "pending", label: k.state || "Unknown" };
		const stateCell = document.createElement("td");
		const badge = document.createElement("span");
		badge.className = "badge " + state.badge;
		badge.textContent = state.label;
		stateCell.appendChild(badge);
		tr.appendChild(stateCell);
		tr.appendChild(tdTime(k.created_at, ""));
		tr.appendChild(tdTime(k.activated_at, "Never"));
		tr.appendChild(tdTime(k.retired_at, "Not scheduled"));
		tr.appendChild(tdTime(k.removed_at, "Not scheduled"));
		tbody.appendChild(tr);
	}
	table.hidden = keys.length === 0;
	const intro = document.getElementById("fedkey-intro");
	if (intro && data && data.issuer) {
		intro.textContent = "Clouds verify each run's identity token against these keys, published at " +
			data.issuer + "/.well-known/jwks.json. Each key's private half is sealed with this " +
			"server's encryption key, which lives outside the database.";
	}
	const pending = keys.find((k) => k.state === "pending");
	const rotate = document.getElementById("fedkey-rotate");
	const emergency = document.getElementById("fedkey-emergency-open");
	const canAct = roleAtLeast("admin");
	if (rotate) {
		rotate.hidden = !canAct;
		rotate.disabled = !!pending || isReadOnly();
		rotate.dataset.tip = pending
			? "A rotation is under way. Key " + fedkeyShortID(pending.id) + " starts signing " +
				relTime(pending.activated_at) + "."
			: "Publish a new key now and start signing with it in 24 hours";
	}
	if (emergency) {
		emergency.hidden = !canAct;
		emergency.disabled = isReadOnly();
	}
	if (keys.length === 0) {
		fedkeyStatus("No signing key yet. The server creates one when it starts with an issuer URL.");
	} else if (pending) {
		fedkeyStatus("A rotation is under way. Key " + fedkeyShortID(pending.id) + " was published " +
			relTime(pending.created_at) + " and starts signing " + relTime(pending.activated_at) +
			". The current key signs until then.");
	} else {
		fedkeyStatus("");
	}
}

// wireFederationKeys hooks the two rotations up. A normal rotation goes straight to the server,
// which refuses it while one is under way and says why. An emergency rotation opens a dialog that
// states what it costs before anything is sent, since it fails runs and cannot be undone.
function wireFederationKeys() {
	const rotate = document.getElementById("fedkey-rotate");
	if (rotate) {
		rotate.addEventListener("click", guardedSubmit(rotate, async () => {
			const data = await postAction("/federation/keys/rotate", {});
			renderFederationKeys(data);
			const key = data.key || {};
			fedkeyStatus("Published key " + fedkeyShortID(key.id) + ". It starts signing " +
				relTime(key.activated_at) + ", and the current key signs until then.");
		}, (err) => fedkeyStatus("Rotation refused: " + err.message)));
	}
	const open = document.getElementById("fedkey-emergency-open");
	if (open) {
		open.addEventListener("click", () => {
			const status = document.getElementById("fedkey-emergency-status");
			if (status) status.textContent = "";
			openDialog("fedkey-emergency");
		});
	}
	const close = document.getElementById("fedkey-emergency-close");
	if (close) close.addEventListener("click", () => closeDialog("fedkey-emergency"));
	const confirm = document.getElementById("fedkey-emergency-confirm");
	if (confirm) {
		confirm.addEventListener("click", async () => {
			if (confirm.disabled) return;
			confirm.disabled = true;
			const status = document.getElementById("fedkey-emergency-status");
			try {
				const data = await postAction("/federation/keys/rotate/emergency", {});
				closeDialog("fedkey-emergency");
				renderFederationKeys(data);
				const key = data.key || {};
				fedkeyStatus("Key " + fedkeyShortID(key.id) + " signs now. Every other key left the " +
					"published set and its private half was erased.");
			} catch (err) {
				if (status) status.textContent = "Emergency rotation failed: " + err.message;
			} finally {
				confirm.disabled = false;
			}
		});
	}
}
