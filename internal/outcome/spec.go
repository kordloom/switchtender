package outcome

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
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
	// PinnedCommit is the git commit the run's project content was pinned to at launch, so an
	// approval covers the exact tree that will run.
	PinnedCommit string `json:"pinned_commit,omitempty"`
	// Command is the tool's primary input for non-Ansible runs.
	Command string `json:"command,omitempty"`
	// Inventory is the inventory path the run targets.
	Inventory string `json:"inventory,omitempty"`
	// InventoryID names the stored inventory the run targets.
	InventoryID string `json:"inventory_id,omitempty"`
	// InventoryResolution is the host set a smart or constructed inventory resolved to at launch,
	// and the inputs it drew them from. The run is held to these hosts, so they are part of what an
	// approver decides on and what the outcome commits: a composed inventory is evaluated rather
	// than stored, and without this the evidence names a filter but never the machines it reached.
	// Absent for every run that targets no composed inventory, which keeps their digests unchanged.
	InventoryResolution *run.InventoryResolution `json:"inventory_resolution,omitempty"`
	// ProjectID names the git project the run reads from.
	ProjectID string `json:"project_id,omitempty"`
	// Limit narrows the run to matching hosts.
	Limit string `json:"limit,omitempty"`
	// Tags and SkipTags select and skip Ansible plays and tasks.
	Tags     []string `json:"tags,omitempty"`
	SkipTags []string `json:"skip_tags,omitempty"`
	// ExtraVars are the run's injected variables, redacted before the record is digested.
	ExtraVars map[string]any `json:"extra_vars,omitempty"`
	// SealedNames names the variables supplied as secret survey answers. The record says that an
	// answer was supplied and never what it was: the answers are sealed on the run and bound here by
	// name, the way credentials are bound by reference.
	SealedNames []string `json:"sealed_vars,omitempty"`
	// SealedDigests binds each of those answers by a digest of its ciphertext, so an approval covers
	// which sealed answer will be opened and not only that one was given: a sealed answer swapped
	// for another under the same name moves the binding. A digest of ciphertext says nothing about
	// the answer. Absent for every run without sealed answers, and for one created before digests
	// existed, which keeps their digests unchanged.
	SealedDigests []run.SealedDigest `json:"sealed_var_digests,omitempty"`
	// CredentialIDs names the stored credentials the run executes with, by reference.
	CredentialIDs []string `json:"credential_ids,omitempty"`
	// Image is the container image the run executes in, resolved and pinned when the run was
	// submitted, by digest when the registry answered then. The image is where the tool, its
	// credentials, and its output live, so it decides what a run does as surely as the command does,
	// and an approval covers it. Absent for a run on the host, which keeps its digest unchanged.
	Image string `json:"image,omitempty"`
	// PullCredentialID names the registry credential for a private image.
	PullCredentialID string `json:"pull_credential_id,omitempty"`
	// InventorySnapshot binds the stored inventory the run executes against, materialized when the
	// run was submitted: the digest of its sealed content, a digest of its content with secrets
	// masked, and its hosts. For a composed inventory it binds the result the run resolved to, its
	// hosts with their variables, beside InventoryResolution. Absent for every run that targets no
	// stored inventory.
	InventorySnapshot *InventorySnapshotSpec `json:"inventory_snapshot,omitempty"`
	// PlanSHA256 binds the sealed plan file a terraform or opentofu apply carries out, the plan the
	// plan gate or a drift check saved, so an approval releases that plan and nothing planned
	// afterward. Absent for every other run.
	PlanSHA256 string `json:"plan_sha256,omitempty"`
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
	// FactCache records that the run serves cached facts and how old they may be, absent while the
	// cache is off so a run that does not use it keeps its digest. See FactCacheSpec.
	FactCache *FactCacheSpec `json:"fact_cache,omitempty"`
	// ModulesDigests are the digests of the Terraform or OpenTofu module trees the gate downloaded
	// and read before it judged the run. Execution refuses a run whose modules differ, so an
	// approval covers the exact module code that will run, not whatever a version constraint
	// resolves to by the time it does. Absent for every run the gate downloaded no modules for,
	// which keeps their digests unchanged.
	ModulesDigests []string `json:"modules_digests,omitempty"`
}

