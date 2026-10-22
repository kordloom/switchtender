package binding

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"slices"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// SealPrefix starts every stored seal and names the record layout and key derivation it was made
// under, so a later layout takes a new prefix rather than reinterpreting an old seal.
const SealPrefix = "bk1:"

// The record kinds, one per kind of row the executor acts on or reads forward.
const (
	// KindSubmit seals a top-level run as it was submitted, before any decision.
	KindSubmit = "submit"
	// KindRun seals a top-level run an approver released, naming its DECISION entry.
	KindRun = "run"
	// KindGate seals a workflow approval step an approver released, over the workflow state the
	// approver decided on, naming the step's DECISION entry.
	KindGate = "gate"
	// KindShard seals one shard of a split to its parent's spec.
	KindShard = "shard"
	// KindStep seals one workflow step to its parent's spec and to the chain entries that released
	// it.
	KindStep = "step"
	// KindResult seals a finished step's status and outputs, which a resumed walk feeds forward.
	KindResult = "result"
	// KindClaim seals a decision claim a process stored on the row it decides, before the decision
	// is recorded.
	KindClaim = "claim"
)

// noIndex is the Index a record carries for a run that is neither a shard nor a step.
const noIndex = -1

// Root names one chain entry a row derives from, by id and by what the entry committed, so a
// rewritten or substituted entry moves the seal.
type Root struct {
	// EntryID is the chain entry's id.
	EntryID string `json:"entry_id"`
	// ContentDigest is the entry's content digest, the commitment to the body it recorded.
	ContentDigest string `json:"content_digest"`
	// Path is the entry's path, which names the run or step it is about and the verdict.
	Path string `json:"path"`
}

// RootOf returns the root naming e.
func RootOf(e *audit.Entry) Root {
	return Root{EntryID: e.ID, ContentDigest: e.ContentDigest, Path: e.Path}
}

// PolicySet is the part of a run's recorded rule set a record covers: the digest, the count, and
// the rule names, as the process wrote them at submit.
type PolicySet struct {
	// Digest is the rule set's digest.
	Digest string `json:"digest"`
	// Count is how many rules the digest covers.
	Count int `json:"count"`
	// Rules names each rule, as recorded.
	Rules []string `json:"rules"`
}

// Record is what a seal is computed over. Every field is always encoded, in declaration order,
// and the record holds no map, so the same row and chain entries reduce to the same bytes on every
// call and in every process. A slice that is empty encodes the same as one that is nil, since a
// store round trip does not keep that difference.
type Record struct {
	// Kind is one of the Kind constants.
	Kind string `json:"kind"`
	// RunID is the sealed row's id.
	RunID string `json:"run_id"`
	// RunKind is the row's own kind: plain, split, pipeline, or approval. It is covered so a row
	// cannot be turned from one that never executes into one that does.
	RunKind string `json:"run_kind"`
	// ParentID is the row's parent, empty for a top-level run.
	ParentID string `json:"parent_id"`
	// Index is the row's step index, or its shard index, or -1 when it has neither.
	Index int `json:"index"`
	// Attempt is the row's retry attempt.
	Attempt int `json:"attempt"`
	// SpecBinding is the row's unredacted spec binding, or for a gate the workflow state binding
	// the approver decided on.
	SpecBinding string `json:"spec_binding"`
	// ParentBinding is the parent's unredacted spec binding, for a shard, a step, or a gate.
	ParentBinding string `json:"parent_binding"`
	// ApprovalRequested is the row's ApprovalRequested flag.
	ApprovalRequested bool `json:"approval_requested"`
	// HeldByPolicy is the rule the row records as having held it.
	HeldByPolicy string `json:"held_by_policy"`
	// RequireDistinctApprover is the row's separation-of-duties flag.
	RequireDistinctApprover bool `json:"require_distinct_approver"`
	// PolicySet is the rule set the row records as in force at submit, nil when it records none.
	PolicySet *PolicySet `json:"policy_set"`
	// Facts are the row's fields a rule, a gate, or an approval check reads, as the row was built.
	Facts Facts `json:"facts"`
	// ApprovedSpecDigest is the spec digest a decision stamped on the row, empty on a row no decision
	// stamped. A submitted, shard, or step row is sealed with it empty, so a row a writer marked as
	// approved does not carry its seal.
	ApprovedSpecDigest string `json:"approved_spec_digest"`
	// ApprovedSpecBinding is the spec binding a decision stamped beside ApprovedSpecDigest.
	ApprovedSpecBinding string `json:"approved_spec_binding"`
	// Roots are the chain entries the row derives from, in the order the sealer named them.
	Roots []Root `json:"roots"`
	// Status is a finished row's status, for a result record.
	Status string `json:"status"`
	// OutputsDigest is the digest of a finished row's outputs, for a result record.
	OutputsDigest string `json:"outputs_digest"`
}

