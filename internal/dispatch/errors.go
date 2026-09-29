package dispatch

import (
	"errors"

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

// ErrPolicyUnavailable is returned when the approval policies cannot be read, so the dispatcher
// cannot tell whether a run needs sign-off. The submission is refused rather than run: a gate that
// cannot be evaluated has not been passed.
var ErrPolicyUnavailable = errors.New("approval policies unavailable")

// ErrChildNotApprovable is returned when a shard or pipeline step is approved on its own. The
// parent carries the decision; a child released by itself would run outside it.
var ErrChildNotApprovable = errors.New("a shard or step is approved through its parent")
