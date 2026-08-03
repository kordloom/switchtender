// openUserEdit fills the user dialog with an existing account and switches it to edit mode. The
// password field becomes optional, so a blank leaves the current password unchanged.
// USER_PROFILE_FIELDS maps each profile input to the account field it edits, so the dialog fills and
// collects the profile in one place instead of naming every field twice.
const USER_PROFILE_FIELDS = {
	"user-fullname": "full_name",
	"user-email": "email",
	"user-phone": "phone",
	"user-title": "title",
	"user-notes": "notes",
};

// fillUserProfile writes an account's profile into the dialog, or clears it when given none.
function fillUserProfile(u) {
	for (const [id, key] of Object.entries(USER_PROFILE_FIELDS)) {
		const el = document.getElementById(id);
		if (el) el.value = (u && u[key]) || "";
	}
	const links = document.getElementById("user-links");
	if (links) links.value = ((u && u.links) || []).join("\n");
}

// collectUserProfile reads the profile out of the dialog. The whole profile is sent on every save, so
// clearing a field clears it on the account. The server validates and bounds each value.
function collectUserProfile() {
	const payload = {};
	for (const [id, key] of Object.entries(USER_PROFILE_FIELDS)) {
		const el = document.getElementById(id);
		payload[key] = el ? el.value.trim() : "";
	}
	const links = document.getElementById("user-links");
	payload.links = links
		? links.value.split("\n").map((l) => l.trim()).filter(Boolean)
		: [];
	return payload;
}

function openUserEdit(u) {
	const form = document.getElementById("user-form");
	form.dataset.editId = u.id;
	document.getElementById("user-name").value = u.username;
	const pw = document.getElementById("user-password");
	pw.value = "";
	pw.required = false;
	pw.placeholder = "Leave blank to keep current";
	document.getElementById("user-role").value = u.role;
	fillUserProfile(u);
	document.getElementById("user-status").textContent = "";
	setModalTitle("user", "Edit user");
	document.getElementById("user-modal").hidden = false;
}

// wireUserForm hooks the user dialog up to POST /users for a new account and PUT /users/{id} when
// editing. The New button resets the dialog to add mode, where a password is required.
function wireUserForm() {
	const form = document.getElementById("user-form");
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("user-name").value = "";
		const pw = document.getElementById("user-password");
		pw.value = "";
		pw.required = true;
		pw.placeholder = "";
		document.getElementById("user-role").value = "operator";
		fillUserProfile(null);
		document.getElementById("user-status").textContent = "";
		setModalTitle("user", "Add a user");
	};
	const openBtn = document.getElementById("user-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still posting, so a fast double click on Save
	// stores the account once rather than twice. A modal save stays on the page, so the button is
	// re-enabled once the request settles either way, leaving the dialog usable for the next account.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("user-status");
		const editId = form.dataset.editId;
		const payload = Object.assign({
			username: document.getElementById("user-name").value.trim(),
			role: document.getElementById("user-role").value,
		}, collectUserProfile());
		const pw = document.getElementById("user-password").value;
		if (pw) payload.password = pw;
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/users/" + editId, payload, "PUT");
			} else {
				await postAction("/users", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("user");
			document.getElementById("users").innerHTML = "";
			loadUsers();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// loadUsers populates the user table with delete actions.
// userActivity counts each account's runs and finds when it last acted, so the users list shows
// who is actually driving the fleet rather than only who exists.
async function userActivity() {
	const map = new Map();
	try {
		const data = await getJSON("/runs?limit=500");
		for (const r of data.runs || []) {
			if (!r.actor) continue;
			const at = r.created_at;
			const cur = map.get(r.actor) || { runs: 0, last: null };
			cur.runs++;
			if (!cur.last || (at && at > cur.last)) cur.last = at;
			map.set(r.actor, cur);
		}
	} catch { /* the columns fall back to zero and never */ }
	return map;
}

async function loadUsers() {
	const activity = await userActivity();
	try {
		const data = await getJSON("/users");
		const users = data.users || [];
		if (users.length === 0) {
			showEmpty("No users yet.");
			return;
		}
		const tbody = document.getElementById("users");
		for (const u of users) {
			const tr = document.createElement("tr");
			tr.appendChild(td(u.username));
			// Who the account belongs to, with their title under the name. Phone, notes, and links stay
			// out of the table: it exports to CSV in one click, and a roster of names and addresses is
			// what an access review needs without also spilling everyone's contact number into a file.
			const who = td("");
			if (u.full_name) {
				const name = document.createElement("span");
				name.textContent = u.full_name;
				who.appendChild(name);
			} else {
				who.appendChild(document.createTextNode("—"));
			}
			if (u.title) {
				const title = document.createElement("span");
				title.className = "user-title";
				title.textContent = u.title;
				who.appendChild(title);
			}
			tr.appendChild(who);
			const role = td("");
			const roleChip = document.createElement("span");
			roleChip.className = "run-kind" + (u.role === "admin" ? " split" : "");
			roleChip.textContent = u.role;
			roleChip.dataset.tip = u.role === "admin"
				? "Full access, including credentials, users, and policies"
				: "Can run and read what they are granted";
			role.appendChild(roleChip);
			tr.appendChild(role);
			const contact = td("");
			if (u.email) {
				const mail = document.createElement("a");
				mail.href = "mailto:" + u.email;
				mail.textContent = u.email;
				mail.dataset.tip = "Click to write to this address";
				contact.appendChild(mail);
			} else {
				contact.textContent = "—";
			}
			// Links open in a new tab and never carry a referrer, and the scheme is checked before
			// the value becomes an href. The server validates every write path and is the
			// authority, but a value that predates a validator, or arrives through one nobody
			// thought of, should not become a script URL because this line trusted it.
			for (const link of u.links || []) {
				if (!/^https?:\/\//i.test(link)) continue;
				const chip = document.createElement("a");
				chip.className = "user-link";
				chip.href = link;
				chip.target = "_blank";
				chip.rel = "noopener noreferrer";
				chip.textContent = linkHost(link);
				chip.dataset.tip = "Opens " + link;
				contact.appendChild(chip);
			}
			tr.appendChild(contact);
			const act = activity.get(u.username) || { runs: 0, last: null };
			const fired = td("");
			if (act.runs) {
				const link = document.createElement("a");
				link.href = "/ui/runs?q=" + encodeURIComponent("actor:" + u.username);
				link.textContent = String(act.runs);
				link.dataset.tip = "Click to see every run this account fired";
				fired.appendChild(link);
			} else {
				fired.textContent = "0";
			}
			tr.appendChild(fired);
			tr.appendChild(act.last ? tdTime(act.last) : td("never"));
			tr.appendChild(tdTime(u.created_at));
			const actions = deleteCell("/users/" + u.id, "user " + u.username, tr, "No users yet.");
			actions.insertBefore(editButton(() => openUserEdit(u), "Click to change this account's role or password"), actions.firstChild);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load users: " + e.message);
	}
}

// linkHost labels a profile link by its host, so a column of links reads as the places they lead
// rather than as a row of full addresses. A value that will not parse is shown as given.
function linkHost(link) {
	try {
		return new URL(link).hostname.replace(/^www\./, "");
	} catch {
		return link;
	}
}

