// Tests for the change register page, /ui/audit/register. The page draws the register the server
// builds as JSON, the same rows the downloaded document carries, through the session's own
// credential. What can go wrong is in how it is drawn and refused: a row in the wrong order or
// without its chain entry, a role refusal dressed up as a licensing offer, a viewer's browser asking
// a server that will only say no, and a dead session left on a page that never loads.
import { test } from "node:test";
import assert from "node:assert/strict";

import { fire } from "./dom.mjs";
import { sandboxOf } from "./loader.mjs";
import { reply } from "./net.mjs";
import { loadPage } from "./pages.mjs";

// REGISTER_JSON matches the page's read of the register, and REGISTER_DOC the document download.
const REGISTER_JSON = (req) => req.path === "/v1/audit/register" && req.params.get("format") === "json";
const REGISTER_DOC = (req) => req.path === "/v1/audit/register" && !req.params.has("format");

// LICENSING is the server's refusal on a Community install, word for word.
const LICENSING = "A Team license is required for the period change register; this install runs " +
	"Community. https://switchtender.com/pricing";

// row builds one register row the way the server encodes it, with defaults for what a test does
// not care about.
function row(over) {
	return Object.assign({
		when: "2026-08-03 12:00", run: "run_x", change: "ansible site.yml", actor: "alice",
		source: "template tpl_web", risk: "low", held: "", decision: "", decision_seq: 0,
		outcome: "succeeded", dry_run: false, not_change_free: false,
	}, over);
}

// register builds a register body the way the server encodes it.
function register(rows, over) {
	return Object.assign({
		status: "verified", status_text: "The chain verifies and carries 1 anchor fixing it outside this install.",
		from: "2026-08-03", to: "2026-08-04", rows, total: rows.length, approved: 0, rejected: 0,
		failed: 0, truncated: false, truncated_text: "", limit: 5000, covered_to: "", pruned: 0,
		pruned_text: "", chain_count: 9, receipt: "9:abc", anchor_problems: null,
		generated_at: "2026-08-04T00:00:00Z",
	}, over);
}

// THREE is a register of three rows that differ in every way a row can: an approval, a rejection on
// behalf of an account, and a dry run with no actor that was not change free.
const THREE = [
	row({ run: "run_a", decision: "Approved by bob", decision_seq: 4 }),
	row({ run: "run_b", when: "2026-08-03 12:01", decision: "Rejected by carol on behalf of ops",
		decision_seq: 7, outcome: "rejected", risk: "high", held: "prod needs a person" }),
	row({ run: "run_c", when: "2026-08-03 12:02", actor: "", change: "plugin-tool plan",
		dry_run: true, not_change_free: true, outcome: "failed" }),
];

// openRegister mounts the register page as a browser opens it, with the stored role given, and
// answers the register read with response. It returns the page once the boot has settled.
async function openRegister(response, role) {
	const page = loadPage("auditregister", {
		path: "/ui/audit/register",
		routes: [[REGISTER_JSON, response], [REGISTER_DOC, reply({})]],
		quiet: true,
	});
	if (role) sandboxOf(page.app).localStorage.setItem("st_role", role);
	fire(page.document, "DOMContentLoaded");
	await page.clock.flush();
	return page;
}

// cells returns the text of every cell in the register table, row by row, in the order shown.
function cells(document) {
	return [...document.getElementById("register-rows").querySelectorAll("tr")]
		.map((tr) => [...tr.querySelectorAll("td")].map((td) => td.textContent.trim()));
}

// shown reports whether an element is in the page and not inside anything hidden.
function shown(el) {
	return Boolean(el && el.isConnected && !el.closest("[hidden]"));
}

