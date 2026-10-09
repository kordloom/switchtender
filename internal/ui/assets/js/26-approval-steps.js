// approvalStepsRunID is the workflow the approval panel is scoped to, empty for every workflow, so a
// decision reloads the same view it was made from.
let approvalStepsRunID = "";

// APPROVALS_REFRESH_MS is how often a page that lists waiting approval steps asks again, so a step
// that arrives while an approver has the page open shows up without a reload.
const APPROVALS_REFRESH_MS = 15000;

// loadApprovalSteps fills the approval panel with the workflow approval steps waiting for a decision,
// only those of one workflow when runID is set. A workflow paused at a step is still running while
// another of its branches is, so the runs list held for approval cannot show it, and this panel is
// where an approver finds it. A failure to read the queue leaves the panel hidden rather than
// putting an error over the page it sits on. A quiet call is a poll: when it finds the queue it
// already drew, it leaves the panel alone, so a reader's focus on Approve is not thrown away on every
// refresh. Every other call redraws, since a decision may have to put its buttons back.
async function loadApprovalSteps(runID, quiet) {
	approvalStepsRunID = runID || "";
	const host = document.getElementById("approval-steps");
	if (!host) return;
	let data;
	try {
		data = await getJSON("/approvals");
	} catch {
		host.hidden = true;
		delete host.dataset.queue;
		return;
	}
	const steps = (data.approvals || []).filter((a) => !approvalStepsRunID || a.run_id === approvalStepsRunID);
	const queue = JSON.stringify(steps);
	if (quiet && host.dataset.queue === queue) return;
	host.dataset.queue = queue;
	renderApprovalSteps(host, steps);
}

// wireApprovalsAutoRefresh keeps the approval panel current while its page is open and visible. The
// panel used to be read once when the page loaded, so a workflow that reached its approval step
// after that never offered Approve until the approver reloaded, and an approver who left the runs
// page open to wait for one waited for nothing.
function wireApprovalsAutoRefresh(runID) {
	window.setInterval(() => {
		if (document.hidden) return;
		loadApprovalSteps(runID, true);
	}, APPROVALS_REFRESH_MS);
}

// renderApprovalSteps draws one entry per waiting step: what the workflow is, what already ran, what
// an approval runs, what a denial runs, and when the step gives up, with Approve and Deny for a
// reader who may decide it. The launcher of a workflow whose rule requires a second person is not
// offered Approve, since the server refuses it, and Deny stays, since withdrawing a request is theirs.
function renderApprovalSteps(host, steps) {
	host.textContent = "";
	host.hidden = steps.length === 0;
	if (!steps.length) return;
	const head = document.createElement("div");
	head.className = "panel-head";
	const title = document.createElement("h2");
	title.textContent = steps.length === 1
		? "1 approval step is waiting for a decision"
		: steps.length + " approval steps are waiting for a decision";
	head.appendChild(title);
	host.appendChild(head);
	for (const step of steps) host.appendChild(approvalStepEntry(step));
}