// InventorySnapshotSpec is the part of a run's inventory snapshot an approval binds. Every field is
// safe to disclose: the content is bound by the digest of its ciphertext and by a digest taken with
// its secret values masked, never by a digest of a secret a reader could guess against.
type InventorySnapshotSpec struct {
	// SealedSHA256 is the hex SHA-256 of the sealed content as stored on the run.
	SealedSHA256 string `json:"sealed_sha256"`
	// ContentSHA256 is the hex SHA-256 of the content with its secret values masked.
	ContentSHA256 string `json:"content_sha256"`
	// Hosts are the hosts the content names when it resolves without Ansible.
	Hosts []string `json:"hosts,omitempty"`
	// Dynamic reports that the content is a definition Ansible resolves at execution, so its hosts
	// are recorded with the outcome rather than bound here.
	Dynamic bool `json:"dynamic,omitempty"`
	// CredentialIDs are the credentials the inventory attached when the run was submitted.
	CredentialIDs []string `json:"credential_ids,omitempty"`
}

// inventorySnapshotSpecOf returns the binding part of r's inventory snapshot, nil for none.
func inventorySnapshotSpecOf(r *run.Run) *InventorySnapshotSpec {
	s := r.InventorySnapshot
	if s == nil {
		return nil
	}
	return &InventorySnapshotSpec{SealedSHA256: s.SealedSHA256, ContentSHA256: s.ContentSHA256,
		Hosts: s.Hosts, Dynamic: s.Dynamic, CredentialIDs: s.CredentialIDs}
}

// Spec assembles the canonical redacted spec bytes for r. Redaction happens here, before the bytes
// leave this function, so no caller ever holds a disclosable spec the redaction did not pass over.
func Spec(r *run.Run) ([]byte, error) {
	raw, err := json.Marshal(specRecordOf(r))
	if err != nil {
		return nil, err
	}
	// A spec that will not redact is withheld rather than disclosed raw: this body is published
	// beside its digest in a receipt, so bytes no redaction passed over are bytes handed to an
	// outside reader in the clear.
	return audit.CanonicalRedacted(raw)
}

// specRecordOf reduces a run to the fields that decide what it executes. It is the one place that
// list lives, so the digest a receipt discloses and the binding the executor checks are built from
// exactly the same fields and cannot drift apart.
func specRecordOf(r *run.Run) SpecRecord {
	rec := SpecRecord{
		Tool: r.Tool, Playbook: r.Playbook, PinnedCommit: r.PinnedCommit, Command: r.Command,
		Inventory: r.Inventory, InventoryID: r.InventoryID,
		InventoryResolution: r.InventoryResolution, ProjectID: r.ProjectID,
		Limit: r.Limit, Tags: r.Tags, SkipTags: r.SkipTags, ExtraVars: r.ExtraVars,
		SealedNames: r.SealedNames, CredentialIDs: r.CredentialIDs, PullCredentialID: r.PullCredentialID,
		DryRun: r.DryRun, Verbosity: r.Verbosity, Forks: r.Forks, DiffMode: r.DiffMode,
		Timeout: r.Timeout, Steps: r.Steps, ModulesDigests: r.ModulesDigests(), Image: r.Image,
		InventorySnapshot: inventorySnapshotSpecOf(r), PlanSHA256: r.PlanSHA256,
	}
	rec.SealedDigests = r.SealedDigests
	if r.ShardCount != nil {
		rec.ShardCount = *r.ShardCount
	}
	rec.FactCache = factCacheSpecOf(r)
	return rec
}

// specRaw returns the run's spec as written, with nothing redacted. It is the input to the binding
// the executor checks and never leaves this process.
//
// json.Marshal is deterministic for this record: the struct fixes field order and the encoder sorts
// the keys of the one map in it, so the same run reduces to the same bytes on every call, which is
// what a digest compared across two moments needs.
func specRaw(r *run.Run) ([]byte, error) {
	rec := specRecordOf(r)
	return json.Marshal(rec)
}