// Facts are the fields of a row that a rule, the plan gate, the distinct-approver check,
// federation, or an approval sweep reads, and that are fixed when the row is built. A field written
// later in the row's life is not here, and the binding test names each one with its reason.
type Facts struct {
	// Actor is who asked for the run.
	Actor string `json:"actor"`
	// ActorType is the kind of caller who asked for it.
	ActorType string `json:"actor_type"`
	// ActorUserID is the account id of the person who asked for it.
	ActorUserID string `json:"actor_user_id"`
	// Account is the token or service account that asked for it.
	Account string `json:"account"`
	// Initiator is the credential chain an agent's run records, every member as written.
	Initiator Initiator `json:"initiator"`
	// Queue is the worker group the run is pinned to.
	Queue string `json:"queue"`
	// Labels are the run's labels, sorted by key.
	Labels []Label `json:"labels"`
	// Reversibility is the run's recorded reversibility, nil when it records none.
	Reversibility *Reversibility `json:"reversibility"`
	// ProposedFrom is the plan run an apply was proposed from.
	ProposedFrom string `json:"proposed_from"`
	// TemplateID is the template the run was launched from.
	TemplateID string `json:"template_id"`
	// Source is what launched the run.
	Source string `json:"source"`
	// SourceID names the launcher's own record.
	SourceID string `json:"source_id"`
	// Intent is the change the run says it makes.
	Intent string `json:"intent"`
	// StepName is a step's name in its workflow.
	StepName string `json:"step_name"`
	// RequireReason is the reason requirement a decision on the run must meet.
	RequireReason string `json:"require_reason"`
	// CreatedAt is when the row was built, in UTC Unix seconds, zero for a row that records no time.
	// PostgreSQL keeps microseconds and SQLite keeps what it is given, so a finer value would refuse
	// an honest row read back.
	CreatedAt int64 `json:"created_at"`
	// PlanDestroys is the destroy count an apply was proposed with, nil when it records none.
	PlanDestroys *int `json:"plan_destroys"`
	// DryRunScans is the canonical JSON of the run's dry run scans, empty when it has none.
	DryRunScans string `json:"dry_run_scans"`
	// OrgID is the organization the run belongs to.
	OrgID string `json:"org_id"`
	// GitRef is the ref the run's project is fetched at.
	GitRef string `json:"git_ref"`
}

// Initiator is the covered form of a run's initiator: every member, empty for a run that records
// none.
type Initiator struct {
	// InitiatedBy is who started the run on the agent's behalf.
	InitiatedBy string `json:"initiated_by"`
	// CredentialID is the credential the agent acted under.
	CredentialID string `json:"credential_id"`
	// BoundTo is what that credential is bound to.
	BoundTo string `json:"bound_to"`
	// ProvisionedBy is who provisioned the credential.
	ProvisionedBy string `json:"provisioned_by"`
	// ProvisionedByType is the kind of caller who provisioned it.
	ProvisionedByType string `json:"provisioned_by_type"`
}

// Label is one run label.
type Label struct {
	// Key is the label's key.
	Key string `json:"key"`
	// Value is the label's value.
	Value string `json:"value"`
}

// Reversibility is the covered form of a run's recorded reversibility.
type Reversibility struct {
	// Class is the reversibility class.
	Class string `json:"class"`
	// Reasons are the reasons given for it.
	Reasons []string `json:"reasons"`
}

