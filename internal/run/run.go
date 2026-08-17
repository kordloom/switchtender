// Package run holds the SwitchTender run domain model and its persistence interface.
// A run records one execution of an engine (playbook) against a manifest (inventory).
package run

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
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
	// Warning explains a degradation the run survived, such as event capture being unavailable, so
	// a run that finished green but has nothing to show says why instead of looking mysteriously
	// empty. It never changes the run's status.
	Warning string `json:"warning,omitempty"`
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
	// Tags runs only Ansible plays and tasks carrying one of these tags. Ignored by other tools.
	Tags []string `json:"tags,omitempty"`
	// SkipTags skips Ansible plays and tasks carrying one of these tags. Ignored by other tools.
	SkipTags []string `json:"skip_tags,omitempty"`
	// Verbosity raises Ansible logging from 0 to 4, for debugging a run without editing the playbook.
	Verbosity int `json:"verbosity,omitempty"`
	// Forks sets how many hosts Ansible addresses in parallel. Zero leaves Ansible's default.
	Forks int `json:"forks,omitempty"`
	// DiffMode shows the before-and-after of every Ansible file and template change.
	DiffMode bool `json:"diff_mode,omitempty"`
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
	// ClaimSecret is the per-claim capability minted when a worker leases the run. It authorizes the
	// reports that worker makes back over the relay, where every worker presents the same shared
	// token and the lease name alone is not proof. The json:"-" tag is load-bearing: it keeps the
	// secret out of the relay's own run reads, every bundle, every SIEM forward, and every evidence
	// document, so a worker cannot read it back the way it can read the lease name. A fresh claim
	// mints a new one and a reclaim clears it, so a report minted against a stale claim no longer
	// verifies. It is not derived from anything a worker supplies.
	ClaimSecret string `json:"-"`
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
	// OrgID is the owning organization stamped from the submitting actor at creation. It is what
	// scopes a run that references no stored object: an inline script, a proposed run, or a
	// terraform working directory names no project, inventory, or credential, so there is nothing
	// for the per-object grant check to filter on and the run would otherwise be readable, cancelable,
	// and approvable across every tenant. The run-scoped authorizer treats an objectless run as owned
	// by this org, so a caller outside it who holds no grant on any object the run references is
	// denied. Empty for a run created outside an actor's request, such as a seeded demo run.
	OrgID string `json:"org_id,omitempty"`
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
	// HeldByPolicy names the approval rule that held this run, as that rule was named when the hold
	// happened. It is recorded at the hold rather than looked up later, because a policy can be
	// renamed or deleted long before anyone reads the evidence, and "which rule stopped this
	// change" is the question a change-management review asks. Empty when nothing held the run.
	HeldByPolicy string `json:"held_by_policy,omitempty"`
	// AuditReceipt is the seq:link of the chain entry that recorded the request which created this
	// run. The entry is written before the handler runs, at a path naming the template or the
	// collection rather than the run it goes on to create, so this is the only thing that ties a
	// run to the record of who asked for it. Empty for a run created outside a recorded request,
	// such as a seeded demo run.
	AuditReceipt string `json:"audit_receipt,omitempty"`
	// ApprovedSpecDigest is the digest of the run's spec at the moment an approver decided on it,
	// stamped when the decision chain entry commits the same value. The executor recomputes the
	// spec digest before running and refuses a mismatch, so an approval releases exactly the change
	// that was decided on. Empty for a run that never needed a decision.
	ApprovedSpecDigest string `json:"approved_spec_digest,omitempty"`
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
	// ActorType is how the requesting actor authenticated, in the audit chain's vocabulary: agent
	// for an AI agent's token, session for a signed-in person, token for an owner-held API token,
	// cli for the command line, webhook for a trigger. Empty when the server does not know. It is
	// what lets a policy treat an agent's request differently from a person's.
	ActorType string `json:"actor_type,omitempty"`
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
	// ntfy, pagerduty, grafana, twilio, or email.
	Kind string `json:"kind"`
	// URL is the incoming webhook or topic URL a URL-configured channel posts to, or the annotation
	// endpoint for grafana. Unused by pagerduty, twilio, and email.
	URL string `json:"url,omitempty"`
	// Key is the per-service secret a channel needs beyond a URL: a PagerDuty routing key or a
	// Grafana API token. It is masked out of read responses the same way a channel URL is, and the
	// account secrets for twilio and email are never carried here, only server-side.
	Key string `json:"key,omitempty"`
	// To names the recipient for a channel whose transport is held server-side: a phone number for
	// twilio, or a comma-separated address list for email. It carries no secret.
	To string `json:"to,omitempty"`
	// OnFailure limits delivery to a failed or interrupted run when set, so a channel can page only
	// on trouble.
	OnFailure bool `json:"on_failure,omitempty"`
}

