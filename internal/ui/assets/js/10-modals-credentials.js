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
	// A read-only install fills the form and refuses the submit, so a visitor sees what launching a
	// run asks for and gets a reason instead of a server refusal. The server refuses this too; the
	// button says so before the request rather than after it.
	if (isReadOnly()) {
		const go = form.querySelector('button[type="submit"]');
		if (go) {
			go.disabled = true;
			go.title = isDemo() ? "Disabled in this read-only demo. Self-host to launch runs." :
				readOnlyReason() + ".";
		}
		if (status) {
			status.textContent = isDemo()
				? "This demo is read-only. The form is here to show what a launch asks for. Self-host " +
					"to run one."
				: "This server is read-only, so it launches nothing.";
		}
		form.addEventListener("submit", (e) => e.preventDefault());
		return;
	}
	// A failed launch says why in the status line, and that sentence belongs to the attempt that
	// failed. Opening the dialog again for a fresh launch showed it beside a form not yet submitted.
	const openBtn = document.getElementById("launch-open");
	if (openBtn) openBtn.addEventListener("click", () => { status.textContent = ""; });
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

// renderSealingNotice puts one line above the credentials table saying this server cannot store a
// new secret, without hiding the ones it already holds.
//
// The wording differs by install because the remedy does. On a read-only demo the reader does not
// run the server, so instructions to set environment variables on it are noise aimed at the wrong
// person.
function renderSealingNotice() {
	const table = document.querySelector("main.content table");
	if (!table) return;
	const anchor = table.closest(".list-scroll") || table;
	if (!anchor.parentNode || anchor.parentNode.querySelector(".seal-notice")) return;
	const note = document.createElement("div");
	note.className = "ro-banner seal-notice";
	note.textContent = isDemo()
		? "This demo holds no encryption key, so no new credential can be stored here. The ones below are seeded, and templates reference them the way a real install would."
		: "This server has no encryption key, so a new credential cannot be sealed and saved. Set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT and restart, keeping the salt stable, since it is what every stored secret is sealed against.";
	anchor.parentNode.insertBefore(note, anchor);
}

// fillCredentialPicker loads stored credentials into the launch multiselect.
async function fillCredentialPicker() {
	const picker = document.getElementById("launch-credentials");
	if (!picker) return;
	try {
		const data = await getJSON("/credentials");
		const creds = data.credentials || [];
		for (const c of creds) {
			const opt = document.createElement("option");
			opt.value = c.id;
			opt.textContent = c.name + " (" + credKindLabel(c) + ")";
			picker.appendChild(opt);
		}
		// A labeled box with nothing in it reads as broken. The Project and Stored inventory selects
		// beside it both carry a placeholder saying what empty means, and this one did not: the first
		// form a stranger opens showed CREDENTIALS above a blank well.
		if (creds.length === 0) placeholderOption(picker, data.sealing === false
			? "Credentials are switched off on this server"
			: "None stored yet");
	} catch (_) {
		placeholderOption(picker, "Credentials are unavailable on this server");
	}
}

