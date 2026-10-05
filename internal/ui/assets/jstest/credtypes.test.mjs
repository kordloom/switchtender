// Integration tests for custom credential types on the credentials page: the type editor, typed
// credentials in the credential dialog, the needs-secret panel for an imported typed shell, and the
// kubeconfig kind. Each drives the real template and asserts on the request the page sent.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// CLUSTER is a stored custom type with a multiline secret field, a one-line secret, and a plain one.
const CLUSTER = {
	id: "ctype_1",
	name: "Cluster access",
	fields: [
		{ name: "kubeconfig", label: "Kubeconfig", secret: true, multiline: true },
		{ name: "token", label: "Token", secret: true },
		{ name: "context" },
	],
	env: { KUBECONFIG: "{{ tower.filename }}", K8S_CONTEXT: "{{ context }}" },
	file: { template: "{{ kubeconfig }}" },
	created_at: "2026-09-30T12:00:00Z",
};

// CERTS is a stored custom type writing two named files.
const CERTS = {
	id: "ctype_2",
	name: "Client certificate",
	fields: [{ name: "cert", multiline: true }, { name: "key", secret: true, multiline: true }],
	extra_vars: { client_cert: "{{ tower.filename.cert }}", client_key: "{{ tower.filename.key }}" },
	file: { "template.cert": "{{ cert }}", "template.key": "{{ key }}" },
	created_at: "2026-09-30T12:00:00Z",
};

// KUBECONFIG_TEXT is a multiline value with indentation, a tab, a CRLF, and edge whitespace, all of
// which must reach the server unchanged.
const KUBECONFIG_TEXT = "  apiVersion: v1\nkind: Config\nclusters:\n- name: prod\n  cluster:\n" +
	"\tserver: https://k8s.example.com:6443\r\n\n";

// typeControls returns the typed field controls the credential dialog drew, keyed by field name.
function typeControls(document) {
	const out = {};
	for (const el of document.getElementById("cred-type-fields").querySelectorAll("input, textarea")) {
		out[el.dataset.field] = el;
	}
	return out;
}