// Notification kinds a per-run target may name.
//
// The first group is fully configured by a URL. PagerDuty and Grafana each carry their own
// per-service key in the target, masked out of read responses. Twilio and email name only a
// recipient, because their account and transport secrets stay server-side and are never carried in a
// target a template stores.
const (
	NotifyWebhook    = "webhook"
	NotifySlack      = "slack"
	NotifyMattermost = "mattermost"
	NotifyRocketChat = "rocketchat"
	NotifyDiscord    = "discord"
	NotifyTeams      = "teams"
	NotifyNtfy       = "ntfy"
	NotifyPagerDuty  = "pagerduty"
	NotifyGrafana    = "grafana"
	NotifyTwilio     = "twilio"
	NotifyEmail      = "email"
)

// notifyNeedsURL is the set of kinds a bare URL configures.
func notifyNeedsURL(k string) bool {
	switch k {
	case NotifyWebhook, NotifySlack, NotifyMattermost, NotifyRocketChat, NotifyDiscord,
		NotifyTeams, NotifyNtfy:
		return true
	}
	return false
}

// ValidateNotifyTarget reports why a target is not deliverable, or nil when it is. It checks that
// each kind carries the field it needs, so a target that would silently reach no one is refused when
// it is saved rather than dropped at run time.
func ValidateNotifyTarget(t NotifyTarget) error {
	switch {
	case notifyNeedsURL(t.Kind):
		if t.URL == "" {
			return fmt.Errorf("%s target needs a url", t.Kind)
		}
	case t.Kind == NotifyPagerDuty:
		if t.Key == "" {
			return fmt.Errorf("pagerduty target needs a routing key")
		}
	case t.Kind == NotifyGrafana:
		if t.URL == "" || t.Key == "" {
			return fmt.Errorf("grafana target needs an annotation url and an api token")
		}
	case t.Kind == NotifyTwilio, t.Kind == NotifyEmail:
		if t.To == "" {
			return fmt.Errorf("%s target needs a recipient", t.Kind)
		}
	default:
		return fmt.Errorf("unknown notification kind %q", t.Kind)
	}
	return nil
}