// canonical returns the bytes a seal is computed over.
func (rec Record) canonical() []byte {
	if len(rec.Roots) == 0 {
		rec.Roots = nil
	}
	if len(rec.Facts.Labels) == 0 {
		rec.Facts.Labels = nil
	}
	if rec.Facts.Reversibility != nil && len(rec.Facts.Reversibility.Reasons) == 0 {
		v := *rec.Facts.Reversibility
		v.Reasons = nil
		rec.Facts.Reversibility = &v
	}
	if rec.PolicySet != nil && len(rec.PolicySet.Rules) == 0 {
		p := *rec.PolicySet
		p.Rules = nil
		rec.PolicySet = &p
	}
	raw, err := json.Marshal(rec)
	if err != nil {
		// Record holds only strings, integers, booleans, and slices of structs of those, which
		// json.Marshal cannot fail on. Reaching this is a change to Record that broke that.
		panic("binding: marshal record: " + err.Error())
	}
	return raw
}

// Seal returns the stored form of the seal over rec: SealPrefix and the hex HMAC-SHA256 of the
// record's canonical bytes under k.
func (k *Key) Seal(rec Record) string {
	return k.sum(rec.canonical())
}

// Verify reports whether stored is the seal k makes over rec. The comparison takes the same time
// wherever the two differ, so a caller who can submit seals learns nothing from timing.
func (k *Key) Verify(rec Record, stored string) bool {
	return hmac.Equal([]byte(k.Seal(rec)), []byte(stored))
}

// claimRecord is what a claim seal is computed over: the kind, the run the claim is stored on, and
// the claim as the process encoded it without its own seal. Its fields differ from a Record's, so
// no claim seal can stand for a row's seal or the other way around.
type claimRecord struct {
	// Kind is KindClaim.
	Kind string `json:"kind"`
	// RunID is the id of the run the claim is stored on.
	RunID string `json:"run_id"`
	// Claim is the claim's encoding without its own seal.
	Claim string `json:"claim"`
}

// SealClaim returns the stored form of the seal over a decision claim stored on run runID, claim
// being the claim's encoding without its own seal: SealPrefix and the hex HMAC-SHA256 under k. It
// covers every byte of the claim, the chain entry it records with its approver among them, and the
// run it is stored on, so a claim a writer stored, changed, or copied from another run does not
// carry it.
func (k *Key) SealClaim(runID string, claim []byte) string {
	raw, err := json.Marshal(claimRecord{Kind: KindClaim, RunID: runID, Claim: string(claim)})
	if err != nil {
		// claimRecord holds only strings, which json.Marshal cannot fail on.
		panic("binding: marshal claim record: " + err.Error())
	}
	return k.sum(raw)
}

// VerifyClaim reports whether stored is the seal k makes over claim stored on run runID, in time
// that does not depend on where the two differ.
func (k *Key) VerifyClaim(runID string, claim []byte, stored string) bool {
	return hmac.Equal([]byte(k.SealClaim(runID, claim)), []byte(stored))
}

// sum returns SealPrefix and the hex HMAC-SHA256 of raw under k.
func (k *Key) sum(raw []byte) string {
	m := hmac.New(sha256.New, k.mac[:])
	m.Write(raw)
	return SealPrefix + hex.EncodeToString(m.Sum(nil))
}

// SubmitRecord returns the record a top-level run is sealed with when it is created.
func SubmitRecord(r *run.Run) (Record, error) {
	rec, err := rowRecord(KindSubmit, r)
	if err != nil {
		return Record{}, err
	}
	return withSpec(rec, r)
}

// AsStamped returns a copy of r carrying the spec digest and binding a decision is about to stamp
// on it. A decision seals the row before the stamp is written, in the claim that writes it, so the
// seal is computed over the row as it will read once stamped. r is not changed.
func AsStamped(r *run.Run, digest, binding string) *run.Run {
	stamped := *r
	stamped.ApprovedSpecDigest, stamped.ApprovedSpecBinding = digest, binding
	return &stamped
}

