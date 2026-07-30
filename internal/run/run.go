// Package run holds the SwitchTender run domain model and its persistence interface.
// A run records one execution of an engine (playbook) against a manifest (inventory).
package run

import (
	"maps"
	"sort"
	"time"

	"github.com/kordloom/switchtender/internal/idgen"
)

// Status is the lifecycle state of a run.
type Status string

const (
	// StatusPending means the run is accepted but not yet started.
	StatusPending Status = "pending"
	// StatusRunning means the run is executing.
	StatusRunning Status = "running"
	// StatusSucceeded means the run finished with a zero exit code.
	StatusSucceeded Status = "succeeded"
	// StatusFailed means the run finished with a non-zero exit code or could not start.
	StatusFailed Status = "failed"
	// StatusCanceled means the run was stopped before completion.
	StatusCanceled Status = "canceled"
	// StatusInterrupted means the run was abandoned when the server stopped and cannot resume.
	StatusInterrupted Status = "interrupted"
	// StatusPendingApproval means the run is held and cannot be claimed until an approver releases it.
	StatusPendingApproval Status = "pending_approval"
	// StatusRejected means an approver denied the run, so it never executed.
	StatusRejected Status = "rejected"
)

const (
	// KindSplit marks a parent run whose children are inventory shards.
	KindSplit = "split"
	// KindPipeline marks a parent run whose children are pipeline steps.
	KindPipeline = "pipeline"
)

const (
	// ToolAnsible runs an Ansible playbook against an inventory. It is the default when Tool is empty.
	ToolAnsible = "ansible"
	// ToolBash runs a shell script with bash. The script text is carried in Command.
	ToolBash = "bash"
	// ToolTerraform runs Terraform in a working directory. The directory is carried in Command.
	ToolTerraform = "terraform"
	// ToolOpenTofu runs OpenTofu in a working directory, exactly like the Terraform tool but with
	// the tofu binary. The directory is carried in Command.
	ToolOpenTofu = "opentofu"
	// ToolPython runs a Python script. The script text is carried in Command.
	ToolPython = "python"
	// ToolPowerShell runs a PowerShell script with pwsh. The script text is carried in Command.
	ToolPowerShell = "powershell"
	// ToolGo runs a Go program. The source text is carried in Command.
	ToolGo = "go"
)

// NormalizeTool maps an empty tool to the Ansible default and otherwise returns tool unchanged.
func NormalizeTool(tool string) string {
	if tool == "" {
		return ToolAnsible
	}
	return tool
}

// builtinTool reports whether tool is one of the compiled-in execution tools.
func builtinTool(tool string) bool {
	switch NormalizeTool(tool) {
	case ToolAnsible, ToolBash, ToolTerraform, ToolOpenTofu, ToolPython, ToolPowerShell, ToolGo:
		return true
	default:
		return false
	}
}

// extraTools holds tool names added by an extension so ValidTool accepts them alongside the
// built-ins. Registration happens at startup, before runs are accepted, so reads during serving
// need no lock, matching how secretsource registers a new engine. A registered tool takes its
// input from Command, like every non-Ansible built-in.
var extraTools = map[string]bool{}

// RegisterTool records an execution tool name added by an extension so ValidTool accepts it. Pair it
// with a runner in the execution layer. It panics on an empty, duplicate, or built-in name, which is
// a programming error caught at startup.
func RegisterTool(name string) {
	if name == "" {
		panic("run: cannot register an empty tool name")
	}
	if builtinTool(name) || extraTools[NormalizeTool(name)] {
		panic("run: duplicate tool " + name)
	}
	extraTools[NormalizeTool(name)] = true
}

// ValidTool reports whether tool names a supported execution tool, built-in or registered. Empty is
// valid and means Ansible.
func ValidTool(tool string) bool {
	return builtinTool(tool) || extraTools[NormalizeTool(tool)]
}

