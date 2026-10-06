package dispatch

import (
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

var (
	// ErrNoPlaybook is returned when an Ansible run is submitted without a playbook path.
	ErrNoPlaybook = errors.New("no playbook")
	// ErrNoCommand is returned when a bash, terraform, or python run is submitted without a command.
	ErrNoCommand = errors.New("no command")
	// ErrUnknownTool is returned when a run names an execution tool the dispatcher does not support.
	ErrUnknownTool = errors.New("unknown execution tool")
	// ErrToolCredential is returned when a run attaches a credential whose kind only takes effect
	// under Ansible to a non-Ansible tool, so the mismatch fails at submit instead of the credential
	// being silently ignored at execution.
	ErrToolCredential = errors.New("credential kind does not apply to this tool")
	// ErrSecretAnswer is returned when a run carries secret survey answers this executor cannot
	// open: they never reached it, which is the case on a relay worker, or it holds no key.
	ErrSecretAnswer = errors.New("secret survey answer unavailable")
	// ErrNotDelivered is returned on a relay worker when a run needs a secret the control node did not
	// deliver with its claim, or delivered for another execution, so the run fails rather than
	// executing without it.
	ErrNotDelivered = errors.New("secret not delivered to this worker")
	// ErrInventorySnapshot is returned when a run cannot execute the inventory snapshot it was
	// submitted with: it carries none, the stored snapshot changed since it was bound, or it does not
	// open. The run is refused rather than executed against whatever the store holds now.
	ErrInventorySnapshot = errors.New("inventory snapshot refused")
	// ErrPlanFile is returned when a gated apply cannot carry out the plan file its approval bound:
	// the file is missing, changed since it was bound, or does not open. The apply is refused rather
	// than planned again.
	ErrPlanFile = errors.New("plan file refused")
	// ErrImagePin is returned when a run's container image does not match what was pinned when it was
	// submitted, or when an image could not be pinned to the run at all.
	ErrImagePin = errors.New("image pin refused")
	// ErrNoHostLister is returned when a split is requested but the runner cannot list hosts.
	ErrNoHostLister = errors.New("host listing unavailable")
	// ErrNoSteps aliases the run-package error so callers matching dispatch.ErrNoSteps still match
	// what run.ValidatePipeline returns. The pipeline validators moved to the run package, which owns
	// the step type, so the dispatcher and the template layer validate through one definition.
	ErrNoSteps = run.ErrNoSteps
	// ErrTooManySteps and ErrStepInput alias the run-package pipeline errors for the same reason.
	ErrTooManySteps = run.ErrTooManySteps
	ErrStepInput    = run.ErrStepInput
	// ErrNotSplit is returned when a shard retry targets a run that is not a split parent.
	ErrNotSplit = errors.New("not a split run")
	// ErrNotFinished is returned when a shard retry targets a run that has not finished.
	ErrNotFinished = errors.New("run not finished")
	// ErrNoFailedShards is returned when a shard retry finds nothing to retry.
	ErrNoFailedShards = errors.New("no failed shards")
	// ErrIncompleteSplit is returned when a shard retry targets a split that stored fewer shards than
	// it counts, so no retry can tell which hosts never had one.
	ErrIncompleteSplit = errors.New("split is missing shards")
	// ErrNoFailedHosts is returned when a failed-host relaunch finds no host that failed.
	ErrNoFailedHosts = errors.New("no failed hosts")
	// ErrNoHostSummary is returned when a relaunch targets a run that recorded no per-host results,
	// such as a non-Ansible run.
	ErrNoHostSummary = errors.New("run has no per-host results")
	// ErrUnnamedStep, ErrDuplicateStep, ErrUnknownDependency, and ErrDependencyCycle alias the
	// run-package pipeline graph errors, so existing dispatch.Err* matches keep working.
	ErrUnnamedStep       = run.ErrUnnamedStep
	ErrDuplicateStep     = run.ErrDuplicateStep
	ErrUnknownDependency = run.ErrUnknownDependency
	ErrDependencyCycle   = run.ErrDependencyCycle
	// ErrNotPendingApproval is returned when approve or reject targets a run not awaiting approval.
	ErrNotPendingApproval = errors.New("run is not awaiting approval")
)

// ErrPolicyDenied is returned when a deny policy refuses a submission outright, so the run is
// never created. The refused request is still evidence: the gate records every mutation on the
// audit chain before its handler acts.
var ErrPolicyDenied = errors.New("submission denied by policy")

// ErrAgentWorkflowApply is returned, wrapped in ErrPolicyDenied, when an agent's workflow carries a
// Terraform or OpenTofu apply step no exemption covers. A workflow's approval does not show its
// steps' plans, so the apply would run a plan nobody approved.
var ErrAgentWorkflowApply = errors.New(policy.AgentWorkflowApplyName)

// ErrPolicyUnavailable is returned when the approval policies cannot be read, so the dispatcher
// cannot tell whether a run needs sign-off. The submission is refused rather than run: a gate that
// cannot be evaluated has not been passed.
var ErrPolicyUnavailable = errors.New("approval policies unavailable")

// ErrCommitMoved is returned when a run pinned to a commit finds the project on a different one. It
// is the plan gate's guarantee that the code an approver read is the code that runs.
var ErrCommitMoved = errors.New("the project moved to a different commit since this run was approved")

// ErrSelfApproval is returned when the person who asked for a run tries to approve it and the rule
// that held it requires a different approver. It is separation of duties: a gate the requester can
// release themselves records a signature but stops nothing.
var ErrSelfApproval = errors.New("the requester cannot approve their own run")

// ErrNotApprovalStep is returned when a step decision names a run that is not a workflow approval
// step.
var ErrNotApprovalStep = errors.New("not a workflow approval step")

// ErrAgentApproval is returned when an AI agent tries to approve a held run or a workflow approval
// step. An agent may propose work and see that it waits, never release it.
var ErrAgentApproval = errors.New("an agent cannot approve")

// ErrReasonRequired is returned when a decision carries no reason and the rule that held the run
// requires one for that decision.
var ErrReasonRequired = errors.New("a reason is required for this decision")

// ErrReasonTooLong is returned when an approver's reason or correction is over the length cap.
var ErrReasonTooLong = errors.New("the reason is too long")

// ErrDecisionNotFound is returned when a correction or a redaction names a decision record the run
// does not have.
var ErrDecisionNotFound = errors.New("no such decision on this run")

// ErrNoReasonText is returned when a correction carries no text.
var ErrNoReasonText = errors.New("a correction needs text")

// ErrRedactionCategory is returned when a redaction names no category this product recognizes.
var ErrRedactionCategory = errors.New("unknown redaction category")

// ErrStateMoved is returned when an approver decides on an approval step whose workflow no longer
// reduces to the state they were shown, so their decision would bind to something they never saw.
var ErrStateMoved = errors.New("the workflow changed since this approval step was shown")

// ErrStepPending is returned when a whole-run decision targets a workflow that already started and
// is paused at an approval step. Approving it as a run would walk the graph from the top and repeat
// every step that already changed something, so the step is decided instead.
var ErrStepPending = errors.New("this workflow is paused at an approval step: decide the step")

// ErrChildNotApprovable is returned when a shard or pipeline step is approved on its own. The
// parent carries the decision; a child released by itself would run outside it.
var ErrChildNotApprovable = errors.New("a shard or step is approved through its parent")

// errLeaseLost is the cause carried when an executor's own heartbeats discover its lease is gone:
// the run was requeued or settled elsewhere, and this process's job is to stop its tool and stand
// down. It maps to interrupted, never canceled, because nobody decided anything.
var errLeaseLost = errors.New("this executor lost its lease while the run executed")

// ErrQueueUnlicensed is returned when a run would be pinned to a named queue on an install that
// cannot run a worker to serve it. Nothing can ever claim such a run, so it is refused at submit
// rather than accepted and left pending forever with no error anywhere to explain it.
var ErrQueueUnlicensed = errors.New("named queues need a license this install does not have")

// ErrToolImage is returned when a run names an execution image for a tool that cannot run inside
// one. A tool registered through the SDK or a plugin executes on the host, so an image on such a run
// would be recorded as an environment the run never entered.
var ErrToolImage = errors.New("this tool cannot execute in an image")

// errDecisionFound stops the audit chain scan once the approval decision has been read.
var errDecisionFound = errors.New("decision found")

// errFederatedCredential is openCredential's refusal of a federated credential, which has no stored
// value to open. Run materialization recognizes it and mints the run's token instead.
var errFederatedCredential = fmt.Errorf("%w: a federated credential has no stored secret",
	credential.ErrBadKind)

// ErrBadGitRef is returned when a run names a git ref to fetch its commit from that cannot be
// honored: one with no project, no pinned commit, or a malformed reference name.
var ErrBadGitRef = errors.New("invalid git ref for this run")
