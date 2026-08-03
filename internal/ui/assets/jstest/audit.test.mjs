// Tests for auditChange in 09-audit.js, which derives the readable sentence for a recorded
// method and path.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "09-audit.js"]);

test("auditChange turns a method and path into a sentence", () => {
	const tests = [
		// Test 0: Creating a member of a collection.
		{ Method: "POST", Path: "/v1/templates", Want: "Created template" },
		// Test 1: Deleting a named object.
		{ Method: "DELETE", Path: "/v1/templates/tpl_123", Want: "Deleted template tpl_123" },
		// Test 2: Updating a named object.
		{ Method: "PUT", Path: "/v1/credentials/cred_9", Want: "Updated credential cred_9" },
		// Test 3: PATCH reads as an update too.
		{ Method: "PATCH", Path: "/v1/users/usr_2", Want: "Updated user usr_2" },
		// Test 4: A trailing action is the change itself, not a field of one.
		{ Method: "POST", Path: "/v1/runs/run_1/approve", Want: "Approved run run_1" },
		// Test 5: Cancel.
		{ Method: "POST", Path: "/v1/runs/run_2/cancel", Want: "Canceled run run_2" },
		// Test 6: The secret rotation action carries its own phrasing.
		{
			Method: "POST", Path: "/v1/credentials/cred_1/rotate-secret",
			Want: "Rotated the secret on credential cred_1",
		},
		// Test 7: An unknown trailing action still reads, hyphens softened.
		{ Method: "POST", Path: "/v1/runs/run_9/weird-thing", Want: "weird thing on run run_9" },
		// Test 8: A hyphenated collection reads as words.
		{
			Method: "POST", Path: "/v1/inventory-sources/src_1",
			Want: "Created inventory source src_1",
		},
		// Test 9: An unmapped collection falls back to its own name.
		{ Method: "POST", Path: "/v1/flux-capacitors", Want: "Created flux capacitors" },
		// Test 10: Named CLI changes use their own sentences.
		{ Method: "CLI", Path: "/v1/cli/backup", Want: "Took a backup" },
		// Test 11: A nested CLI path matches whole.
		{ Method: "CLI", Path: "/v1/cli/audit/anchor", Want: "Anchored the audit chain" },
		// Test 12: An unnamed CLI change still reads as a command line action.
		{
			Method: "CLI", Path: "/v1/cli/cache/prune-old",
			Want: "Ran cache prune old from the command line",
		},
		// Test 13: Webhook deliveries collapse to one sentence.
		{ Method: "POST", Path: "/v1/hooks/github/abc123", Want: "Fired a webhook trigger" },
		// Test 14: An SSO callback is a sign-in.
		{ Method: "POST", Path: "/v1/auth/oidc/callback", Want: "Signed in" },
		// Test 15: A SAML assertion is a sign-in too.
		{ Method: "POST", Path: "/v1/auth/saml/acs", Want: "Signed in" },
		// Test 16: Any other auth path is an authentication change.
		{ Method: "PUT", Path: "/v1/auth/keys", Want: "Changed authentication" },
		// Test 17: A method with no verb mapping passes through.
		{ Method: "GET", Path: "/v1/runs", Want: "GET on run" },
		// Test 18: A lowercase method still finds its verb.
		{ Method: "delete", Path: "/v1/runs/run_1", Want: "Deleted run run_1" },
		// Test 19: A path without the version prefix reads the same.
		{ Method: "POST", Path: "templates", Want: "Created template" },
		// Test 20: An empty path is an empty sentence.
		{ Method: "POST", Path: "", Want: "" },
		// Test 21: Null inputs are an empty sentence, not a crash.
		{ Method: null, Path: null, Want: "" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.equal(app.auditChange(tc.Method, tc.Path), tc.Want, "test " + i + ": " + tc.Path);
	}
});
