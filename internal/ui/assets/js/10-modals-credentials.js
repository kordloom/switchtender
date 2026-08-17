// closeModal hides a create dialog by name, used after a successful save.
function closeModal(name) {
	const modal = document.getElementById(name + "-modal");
	if (modal) modal.hidden = true;
}

// setModalTitle rewrites a dialog's heading, used to switch a create dialog between add and edit.
function setModalTitle(name, text) {
	const h = document.querySelector("#" + name + "-modal .modal-head h2");
	if (h) h.textContent = text;
}

// editButton builds an inline Edit action for a table row. Its click does not bubble, so the row's
// inspect drawer stays closed.
function editButton(onClick, what) {
	const b = document.createElement("button");
	b.className = "button";
	b.dataset.mutates = "true";
	b.dataset.tip = what || "Click to edit this record";
	b.textContent = "Edit";
	b.addEventListener("click", (e) => {
		e.preventDefault();
		e.stopPropagation();
		onClick();
	});
	return b;
}

// guardedSubmit wraps an async submit handler so a second click while the first is still in flight
// is dropped rather than starting a duplicate run against the same hosts. The control is disabled
// before the await and re-enabled only when the handler throws, since a launch that succeeds
// navigates to the new run and a live button in the meantime is an invitation to launch twice.
// The handler reports failure by throwing, and report shows it with the control usable again.
function guardedSubmit(control, fn, report) {
	let inFlight = false;
	return async function (...args) {
		if (inFlight) return;
		inFlight = true;
		if (control) control.disabled = true;
		try {
			await fn.apply(this, args);
		} catch (err) {
			inFlight = false;
			if (control) control.disabled = false;
			if (report) report(err);
			else throw err;
		}
	};
}

// wireLaunchForm hooks the launch panel up to POST /runs and fills the credential picker. The tool
// selector swaps the Ansible fields for a single command box, so bash, terraform, python, and go
// launch from the same panel.
function wireLaunchForm() {
	const form = document.getElementById("launch-form");
	if (!form) return;
	fillCredentialPicker();
	fillSelect(document.getElementById("launch-project"), "/projects", "projects", (p) => p.name);
	fillSelect(document.getElementById("launch-inventory-id"), "/inventories", "inventories",
		(i) => i.name);

	const toolSel = document.getElementById("launch-tool");
	const ansibleFields = ["launch-field-playbook", "launch-field-inventory",
		"launch-field-inventory-id", "launch-field-shards"];
	const commandField = document.getElementById("launch-field-command");
	const commandInput = document.getElementById("launch-command");
	const syncTool = () => {
		const tool = toolSel.value;
		const ansible = tool === "ansible" || tool === "";
		for (const id of ansibleFields) {
			const el = document.getElementById(id);
			if (el) el.hidden = !ansible;
		}
		commandField.hidden = ansible;
		if (tool === "terraform" || tool === "opentofu") commandInput.placeholder = "working directory, e.g. infra";
		else if (tool === "python") commandInput.placeholder = "print('hello from python')";
		else if (tool === "go") commandInput.placeholder = "package main\n\nfunc main() { println(\"hi\") }";
		else commandInput.placeholder = "echo hello";
	};
	toolSel.addEventListener("change", syncTool);
	syncTool();

	const status = document.getElementById("launch-status");
	const submit = guardedSubmit(form.querySelector('button[type="submit"]'), async () => {
		const tool = toolSel.value;
		const payload = {};
		if (tool && tool !== "ansible") {
			payload.tool = tool;
			payload.command = commandInput.value.trim();
		} else {
			payload.playbook = document.getElementById("launch-playbook").value.trim();
			payload.inventory = document.getElementById("launch-inventory").value.trim();
			const inventoryID = document.getElementById("launch-inventory-id").value;
			if (inventoryID) {
				payload.inventory_id = inventoryID;
				delete payload.inventory;
			}
			const shards = parseInt(document.getElementById("launch-shards").value, 10);
			if (shards >= 2) payload.shards = shards;
		}
		const projectID = document.getElementById("launch-project").value;
		if (projectID) payload.project_id = projectID;
		const queue = document.getElementById("launch-queue").value.trim();
		if (queue) payload.queue = queue;
		const picked = Array.from(document.getElementById("launch-credentials").selectedOptions)
			.map((o) => o.value);
		if (picked.length) payload.credential_ids = picked;
		if (document.getElementById("launch-dry-run").checked) payload.dry_run = true;
		if (document.getElementById("launch-require-approval").checked) payload.require_approval = true;
		status.textContent = "Launching.";
		const created = await postAction("/runs", payload);
		location.href = "/ui/runs/" + created.id;
	}, (err) => {
		status.textContent = "Launch failed: " + err.message;
	});
	// The guard drops a repeat submit, so the default form action is canceled out here rather than
	// inside it, where a dropped submit would fall through to a full page post.
	form.addEventListener("submit", (e) => {
		e.preventDefault();
		submit();
	});
}