test("rows render in the register's order, each decision with its chain entry", async () => {
	const page = await openRegister(reply(register(THREE, { approved: 1, rejected: 1, failed: 1 })));
	const doc = page.document;
	assert.equal(doc.body.dataset.page, "register");
	assert.ok(shown(doc.getElementById("register-table")), "the table is not shown");
	assert.deepEqual(cells(doc), [
		["2026-08-03 12:00", "run_a", "ansible site.yml", "alice", "template tpl_web", "low",
			"—", "Approved by bob #4", "succeeded"],
		["2026-08-03 12:01", "run_b", "ansible site.yml", "alice", "template tpl_web", "high",
			"prod needs a person", "Rejected by carol on behalf of ops #7", "rejected"],
		["2026-08-03 12:02", "run_c", "plugin-tool plan dry run not change free", "none recorded",
			"template tpl_web", "low", "—", "—", "failed"],
	]);
	// Each run opens on its own page, the run's own evidence one click further.
	const link = doc.getElementById("register-rows").querySelector("a");
	assert.equal(link.getAttribute("href"), "/ui/runs/run_a");
	assert.equal(doc.getElementById("status").hidden, true, "the loading line was left up");
	assert.equal(page.net.calls.filter(REGISTER_JSON).length, 1);
});

test("the head carries the period, the chain's verdict, and the document's four tallies", async () => {
	const page = await openRegister(reply(register(THREE, { approved: 1, rejected: 1, failed: 1 })));
	const doc = page.document;
	assert.match(doc.getElementById("register-period").textContent, /2026-08-03 to 2026-08-04/);
	const chain = doc.getElementById("register-chain");
	assert.ok(shown(chain), "the chain's verdict is not shown");
	assert.match(chain.textContent, /^Verified The chain verifies/);
	const labels = [...doc.querySelectorAll("#register-summary .stat-label")].map((l) => l.textContent);
	assert.deepEqual(labels, ["Changes", "Approved", "Rejected", "Failed"]);
	assert.equal(doc.getElementById("register-truncated").hidden, true);
	assert.equal(doc.getElementById("register-pruned").hidden, true);
});

test("a truncated or pruned register shows the document's own sentence and says Changes shown", async () => {
	const truncated = "This register is truncated and is not the whole period. A register carries at " +
		"most 5000 changes; this one carries the earliest 3 and stops at 2026-08-03T12:03:00Z.";
	const pruned = "This register is incomplete. The chain records 2 further changes in this period.";
	const page = await openRegister(reply(register(THREE, {
		truncated: true, truncated_text: truncated, pruned: 2, pruned_text: pruned,
	})));
	const doc = page.document;
	assert.equal(doc.getElementById("register-truncated").textContent, truncated);
	assert.ok(shown(doc.getElementById("register-truncated")));
	assert.equal(doc.getElementById("register-pruned").textContent, pruned);
	assert.equal(doc.querySelector("#register-summary .stat-label").textContent, "Changes shown");
});

test("a period with nothing in it says so, and a pruned one still shows why", async () => {
	const pruned = "This register is incomplete. The chain records 4 further changes in this period.";
	const page = await openRegister(reply(register([], { pruned: 4, pruned_text: pruned })));
	const doc = page.document;
	assert.equal(doc.getElementById("register-table").hidden, true, "an empty table was left up");
	assert.match(doc.getElementById("status").textContent, /lists no changes in this period/);
	assert.ok(shown(doc.getElementById("register-pruned")),
		"an emptied period read as a quiet one, with nothing saying retention removed its changes");
});

test("a licensing refusal with the pricing link becomes the Team offer", async () => {
	const page = await openRegister(reply({ error: LICENSING }, { status: 403 }));
	const doc = page.document;
	const status = doc.getElementById("status");
	assert.match(status.textContent, /A Team license is required/);
	const link = status.querySelector("a");
	assert.ok(link, "the offer carries no link");
	assert.equal(link.textContent, "See what Team includes");
	assert.equal(link.getAttribute("href"), "https://switchtender.com/pricing");
	assert.equal(doc.getElementById("register-table").hidden, true, "the table is shown over a refusal");
	assert.equal(doc.getElementById("register-download").hidden, true,
		"a download the server will refuse the same way is still offered");
});