// RunRecord returns the record a top-level run is sealed with when an approver releases it, root
// naming the DECISION entry that records the approval.
func RunRecord(r *run.Run, root Root) (Record, error) {
	rec, err := rowRecord(KindRun, r)
	if err != nil {
		return Record{}, err
	}
	rec.Roots = []Root{root}
	return withSpec(rec, r)
}

// GateRecord returns the record a workflow approval step is sealed with when an approver releases
// it: stateBinding is the binding of the workflow state the approver decided on, and root names the
// step's DECISION entry.
func GateRecord(parent, node *run.Run, stateBinding string, root Root) (Record, error) {
	rec, err := childRecord(KindGate, parent, node)
	if err != nil {
		return Record{}, err
	}
	rec.SpecBinding = stateBinding
	rec.Roots = []Root{root}
	return rec, nil
}

// ShardRecord returns the record a shard is sealed with when its split is built.
func ShardRecord(parent, child *run.Run) (Record, error) {
	rec, err := childRecord(KindShard, parent, child)
	if err != nil {
		return Record{}, err
	}
	return withSpec(rec, child)
}

// StepRecord returns the record a workflow step is sealed with when it is built, roots naming the
// chain entries that released it in the order the walk names them.
func StepRecord(parent, child *run.Run, roots []Root) (Record, error) {
	rec, err := childRecord(KindStep, parent, child)
	if err != nil {
		return Record{}, err
	}
	rec.Roots = slices.Clone(roots)
	return withSpec(rec, child)
}

// ResultRecord returns the record a finished step is sealed with in the write that ends it, over
// its status and its outputs.
func ResultRecord(child *run.Run) (Record, error) {
	digest, err := OutputsDigest(child.Outputs)
	if err != nil {
		return Record{}, err
	}
	return Record{
		Kind: KindResult, RunID: child.ID, RunKind: child.Kind, ParentID: parentID(child),
		Index: indexOf(child), Attempt: child.Attempt, Status: string(child.Status),
		OutputsDigest: digest,
	}, nil
}