// ValidNotifyKind reports whether k names a supported per-run notification channel.
func ValidNotifyKind(k string) bool {
	switch k {
	case NotifyWebhook, NotifySlack, NotifyMattermost, NotifyRocketChat, NotifyDiscord,
		NotifyTeams, NotifyNtfy, NotifyPagerDuty, NotifyGrafana, NotifyTwilio, NotifyEmail:
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

// WithHeldByPolicy records the approval rule that held the run, as it was named at the hold.
func WithHeldByPolicy(label string) SubmitOption {
	return func(r *Run) { r.HeldByPolicy = label }
}

// WithAuditReceiptOf records that this run was set in motion by the request behind receipt, for a
// run created after that request returned. It is deliberately not part of ExecutionOptions: rerun,
// shard retry, and reconcile replay those, and each is a new request with its own authorization.
func WithAuditReceiptOf(receipt string) SubmitOption {
	return func(r *Run) { r.AuditReceipt = receipt }
}

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

// WithOrgID stamps the owning organization on the run, the org of the submitting actor. It is what
// scopes an objectless run to a tenant. An empty id leaves the run unowned.
func WithOrgID(orgID string) SubmitOption {
	return func(r *Run) {
		if orgID != "" {
			r.OrgID = orgID
		}
	}
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

// WithActorType stamps how the requesting actor authenticated, so a policy can tell an agent's
// request from a person's.
func WithActorType(kind string) SubmitOption {
	return func(r *Run) { r.ActorType = kind }
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

// WithTags runs only the Ansible plays and tasks carrying one of these tags.
func WithTags(tags ...string) SubmitOption {
	return func(r *Run) { r.Tags = cloneNonEmpty(tags) }
}

// WithSkipTags skips the Ansible plays and tasks carrying one of these tags.
func WithSkipTags(tags ...string) SubmitOption {
	return func(r *Run) { r.SkipTags = cloneNonEmpty(tags) }
}

// WithVerbosity raises Ansible logging from 0 to 4, clamped to that range.
func WithVerbosity(level int) SubmitOption {
	return func(r *Run) {
		if level < 0 {
			level = 0
		}
		if level > 4 {
			level = 4
		}
		r.Verbosity = level
	}
}

// WithForks sets how many hosts Ansible addresses in parallel. A value below one leaves the default.
func WithForks(n int) SubmitOption {
	return func(r *Run) {
		if n > 0 {
			r.Forks = n
		}
	}
}

// WithDiffMode shows the before-and-after of every Ansible file and template change.
func WithDiffMode(diff bool) SubmitOption {
	return func(r *Run) { r.DiffMode = diff }
}

// cloneNonEmpty returns a copy of in with blank entries dropped, or nil when nothing remains, so a
// stored tag list never carries an empty tag that would widen or narrow a run in a surprising way.
func cloneNonEmpty(in []string) []string {
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s != "" {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
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

// WithRetryOf links a run back to the run it was derived from, so a failed-host relaunch or a retry
// records its lineage in the chain rather than looking like an unrelated run.
func WithRetryOf(sourceID string) SubmitOption {
	return func(r *Run) {
		if sourceID != "" {
			r.RetryOf = &sourceID
		}
	}
}

// ExecutionOptions returns the options that carry how r executes onto another run: the tool and its
// command, the dry-run flag, the variables, the credentials, the project, the inventory, the queue,
// the timeout, and the execution image with its pull credential.
//
// It is the one description of a run's execution spec. Every path that derives a run from another
// run used to write its own list, and every one of those lists lost a field over time: a shard
// dropped the parent's extra vars, image, and timeout, a pipeline step dropped its own set, and a
// rerun dropped the timeout and notifications. A dry-run-only spec that loses its flag makes real
// changes, so a dropped field here is not cosmetic.
//
// It deliberately excludes what belongs to one run rather than to the spec: Limit, which names a
// shard's own hosts and must not be overwritten by the parent's, along with the labels,
// notifications, and provenance a caller decides for itself.
func (r *Run) ExecutionOptions() []SubmitOption {
	opts := []SubmitOption{
		WithTool(r.Tool),
		WithCommand(r.Command),
		WithDryRun(r.DryRun),
		WithExtraVars(r.ExtraVars),
		WithCredentialIDs(r.CredentialIDs),
		WithTags(r.Tags...),
		WithSkipTags(r.SkipTags...),
		WithVerbosity(r.Verbosity),
		WithForks(r.Forks),
		WithDiffMode(r.DiffMode),
	}
	if r.ProjectID != "" {
		opts = append(opts, WithProject(r.ProjectID))
	}
	if r.InventoryID != "" {
		opts = append(opts, WithInventory(r.InventoryID))
	}
	if r.Queue != "" {
		opts = append(opts, WithQueue(r.Queue))
	}
	if r.Timeout > 0 {
		opts = append(opts, WithTimeout(r.Timeout))
	}
	if r.Image != "" {
		opts = append(opts, WithImage(r.Image, r.PullCredentialID))
	}
	return opts
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

// NewClaimSecret returns a fresh 256-bit capability for a claim, hex encoded. It is minted by the
// control node when a worker leases a run and never derived from anything the worker supplies, so a
// worker cannot predict or reconstruct another claim's secret.
func NewClaimSecret() string {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		// The system random source failing is not a condition to paper over with a weak secret: a
		// predictable capability is worse than none, so this is a programming-time fault.
		panic("run: read claim secret: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