test("a bare forbidden is a role refusal and is never shown as an offer", async () => {
	const page = await openRegister(reply({ error: "forbidden" }, { status: 403 }));
	const status = page.document.getElementById("status");
	assert.doesNotMatch(status.textContent, /See what Team includes/);
	assert.equal(status.querySelector("a"), null, "a role refusal carries a link");
	assert.match(status.textContent, /readable by admins/);
});

test("a viewer or an operator is told the register is for admins and nothing is asked", async () => {
	for (const role of ["viewer", "operator"]) {
		const page = await openRegister(reply(register(THREE)), role);
		assert.equal(page.net.calls.filter((c) => c.url.includes("/audit/register")).length, 0,
			role + ": the page asked a server that refuses this session");
		assert.match(page.document.getElementById("status").textContent, /readable by admins/, role);
		assert.equal(page.document.getElementById("register-download").hidden, true,
			role + ": the download is offered to a session the server refuses");
	}
});

test("a dead session is sent to sign in", async () => {
	const page = await openRegister(reply({ error: "unauthorized" }, { status: 401 }));
	assert.ok(page.app.location.navigations.includes("/ui/login"),
		"a 401 left the reader on a page that will never load: " +
		JSON.stringify(page.app.location.navigations));
});

test("Download document saves the document the Evidence pack saves", async () => {
	const page = await openRegister(reply(register(THREE)));
	const downloads = [];
	sandboxOf(page.app).downloadBlob = (name, type) => downloads.push({ name, type });
	page.net.only([[REGISTER_DOC, reply({})]]);
	fire(page.document.getElementById("register-download"), "click");
	await page.clock.flush();
	assert.equal(page.net.calls.filter(REGISTER_DOC).length, 1, "the button did not ask for the document");
	assert.deepEqual(downloads, [{ name: "switchtender-change-register.html", type: "text/html" }]);
});

test("the page's export carries the register's rows, whatever the pager shows", async () => {
	const many = [];
	for (let i = 0; i < 30; i++) {
		many.push(row({ run: "run_" + String(i).padStart(2, "0"), when: "2026-08-03 12:" + String(i).padStart(2, "0") }));
	}
	const page = await openRegister(reply(register(many)));
	const downloads = [];
	sandboxOf(page.app).downloadBlob = (name, type, content) => downloads.push({ name, content });
	const csv = [...page.document.querySelectorAll("button.table-export")]
		.find((b) => b.textContent.trim() === "CSV");
	assert.ok(csv, "the register page has no CSV export");
	fire(csv, "click");
	await page.clock.flush();
	const lines = downloads[0].content.trim().split("\n");
	assert.equal(lines[0], "When (UTC),Run,Change,Actor,Source,Risk,Held by,Decision,Outcome");
	assert.equal(lines.length, 31, "the export stopped at the pager instead of carrying every row");
	assert.match(downloads[0].name, /^switchtender-register-/);
});

test("the Audit page links to the register beside Evidence pack, and only for admins", async () => {
	const page = loadPage("audit", { routes: [[() => true, reply({ entries: [], count: 0 })]], quiet: true });
	fire(page.document, "DOMContentLoaded");
	await page.clock.flush();
	const link = page.document.getElementById("audit-register-view");
	assert.equal(link.getAttribute("href"), "/ui/audit/register");
	assert.ok(shown(link), "the register link is not shown to an admin");
	assert.ok(shown(page.document.getElementById("audit-register")), "Evidence pack was removed");
	assert.match(link.textContent, /Team/, "the link is not marked as Team the way Evidence pack is");

	const operator = loadPage("audit", { quiet: true });
	sandboxOf(operator.app).localStorage.setItem("st_role", "operator");
	fire(operator.document, "DOMContentLoaded");
	await operator.clock.flush();
	assert.equal(operator.document.getElementById("audit-register-view").hidden, true,
		"the register link is offered to a session the server refuses");
});