// OutputsDigest returns the hex SHA-256 of outputs in canonical JSON, prefixed "sha256:". The
// outputs are passed through one JSON round trip first, the one a store applies, so a value held in
// memory and the same value read back from a store digest the same. Nil and empty digest the same.
func OutputsDigest(outputs map[string]any) (string, error) {
	if outputs == nil {
		outputs = map[string]any{}
	}
	raw, err := json.Marshal(outputs)
	if err != nil {
		return "", fmt.Errorf("outputs digest: %w", err)
	}
	var stored any
	if err := json.Unmarshal(raw, &stored); err != nil {
		return "", fmt.Errorf("outputs digest: %w", err)
	}
	raw, err = json.Marshal(stored)
	if err != nil {
		return "", fmt.Errorf("outputs digest: %w", err)
	}
	sum := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// rowRecord returns the fields every sealed row's record shares, without its spec binding.
func rowRecord(kind string, r *run.Run) (Record, error) {
	facts, err := factsOf(r)
	if err != nil {
		return Record{}, fmt.Errorf("facts of run %s: %w", r.ID, err)
	}
	return Record{
		Kind: kind, RunID: r.ID, RunKind: r.Kind, ParentID: parentID(r), Index: indexOf(r),
		Attempt: r.Attempt, ApprovalRequested: r.ApprovalRequested, HeldByPolicy: r.HeldByPolicy,
		RequireDistinctApprover: r.RequireDistinctApprover, PolicySet: policySetOf(r.PolicySet),
		Facts: facts, ApprovedSpecDigest: r.ApprovedSpecDigest,
		ApprovedSpecBinding: r.ApprovedSpecBinding,
	}, nil
}

// factsOf returns the covered form of r's facts. The dry run scans are encoded once, the way a
// store writes them, so a value held in memory and the same value read back encode the same.
func factsOf(r *run.Run) (Facts, error) {
	f := Facts{
		Actor: r.Actor, ActorType: r.ActorType, ActorUserID: r.ActorUserID, Account: r.Account,
		Queue: r.Queue, ProposedFrom: r.ProposedFrom, TemplateID: r.TemplateID, Source: r.Source,
		SourceID: r.SourceID, Intent: r.Intent, StepName: r.StepName,
		RequireReason: r.RequireReason, OrgID: r.OrgID, GitRef: r.GitRef,
	}
	if !r.CreatedAt.IsZero() {
		f.CreatedAt = r.CreatedAt.UTC().Unix()
	}
	if i := r.Initiator; i != nil {
		f.Initiator = Initiator{InitiatedBy: i.InitiatedBy, CredentialID: i.CredentialID,
			BoundTo: i.BoundTo, ProvisionedBy: i.ProvisionedBy, ProvisionedByType: i.ProvisionedByType}
	}
	for _, k := range slices.Sorted(maps.Keys(r.Labels)) {
		f.Labels = append(f.Labels, Label{Key: k, Value: r.Labels[k]})
	}
	if v := r.Reversibility; v != nil {
		f.Reversibility = &Reversibility{Class: v.Class, Reasons: slices.Clone(v.Reasons)}
	}
	if r.PlanDestroys != nil {
		n := *r.PlanDestroys
		f.PlanDestroys = &n
	}
	if len(r.DryRunScans) > 0 {
		raw, err := json.Marshal(r.DryRunScans)
		if err != nil {
			return Facts{}, fmt.Errorf("encode dry run scans: %w", err)
		}
		f.DryRunScans = string(raw)
	}
	return f, nil
}

// childRecord returns rowRecord for child with its parent's spec binding, refusing a child that
// does not name parent as its parent.
func childRecord(kind string, parent, child *run.Run) (Record, error) {
	if child.ParentID == nil || *child.ParentID != parent.ID {
		return Record{}, fmt.Errorf("%w: run %s, parent %s", ErrParent, child.ID, parent.ID)
	}
	rec, err := rowRecord(kind, child)
	if err != nil {
		return Record{}, err
	}
	pb, err := storedBinding(parent)
	if err != nil {
		return Record{}, fmt.Errorf("bind parent %s: %w", parent.ID, err)
	}
	rec.ParentBinding = pb
	return rec, nil
}

// withSpec sets rec's spec binding to r's.
func withSpec(rec Record, r *run.Run) (Record, error) {
	b, err := storedBinding(r)
	if err != nil {
		return Record{}, fmt.Errorf("bind run %s: %w", r.ID, err)
	}
	rec.SpecBinding = b
	return rec, nil
}

// storedBinding returns r's spec binding as a store would compute it from the row it holds. A row
// is sealed in memory before it is saved, and the SQL stores hand its extra vars back through a
// JSON round trip: an integer past 2^53 comes back as the nearest float, and a struct comes back as
// a map with its keys sorted. Binding the vars as written would refuse that honest row once it was
// read back, so the vars are bound after the same round trip, on a copy, whichever side is
// binding. The caller's run is not changed.
func storedBinding(r *run.Run) (string, error) {
	if len(r.ExtraVars) == 0 {
		return outcome.SpecBinding(r)
	}
	raw, err := json.Marshal(r.ExtraVars)
	if err != nil {
		return "", fmt.Errorf("encode extra vars: %w", err)
	}
	var vars map[string]any
	if err := json.Unmarshal(raw, &vars); err != nil {
		return "", fmt.Errorf("decode extra vars: %w", err)
	}
	stored := *r
	stored.ExtraVars = vars
	return outcome.SpecBinding(&stored)
}

// parentID returns r's parent id, empty for a top-level run.
func parentID(r *run.Run) string {
	if r.ParentID == nil {
		return ""
	}
	return *r.ParentID
}

// indexOf returns r's step index, or its shard index, or noIndex when it has neither.
func indexOf(r *run.Run) int {
	switch {
	case r.StepIndex != nil:
		return *r.StepIndex
	case r.ShardIndex != nil:
		return *r.ShardIndex
	}
	return noIndex
}

// policySetOf copies the covered part of a run's recorded rule set, nil for none.
func policySetOf(p *run.PolicySet) *PolicySet {
	if p == nil {
		return nil
	}
	return &PolicySet{Digest: p.Digest, Count: p.Count, Rules: slices.Clone(p.Rules)}
}