// fillCredentialPicker loads stored credentials into the launch multiselect.
async function fillCredentialPicker() {
	const picker = document.getElementById("launch-credentials");
	if (!picker) return;
	try {
		const data = await getJSON("/credentials");
		for (const c of data.credentials || []) {
			const opt = document.createElement("option");
			opt.value = c.id;
			opt.textContent = c.name + " (" + c.kind + ")";
			picker.appendChild(opt);
		}
	} catch (_) { /* credentials disabled or unauthorized; picker stays empty */ }
}

// CRED_KINDS describes every credential kind the server can materialize: the shape its secret takes
// and what a run does with it. A typed kind's secret is KEY=VALUE lines, so the placeholder shows the
// exact field names its injector reads and the hint names which of them are required. Kinds marked
// ansibleOnly are delivered through Ansible flags or extra vars and are rejected on any other tool,
// which is worth saying before the secret is pasted rather than at submit.
const CRED_KINDS = {
	ssh_key: {
		placeholder: "-----BEGIN OPENSSH PRIVATE KEY-----",
		hint: "The private key itself, passed to the run as --private-key.", ansibleOnly: true,
	},
	ssh_password: {
		placeholder: "user=deploy\npassword=the password",
		hint: "Fields: user and password, both required. Becomes ansible_user and ansible_password.",
		ansibleOnly: true,
	},
	network: {
		placeholder: "user=admin\npassword=the password\nnetwork_os=ios\nconnection=network_cli",
		hint: "Fields: user and password required, network_os and connection optional. Connection " +
			"defaults to network_cli.",
		ansibleOnly: true,
	},
	vault_password: {
		placeholder: "the vault password",
		hint: "Passed to the run as --vault-password-file.", ansibleOnly: true,
	},
	become_password: {
		placeholder: "the privilege escalation password",
		hint: "Becomes ansible_become_password, delivered through a file so it stays off the command " +
			"line.",
		ansibleOnly: true,
	},
	become: {
		placeholder: "password=the password\nmethod=sudo\nuser=root",
		hint: "Fields: password required, method and user optional.", ansibleOnly: true,
	},
	env: {
		placeholder: "AWS_PROFILE=prod\nTF_VAR_region=us-east-1",
		hint: "One KEY=VALUE per line, injected into the run's environment. Blank lines and # comments " +
			"are ignored.",
	},
	token: {
		placeholder: "the token or JWT",
		hint: "Exposed to the run as the SWITCHTENDER_TOKEN environment variable.",
	},
	registry: {
		placeholder: "username\nthe password or access token",
		hint: "Username on the first line, password on every line after it. Used to pull execution " +
			"images.",
	},
	aws: {
		placeholder: "access_key=AKIAEXAMPLE\nsecret_key=the secret\nregion=us-east-1",
		hint: "Fields: access_key and secret_key required, session_token and region optional. Injects " +
			"the standard AWS_ variables.",
	},
	azure: {
		placeholder: "client_id=the id\nsecret=the secret\nsubscription_id=the id\ntenant_id=the id",
		hint: "All four fields are required. Injects both the ARM_ variables Terraform reads and the " +
			"AZURE_ variables Ansible reads.",
	},
	gcp: {
		placeholder: '{"type": "service_account", "project_id": "…", "private_key": "…"}',
		hint: "The service account JSON, written to a private file and bound to " +
			"GOOGLE_APPLICATION_CREDENTIALS.",
	},
	openstack: {
		placeholder: "auth_url=https://keystone.example:5000/v3\nusername=deploy\npassword=the password\nproject_name=prod",
		hint: "Fields: auth_url, username, password, and project_name required; user_domain_name, " +
			"project_domain_name (both default to Default), and region_name optional. Injects the " +
			"OS_ variables.",
	},
	vmware: {
		placeholder: "host=vcenter.example.com\nuser=administrator@vsphere.local\npassword=the password",
		hint: "Fields: host, user, and password required, validate_certs optional. Injects the VMWARE_ " +
			"variables.",
	},
};