// SpecBinding returns the digest the executor holds an approved run to. It covers the unredacted
// spec, so any change to what will execute moves it.
//
// SpecDigest cannot serve here. It is taken over the redacted spec because that is what a receipt
// discloses, and redaction is lossy: an unquoted secret assignment is masked to the end of its line,
// so two commands differing only after the secret reduce to the same bytes and share one digest. A
// gate built on that value let an approved run be rewritten past its own approval, which is the one
// thing the gate exists to stop.
func SpecBinding(r *run.Run) (string, error) {
	raw, err := specRaw(r)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// SpecDigest returns the unkeyed digest of r's canonical redacted spec. It is unkeyed because the
// spec is disclosed beside it in a receipt, and recomputability by any holder is the point.
func SpecDigest(r *run.Run) (string, error) {
	body, err := Spec(r)
	if err != nil {
		return "", err
	}
	// Digested over the exact bytes Spec returns, which are the exact bytes a receipt discloses as
	// spec_body. Reducing them again here redacted a second time, and redaction is not idempotent,
	// so the digest stopped covering the disclosed spec and two different commands could share one
	// digest, walking through the approved-spec gate.
	return audit.UnkeyedDigestOfReduced(body), nil
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
	// DecisionID is the id of the decision record kept beside the chain, which is also the id of the
	// chain entry committing this body. It is what a correction and a redaction name, and it is the
	// event id the reason commitment is bound to. Omitted on a decision recorded before decision
	// records existed, which reduces to the bytes it always did.
	DecisionID string `json:"decision_id,omitempty"`
	// ReasonCommitment is the hiding commitment to the approver's masked reason, omitted when none
	// was given. The reason itself is never in the body.
	ReasonCommitment string `json:"reason_commitment,omitempty"`
	// SeparationOfDuties is how separation of duties was evaluated for a decision on a run an agent
	// asked for, omitted for every other run.
	SeparationOfDuties *decision.SeparationOfDuties `json:"separation_of_duties,omitempty"`
	// Comment is the pull request comment the decision was made from: the forge, the comment's
	// and its author's numeric ids, and the SHA-256 of its body. Omitted for a decision made any
	// other way, which reduces to the bytes it always did.
	Comment *decision.Comment `json:"comment,omitempty"`
}

// DecisionBody assembles the canonical decision record for r and returns its JSON with the spec
// digest it embeds. It is exported so a receipt can rebuild the same bytes for disclosure.
func DecisionBody(r *run.Run, verdict string) (body []byte, specDigest string, err error) {
	return DecisionBodyWith(r, verdict, DecisionExtras{})
}

// DecisionBodyWith is DecisionBody for a decision recorded with a decision record, committing the
// record's id, the reason commitment, and the separation-of-duties evaluation beside the verdict.
func DecisionBodyWith(r *run.Run, verdict string, extras DecisionExtras) (body []byte,
	specDigest string, err error) {
	specDigest, err = SpecDigest(r)
	if err != nil {
		return nil, "", err
	}
	body, err = json.Marshal(DecisionRecord{RunID: r.ID, Verdict: verdict, SpecDigest: specDigest,
		DecisionID: extras.ID, ReasonCommitment: extras.ReasonCommitment,
		SeparationOfDuties: extras.SeparationOfDuties, Comment: extras.Comment})
	if err != nil {
		return nil, "", err
	}
	return body, specDigest, nil
}

// VerdictRefused is the decision a deny policy makes about a submission it refuses.
const VerdictRefused = "refused"

// policyDecider is who the chain names for a refusal: the policy engine, acting on behalf of
// whoever asked for the run, since no person made the decision.
const policyDecider = "system:policy"

// RefusalPath is the chain path of a refusal: the run that was refused, for a Rego policy the full
// digest of the bundle that decided, and the rule that refused it. The refused run is never
// created, so nothing could be looked up by its id afterward, and the path is the one part of the
// entry an exported chain discloses as written. The rule's name comes last and runs to the end of
// the path, so a name holding a slash cannot pose as a bundle digest.
func RefusalPath(runID, rule, bundle string) string {
	path := "/runs/" + runID + "/decision/" + VerdictRefused
	if bundle != "" {
		path += "/rego/sha256:" + bundle
	}
	return path + "/policy/" + rule
}

// CommitRefusal records that a deny policy refused r's submission, naming the rule and, for a Rego
// policy, the bundle digest, and committing the decision body over the spec that was refused.
//
// A refusal used to leave only the attempt the request middleware records, which says that a
// launch was asked for and nothing about what refused it. The policy in force could change a minute
// later, so the chain could not show which rule, or which exact Rego bundle, turned a submission
// away.
func CommitRefusal(ctx context.Context, audits audit.Store, r *run.Run, rule, bundle string,
	now func() time.Time) error {
	body, _, err := DecisionBody(r, VerdictRefused)
	if err != nil {
		return err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return err
	}
	return audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: now(),
		Actor: policyDecider, ActorType: "system", OnBehalfOf: r.Actor,
		Method: audit.MethodDecision, Path: RefusalPath(r.ID, rule, bundle),
		ContentDigest: digest, Nonce: nonce,
	})
}