// approvalStepEntry builds one waiting step's entry.
function approvalStepEntry(step) {
	const entry = document.createElement("div");
	entry.className = "approval-step";
	entry.dataset.stepId = step.id;

	const head = document.createElement("div");
	head.className = "approval-step-head";
	const name = document.createElement("strong");
	if (approvalStepsRunID) {
		name.textContent = step.step;
	} else {
		const link = document.createElement("a");
		link.href = "/ui/runs/" + encodeURIComponent(step.run_id);
		link.textContent = (step.workflow || step.run_id) + ", " + step.step;
		name.appendChild(link);
	}
	head.appendChild(name);
	const who = document.createElement("span");
	who.className = "muted";
	who.textContent = step.requested_by ? "Launched by " + step.requested_by : "";
	head.appendChild(who);
	entry.appendChild(head);

	if (step.description) {
		const what = document.createElement("p");
		what.className = "approval-step-paths";
		what.textContent = step.description;
		entry.appendChild(what);
	}
	const paths = document.createElement("p");
	paths.className = "approval-step-paths muted";
	paths.textContent = approvalStepPaths(step);
	entry.appendChild(paths);

	const admin = roleAtLeast("admin");
	const ownRequest = !!step.require_distinct_approver && signedInAs(step.requested_by);
	if (admin) {
		const actions = document.createElement("div");
		actions.className = "approval-step-actions";
		const approve = document.createElement("button");
		approve.type = "button";
		approve.className = "button primary";
		approve.textContent = "Approve";
		approve.hidden = ownRequest;
		const deny = document.createElement("button");
		deny.type = "button";
		deny.className = "button danger";
		deny.textContent = "Deny";
		actions.appendChild(approve);
		actions.appendChild(deny);
		entry.appendChild(actions);
		// A read-only server refuses every decision, so a live control would only open the reason
		// dialog and then report the refusal as a failure. The controls are drawn disabled and say
		// why, the way the launch dialog's Launch is, and no click handler is attached.
		if (isReadOnly()) {
			for (const btn of [approve, deny]) {
				btn.disabled = true;
				btn.title = readOnlyReason() + ".";
			}
			const why = document.createElement("p");
			why.className = "muted";
			why.textContent = readOnlyReason() + ". An admin decides this step here on a writable install.";
			entry.appendChild(why);
			return entry;
		}
		approve.addEventListener("click", () => decideApprovalStep(step, true, approve));
		deny.addEventListener("click", () => decideApprovalStep(step, false, deny));
		if (ownRequest) {
			const why = document.createElement("p");
			why.className = "muted";
			why.textContent = "You launched this workflow, and the rule in force requires a different " +
				"person to approve its steps. You can still deny it to withdraw the request.";
			entry.appendChild(why);
		}
	}
	return entry;
}

// approvalStepPaths says what already ran and what each answer runs, in one sentence an approver
// reads before deciding. A step with no deny path says a denial fails the workflow, which is what it
// does, rather than leaving the reader to guess.
function approvalStepPaths(step) {
	const parts = [];
	const ran = (step.upstream || []).map((u) => u.name + " " + u.status);
	if (ran.length) parts.push("Already ran: " + ran.join(", ") + ".");
	const next = step.on_approve || [];
	parts.push(next.length ? "Approving runs " + next.join(", ") + "." : "Approving runs nothing further.");
	const deny = step.on_deny || [];
	parts.push(deny.length
		? "Denying runs " + deny.join(", ") + "."
		: "Denying fails the workflow and runs nothing further.");
	if (step.expires_at) parts.push("Times out " + relTime(step.expires_at) + ", then takes the deny path.");
	return parts.join(" ");
}

// decideApprovalStep posts a decision on one step, bound to the state digest the panel showed, so a
// workflow that moved since the panel was drawn is refused rather than approved unseen. Both answers
// ask for the decider's reason, which the server masks and records as audit evidence, and which the
// rule in force may require.
async function decideApprovalStep(step, approve, btn) {
	btn.disabled = true;
	try {
		const done = await decideWithReason(
			"/runs/" + encodeURIComponent(step.id) + (approve ? "/approve" : "/reject"),
			{ state_digest: step.state_digest }, {
				title: (approve ? "Approve " : "Deny ") + step.step,
				action: approve ? "Approve" : "Deny",
				required: reasonRequired(step.require_reason, approve),
				requiredBy: "the rule in force for this workflow",
			});
		if (done === null) {
			btn.disabled = false;
			return;
		}
		setStatus((approve ? "Approved " : "Denied ") + step.step + ".");
		await loadApprovalSteps(approvalStepsRunID);
	} catch (e) {
		setStatus((approve ? "Approve" : "Deny") + " failed: " + e.message);
		btn.disabled = false;
	}
}