// CRED_SOURCES describes where a secret comes from. A source other than local means the stored value
// is a lookup rather than the secret, so its placeholder shows the lookup's shape and the kind's own
// placeholder no longer applies.
const CRED_SOURCES = {
	local: { hint: "The value below is the secret, sealed and stored here." },
	command: {
		placeholder: "vault kv get -field=password secret/prod-fleet",
		hint: "The command runs on the executor at launch and its standard output is the secret.",
	},
	vault: {
		placeholder: '{"addr":"https://vault:8200","path":"secret/data/ci","field":"token"}',
		hint: "Read from HashiCorp Vault over HTTP at launch.",
	},
	vault_dynamic: {
		placeholder: '{"addr":"https://vault:8200","path":"database/creds/app","field":"password"}',
		hint: "Vault mints a short-lived credential for each run and it is revoked when the run ends.",
	},
	gsm: {
		placeholder: '{"project":"my-project","secret":"ci-token","version":"latest"}',
		hint: "Read from Google Secret Manager at launch.",
	},
	aws: {
		placeholder: '{"secret_id":"prod/db-password","region":"us-east-1"}',
		hint: "Read from AWS Secrets Manager at launch with a signed request.",
	},
	aws_sts: {
		placeholder: '{"role_arn":"arn:aws:iam::123456789012:role/deploy","region":"us-east-1"}',
		hint: "AWS STS mints short-lived role credentials for each run.",
	},
	azure: {
		placeholder: '{"vault":"prod-kv","secret":"db-password"}',
		hint: "Read from Azure Key Vault at launch.",
	},
	conjur: {
		placeholder: '{"url":"https://conjur.example.com","account":"prod","login":"host/app",' +
			'"api_key":"…","variable":"db/password"}',
		hint: "Read from CyberArk Conjur at launch.",
	},
	ccp: {
		placeholder: '{"url":"https://ccp.example.com","app_id":"switchtender","safe":"Prod",' +
			'"object":"db-password"}',
		hint: "Read from the CyberArk Central Credential Provider at launch.",
	},
	onepassword: {
		placeholder: '{"url":"https://connect.example.com","token":"…","vault":"Prod",' +
			'"item":"db","field":"password"}',
		hint: "Read from 1Password Connect at launch, with no op CLI on the runner.",
	},
};

// syncCredFields matches the secret field to the kind and source chosen, so the box always shows the
// shape of the thing being pasted into it and says what the run will do with it.
function syncCredFields() {
	const kind = document.getElementById("cred-kind").value;
	const source = document.getElementById("cred-source").value || "local";
	const kindSpec = CRED_KINDS[kind] || {};
	const sourceSpec = CRED_SOURCES[source] || {};
	const secret = document.getElementById("cred-secret");
	// On edit the placeholder explains that a blank keeps what is stored, which outranks either shape.
	if (!secret.required) {
		secret.placeholder = "Leave blank to keep the current secret";
	} else {
		secret.placeholder = source === "local"
			? (kindSpec.placeholder || "")
			: (sourceSpec.placeholder || "");
	}
	const hint = document.getElementById("cred-kind-hint");
	if (hint) {
		hint.textContent = (kindSpec.hint || "") +
			(kindSpec.ansibleOnly ? " Takes effect under Ansible only." : "");
	}
	const sourceHint = document.getElementById("cred-source-hint");
	if (sourceHint) sourceHint.textContent = sourceSpec.hint || "";
	toggleCredPassphrase();
}

// openCredentialEdit fills the credential dialog with an existing record and switches it to edit
// mode. The secret field becomes optional, so a blank keeps the stored secret; the list never
// returns secret material, so the field always starts empty.
function openCredentialEdit(c) {
	const form = document.getElementById("cred-form");
	form.dataset.editId = c.id;
	document.getElementById("cred-name").value = c.name;
	document.getElementById("cred-kind").value = c.kind;
	document.getElementById("cred-source").value = c.source || "local";
	const sec = document.getElementById("cred-secret");
	sec.value = "";
	sec.required = false;
	document.getElementById("cred-passphrase").value = "";
	const vaultID = document.getElementById("cred-vault-id");
	if (vaultID) vaultID.value = c.vault_id || "";
	const settings = document.getElementById("cred-settings");
	if (settings) settings.value = credSettingsText(c.settings);
	syncCredFields();
	document.getElementById("cred-status").textContent = "";
	setModalTitle("cred", "Edit credential");
	document.getElementById("cred-modal").hidden = false;
}