test("the type editor posts the parsed definition, file injector included, and shows a refusal", async () => {
	const posted = [];
	const refusal = "file template.key is not referenced by any env or extra_vars injector";
	const routes = {
		"/v1/credential-types": (req) => {
			if (req.method === "POST") {
				posted.push(JSON.parse(req.body));
				return reply({ error: refusal }, { status: 400 });
			}
			return reply({ types: [], count: 0 });
		},
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	app.wireCredentialTypes();

	fire(document.getElementById("ctype-open"), "click");
	const box = document.getElementById("ctype-json");
	assert.equal(document.getElementById("ctype-modal").hidden, false);
	// The prefilled example is valid JSON and shows a file injector handing its path to KUBECONFIG.
	assert.deepEqual(JSON.parse(box.value).file, { template: "{{ kubeconfig }}" });
	assert.equal(JSON.parse(box.value).env.KUBECONFIG, "{{ tower.filename }}");

	const def = {
		name: "Client certificate",
		fields: [{ name: "cert", multiline: true }, { name: "key", secret: true, multiline: true }],
		file: { "template.cert": "{{ cert }}", "template.key": "{{ key }}" },
		extra_vars: { client_cert: "{{ tower.filename.cert }}" },
	};
	box.value = JSON.stringify(def, null, 2);
	const form = document.getElementById("ctype-form");
	const events = [fire(form, "submit"), fire(form, "submit")];
	await clock.flush();
	assert.deepEqual(events.map((e) => e.defaultPrevented), [true, true]);
	assert.equal(posted.length, 1, "a double submit posted the type twice");
	assert.deepEqual(posted[0], def, "the posted body is not the definition as written");
	assert.equal(document.getElementById("ctype-status").textContent, "Save failed: " + refusal);
	assert.equal(form.querySelector('button[type="submit"]').disabled, false);

	// Malformed JSON is reported here and never sent.
	box.value = '{"name": "broken",';
	fire(form, "submit");
	await clock.flush();
	assert.equal(posted.length, 1, "invalid JSON was sent to the server");
	assert.match(document.getElementById("ctype-status").textContent, /^Invalid JSON: /);
	net.assertClean();
});

test("editing a type shows it without id and created_at and saves it with PUT", async () => {
	let put = null;
	const routes = {
		"/v1/credential-types/ctype_1": (req) => {
			assert.equal(req.method, "PUT");
			put = JSON.parse(req.body);
			return reply(Object.assign({ id: "ctype_1" }, put));
		},
		"/v1/credential-types": reply({ types: [CLUSTER], count: 1 }),
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	app.wireCredentialTypes();
	await app.loadCredentialTypes();

	const row = document.querySelector("#ctypes tr");
	fire(Array.from(row.querySelectorAll("button")).find((b) => b.textContent === "Edit"), "click");
	const shown = JSON.parse(document.getElementById("ctype-json").value);
	assert.equal(shown.id, undefined);
	assert.equal(shown.created_at, undefined);
	assert.deepEqual(shown.file, CLUSTER.file);

	fire(document.getElementById("ctype-form"), "submit");
	await clock.flush();
	assert.deepEqual(put, shown);
	assert.equal(document.getElementById("ctype-modal").hidden, true);
	net.assertClean();
});

test("the types table summarizes fields and injectors, and Delete confirms first", async () => {
	let deleted = 0;
	const routes = {
		"/v1/credential-types/ctype_2": (req) => {
			assert.equal(req.method, "DELETE");
			deleted++;
			return reply({}, { status: 204 });
		},
		"/v1/credential-types": reply({ types: [CLUSTER, CERTS], count: 2 }),
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	await app.loadCredentialTypes();

	assert.equal(document.getElementById("ctype-section").hidden, false);
	const rows = Array.from(document.querySelectorAll("#ctypes tr"));
	assert.deepEqual(rows.map((tr) => tr.cells.slice(0, 3).map((c) => c.textContent)), [
		["Cluster access", "kubeconfig (secret, multiline), token (secret), context",
			"env: K8S_CONTEXT, KUBECONFIG · file: template"],
		["Client certificate", "cert (multiline), key (secret, multiline)",
			"extra vars: client_cert, client_key · files: cert, key"],
	]);

	const del = rows[1].querySelector("button.danger");
	app.confirm = () => false;
	fire(del, "click");
	await clock.flush();
	assert.equal(deleted, 0, "a declined confirm still deleted the type");
	app.confirm = () => true;
	fire(del, "click");
	await clock.flush();
	assert.equal(deleted, 1);
	net.assertClean();
});

test("an install without credential types hides the section quietly", async () => {
	const routes = {
		"/v1/credential-types": reply({ error: "credential types are not enabled" }, { status: 404 }),
	};
	const { app, document, net } = loadPage("credentials", { parts: ALL_PARTS, routes });
	await app.loadCredentialTypes();
	assert.equal(document.getElementById("ctype-section").hidden, true);
	assert.ok(!document.getElementById("cred-kind-custom"),
		"the dialog offered custom types the server does not have");
	net.assertClean();
});

test("a typed credential posts its type and every field, a multiline value byte for byte", async () => {
	let created = null;
	const routes = {
		"/v1/credential-types": reply({ types: [CLUSTER], count: 1 }),
		"/v1/credentials": (req) => {
			if (req.method === "POST") { created = JSON.parse(req.body); return reply({ id: "cred_9" }); }
			return reply({ credentials: [] });
		},
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	app.wireCredentialForm();
	await app.loadCredentialTypes();

	const kind = document.getElementById("cred-kind");
	const option = Array.from(kind.options).find((o) => o.value === "type:ctype_1");
	assert.ok(option, "the kind select has no option for the custom type");
	assert.equal(option.textContent, "Cluster access");
	assert.equal(option.parentNode.getAttribute("label"), "Custom types");

	kind.value = "type:ctype_1";
	fire(kind, "change");
	for (const id of ["cred-source-field", "cred-secret-field", "cred-settings-field"]) {
		assert.equal(document.getElementById(id).hidden, true, id + " stayed visible for a custom type");
	}
	assert.equal(document.getElementById("cred-secret").required, false,
		"a hidden secret box stayed required and would block the submit");
	assert.equal(document.getElementById("cred-type-fields").hidden, false);
	const controls = typeControls(document);
	assert.deepEqual(Object.keys(controls), ["kubeconfig", "token", "context"]);
	assert.equal(controls.kubeconfig.tagName, "TEXTAREA");
	assert.equal(controls.kubeconfig.className, "input mono");
	assert.equal(controls.kubeconfig.rows, 6);
	assert.equal(controls.token.getAttribute("type"), "password");
	assert.equal(controls.token.getAttribute("autocomplete"), "off");
	assert.equal(controls.context.getAttribute("type"), "text");
	assert.equal(controls.kubeconfig.parentNode.textContent.startsWith("Kubeconfig"), true);
	assert.equal(controls.context.parentNode.textContent.startsWith("context"), true);

	document.getElementById("cred-name").value = "prod-cluster";
	controls.kubeconfig.value = KUBECONFIG_TEXT;
	controls.token.value = "s3cret";
	fire(document.getElementById("cred-form"), "submit");
	await clock.flush();
	assert.deepEqual(created, {
		name: "prod-cluster", type_id: "ctype_1",
		fields: { kubeconfig: KUBECONFIG_TEXT, token: "s3cret", context: "" },
	});
	assert.equal(created.fields.kubeconfig, KUBECONFIG_TEXT, "the multiline value was altered");
	assert.equal(document.getElementById("cred-status").textContent, "Saved.");

	// Switching back to a built-in kind restores the normal fields.
	kind.value = "ssh_key";
	fire(kind, "change");
	for (const id of ["cred-source-field", "cred-secret-field"]) {
		assert.equal(document.getElementById(id).hidden, false, id + " did not come back");
	}
	assert.equal(document.getElementById("cred-type-fields").hidden, true);
	assert.equal(document.getElementById("cred-secret").required, true);
	net.assertClean();
});

test("editing a typed credential keeps its values when blank and replaces them all otherwise", async () => {
	const puts = [];
	const routes = {
		"/v1/credential-types": reply({ types: [CLUSTER], count: 1 }),
		"/v1/credentials/cred_7": (req) => {
			assert.equal(req.method, "PUT");
			puts.push(JSON.parse(req.body));
			return reply({ id: "cred_7" });
		},
		"/v1/credentials": reply({ credentials: [] }),
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	app.wireCredentialForm();
	await app.loadCredentialTypes();
	const stored = { id: "cred_7", name: "prod-cluster", kind: "", type_id: "ctype_1", source: "local" };

	app.openCredentialEdit(stored);
	assert.equal(document.getElementById("cred-kind").value, "type:ctype_1");
	assert.equal(document.getElementById("cred-modal").hidden, false);
	assert.match(document.getElementById("cred-type-fields").textContent,
		/Leave every field blank to keep the stored values\. Entering any replaces them all\./);
	assert.deepEqual(Object.values(typeControls(document)).map((c) => c.value), ["", "", ""]);
	document.getElementById("cred-name").value = "prod-cluster-renamed";
	fire(document.getElementById("cred-form"), "submit");
	await clock.flush();
	assert.deepEqual(puts[0], { name: "prod-cluster-renamed" }, "a rename sent field values");

	app.openCredentialEdit(stored);
	typeControls(document).context.value = "staging";
	fire(document.getElementById("cred-form"), "submit");
	await clock.flush();
	assert.deepEqual(puts[1], {
		name: "prod-cluster", fields: { kubeconfig: "", token: "", context: "staging" },
	}, "one field entered did not send every field");
	net.assertClean();
});

test("the kubeconfig kind is offered and says which variables it binds", () => {
	const { app, document, net } = loadPage("credentials", { parts: ALL_PARTS, routes: {} });
	app.wireCredentialForm();
	const option = Array.from(document.getElementById("cred-kind").options)
		.find((o) => o.value === "kubeconfig");
	assert.ok(option, "no kubeconfig option");
	assert.equal(option.textContent, "Kubernetes kubeconfig");
	assert.equal(option.parentNode.getAttribute("label"), "Cloud and platform");

	const kind = document.getElementById("cred-kind");
	kind.value = "kubeconfig";
	fire(kind, "change");
	const hint = document.getElementById("cred-kind-hint").textContent;
	for (const name of ["KUBECONFIG", "K8S_AUTH_KUBECONFIG", "KUBE_CONFIG_PATH"]) {
		assert.ok(hint.includes(name), "the kubeconfig hint does not mention " + name);
	}
	assert.match(document.getElementById("cred-secret").placeholder, /^apiVersion: v1\nkind: Config/);
	net.assertClean();
});

test("an imported typed credential gets Set fields in the panel instead of a secret box", async () => {
	const routes = {
		"/v1/credential-types": reply({ types: [CLUSTER], count: 1 }),
		"/v1/credentials": reply({ credentials: [
			{ id: "cred_9", name: "imported-cluster", kind: "", type_id: "ctype_1", source: "local",
				needs_secret: true },
			{ id: "cred_2", name: "aws-prod", kind: "aws", source: "local", needs_secret: true },
		] }),
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	await app.loadCredentials();
	await app.loadCredentialTypes();
	await clock.flush();

	const [typed, builtin] = document.querySelectorAll(".cred-needs-row");
	// Checked as a boolean: a failing equal on a DOM node makes the reporter serialize the whole tree.
	assert.ok(!typed.querySelector("textarea"), "a typed shell was offered one secret box");
	const setFields = typed.querySelector("button");
	assert.equal(setFields.textContent, "Set fields");
	assert.equal(typed.querySelector(".cred-needs-kind").textContent, "custom: Cluster access");
	// A built-in kind keeps its secret box and Save.
	assert.ok(builtin.querySelector("textarea"));
	assert.equal(builtin.querySelector("button").textContent, "Save");
	// The table's kind chip names the type once the types are in.
	const chips = Array.from(document.querySelectorAll("#credentials .cred-kind:not(.cred-source)"))
		.map((el) => el.textContent);
	assert.deepEqual(chips, ["custom: Cluster access", "aws"]);

	fire(setFields, "click");
	assert.equal(document.getElementById("cred-modal").hidden, false);
	assert.equal(document.getElementById("cred-form").dataset.editId, "cred_9");
	assert.equal(document.getElementById("cred-kind").value, "type:ctype_1");
	assert.deepEqual(Object.keys(typeControls(document)), ["kubeconfig", "token", "context"]);
	net.assertClean();
});

test("a typed credential edited onto the kubeconfig kind sends kind and secret at once", async () => {
	const puts = [];
	const routes = {
		"/v1/credential-types": reply({ types: [CLUSTER, CERTS], count: 2 }),
		"/v1/credentials/cred_7": (req) => {
			assert.equal(req.method, "PUT");
			puts.push(JSON.parse(req.body));
			return reply({ id: "cred_7" });
		},
		"/v1/credentials": reply({ credentials: [] }),
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	app.wireCredentialForm();
	await app.loadCredentialTypes();
	const stored = {
		id: "cred_7", name: "prod-cluster", kind: "", type_id: "ctype_1", source: "local",
	};
	const kind = document.getElementById("cred-kind");
	const status = document.getElementById("cred-status");
	const form = document.getElementById("cred-form");

	app.openCredentialEdit(stored);
	kind.value = "kubeconfig";
	fire(kind, "change");
	assert.equal(document.getElementById("cred-secret-field").hidden, false);
	assert.equal(document.getElementById("cred-type-fields").hidden, true);
	assert.equal(document.getElementById("cred-secret").required, true,
		"moving off a type replaces its values, so the secret is required");

	// Without the document nothing is sent, and the dialog says what is missing.
	fire(form, "submit");
	await clock.flush();
	assert.equal(puts.length, 0, "a switch without its secret was sent");
	assert.equal(status.textContent, "Moving a credential off its custom type replaces its field " +
		"values, so enter the secret for the new kind.");

	document.getElementById("cred-secret").value = KUBECONFIG_TEXT;
	fire(form, "submit");
	await clock.flush();
	assert.deepEqual(puts, [{
		name: "prod-cluster", kind: "kubeconfig", source: "local", secret: KUBECONFIG_TEXT,
		settings: {},
	}]);

	// Another custom type is not something an existing credential moves to.
	app.openCredentialEdit(stored);
	kind.value = "type:ctype_2";
	fire(kind, "change");
	fire(form, "submit");
	await clock.flush();
	assert.equal(puts.length, 1, "a move to another custom type was sent");
	assert.equal(status.textContent,
		"A custom type is chosen when a credential is created. Create a new credential for this type.");
	net.assertClean();
});

test("editing an imported type leaves its origin to the server", async () => {
	let put = null;
	const imported = Object.assign({}, CLUSTER, { id: "ctype_9", origin: "awx" });
	const routes = {
		"/v1/credential-types/ctype_9": (req) => {
			put = JSON.parse(req.body);
			return reply(Object.assign({ id: "ctype_9" }, put));
		},
		"/v1/credential-types": reply({ types: [imported], count: 1 }),
	};
	const { app, document, net, clock } = loadPage("credentials", { parts: ALL_PARTS, routes });
	app.wireCredentialTypes();
	await app.loadCredentialTypes();
	const row = document.querySelector("#ctypes tr");
	fire(Array.from(row.querySelectorAll("button")).find((b) => b.textContent === "Edit"), "click");
	const shown = JSON.parse(document.getElementById("ctype-json").value);
	assert.equal(shown.origin, undefined, "the editor showed a field only the server sets");
	fire(document.getElementById("ctype-form"), "submit");
	await clock.flush();
	assert.equal(put.origin, undefined, "the edit sent the origin back");
	net.assertClean();
});
