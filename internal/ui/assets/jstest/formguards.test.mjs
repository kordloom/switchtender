// Lightweight in-flight-guard tests for the remaining create dialogs that carried the same
// duplicate-submit defect: inventory, inventory source, policy, and user. The two full flow tests,
// templateflow and scheduleflow, walk one modal form and one page form all the way through save and
// re-save. These four share the exact same guard, so each is checked the one way that matters: three
// rapid submits with the create POST held open store the record once and disable Save while it posts,
// and a rejection hands the button back. The create POST is held rather than answered, so the success
// reload paths stay out of the way and every test asserts only the guard.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { deferred, reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";
import { ALL_PARTS } from "./loader.mjs";

// guardCase drives one form: it wires the dialog, fills the fields, fires three rapid submits with
// the create POST held open, and asserts one POST and a dead Save button, then rejects the held
// request and asserts the button comes back. postPath is the create endpoint under /v1, wire is the
// wiring function name, and fill sets the fields the payload requires.
async function guardCase({ page, wire, postPath, fill, statusId }) {
	const gate = deferred();
	let posts = 0;
	const routes = {
		// The pickers each dialog loads at wire time answer empty; the create POST is held open so a
		// second submit lands while the first is still in flight.
		"/v1/credentials": reply({ credentials: [] }),
		"/v1/projects": reply({ projects: [] }),
		"/v1/inventories": reply({ inventories: [] }),
		[postPath]: (req) => {
			if (req.method === "POST") { posts++; return gate.promise; }
			return reply({});
		},
	};
	const { app, document, net, clock } = loadPage(page, { parts: ALL_PARTS, routes });
	app[wire]();
	// Let the wire-time picker lookups settle before the form is used.
	await clock.flush();

	const form = document.querySelector("form");
	const button = form.querySelector('button[type="submit"]');
	fill(document);

	const events = [fire(form, "submit"), fire(form, "submit"), fire(form, "submit")];

	assert.equal(posts, 1, "a repeat submit created the record a second time");
	assert.equal(button.disabled, true, "the Save button stayed live while the record was saving");
	assert.deepEqual(events.map((e) => e.defaultPrevented), [true, true, true]);

	// A rejection is the one settle that stays on the page without the success reload, so it proves
	// the finally hands the button back on failure the same as on success.
	gate.reject(new Error("rejected"));
	await clock.flush();
	assert.equal(posts, 1);
	assert.equal(button.disabled, false, "a settled save left the Save button dead");
	assert.match(document.getElementById(statusId).textContent, /^Save failed: /);
	net.assertClean();
}

test("the inventory form stores one inventory however fast Save is clicked", async () => {
	await guardCase({
		page: "inventories", wire: "wireInventoryForm", postPath: "/v1/inventories", statusId: "inv-status",
		fill: (d) => {
			d.getElementById("inv-name").value = "production fleet";
			d.getElementById("inv-content").value = "web1";
		},
	});
});

test("the inventory source form stores one source however fast Save is clicked", async () => {
	await guardCase({
		page: "sources", wire: "wireSourceForm", postPath: "/v1/inventory-sources", statusId: "src-status",
		fill: (d) => {
			d.getElementById("src-name").value = "aws production";
			d.getElementById("src-source").value = "aws_ec2.yml";
		},
	});
});

test("the policy form stores one policy however fast Save is clicked", async () => {
	await guardCase({
		page: "policies", wire: "wirePolicyForm", postPath: "/v1/policies", statusId: "policy-status",
		fill: (d) => {
			d.getElementById("policy-name").value = "prod terraform destroy";
		},
	});
});

test("the user form stores one account however fast Save is clicked", async () => {
	await guardCase({
		page: "users", wire: "wireUserForm", postPath: "/v1/users", statusId: "user-status",
		fill: (d) => {
			d.getElementById("user-name").value = "ada";
			d.getElementById("user-password").value = "the password";
		},
	});
});