// credSettingsText renders a settings map as one key=value per line for the dialog textarea, keys
// sorted so the same credential always renders the same way.
function credSettingsText(settings) {
	return Object.entries(settings || {}).sort((a, b) => a[0].localeCompare(b[0]))
		.map(([k, v]) => k + "=" + v).join("\n");
}

// parseCredSettings reads the dialog textarea into a settings map, skipping blank lines. A line
// with no = is kept as a key with an empty value so the server's validation names the mistake
// instead of the dialog dropping the line silently.
function parseCredSettings(text) {
	const out = {};
	for (const line of (text || "").split("\n")) {
		const trimmed = line.trim();
		if (!trimmed) continue;
		const i = trimmed.indexOf("=");
		if (i === -1) {
			out[trimmed] = "";
			continue;
		}
		out[trimmed.slice(0, i).trim()] = trimmed.slice(i + 1).trim();
	}
	return out;
}

// toggleCredPassphrase shows the passphrase field only for a locally stored SSH key, the one case
// where a passphrase unlocks the key at run time, and clears it when hidden so it is never sent.
function toggleCredPassphrase() {
	const kind = document.getElementById("cred-kind").value;
	const source = document.getElementById("cred-source").value;
	const field = document.getElementById("cred-passphrase-field");
	const show = kind === "ssh_key" && source === "local";
	field.hidden = !show;
	if (!show) document.getElementById("cred-passphrase").value = "";
	toggleCredVaultID();
}

// toggleCredVaultID shows the vault label field only for a vault password, the one kind the label
// means anything to.
function toggleCredVaultID() {
	const kind = document.getElementById("cred-kind").value;
	const field = document.getElementById("cred-vault-id-field");
	if (!field) return;
	const show = kind === "vault_password";
	field.hidden = !show;
	if (!show) document.getElementById("cred-vault-id").value = "";
}