// Decider is who made an approval decision, in the audit chain's vocabulary. The three fields
// travel together because the decision entry has to name the decider exactly as the entry recording
// the request that carried the decision does. A token's label alone cannot say whose authority a
// decision used: two tokens sharing a label on different accounts read identically, and the account
// behind a decision was recoverable only by pairing the entry with its request.
type Decider struct {
	// Name is the decider as the chain records it: a token's label, a username, or a caller class.
	Name string
	// Type is how the decider authenticated, in the gate's vocabulary: session, token, agent, or
	// unauthenticated.
	Type string
	// OnBehalfOf is the account whose authority the decider used, empty when it acted as itself.
	OnBehalfOf string
	// AccountID is the id of the account the decider authenticated as, empty when the caller names
	// no account. It is never written to the chain, which names accounts by OnBehalfOf. It is what
	// separation of duties compares, because a person's token and their browser session carry
	// different names for one account, and an agent's runs count its bound account as the requester.
	AccountID string
}

// CommitDecision records an approval decision as a tamper-evident chain entry naming the decider
// and committing the decision body, and returns the spec digest it committed. The caller appends it
// before releasing the run, fail-closed: a decision that cannot be recorded is not a decision this
// system acts on.
func CommitDecision(ctx context.Context, audits audit.Store, r *run.Run, verdict string, by Decider,
	now func() time.Time) (string, error) {
	return CommitDecisionWith(ctx, audits, r, verdict, by, now, DecisionExtras{})
}

// CommitDecisionWith is CommitDecision for a decision recorded with a decision record. The entry
// takes the record's id, so the record and the entry that committed it name each other.
func CommitDecisionWith(ctx context.Context, audits audit.Store, r *run.Run, verdict string,
	by Decider, now func() time.Time, extras DecisionExtras) (string, error) {
	entry, specDigest, err := DecisionEntry(r, verdict, by, now, extras)
	if err != nil {
		return "", err
	}
	if err := audits.Append(ctx, entry); err != nil {
		return "", err
	}
	return specDigest, nil
}

// DecisionEntry builds the chain entry CommitDecisionWith appends, without appending it, and
// returns it with the spec digest its body commits. A decision is claimed before it is recorded,
// and the claim carries this entry, so whichever process finishes the decision appends exactly
// these bytes under exactly this id.
func DecisionEntry(r *run.Run, verdict string, by Decider, now func() time.Time,
	extras DecisionExtras) (*audit.Entry, string, error) {
	body, specDigest, err := DecisionBodyWith(r, verdict, extras)
	if err != nil {
		return nil, "", err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return nil, "", err
	}
	if now == nil {
		now = time.Now
	}
	return &audit.Entry{
		ID: entryID(extras.ID), At: now(),
		Actor: by.Name, ActorType: by.Type, OnBehalfOf: by.OnBehalfOf,
		Method: audit.MethodDecision, Path: "/runs/" + r.ID + "/decision/" + verdict,
		ContentDigest: digest, Nonce: nonce,
	}, specDigest, nil
}