// ExtraToolNames returns the registered extension tool names, sorted, so the UI can offer them
// beside the built-ins. Registration is startup-only, so reads while serving need no lock.
func ExtraToolNames() []string {
	names := make([]string, 0, len(extraTools))
	for name := range extraTools {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// Terminal reports whether the status is a final state.
func (s Status) Terminal() bool {
	switch s {
	case StatusSucceeded, StatusFailed, StatusCanceled, StatusInterrupted, StatusRejected:
		return true
	default:
		return false
	}
}

// Run is a single execution of a playbook against an inventory.
type Run struct {
	// ID is the unique run identifier.
	ID string `json:"id"`
	// Playbook is the path to the Ansible playbook to execute. Used by the Ansible tool.
	Playbook string `json:"playbook"`
	// Inventory is the path to the Ansible inventory to target.
	Inventory string `json:"inventory"`
	// Tool selects the execution engine: ansible, bash, terraform, opentofu, python, powershell, or go. Empty means ansible.
	Tool string `json:"tool,omitempty"`
	// Command carries the tool's primary input for non-Ansible tools: the script for bash and python,
	// the working directory for terraform. Ignored by the Ansible tool.
	Command string `json:"command,omitempty"`
	// DryRun runs the tool in its no-change mode: ansible --check, terraform plan, a syntax check for
	// bash and python.
	DryRun bool `json:"dry_run,omitempty"`
	// Status is the current lifecycle state.
	Status Status `json:"status"`
	// ExitCode is the process exit code, set once the run reaches a terminal state.
	ExitCode *int `json:"exit_code,omitempty"`
	// Error holds a failure detail when the run could not start.
	Error string `json:"error,omitempty"`
	// CreatedAt is when the run was accepted.
	CreatedAt time.Time `json:"created_at"`
	// StartedAt is when execution began.
	StartedAt *time.Time `json:"started_at,omitempty"`
	// EndedAt is when execution finished.
	EndedAt *time.Time `json:"ended_at,omitempty"`
	// ParentID links a shard run to its parent split run.
	ParentID *string `json:"parent_id,omitempty"`
	// ShardIndex is this shard's position within the split, zero based.
	ShardIndex *int `json:"shard_index,omitempty"`
	// ShardCount is the total number of shards in the split.
	ShardCount *int `json:"shard_count,omitempty"`
	// Limit restricts execution to a host pattern, used to target a shard's hosts.
	Limit string `json:"limit,omitempty"`
	// Kind distinguishes a plain run from a split or pipeline parent. Empty means a plain run.
	Kind string `json:"kind,omitempty"`
	// RetryOf links a split created by a failed shard retry back to the run it retries.
	RetryOf *string `json:"retry_of,omitempty"`
	// Steps is the step graph when this run is a pipeline parent. It is stored so a pipeline held
	// for approval can be executed after a restart, and so a finished pipeline can still show the
	// shape it ran, including the dependencies that the child runs alone do not record.
	Steps []PipelineStep `json:"steps,omitempty"`
	// StepName is the step name when this run is a pipeline step.
	StepName string `json:"step_name,omitempty"`
	// StepIndex is the step order when this run is a pipeline step.
	StepIndex *int `json:"step_index,omitempty"`
	// Attempt is the retry attempt number for a pipeline step run, zero for the first try.
	Attempt int `json:"attempt,omitempty"`
	// ExtraVars are the variables injected into the run, for a pipeline step the merged outputs
	// of the steps it depends on.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// Outputs are the values the playbook published with set_stats for downstream steps.
	Outputs map[string]any `json:"outputs,omitempty"`
	// ClaimedBy names the process that leased this run for execution, empty while queued.
	ClaimedBy string `json:"claimed_by,omitempty"`
	// ClaimedAt is when the lease was taken or last renewed.
	ClaimedAt *time.Time `json:"claimed_at,omitempty"`
	// CancelRequested asks whichever process holds the run to stop it.
	CancelRequested bool `json:"cancel_requested,omitempty"`
	// CredentialIDs names the stored credentials materialized for this run.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// ProjectID names the git project the playbook and inventory paths resolve inside.
	ProjectID string `json:"project_id,omitempty"`
	// CommitSHA is the exact commit the run executed, stamped after the project sync.
	CommitSHA string `json:"commit_sha,omitempty"`
	// InventoryID names a stored inventory materialized for this run instead of a file path.
	InventoryID string `json:"inventory_id,omitempty"`
	// Queue restricts execution to workers serving this queue. Empty runs on the default pool.
	Queue string `json:"queue,omitempty"`
	// Image names a container image the run executes inside, its execution environment. It outranks
	// the project's image. Every built-in tool runs in a container; the runner builds a per-tool plan.
	Image string `json:"image,omitempty"`
	// PullCredentialID names a registry credential for pulling a private Image. Empty for public.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// ProposedFrom names the drift check run this reconcile proposal was built from. A run carrying
	// it was machine proposed and is born held for approval, so a person releases it or it never
	// executes.
	ProposedFrom string `json:"proposed_from,omitempty"`
	// Intent is the plain-language request an AI turned into this proposed run. A run carrying it
	// was proposed from a description and is born held for approval, so an approver can judge the
	// generated run against what was asked before anything executes.
	Intent string `json:"intent,omitempty"`
	// Source names what fired the run: manual, api, template, schedule, rerun, reconcile, or
	// propose. Empty on runs recorded before provenance stamping.
	Source string `json:"source,omitempty"`
	// SourceID is the object behind Source: the template or schedule id, or the origin run for a
	// rerun.
	SourceID string `json:"source_id,omitempty"`
	// Actor is the authenticated user who fired the run, when the server knows one.
	Actor string `json:"actor,omitempty"`
	// RerunOf is the finished run whose spec this run replayed.
	RerunOf string `json:"rerun_of,omitempty"`
	// Labels are user-supplied key values for slicing runs: env, ticket, team.
	Labels map[string]string `json:"labels,omitempty"`
	// IdempotencyKey dedupes a submission. A retried submit carrying the same key returns the
	// original run instead of creating a second, so a dropped response or a client retry cannot
	// double-fire a run. Empty means no dedup. It is set from the Idempotency-Key request header and
	// is a server-side control field, not part of the run's public representation.
	IdempotencyKey string `json:"-"`
	// Timeout bounds how many seconds the run may execute before it is canceled and finalized failed.
	// It overrides the dispatcher's default cap. Zero uses the default, which may itself be off.
	Timeout int `json:"timeout,omitempty"`
	// Risk grades the run's blast radius for an approver. It is computed on read, never stored, so
	// it is nil unless a handler filled it in.
	Risk *Risk `json:"risk,omitempty"`
	// Notifications are per-run notification targets, copied from the launching template, that
	// receive this run's terminal state in addition to the server-wide channels.
	Notifications []NotifyTarget `json:"notifications,omitempty"`
}

// NotifyTarget routes one run's terminal notification to a specific channel, so a template can page
// its own team instead of only the server-wide channels.
type NotifyTarget struct {
	// Kind selects the channel formatter: webhook, slack, mattermost, rocketchat, discord, teams,
	// or ntfy.
	Kind string `json:"kind"`
	// URL is the incoming webhook or topic URL the formatted message posts to.
	URL string `json:"url"`
	// OnFailure limits delivery to a failed or interrupted run when set, so a channel can page only
	// on trouble.
	OnFailure bool `json:"on_failure,omitempty"`
}

// Notification kinds a per-run target may name. These are the server channels a bare URL fully
// configures. PagerDuty, Grafana, Twilio, and email need server-held account credentials, so they
// stay server-wide and cannot be named per run.
const (
	NotifyWebhook    = "webhook"
	NotifySlack      = "slack"
	NotifyMattermost = "mattermost"
	NotifyRocketChat = "rocketchat"
	NotifyDiscord    = "discord"
	NotifyTeams      = "teams"
	NotifyNtfy       = "ntfy"
)

// ValidNotifyKind reports whether k names a supported per-run notification channel.
func ValidNotifyKind(k string) bool {
	switch k {
	case NotifyWebhook, NotifySlack, NotifyMattermost, NotifyRocketChat, NotifyDiscord,
		NotifyTeams, NotifyNtfy:
		return true
	default:
		return false
	}
}

// Clone returns a deep copy so callers cannot mutate stored state through shared pointers.
func (r *Run) Clone() *Run {
	if r == nil {
		return nil
	}
	out := *r
	if r.ExitCode != nil {
		code := *r.ExitCode
		out.ExitCode = &code
	}
	if r.StartedAt != nil {
		t := *r.StartedAt
		out.StartedAt = &t
	}
	if r.EndedAt != nil {
		t := *r.EndedAt
		out.EndedAt = &t
	}
	if r.ParentID != nil {
		id := *r.ParentID
		out.ParentID = &id
	}
	if r.ShardIndex != nil {
		i := *r.ShardIndex
		out.ShardIndex = &i
	}
	if r.ShardCount != nil {
		c := *r.ShardCount
		out.ShardCount = &c
	}
	if r.StepIndex != nil {
		i := *r.StepIndex
		out.StepIndex = &i
	}
	if r.RetryOf != nil {
		id := *r.RetryOf
		out.RetryOf = &id
	}
	out.ExtraVars = maps.Clone(r.ExtraVars)
	out.Outputs = maps.Clone(r.Outputs)
	if r.ClaimedAt != nil {
		t := *r.ClaimedAt
		out.ClaimedAt = &t
	}
	out.CredentialIDs = append([]string(nil), r.CredentialIDs...)
	out.Notifications = append([]NotifyTarget(nil), r.Notifications...)
	out.Labels = maps.Clone(r.Labels)
	if r.Steps != nil {
		out.Steps = make([]PipelineStep, len(r.Steps))
		for i, s := range r.Steps {
			s.DependsOn = append([]string(nil), s.DependsOn...)
			out.Steps[i] = s
		}
	}
	return &out
}

// SubmitOption customizes a run at submission.
type SubmitOption func(*Run)

// WithCredentialIDs attaches stored credentials to the run.
func WithCredentialIDs(ids []string) SubmitOption {
	return func(r *Run) { r.CredentialIDs = append([]string(nil), ids...) }
}

// WithProject sources the run's playbook and inventory paths from a git project.
func WithProject(id string) SubmitOption {
	return func(r *Run) { r.ProjectID = id }
}

// WithInventory targets a stored inventory instead of a file path.
func WithInventory(id string) SubmitOption {
	return func(r *Run) { r.InventoryID = id }
}

// WithQueue restricts the run to workers serving the named queue.
func WithQueue(queue string) SubmitOption {
	return func(r *Run) { r.Queue = queue }
}

// WithImage runs the playbook inside the named container image, pulled with the optional registry
// credential. It outranks the project's image.
func WithImage(image, pullCredentialID string) SubmitOption {
	return func(r *Run) {
		r.Image = image
		r.PullCredentialID = pullCredentialID
	}
}

// WithExtraVars injects variables into the run.
func WithExtraVars(vars map[string]any) SubmitOption {
	return func(r *Run) {
		if len(vars) == 0 {
			return
		}
		r.ExtraVars = maps.Clone(vars)
	}
}

// WithTool selects the execution tool for the run. An empty tool leaves the Ansible default.
func WithTool(tool string) SubmitOption {
	return func(r *Run) { r.Tool = tool }
}

// WithCommand sets the tool's primary input: the script for bash and python, the working directory
// for terraform.
func WithCommand(command string) SubmitOption {
	return func(r *Run) { r.Command = command }
}

// WithDryRun runs the tool in its no-change mode.
func WithDryRun(dryRun bool) SubmitOption {
	return func(r *Run) { r.DryRun = dryRun }
}

// WithRequireApproval holds the run for approval before it can be claimed, when require is true. The
// run starts at StatusPendingApproval, which the claim loop never selects, until an approver releases
// it to pending or rejects it.
func WithRequireApproval(require bool) SubmitOption {
	return func(r *Run) {
		if require {
			r.Status = StatusPendingApproval
		}
	}
}

// WithSource stamps what fired the run and which object did it, so lineage survives.
func WithSource(source, sourceID string) SubmitOption {
	return func(r *Run) {
		r.Source = source
		r.SourceID = sourceID
	}
}

// WithActor stamps the authenticated user who fired the run.
func WithActor(actor string) SubmitOption {
	return func(r *Run) { r.Actor = actor }
}

// WithRerunOf records the finished run whose spec this run replays.
func WithRerunOf(id string) SubmitOption {
	return func(r *Run) { r.RerunOf = id }
}

// WithLabels attaches user-supplied key values to the run.
func WithLabels(labels map[string]string) SubmitOption {
	return func(r *Run) {
		if len(labels) == 0 {
			return
		}
		r.Labels = maps.Clone(labels)
	}
}

// WithLimit restricts the run to a host pattern, so a reconcile can target exactly the hosts that
// drifted.
func WithLimit(pattern string) SubmitOption {
	return func(r *Run) {
		r.Limit = pattern
	}
}

// WithProposedFrom marks the run as a machine-built reconcile proposal and records the drift check
// run it was derived from.
func WithProposedFrom(checkRunID string) SubmitOption {
	return func(r *Run) {
		r.ProposedFrom = checkRunID
	}
}

// WithIntent records the plain-language request an AI turned into this proposed run, so an approver
// can judge the generated run against what was asked.
func WithIntent(intent string) SubmitOption {
	return func(r *Run) {
		r.Intent = intent
	}
}

// WithIdempotencyKey dedupes the submission under key so a retried submit returns the original run
// rather than firing a second. An empty key is a no-op and never dedupes.
func WithIdempotencyKey(key string) SubmitOption {
	return func(r *Run) {
		r.IdempotencyKey = key
	}
}

// WithTimeout caps the run at seconds of execution, overriding the dispatcher default. Zero or less
// leaves it on the default.
func WithTimeout(seconds int) SubmitOption {
	return func(r *Run) {
		if seconds > 0 {
			r.Timeout = seconds
		}
	}
}

// WithNotifications attaches per-run notification targets, which receive the run's terminal state
// alongside the server-wide channels. Empty is a no-op.
func WithNotifications(targets []NotifyTarget) SubmitOption {
	return func(r *Run) {
		if len(targets) > 0 {
			r.Notifications = append([]NotifyTarget(nil), targets...)
		}
	}
}

// ApplyOptions applies opts to r.
func ApplyOptions(r *Run, opts []SubmitOption) {
	for _, opt := range opts {
		opt(r)
	}
}

// NewID returns a random run identifier prefixed with "run_".
func NewID() string {
	return idgen.New("run_", 8)
}