// wireCredentialForm hooks the credential dialog up to POST /credentials for a new record and PUT
// /credentials/{id} when editing. The New button resets the dialog to add mode, where a secret is
// required; on edit the secret is only sent when changed.
function wireCredentialForm() {
	const form = document.getElementById("cred-form");
	document.getElementById("cred-source").addEventListener("change", syncCredFields);
	document.getElementById("cred-kind").addEventListener("change", syncCredFields);
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("cred-name").value = "";
		document.getElementById("cred-source").value = "local";
		const sec = document.getElementById("cred-secret");
		sec.value = "";
		sec.required = true;
		document.getElementById("cred-passphrase").value = "";
		const settings = document.getElementById("cred-settings");
		if (settings) settings.value = "";
		syncCredFields();
		document.getElementById("cred-status").textContent = "";
		setModalTitle("cred", "Add a credential");
	};
	syncCredFields();
	const openBtn = document.getElementById("cred-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still posting, so a fast double click on Save
	// stores the credential once rather than twice. Unlike the launch form, a modal save stays on the
	// page, so the button is re-enabled once the request settles either way, leaving the dialog usable
	// for the next credential.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("cred-status");
		const editId = form.dataset.editId;
		const payload = {
			name: document.getElementById("cred-name").value.trim(),
			kind: document.getElementById("cred-kind").value,
			source: document.getElementById("cred-source").value,
		};
		const secret = document.getElementById("cred-secret").value;
		if (secret) payload.secret = secret;
		if (payload.kind === "vault_password") {
			payload.vault_id = document.getElementById("cred-vault-id").value.trim();
		}
		const passphrase = document.getElementById("cred-passphrase").value;
		if (passphrase && payload.kind === "ssh_key" && payload.source === "local") {
			payload.passphrase = passphrase;
		}
		// On edit the form state is the whole truth: sending the parsed map replaces the stored
		// settings, and an emptied textarea sends {} which clears them. On create an empty map is
		// simply omitted.
		const settingsField = document.getElementById("cred-settings");
		if (settingsField) {
			const settings = parseCredSettings(settingsField.value);
			if (editId || Object.keys(settings).length) payload.settings = settings;
		}
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/credentials/" + editId, payload, "PUT");
			} else {
				await postAction("/credentials", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("cred");
			document.getElementById("credentials").innerHTML = "";
			loadCredentials();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// loadCredentials populates the credential table with delete actions.
// credentialUsers maps a credential id to the names of the templates that reference it, so the
// list can answer what breaks if one is deleted.
async function credentialUsers() {
	const map = new Map();
	try {
		const data = await getJSON("/templates");
		for (const t of data.templates || []) {
			const ids = [].concat(t.credential_ids || [], t.selectable_credential_ids || [],
				t.pull_credential_id ? [t.pull_credential_id] : []);
			for (const id of ids) {
				if (!map.has(id)) map.set(id, []);
				if (!map.get(id).includes(t.name)) map.get(id).push(t.name);
			}
		}
	} catch { /* the column falls back to a dash */ }
	return map;
}

async function loadCredentials() {
	const templateUsers = await credentialUsers();
	try {
		const data = await getJSON("/credentials");
		const creds = data.credentials || [];
		if (creds.length === 0) {
			showEmpty("No credentials yet.");
			return;
		}
		renderNeedsSecret(creds);
		const tbody = document.getElementById("credentials");
		for (const c of creds) {
			const tr = document.createElement("tr");
			const name = td(c.name);
			// Non-secret settings surface on hover rather than as a column, so a machine credential's
			// connection user is one hover away without widening the table.
			const settingsEntries = Object.entries(c.settings || {}).sort((a, b) =>
				a[0].localeCompare(b[0]));
			if (settingsEntries.length) {
				name.dataset.tip = "Settings: " +
					settingsEntries.map(([k, v]) => k + "=" + v).join(", ");
			}
			tr.appendChild(name);
			// The row opens as a drawer too, so the settings a tooltip carries are reachable on
			// touch and readable whole rather than clipped into one hover line.
			inspectable(tr, c.name, [
				{ label: "Kind", value: c.kind },
				{ label: "Source", value: c.source || "local" },
				{ label: "Settings", value: settingsEntries.map(([k, v]) => k + "=" + v).join("\n"), block: true },
			]);
			// Kind and source are chips rather than bare text, so the column reads at a glance and the
			// facet menus can offer them as values to tick.
			const kind = td("");
			const kindChip = document.createElement("span");
			kindChip.className = "cred-kind";
			kindChip.textContent = c.kind;
			const kindSpec = CRED_KINDS[c.kind];
			if (kindSpec) {
				kindChip.dataset.tip = kindSpec.hint +
					(kindSpec.ansibleOnly ? " Takes effect under Ansible only." : "");
			}
			kind.appendChild(kindChip);
			tr.appendChild(kind);
			// Where the secret comes from is set on the form but was never shown here, so a credential
			// that resolves out of Vault looked identical to one stored locally.
			const source = td("");
			const sourceName = c.source || "local";
			const sourceChip = document.createElement("span");
			sourceChip.className = "cred-kind cred-source" + (sourceName === "local" ? "" : " external");
			sourceChip.textContent = sourceName;
			const sourceSpec = CRED_SOURCES[sourceName];
			if (sourceSpec) sourceChip.dataset.tip = sourceSpec.hint;
			source.appendChild(sourceChip);
			tr.appendChild(source);
			const secret = td("");
			const secretChip = document.createElement("span");
			secretChip.className = c.needs_secret ? "chip flaky" : "chip ok";
			secretChip.textContent = c.needs_secret ? "needs a secret" : "set";
			secretChip.dataset.tip = c.needs_secret
				? "No secret stored yet, so any run using this credential fails"
				: "A secret is stored, encrypted at rest and never shown again";
			secret.appendChild(secretChip);
			tr.appendChild(secret);
			const usedBy = td("");
			const users = templateUsers.get(c.id) || [];
			if (users.length) {
				const link = document.createElement("a");
				link.href = "/ui/templates";
				link.textContent = users.length === 1 ? users[0] : users.length + " templates";
				link.dataset.tip = "Used by: " + users.join(", ") + ". Click to open templates";
				usedBy.appendChild(link);
			} else {
				usedBy.textContent = "\u2014";
				usedBy.dataset.tip = "No template references this credential";
			}
			tr.appendChild(usedBy);
			tr.appendChild(tdTime(c.created_at));
			const actions = document.createElement("td");
			const del = document.createElement("button");
			del.className = "button danger";
	del.dataset.mutates = "true";
	del.dataset.tip = "Click to delete this permanently";
			del.textContent = "Delete";
			del.addEventListener("click", async (e) => {
				e.preventDefault();
				if (!window.confirm("Delete credential " + c.name + "?")) return;
				try {
					await authedDelete("/credentials/" + c.id);
					removeRow(tr, "No credentials yet.");
				} catch (err) {
					setStatus("Delete failed: " + err.message);
				}
			});
			actions.appendChild(editButton(() => openCredentialEdit(c), "Click to replace this credential's secret"));
			actions.appendChild(document.createTextNode(" "));
			actions.appendChild(del);
			tr.appendChild(actions);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load credentials: " + e.message);
	}
}

// renderNeedsSecret fills the panel listing credentials that still have no secret and lets an admin
// set each one in place, so a freshly imported set is finished on a single screen instead of opening
// each credential. In the read-only demo the inputs are disabled.
function renderNeedsSecret(creds) {
	const panel = document.getElementById("cred-needs");
	if (!panel) return;
	panel.innerHTML = "";
	const pending = creds.filter((c) => c.needs_secret);
	if (pending.length === 0) {
		panel.hidden = true;
		return;
	}
	const readOnly = isReadOnly();

	const head = document.createElement("div");
	head.className = "cred-needs-head";
	const title = document.createElement("strong");
	const setTitle = (n) => {
		title.textContent = n + (n === 1 ? " credential needs a secret" : " credentials need a secret");
	};
	setTitle(pending.length);
	const sub = document.createElement("span");
	sub.className = "cred-needs-sub";
	sub.textContent = "Set a secret on each to make it usable. Imported credentials arrive this way. ";
	const guide = document.createElement("a");
	guide.href = "/ui/docs/tutorial-set-a-secret";
	guide.textContent = "How secrets work";
	guide.dataset.tip = "Open the Set a secret guide";
	sub.appendChild(guide);
	head.appendChild(title);
	head.appendChild(sub);
	panel.appendChild(head);

	const list = document.createElement("div");
	list.className = "cred-needs-list";
	let remaining = pending.length;
	for (const c of pending) {
		const row = document.createElement("div");
		row.className = "cred-needs-row";
		const meta = document.createElement("div");
		meta.className = "cred-needs-meta";
		const name = document.createElement("span");
		name.className = "cred-needs-name";
		name.textContent = c.name;
		const kind = document.createElement("span");
		kind.className = "cred-needs-kind";
		kind.textContent = c.kind;
		meta.appendChild(name);
		meta.appendChild(kind);

		const input = document.createElement("textarea");
		input.className = "input mono cred-needs-input";
		input.rows = 2;
		input.placeholder = "Paste the secret";
		input.disabled = readOnly;
		const save = document.createElement("button");
		save.className = "button primary";
		save.dataset.mutates = "true";
		save.dataset.tip = "Click to store this secret, encrypted at rest";
		save.textContent = "Save";
		save.disabled = readOnly;
		const status = document.createElement("span");
		status.className = "cred-needs-status muted";
		if (readOnly) status.textContent = "Disabled in the demo";

		save.addEventListener("click", async () => {
			const secret = input.value;
			if (!secret.trim()) {
				status.textContent = "Enter a secret first.";
				return;
			}
			save.disabled = true;
			status.textContent = "Saving…";
			try {
				await postAction("/credentials/" + c.id, { name: c.name, secret }, "PUT");
				row.remove();
				remaining -= 1;
				if (remaining === 0) {
					panel.hidden = true;
				} else {
					setTitle(remaining);
				}
			} catch (err) {
				save.disabled = false;
				status.textContent = "Save failed: " + err.message;
			}
		});

		row.appendChild(meta);
		row.appendChild(input);
		row.appendChild(save);
		row.appendChild(status);
		list.appendChild(row);
	}
	panel.appendChild(list);
	panel.hidden = false;
}

// fillSelect loads options into a select from a list endpoint.
async function fillSelect(el, url, listKey, labelFor) {
	try {
		const data = await getJSON(url);
		for (const item of data[listKey] || []) {
			const opt = document.createElement("option");
			opt.value = item.id;
			opt.textContent = labelFor(item);
			el.appendChild(opt);
		}
	} catch (_) { /* feature disabled or unauthorized; the select keeps its defaults */ }
}

// openProjectEdit fills the project dialog with an existing record and switches it to edit mode so
// the next save issues a PUT rather than a create.
function openProjectEdit(p) {
	const form = document.getElementById("project-form");
	form.dataset.editId = p.id;
	document.getElementById("project-name").value = p.name;
	document.getElementById("project-repo").value = p.repo_url;
	document.getElementById("project-branch").value = p.branch || "";
	document.getElementById("project-credential").value = p.credential_id || "";
	document.getElementById("project-deps").checked = p.install_deps !== false;
	document.getElementById("project-image").value = p.image || "";
	document.getElementById("project-pull-credential").value = p.pull_credential_id || "";
	document.getElementById("project-status").textContent = "";
	setModalTitle("project", "Edit project");
	document.getElementById("project-modal").hidden = false;
}