// placeholderOption puts one unselectable line in an empty picker, saying what empty means.
function placeholderOption(picker, text) {
	if (picker.options.length) return;
	const opt = document.createElement("option");
	opt.textContent = text;
	opt.disabled = true;
	picker.appendChild(opt);
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
	kubeconfig: {
		placeholder: "apiVersion: v1\nkind: Config\nclusters:\n- name: prod\n  cluster:\n    server: " +
			"https://k8s.example.com:6443",
		hint: "The whole kubeconfig YAML, written to a private file and bound to KUBECONFIG, " +
			"K8S_AUTH_KUBECONFIG, and KUBE_CONFIG_PATH. Only its tokens, keys, and passwords are masked " +
			"in run output.",
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
	// The federated kinds store no secret. Each run is handed a short-lived identity token this server
	// signs, so the dialog hides the secret and source and asks for settings instead.
	aws_oidc: {
		federated: true,
		settings: "role_arn=arn:aws:iam::123456789012:role/deploy\nregion=us-east-1\nenvironment=prod",
		hint: "Nothing is stored. Each run assumes the role with a short-lived token through " +
			"AssumeRoleWithWebIdentity. Settings: role_arn required; region, environment, delivery, " +
			"audience, session_name, session_duration, sts_endpoint, and token_ttl optional.",
	},
	gcp_oidc: {
		federated: true,
		settings: "provider=projects/123456789/locations/global/workloadIdentityPools/ci/providers/st\n" +
			"service_account=deployer@my-project.iam.gserviceaccount.com",
		hint: "Nothing is stored. Each run gets an external_account credentials file over a short-lived " +
			"token. Settings: provider required; service_account, environment, delivery, audience, " +
			"sts_endpoint, iam_endpoint, and token_ttl optional.",
	},
	azure_oidc: {
		federated: true,
		settings: "client_id=the application id\ntenant_id=the tenant id\nsubscription_id=the id",
		hint: "Nothing is stored. Each run gets a short-lived token in AZURE_FEDERATED_TOKEN_FILE. " +
			"Settings: client_id and tenant_id required; subscription_id, environment, audience, " +
			"authority_host, and token_ttl optional.",
	},
	oidc_token: {
		federated: true,
		settings: "audience=https://vault.example.com\nenvironment=prod",
		hint: "Nothing is stored. Each run gets a short-lived identity token in the file " +
			"SWITCHTENDER_OIDC_TOKEN_FILE names. Settings: audience required; environment and " +
			"token_ttl optional.",
	},
};

// SETTINGS_PLACEHOLDER is the settings box's example for a kind that stores a secret, where settings
// are optional metadata.
const SETTINGS_PLACEHOLDER = "user=deploy\nbecome_method=sudo";

// CRED_SOURCES describes where a secret comes from. A source other than local means the stored value
// is a lookup rather than the secret, so its placeholder shows the lookup's shape and the kind's own
// placeholder no longer applies.
const CRED_SOURCES = {
	local: { hint: "The value below is the secret, sealed and stored here." },
	command: {
		placeholder: "vault kv get -field=password secret/prod-fleet",
		hint: "The command runs on the executor at launch and its standard output is the secret. " +
			"For an env credential it prints KEY=VALUE lines; a single value belongs on a token.",
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

// CRED_TYPE_PREFIX marks a kind select value naming a custom credential type rather than a built-in
// kind, as in "type:ctype_1".
const CRED_TYPE_PREFIX = "type:";

// CRED_TYPE_EXAMPLE prefills the type editor for a new type: a kubeconfig written to a private file
// whose path reaches the run through KUBECONFIG.
const CRED_TYPE_EXAMPLE = [
	"{",
	'  "name": "Kubeconfig",',
	'  "fields": [{"name": "kubeconfig", "label": "Kubeconfig", "secret": true, "multiline": true}],',
	'  "file": {"template": "{{ kubeconfig }}"},',
	'  "env": {"KUBECONFIG": "{{ tower.filename }}"}',
	"}",
].join("\n");

// credTypes maps each custom credential type's id to its definition. loadCredentialTypes fills it,
// and it stays empty where types are switched off or this session cannot read them.
let credTypes = new Map();

// selectedCredTypeID returns the id of the custom type the credential dialog's kind select names,
// or an empty string when a built-in kind is chosen.
function selectedCredTypeID() {
	const v = document.getElementById("cred-kind").value || "";
	return v.startsWith(CRED_TYPE_PREFIX) ? v.slice(CRED_TYPE_PREFIX.length) : "";
}

// credKindLabel names what a credential holds: its built-in kind, or the custom type it was made
// from. A typed credential has no kind of its own, so before the types load it reads as a custom
// type rather than as a blank.
function credKindLabel(c) {
	if (!c.type_id) return c.kind;
	const t = credTypes.get(c.type_id);
	return t ? "custom: " + t.name : "custom type";
}

// credTypeFieldsSummary lists a type's field names, marking the secret and multiline ones.
function credTypeFieldsSummary(t) {
	return (t.fields || []).map((f) => {
		const marks = [];
		if (f.secret) marks.push("secret");
		if (f.multiline) marks.push("multiline");
		return f.name + (marks.length ? " (" + marks.join(", ") + ")" : "");
	}).join(", ");
}

// credTypeInjects summarizes what a type hands a run: environment variable names, extra var names,
// and the files it writes. A file is named by its template key, the bare key reading as "template".
function credTypeInjects(t) {
	const parts = [];
	const env = Object.keys(t.env || {}).sort();
	if (env.length) parts.push("env: " + env.join(", "));
	const vars = Object.keys(t.extra_vars || {}).sort();
	if (vars.length) parts.push("extra vars: " + vars.join(", "));
	const files = Object.keys(t.file || {}).sort()
		.map((k) => (k === "template" ? k : k.replace(/^template\./, "")));
	if (files.length) parts.push((files.length === 1 ? "file: " : "files: ") + files.join(", "));
	return parts.length ? parts.join(" \u00b7 ") : "none";
}

// fillCredTypeOptions rebuilds the Custom types group in the credential dialog's kind select, one
// option per loaded type. With no types the group is left out, so the select offers only kinds this
// server can store.
function fillCredTypeOptions() {
	const select = document.getElementById("cred-kind");
	if (!select) return;
	const current = select.value;
	const old = document.getElementById("cred-kind-custom");
	if (old) old.remove();
	if (credTypes.size === 0) return;
	const group = credTypeGroup(select);
	for (const t of credTypes.values()) {
		const opt = document.createElement("option");
		opt.value = CRED_TYPE_PREFIX + t.id;
		opt.textContent = t.name;
		group.appendChild(opt);
	}
	select.value = current;
}

// credTypeGroup returns the Custom types group in the kind select, adding it when missing.
function credTypeGroup(select) {
	let group = document.getElementById("cred-kind-custom");
	if (group) return group;
	group = document.createElement("optgroup");
	group.id = "cred-kind-custom";
	group.setAttribute("label", "Custom types");
	select.appendChild(group);
	return group;
}

// ensureCredTypeOption makes sure the kind select can name a type, adding a stand-in option for one
// whose definition did not load, so editing a typed credential never falls back to a built-in kind.
function ensureCredTypeOption(typeID) {
	const select = document.getElementById("cred-kind");
	const value = CRED_TYPE_PREFIX + typeID;
	if (Array.from(select.options).some((o) => o.value === value)) return;
	const opt = document.createElement("option");
	opt.value = value;
	opt.textContent = "custom type";
	credTypeGroup(select).appendChild(opt);
}

// clearCredTypeFields empties the typed field controls, so the next sync draws them fresh.
function clearCredTypeFields() {
	const box = document.getElementById("cred-type-fields");
	if (!box) return;
	box.textContent = "";
	delete box.dataset.typeId;
}

// renderCredTypeFields draws one control per field of the chosen custom type: a textarea for a
// multiline field, a password input for a secret one, and a text input otherwise. Values already
// typed survive a sync that leaves the type unchanged. With no type chosen the container hides.
function renderCredTypeFields(typeID) {
	const box = document.getElementById("cred-type-fields");
	if (!box) return;
	if (!typeID) {
		box.hidden = true;
		clearCredTypeFields();
		return;
	}
	box.hidden = false;
	if (box.dataset.typeId === typeID) return;
	box.textContent = "";
	box.dataset.typeId = typeID;
	const t = credTypes.get(typeID);
	const editing = Boolean(document.getElementById("cred-form").dataset.editId);
	if (!t) {
		const note = document.createElement("p");
		note.className = "field-hint";
		note.textContent = "This credential's type could not be read, so only its name can change here.";
		box.appendChild(note);
		return;
	}
	if (editing) {
		const note = document.createElement("p");
		note.className = "field-hint";
		note.textContent = "Leave every field blank to keep the stored values. Entering any " +
			"replaces them all.";
		box.appendChild(note);
	}
	for (const f of t.fields || []) {
		const label = document.createElement("label");
		label.className = "field-label";
		label.appendChild(document.createTextNode(f.label || f.name));
		const control = document.createElement(f.multiline ? "textarea" : "input");
		if (f.multiline) {
			control.className = "input mono";
			control.rows = 6;
		} else {
			control.className = "input";
			control.setAttribute("type", f.secret ? "password" : "text");
		}
		control.setAttribute("autocomplete", "off");
		control.id = "cred-type-field-" + f.name;
		control.dataset.field = f.name;
		label.appendChild(control);
		box.appendChild(label);
	}
}

// credTypeFieldValues reads every typed field control into a map by field name, exactly as entered.
// A multiline value keeps its inner newlines and surrounding whitespace.
function credTypeFieldValues() {
	const out = {};
	const box = document.getElementById("cred-type-fields");
	if (!box) return out;
	for (const control of box.querySelectorAll("input, textarea")) {
		if (control.dataset.field) out[control.dataset.field] = control.value;
	}
	return out;
}

// syncCredFields matches the secret field to the kind and source chosen, so the box always shows the
// shape of the thing being pasted into it and says what the run will do with it. A custom type
// replaces the source, secret, and settings fields with its own declared fields.
function syncCredFields() {
	const kind = document.getElementById("cred-kind").value;
	const source = document.getElementById("cred-source").value || "local";
	const kindSpec = CRED_KINDS[kind] || {};
	const sourceSpec = CRED_SOURCES[source] || {};
	const secret = document.getElementById("cred-secret");
	const typeID = selectedCredTypeID();
	const form = document.getElementById("cred-form");
	// A hidden required field blocks the submit with nowhere to show why, so a typed credential
	// drops the requirement, and a built-in kind takes it back when creating, or when an edit moves a
	// typed credential onto it, since that replaces the type's field values whole.
	const leavingType = Boolean(form && form.dataset.editId && form.dataset.editTypeId);
	secret.required = !typeID && (!(form && form.dataset.editId) || leavingType);
	for (const id of ["cred-source-field", "cred-secret-field", "cred-settings-field"]) {
		const el = document.getElementById(id);
		if (el) el.hidden = Boolean(typeID);
	}
	renderCredTypeFields(typeID);
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
	if (hint && typeID) {
		const t = credTypes.get(typeID);
		hint.textContent = t
			? "A custom type. Injects " + credTypeInjects(t) + "."
			: "A custom type this session cannot read.";
	}
	const sourceHint = document.getElementById("cred-source-hint");
	if (sourceHint) sourceHint.textContent = sourceSpec.hint || "";
	// A federated kind has no secret and no source, so both are hidden and the secret box is taken
	// out of the form's validation, and the settings box shows what that kind needs instead. A custom
	// type hides them too, since its own fields replace them.
	const federated = !!kindSpec.federated;
	const secretField = document.getElementById("cred-secret-field");
	if (secretField) secretField.hidden = federated || Boolean(typeID);
	const sourceField = document.getElementById("cred-source-field");
	if (sourceField) sourceField.hidden = federated || Boolean(typeID);
	secret.disabled = federated;
	const settings = document.getElementById("cred-settings");
	if (settings) settings.placeholder = federated ? kindSpec.settings : SETTINGS_PLACEHOLDER;
	toggleCredPassphrase();
}

// openCredentialEdit fills the credential dialog with an existing record and switches it to edit
// mode. The secret field becomes optional, so a blank keeps the stored secret; the list never
// returns secret material, so the field always starts empty.
function openCredentialEdit(c) {
	const form = document.getElementById("cred-form");
	form.dataset.editId = c.id;
	// What the stored record is, so a submit can tell a real change from leaving the fields alone.
	form.dataset.editKind = c.kind;
	form.dataset.editSource = c.source || "local";
	form.dataset.editTypeId = c.type_id || "";
	document.getElementById("cred-name").value = c.name;
	if (c.type_id) {
		ensureCredTypeOption(c.type_id);
		document.getElementById("cred-kind").value = CRED_TYPE_PREFIX + c.type_id;
	} else {
		document.getElementById("cred-kind").value = c.kind;
	}
	clearCredTypeFields();
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
		delete form.dataset.editKind;
		delete form.dataset.editSource;
		delete form.dataset.editTypeId;
		// A stand-in option for a type that never loaded cannot create anything, so a new credential
		// starts from a built-in kind instead.
		const typeID = selectedCredTypeID();
		if (typeID && !credTypes.has(typeID)) document.getElementById("cred-kind").value = "ssh_key";
		clearCredTypeFields();
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
		// A typed credential edited onto a built-in kind is saved the built-in way, kind and secret
		// together, which is how the server moves it off its type in one step.
		const payload = selectedCredTypeID()
			? typedCredPayload(form, status)
			: builtinCredPayload(form, status);
		if (!payload) return;
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
			loadCredentials();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// builtinCredPayload reads the credential dialog into the body a built-in kind is saved with, or
// returns null after saying in status why the save cannot go ahead.
function builtinCredPayload(form, status) {
	const editId = form.dataset.editId;
	const payload = {
		name: document.getElementById("cred-name").value.trim(),
		kind: document.getElementById("cred-kind").value,
		source: document.getElementById("cred-source").value,
	};
	const federated = !!(CRED_KINDS[payload.kind] || {}).federated;
	// A federated credential stores nothing, so whatever was left in the hidden boxes stays here.
	const secret = federated ? "" : document.getElementById("cred-secret").value;
	if (secret) payload.secret = secret;
	if (federated) delete payload.source;
	if (federated && editId && payload.kind !== form.dataset.editKind) {
		status.textContent = "A federated credential keeps its kind. Create a new credential for " +
			"another kind.";
		return null;
	}
	if (payload.kind === "vault_password") {
		payload.vault_id = document.getElementById("cred-vault-id").value.trim();
	}
	const passphrase = document.getElementById("cred-passphrase").value;
	if (passphrase && payload.kind === "ssh_key" && payload.source === "local") {
		payload.passphrase = passphrase;
	}
	// A kind or source change re-seals the secret in a different form, so the server only applies
	// one when a new secret comes with it. The dialog sent the change anyway and reported "Saved",
	// which is the one answer that is not true: an admin moving a credential from local storage to
	// a secrets manager, with the secret box left blank as its placeholder invites, was told it had
	// moved while the plaintext key stayed sealed locally and kept being injected. Say what is
	// actually required instead of reporting a change that did not happen.
	if (editId && !secret && !federated && form.dataset.editTypeId) {
		status.textContent = "Moving a credential off its custom type replaces its field values, " +
			"so enter the secret for the new kind.";
		return null;
	}
	if (editId && !secret && !federated &&
		(payload.kind !== form.dataset.editKind || payload.source !== form.dataset.editSource)) {
		status.textContent = "Changing the kind or source re-seals the secret, so enter the " +
			"secret again to make that change.";
		return null;
	}
	// On edit the form state is the whole truth: sending the parsed map replaces the stored
	// settings, and an emptied textarea sends {} which clears them. On create an empty map is
	// simply omitted.
	const settingsField = document.getElementById("cred-settings");
	if (settingsField) {
		const settings = parseCredSettings(settingsField.value);
		if (editId || Object.keys(settings).length) payload.settings = settings;
	}
	return payload;
}

// typedCredPayload reads the credential dialog into the body a custom type's credential is saved
// with, or returns null after saying in status why the save cannot go ahead.
//
// A create sends every field. An edit stores its values sealed together, so the server replaces all
// of them or none: every field left blank sends only the name and keeps the stored values, and any
// field entered sends them all, a blank one as an empty string.
function typedCredPayload(form, status) {
	const editId = form.dataset.editId;
	const typeID = selectedCredTypeID();
	const name = document.getElementById("cred-name").value.trim();
	if (editId && typeID !== (form.dataset.editTypeId || "")) {
		status.textContent = "A custom type is chosen when a credential is created. Create a new " +
			"credential for this type.";
		return null;
	}
	const fields = credTypeFieldValues();
	if (!editId) return { name, type_id: typeID, fields };
	if (Object.values(fields).every((v) => v.trim() === "")) return { name };
	return { name, fields };
}

// loadCredentials populates the credential table with delete actions. keepPanel leaves the panel of
// credentials still waiting for a secret as it stands, for the refresh after a save made there:
// rebuilding it would throw away a secret pasted into another of its rows.
async function loadCredentials(keepPanel) {
	try {
		const data = await getJSON("/credentials");
		const creds = data.credentials || [];
		// An install without the encryption key and salt lists credentials fine and refuses every
		// write. Saying so is worth doing, but it is a fact about WRITING, and gating the render on
		// it hid four real rows on the public demo while the launch dialog two clicks away listed
		// the same four by name. The notice is a banner over the table now, not a replacement for
		// it, and only the control that would write is disabled.
		if (data.sealing === false) {
			renderSealingNotice();
			const add = document.querySelector(".page-head .button.primary");
			if (add) {
				add.disabled = true;
				add.dataset.tip = isDemo()
					? "This demo is read-only, and it stores no encryption key, so nothing can be saved here."
					: "Set SWITCHTENDER_ENCRYPTION_KEY and SWITCHTENDER_ENCRYPTION_SALT on the server to store credentials.";
			}
		}
		if (creds.length === 0) {
			showEmpty(data.sealing === false
				? "No credentials are stored, and this server could not seal one anyway until it has an encryption key and salt."
				: "No credentials yet. Add one and templates can reach hosts with it, sealed at rest and injected only at execution.");
			return;
		}
		if (!keepPanel) renderNeedsSecret(creds);
		const tbody = document.getElementById("credentials");
		// The rows are replaced once the answer is in, not cleared before the request, so two refreshes
		// in flight together each redraw the table rather than both appending to it.
		tbody.textContent = "";
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
				{ label: "Kind", value: credKindLabel(c) },
				{ label: "Source", value: c.source || "local" },
				{ label: "Settings", value: settingsEntries.map(([k, v]) => k + "=" + v).join("\n"), block: true },
			]);
			// Kind and source are chips rather than bare text, so the column reads at a glance and the
			// facet menus can offer them as values to tick.
			const kind = td("");
			const kindChip = document.createElement("span");
			kindChip.className = "cred-kind";
			kindChip.textContent = credKindLabel(c);
			if (c.type_id) {
				kindChip.dataset.typeId = c.type_id;
				kindChip.dataset.tip = "A custom type, whose fields are set in the edit dialog";
			}
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
			if (kindSpec && kindSpec.federated) {
				secretChip.textContent = "minted per run";
				secretChip.dataset.tip = "Nothing is stored. Each run gets a short-lived identity token " +
					"this server signs";
			}
			secret.appendChild(secretChip);
			tr.appendChild(secret);
			// The server computes used_by with the same reading its delete guard uses, across
			// templates, inventories, projects, and inventory sources. Counting templates alone here
			// made a credential holding a project's deploy key read as safe to delete right up until
			// the delete was refused for being in use.
			const usedBy = td("");
			const kinds = Object.entries(c.used_by || {}).filter(([, v]) => v && v.length);
			if (kinds.length) {
				const total = kinds.reduce((n, [, v]) => n + v.length, 0);
				const pages = { templates: "/ui/templates", inventories: "/ui/inventories",
					projects: "/ui/projects", inventory_sources: "/ui/sources" };
				const link = document.createElement("a");
				const firstPage = pages[kinds[0][0]] || "/ui/templates";
				link.href = total === 1
					? firstPage + "?q=" + encodeURIComponent(kinds[0][1][0])
					: firstPage;
				link.textContent = total === 1 ? kinds[0][1][0] : total + " objects";
				link.dataset.tip = "Used by " + kinds.map(([k, v]) =>
					k.replace("_", " ") + ": " + v.join(", ")).join(" \u00b7 ") +
					". Deleting is refused while anything still uses it";
				usedBy.appendChild(link);
			} else {
				usedBy.textContent = "\u2014";
				usedBy.dataset.tip = "Nothing references this credential, so it can be deleted";
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
					dropNeedsSecret(c.id);
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
	title.textContent = needsSecretTitle(pending.length);
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
	for (const c of pending) {
		const row = document.createElement("div");
		row.className = "cred-needs-row";
		row.dataset.credId = c.id;
		const meta = document.createElement("div");
		meta.className = "cred-needs-meta";
		const name = document.createElement("span");
		name.className = "cred-needs-name";
		name.textContent = c.name;
		const kind = document.createElement("span");
		kind.className = "cred-needs-kind";
		kind.textContent = credKindLabel(c);
		if (c.type_id) kind.dataset.typeId = c.type_id;
		meta.appendChild(name);
		meta.appendChild(kind);

		// A custom type holds several fields sealed together, which one pasted secret cannot fill,
		// so its row hands over to the edit dialog that draws each field.
		if (c.type_id) {
			const why = document.createElement("span");
			why.className = "muted";
			why.textContent = "Its type declares several fields, entered in the edit dialog.";
			const open = document.createElement("button");
			open.className = "button primary";
			open.dataset.mutates = "true";
			open.dataset.tip = "Click to enter this credential's field values";
			open.textContent = "Set fields";
			open.disabled = readOnly;
			open.addEventListener("click", () => openCredentialEdit(c));
			const note = document.createElement("span");
			note.className = "cred-needs-status muted";
			if (readOnly) note.textContent = readOnlyReason();
			row.appendChild(meta);
			row.appendChild(why);
			row.appendChild(open);
			row.appendChild(note);
			list.appendChild(row);
			continue;
		}

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
		if (readOnly) status.textContent = readOnlyReason();

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
				settleNeedsSecret(panel);
				// The table below went on calling this credential "needs a secret" until a reload,
				// while the server already held the secret. It is read again, and this panel is not.
				loadCredentials(true);
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

// needsSecretTitle heads the panel with how many credentials it lists.
function needsSecretTitle(n) {
	return n + (n === 1 ? " credential needs a secret" : " credentials need a secret");
}

// settleNeedsSecret retitles the panel after a row leaves it, and takes the panel down once none
// are left. It counts the rows the panel still holds rather than keeping a tally, because a row
// leaves either through its own Save or through a delete made in the table below.
function settleNeedsSecret(panel) {
	const n = panel.querySelectorAll(".cred-needs-row").length;
	const title = panel.querySelector(".cred-needs-head strong");
	if (title) title.textContent = needsSecretTitle(n);
	panel.hidden = n === 0;
}

// dropNeedsSecret takes a deleted credential out of the panel. A delete in the table left it listed
// here, still asking for a secret for something that no longer existed, until a reload. Only its
// own row goes: rebuilding the panel would throw away a secret pasted into another row.
function dropNeedsSecret(id) {
	const panel = document.getElementById("cred-needs");
	if (!panel) return;
	for (const row of panel.querySelectorAll(".cred-needs-row")) {
		if (row.dataset.credId !== id) continue;
		row.remove();
		settleNeedsSecret(panel);
	}
}

// relabelTypedKinds rewrites the kind of every typed credential already drawn in the table and the
// panel, for the types arriving after the credentials did.
function relabelTypedKinds() {
	const drawn = document.querySelectorAll("#credentials [data-type-id], #cred-needs [data-type-id]");
	for (const el of drawn) {
		el.textContent = credKindLabel({ type_id: el.dataset.typeId });
	}
}

// loadCredentialTypes fills the Credential types section and the custom options in the credential
// dialog. Types are for admins and can be switched off, so a 403 or 404 hides the section without a
// word: neither is something this page went wrong at.
async function loadCredentialTypes() {
	const section = document.getElementById("ctype-section");
	if (!section) return;
	if (!roleAtLeast("admin")) {
		section.hidden = true;
		return;
	}
	const status = document.getElementById("ctype-list-status");
	let types;
	try {
		const data = await getJSON("/credential-types");
		types = data.types || [];
	} catch (err) {
		if (err.status === 403 || err.status === 404) {
			section.hidden = true;
			return;
		}
		section.hidden = false;
		if (status) status.textContent = "Failed to load credential types: " + err.message;
		return;
	}
	credTypes = new Map(types.map((t) => [t.id, t]));
	fillCredTypeOptions();
	relabelTypedKinds();
	renderCredTypes(types);
	section.hidden = false;
}

// renderCredTypes draws the credential types table, one row per type with its fields, what it
// injects, and Edit and Delete actions.
function renderCredTypes(types) {
	const tbody = document.getElementById("ctypes");
	const table = document.getElementById("ctype-table");
	const status = document.getElementById("ctype-list-status");
	if (!tbody || !table) return;
	tbody.textContent = "";
	if (types.length === 0) {
		table.hidden = true;
		if (status) {
			status.textContent = "No credential types yet. Define one to give a credential its own " +
				"fields and injectors.";
		}
		return;
	}
	if (status) status.textContent = "";
	for (const t of types) {
		const tr = document.createElement("tr");
		tr.appendChild(td(t.name));
		tr.appendChild(td(credTypeFieldsSummary(t)));
		tr.appendChild(td(credTypeInjects(t)));
		const actions = document.createElement("td");
		const del = document.createElement("button");
		del.className = "button danger";
		del.dataset.mutates = "true";
		del.dataset.tip = "Click to delete this credential type permanently";
		del.textContent = "Delete";
		del.addEventListener("click", async (e) => {
			e.preventDefault();
			if (!window.confirm("Delete credential type " + t.name + "?")) return;
			try {
				await authedDelete("/credential-types/" + encodeURIComponent(t.id));
				loadCredentialTypes();
			} catch (err) {
				if (status) status.textContent = "Delete failed: " + err.message;
			}
		});
		actions.appendChild(editButton(() => openCredTypeEditor(t),
			"Click to edit this credential type"));
		actions.appendChild(document.createTextNode(" "));
		actions.appendChild(del);
		tr.appendChild(actions);
		tbody.appendChild(tr);
	}
	table.hidden = false;
}

// openCredTypeEditor fills the type editor and shows it. A stored type is shown as the JSON it is
// saved from, without the id, creation time, and origin the server assigns, and saves with PUT.
// With no type the editor starts from an example and saves with POST.
function openCredTypeEditor(t) {
	const form = document.getElementById("ctype-form");
	const box = document.getElementById("ctype-json");
	if (t) {
		form.dataset.editId = t.id;
		const def = Object.assign({}, t);
		delete def.id;
		delete def.created_at;
		delete def.origin;
		box.value = JSON.stringify(def, null, 2);
		setModalTitle("ctype", "Edit credential type");
	} else {
		delete form.dataset.editId;
		box.value = CRED_TYPE_EXAMPLE;
		setModalTitle("ctype", "Add a credential type");
	}
	document.getElementById("ctype-status").textContent = "";
	document.getElementById("ctype-modal").hidden = false;
}

// wireCredentialTypes hooks the type editor up to POST /credential-types for a new type and PUT
// /credential-types/{id} when editing. The definition is parsed here first, so malformed JSON is
// reported without a request, and the server's own refusal is shown as it wrote it.
function wireCredentialTypes() {
	const form = document.getElementById("ctype-form");
	if (!form) return;
	const openBtn = document.getElementById("ctype-open");
	if (openBtn) openBtn.addEventListener("click", () => openCredTypeEditor(null));
	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still saving, the same guard the credential
	// dialog uses, so a double click on Save creates the type once.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("ctype-status");
		let def;
		try {
			def = JSON.parse(document.getElementById("ctype-json").value);
		} catch (err) {
			status.textContent = "Invalid JSON: " + err.message;
			return;
		}
		if (!def || typeof def !== "object" || Array.isArray(def)) {
			status.textContent = "Invalid JSON: the definition must be an object.";
			return;
		}
		const editId = form.dataset.editId;
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/credential-types/" + encodeURIComponent(editId), def, "PUT");
			} else {
				await postAction("/credential-types", def);
			}
			status.textContent = "Saved.";
			closeModal("ctype");
			loadCredentialTypes();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
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

