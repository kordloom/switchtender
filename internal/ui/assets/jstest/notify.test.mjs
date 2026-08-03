// Tests for the notification channel rules in 12-templates-notify.js. The kind list and the
// URL-kind predicate must mirror the server's ValidNotifyKind and ValidateNotifyTarget rules,
// or the dialog builds targets the API rejects.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts } from "./loader.mjs";

const app = loadParts(["01-boot.js", "12-templates-notify.js"]);

// REQUIRED_FIELDS is the server's per-kind deliverability rule: which fields each channel must
// carry. The UI predicate under test must agree with the url column of this table.
const REQUIRED_FIELDS = {
	webhook: ["url"],
	slack: ["url"],
	mattermost: ["url"],
	rocketchat: ["url"],
	discord: ["url"],
	teams: ["url"],
	ntfy: ["url"],
	pagerduty: ["key"],
	grafana: ["url", "key"],
	twilio: ["to"],
	email: ["to"],
};

test("NOTIFY_KINDS matches the server's supported channels", () => {
	assert.deepEqual(JSON.parse(JSON.stringify(app.NOTIFY_KINDS)), [
		"webhook", "slack", "mattermost", "rocketchat", "discord", "teams", "ntfy",
		"pagerduty", "grafana", "twilio", "email",
	]);
	assert.deepEqual([...app.NOTIFY_KINDS].sort(), Object.keys(REQUIRED_FIELDS).sort());
});

test("notifyNeedsURL shows a URL field exactly where the server requires one", () => {
	for (const kind of app.NOTIFY_KINDS) {
		assert.equal(
			app.notifyNeedsURL(kind), REQUIRED_FIELDS[kind].includes("url"),
			kind + " URL requirement",
		);
	}
});

test("notifyNeedsURL rejects unknown kinds", () => {
	assert.equal(app.notifyNeedsURL(""), false);
	assert.equal(app.notifyNeedsURL("carrier-pigeon"), false);
	assert.equal(app.notifyNeedsURL(undefined), false);
});
