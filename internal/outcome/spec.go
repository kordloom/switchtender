package outcome

import (
	"context"
	"encoding/json"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// SpecRecord is the canonical record of what a run was asked to execute: every field that decides
// what the run does to the systems it touches, and nothing about where or when it was scheduled.
// The digest of this record is what an approval binds to and what the outcome commits, so the
// field set is fixed; adding a field changes every new digest, which is the point of having one.
type SpecRecord struct {
	// Tool is the execution engine.
	Tool string `json:"tool,omitempty"`
	// Playbook is the playbook path for an Ansible run.
	Playbook string `json:"playbook,omitempty"`
	// Command is the tool's primary input for non-Ansible runs.
	Command string `json:"command,omitempty"`
	// Inventory is the inventory path the run targets.
	Inventory string `json:"inventory,omitempty"`
	// InventoryID names the stored inventory the run targets.
	InventoryID string `json:"inventory_id,omitempty"`
	// ProjectID names the git project the run reads from.
	ProjectID string `json:"project_id,omitempty"`
	// Limit narrows the run to matching hosts.
	Limit string `json:"limit,omitempty"`
	// Tags and SkipTags select and skip Ansible plays and tasks.
	Tags     []string `json:"tags,omitempty"`
	SkipTags []string `json:"skip_tags,omitempty"`
	// ExtraVars are the run's injected variables, redacted before the record is digested.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// CredentialIDs names the stored credentials the run executes with, by reference.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// PullCredentialID names the registry credential for a private image. The image reference
	// itself is deliberately absent: the dispatcher resolves it onto the run when the run
	// finalizes, so including it would make the executed digest differ from the approved one on
	// every run that used a project or server default. The resolved image is committed by the
	// outcome record instead.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// DryRun runs the tool in its no-change mode.
	DryRun bool `json:"dry_run,omitempty"`
	// Verbosity, Forks, and DiffMode are the Ansible execution controls.
	Verbosity int  `json:"verbosity,omitempty"`
	Forks     int  `json:"forks,omitempty"`
	DiffMode  bool `json:"diff_mode,omitempty"`
	// Timeout caps how many seconds the run may execute.
	Timeout int `json:"timeout,omitempty"`
	// ShardCount is how many slices a split run fans out to.
	ShardCount int `json:"shard_count,omitempty"`
	// Steps are a pipeline's declared steps, the graph an approver decided on.
	Steps []run.PipelineStep `json:"steps,omitempty"`
}

// Spec assembles the canonical redacted spec bytes for r. Redaction happens here, before the bytes
// leave this function, so no caller ever holds a disclosable spec the redaction did not pass over.
func Spec(r *run.Run) ([]byte, error) {
	rec := SpecRecord{
		Tool: r.Tool, Playbook: r.Playbook, Command: r.Command,
		Inventory: r.Inventory, InventoryID: r.InventoryID, ProjectID: r.ProjectID,
		Limit: r.Limit, Tags: r.Tags, SkipTags: r.SkipTags, ExtraVars: r.ExtraVars,
		CredentialIDs: r.CredentialIDs, PullCredentialID: r.PullCredentialID,
		DryRun: r.DryRun, Verbosity: r.Verbosity, Forks: r.Forks, DiffMode: r.DiffMode,
		Timeout: r.Timeout, Steps: r.Steps,
	}
	if r.ShardCount != nil {
		rec.ShardCount = *r.ShardCount
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		return nil, err
	}
	return audit.CanonicalRedacted(raw), nil
}

// SpecDigest returns the unkeyed digest of r's canonical redacted spec. It is unkeyed because the
// spec is disclosed beside it in a receipt, and recomputability by any holder is the point.
func SpecDigest(r *run.Run) (string, error) {
	body, err := Spec(r)
	if err != nil {
		return "", err
	}
	return audit.UnkeyedDigestOf(body), nil
}

// DecisionRecord is the canonical body an approval decision commits: which run, which verdict, and
// the digest of the exact spec decided on. The spec digest is what makes the decision bind to
// content; without it an approval names a run id whose meaning the evidence cannot pin down.
type DecisionRecord struct {
	// RunID is the run decided on.
	RunID string `json:"run_id"`
	// Verdict is approved or rejected.
	Verdict string `json:"verdict"`
	// SpecDigest is the digest of the run's spec at the moment of the decision.
	SpecDigest string `json:"spec_digest"`
}

// DecisionBody assembles the canonical decision record for r and returns its JSON with the spec
// digest it embeds. It is exported so a receipt can rebuild the same bytes for disclosure.
func DecisionBody(r *run.Run, verdict string) (body []byte, specDigest string, err error) {
	specDigest, err = SpecDigest(r)
	if err != nil {
		return nil, "", err
	}
	body, err = json.Marshal(DecisionRecord{RunID: r.ID, Verdict: verdict, SpecDigest: specDigest})
	if err != nil {
		return nil, "", err
	}
	return body, specDigest, nil
}

// CommitDecision records an approval decision as a tamper-evident chain entry naming the deciding
// actor and committing the decision body, and returns the spec digest it committed. The caller
// appends it before releasing the run, fail-closed: a decision that cannot be recorded is not a
// decision this system acts on.
func CommitDecision(ctx context.Context, audits audit.Store, r *run.Run, verdict, actor,
	actorType string) (string, error) {
	body, specDigest, err := DecisionBody(r, verdict)
	if err != nil {
		return "", err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return "", err
	}
	entry := &audit.Entry{
		ID: audit.NewID(), At: time.Now(),
		Actor: actor, ActorType: actorType,
		Method: audit.MethodDecision, Path: "/runs/" + r.ID + "/decision/" + verdict,
		ContentDigest: digest, Nonce: nonce,
	}
	if err := audits.Append(ctx, entry); err != nil {
		return "", err
	}
	return specDigest, nil
}
