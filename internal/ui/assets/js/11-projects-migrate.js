// wireProjectForm hooks the project dialog up to POST /projects for a new record and PUT
// /projects/{id} when editing. The New button resets the dialog to add mode.
function wireProjectForm() {
	fillSelect(document.getElementById("project-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	fillSelect(document.getElementById("project-pull-credential"), "/credentials", "credentials",
		(c) => c.name + " (" + c.kind + ")");
	const form = document.getElementById("project-form");
	const resetToCreate = () => {
		delete form.dataset.editId;
		document.getElementById("project-name").value = "";
		document.getElementById("project-repo").value = "";
		document.getElementById("project-branch").value = "";
		document.getElementById("project-credential").value = "";
		document.getElementById("project-deps").checked = true;
		document.getElementById("project-image").value = "";
		document.getElementById("project-pull-credential").value = "";
		document.getElementById("project-status").textContent = "";
		setModalTitle("project", "Add a project");
	};
	const openBtn = document.getElementById("project-open");
	if (openBtn) openBtn.addEventListener("click", resetToCreate);

	const submitBtn = form.querySelector('button[type="submit"]');
	// inFlight drops a second submit while the first is still posting, so a fast double click on Save
	// creates the project once rather than twice. The modal save stays on the page, so the button is
	// re-enabled once the request settles either way, leaving the dialog usable for the next project.
	let inFlight = false;
	form.addEventListener("submit", async (e) => {
		e.preventDefault();
		if (inFlight) return;
		const status = document.getElementById("project-status");
		const editId = form.dataset.editId;
		const payload = {
			name: document.getElementById("project-name").value.trim(),
			repo_url: document.getElementById("project-repo").value.trim(),
			branch: document.getElementById("project-branch").value.trim(),
			credential_id: document.getElementById("project-credential").value,
			install_deps: document.getElementById("project-deps").checked,
			image: document.getElementById("project-image").value.trim(),
			pull_credential_id: document.getElementById("project-pull-credential").value,
		};
		inFlight = true;
		if (submitBtn) submitBtn.disabled = true;
		try {
			if (editId) {
				await postAction("/projects/" + editId, payload, "PUT");
			} else {
				await postAction("/projects", payload);
			}
			resetToCreate();
			status.textContent = "Saved.";
			closeModal("project");
			document.getElementById("projects").innerHTML = "";
			loadProjects();
		} catch (err) {
			status.textContent = "Save failed: " + err.message;
		} finally {
			inFlight = false;
			if (submitBtn) submitBtn.disabled = false;
		}
	});
}

// loadProjects populates the project table with delete actions.
async function loadProjects() {
	try {
		// Credential names for the inspect drawer: a raw id answers nothing, and the reader had to
		// carry it to the credentials page by hand to learn what it was. A failed lookup falls back
		// to showing the id rather than hiding the field.
		let credNames = {};
		try {
			const cd = await getJSON("/credentials");
			for (const c of cd.credentials || []) credNames[c.id] = c.name;
		} catch { /* names stay unresolved */ }
		const credLabel = (id) => id ? (credNames[id] || id) : "";
		const data = await getJSON("/projects");
		const projects = data.projects || [];
		if (projects.length === 0) {
			showEmpty("No projects yet.");
			return;
		}
		const tbody = document.getElementById("projects");
		for (const p of projects) {
			const tr = document.createElement("tr");
			tr.appendChild(td(p.name));
			// The repository is a real link when it is https, so a reader can jump straight to the
			// source; ssh and scp-style remotes stay plain text.
			const repoCell = td("", "mono");
			if (/^https?:\/\//.test(p.repo_url || "")) {
				const a = document.createElement("a");
				a.href = p.repo_url;
				a.target = "_blank";
				a.rel = "noopener";
				a.textContent = p.repo_url;
				a.addEventListener("click", (e) => e.stopPropagation());
				repoCell.appendChild(a);
			} else {
				repoCell.textContent = p.repo_url;
			}
			tr.appendChild(repoCell);
			tr.appendChild(td(p.branch || "default", "mono"));
			tr.appendChild(tdTime(p.created_at));
			const actions = deleteCell("/projects/" + p.id, "project " + p.name, tr, "No projects yet.");
			const browse = document.createElement("button");
			browse.className = "button";
			browse.textContent = "Files";
			browse.dataset.tip = "Click to browse this project's cached checkout";
			browse.addEventListener("click", (e) => { e.preventDefault(); e.stopPropagation(); openProjectFiles(p); });
			actions.insertBefore(browse, actions.firstChild);
			actions.insertBefore(editButton(() => openProjectEdit(p), "Click to edit this project's repository, branch, and credentials"), actions.firstChild);
			tr.appendChild(actions);
			inspectable(tr, p.name, [
				{ label: "Repository", value: p.repo_url, copy: true },
				{ label: "Branch", value: p.branch || "default" },
				{ label: "Credential", value: credLabel(p.credential_id) },
				{ label: "Image", value: p.image },
				{ label: "Install deps", value: p.install_deps ? "yes" : "no" },
				{ label: "Pull credential", value: credLabel(p.pull_credential_id) },
				{ label: "Created", value: fmtTime(p.created_at) },
				{ label: "ID", value: p.id, copy: true },
			]);
			tbody.appendChild(tr);
		}
		setStatus("");
		document.querySelector("table.runs").hidden = false;
		showListControls();
	} catch (e) {
		setStatus("Failed to load projects: " + e.message);
	}
}

// wireMigrate hooks the Preview and Import buttons up to the import endpoint. Preview shows the plan;
// Import writes it. The buttons are wired even in the read-only demo, where applyReadOnly disables
// them.
// SAMPLE_AWX_EXPORT is a small but real AWX export in the shape the importer accepts, so the
// migration can be tried without having an AWX instance to export from. It covers projects,
// grouped inventories, credentials, templates with a survey and slicing, and a schedule.
const SAMPLE_AWX_EXPORT = {
	projects: [
		{ name: "web-platform", scm_type: "git", scm_url: "https://github.com/acme/web-platform.git", scm_branch: "main" },
		{ name: "database-ops", scm_type: "git", scm_url: "https://github.com/acme/database-ops.git", scm_branch: "main" },
	],
	inventories: [
		{
			name: "production",
			groups: [
				{ name: "web", hosts: [{ name: "web01" }, { name: "web02" }, { name: "web03" }] },
				{ name: "db", hosts: [{ name: "db01" }, { name: "db02" }] },
			],
		},
		{ name: "staging", hosts: [{ name: "stage01" }] },
	],
	credentials: [
		{ name: "prod-ssh", credential_type: "Machine" },
		{ name: "vault-password", credential_type: "Vault" },
	],
	job_templates: [
		{
			name: "Deploy web", playbook: "site.yml", project: "web-platform",
			inventory: "production", job_slice_count: 3, credentials: ["prod-ssh"],
			survey_spec: {
				spec: [{ variable: "release", question_name: "Release tag", type: "text", required: true }],
			},
		},
		{
			name: "Migrate database", playbook: "migrate.yml", project: "database-ops",
			inventory: "production", credentials: ["prod-ssh", "vault-password"],
		},
		{
			name: "Nightly audit", playbook: "audit.yml", project: "web-platform",
			inventory: "production",
			related: {
				schedules: [{ name: "Every night", rrule: "DTSTART:20260101T020000Z RRULE:FREQ=DAILY;INTERVAL=1" }],
			},
		},
	],
};

function wireMigrate() {
	const sample = document.getElementById("migrate-sample");
	if (sample) {
		sample.addEventListener("click", () => {
			document.getElementById("migrate-format").value = "awx";
			document.getElementById("migrate-export").value =
				JSON.stringify(SAMPLE_AWX_EXPORT, null, 2);
			const status = document.getElementById("migrate-status");
			if (status) status.textContent = "Sample loaded. Preview shows what it would create.";
		});
	}

	const preview = document.getElementById("migrate-preview");
	const apply = document.getElementById("migrate-apply");
	if (preview) {
		preview.addEventListener("click", () => runMigrate(false).catch((err) => {
			document.getElementById("migrate-status").textContent = "Preview failed: " + err.message;
		}));
	}
	if (apply) {
		// Import writes the whole plan, so a second click while the first is still in flight would
		// import it twice. The guard drops the repeat and disables the button until the import fails,
		// when report re-enables it for a retry, matching the launch form. The empty-export check runs
		// before the guard so a no-op click never leaves the button dead.
		const runImport = guardedSubmit(apply, () => runMigrate(true), (err) => {
			document.getElementById("migrate-status").textContent = "Import failed: " + err.message;
		});
		apply.addEventListener("click", () => {
			if (!document.getElementById("migrate-export").value.trim()) {
				document.getElementById("migrate-status").textContent = "Paste an export first.";
				return;
			}
			runImport();
		});
	}

	wireMigrateFile();
	// Only Rundeck needs a target inventory, so the field appears with that format and is hidden
	// otherwise rather than asking for something the other importers would ignore.
	const format = document.getElementById("migrate-format");
	const inventoryField = document.getElementById("migrate-inventory-field");
	if (format && inventoryField) {
		// Jenkins picks an agent by label rather than naming hosts, so it needs the same answer
		// Rundeck does.
		const help = document.getElementById("migrate-inventory-help");
		const sync = () => {
			const needs = format.value === "rundeck" || format.value === "jenkins";
			inventoryField.hidden = !needs;
			if (help) {
				help.textContent = format.value === "jenkins"
					? "Jenkins picks an agent by label rather than naming hosts, so say which machines these jobs run against."
					: "Rundeck jobs name no inventory of their own, so say which hosts they target.";
			}
			const fileHelp = document.getElementById("migrate-file-help");
			if (fileHelp) {
				fileHelp.textContent = format.value === "jenkins"
					? "Zip the jobs directory from your JENKINS_HOME and upload it, or paste one job's config.xml below."
					: "Or paste the export below.";
			}
		};
		format.addEventListener("change", sync);
		sync();
	}
}

// runMigrate posts the raw export text to the import endpoint and renders the result. It sends the
// textarea contents verbatim as the body, since the endpoint reads the export document itself rather
// than a JSON wrapper, and reuses the same bearer auth and 401 handling as the other API calls. It
// throws on failure so the caller can report it and, for an import, re-enable its guarded button.
async function runMigrate(apply) {
	const status = document.getElementById("migrate-status");
	const format = document.getElementById("migrate-format").value;
	// An uploaded archive is bytes, not text, so it cannot travel through the textarea. When one is
	// loaded it is what gets sent, and the box stays empty rather than showing unreadable content.
	const body = migrateFileBytes || document.getElementById("migrate-export").value;
	if (!migrateFileBytes && !String(body).trim()) {
		status.textContent = "Paste an export or choose a file first.";
		return;
	}
	document.getElementById("migrate-plan").innerHTML = "";
	status.textContent = apply ? "Importing." : "Building preview.";
	// Rundeck dispatches by node filter and names no inventory, so the target hosts ride along as a
	// query parameter. The other formats carry their own and ignore it.
	const params = [];
	if (apply) params.push("apply=true");
	const inventory = (document.getElementById("migrate-inventory")?.value || "").trim();
	if ((format === "rundeck" || format === "jenkins") && inventory) {
		params.push("inventory=" + encodeURIComponent(inventory));
	}
	const path = API + "/import/" + format + (params.length ? "?" + params.join("&") : "");
	const res = await fetch(path, { method: "POST", headers: authHeaders(), body });
	if (res.status === 401) {
		requireLogin();
		throw new Error("authentication required");
	}
	const data = await res.json().catch(() => ({}));
	if (!res.ok) throw new Error(data.error || ("HTTP " + res.status));
	status.textContent = apply
		? "Imported " + (data.created || 0) + " objects."
		: "Preview ready.";
	renderMigratePlan(data);
}

// migrateFileBytes holds an uploaded archive's bytes. A zip cannot round trip through a textarea, so
// it is kept aside and sent as the request body directly.
let migrateFileBytes = null;

// wireMigrateFile loads a chosen export file. A text export fills the box so it can still be read
// and edited before importing; an archive is kept as bytes, because showing it would fill the box
// with binary and editing it would corrupt it.
function wireMigrateFile() {
	const input = document.getElementById("migrate-file");
	const box = document.getElementById("migrate-export");
	const status = document.getElementById("migrate-status");
	if (!input || !box) return;
	input.addEventListener("change", async () => {
		const file = input.files && input.files[0];
		migrateFileBytes = null;
		if (!file) return;
		const isArchive = /\.zip$/i.test(file.name);
		try {
			if (isArchive) {
				migrateFileBytes = await file.arrayBuffer();
				box.value = "";
				box.placeholder = "Using " + file.name + " (" + Math.ceil(file.size / 1024) + " KB).";
				status.textContent = "Loaded " + file.name + ".";
				return;
			}
			box.value = await file.text();
			status.textContent = "Loaded " + file.name + ".";
		} catch (err) {
			status.textContent = "Could not read that file: " + err.message;
		}
	});
	// Typing over a loaded archive should use what was typed, not silently send the file instead.
	box.addEventListener("input", () => {
		if (box.value.trim() && migrateFileBytes) {
			migrateFileBytes = null;
			input.value = "";
			box.placeholder = "Paste your AWX, Semaphore, Rundeck, or Jenkins export here";
		}
	});
}

// renderMigratePlan draws the import plan: each non-empty resource list with its names, any warnings,
// and, once applied, the count written plus the reminder to set every imported credential's secret,
// since imports create credential shells with no secret.
function renderMigratePlan(data) {
	const el = document.getElementById("migrate-plan");
	el.innerHTML = "";
	// A migration plan is a record worth keeping: it is what was about to change, or what did.
	const exportRow = document.createElement("div");
	exportRow.className = "drill-actions migrate-export";
	for (const fmt of ["JSON", "YAML"]) {
		const btn = document.createElement("button");
		btn.type = "button";
		btn.className = "button";
		btn.textContent = fmt;
		btn.dataset.tip = "Click to download this migration plan as " + fmt;
		btn.addEventListener("click", () => {
			const stamp = new Date().toISOString().slice(0, 10);
			if (fmt === "YAML") {
				downloadBlob("switchtender-migration-" + stamp + ".yaml", "text/yaml", toYAML(data));
			} else {
				downloadBlob("switchtender-migration-" + stamp + ".json", "application/json",
					JSON.stringify(data, null, 2) + "\n");
			}
		});
		exportRow.appendChild(btn);
	}
	el.appendChild(exportRow);

	if (data.applied) {
		const done = document.createElement("div");
		done.className = "migrate-applied";
		done.textContent = "Imported " + (data.created || 0) + " objects. Set the secret on each " +
			"imported credential before running templates that need it.";
		el.appendChild(done);
	}

	const groups = [
		["Projects", data.projects],
		["Inventories", data.inventories],
		["Sources", data.sources],
		["Credentials", data.credentials],
		["Templates", data.templates],
		["Schedules", data.schedules],
	];
	let shown = 0;
	for (const [label, names] of groups) {
		if (names && names.length) {
			el.appendChild(migrateGroup(label, names));
			shown++;
		}
	}

	if (data.warnings && data.warnings.length) {
		el.appendChild(migrateGroup("Warnings", data.warnings));
		shown++;
	}

	// The export buttons land in el before the groups do, so checking children could never see an
	// empty preview: a file with nothing importable showed two lone buttons offering to export
	// nothing, and the message written for that case was unreachable.
	if (!shown && !data.applied) {
		exportRow.remove();
		el.appendChild(emptyLine("Nothing to import from this export."));
	}
}

// migrateGroup builds a labeled block listing the names in one import category.
function migrateGroup(label, names) {
	const group = document.createElement("div");
	group.className = "migrate-group";
	const heading = document.createElement("h2");
	heading.textContent = label + " (" + names.length + ")";
	group.appendChild(heading);
	const list = document.createElement("ul");
	list.className = "migrate-list";
	for (const name of names) {
		const item = document.createElement("li");
		item.textContent = name;
		list.appendChild(item);
	}
	group.appendChild(list);
	return group;
}

// syncTemplateTool shows the Ansible fields or the command box in the template dialog to match the
// selected tool, so a bash, terraform, python, or go template hides playbook, inventory, limit,
// shards, and the execution image.
function syncTemplateTool() {
	const tool = document.getElementById("tpl-tool").value;
	const ansible = tool === "ansible" || tool === "";
	// The execution image and its pull credential are not Ansible-only: the container runner plans
	// every tool, so a terraform or bash template can be pinned to an image. Hiding them for those
	// tools left an image nobody could see or set, and an edit of a containerized non-Ansible
	// template stripped it, so the next run executed on the host instead of in the container.
	const ansibleFields = ["tpl-field-playbook", "tpl-field-inventory", "tpl-field-limit",
		"tpl-field-shards"];
	for (const id of ansibleFields) {
		const el = document.getElementById(id);
		if (el) el.hidden = !ansible;
	}
	const cmd = document.getElementById("tpl-field-command");
	if (cmd) cmd.hidden = ansible;
}

