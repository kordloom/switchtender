package outcome

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"slices"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/run"
)

// Verdicts an approval step's chain entries carry. Approved and rejected are the words a run's own
// decision uses, so one vocabulary reads across both. Requested records the step asking, and timed
// out records the step giving up on its own.
const (
	StepRequested = "requested"
	StepApproved  = "approved"
	StepRejected  = "rejected"
	StepTimedOut  = "timed_out"
)

// StepUpstream is one step an approval step waited behind, as its approver was shown it.
type StepUpstream struct {
	// Name is the step's name in the graph.
	Name string `json:"name"`
	// RunID is the run the step's last attempt executed as, empty when it never ran.
	RunID string `json:"run_id,omitempty"`
	// Status is how the step ended, or skipped when it never ran.
	Status string `json:"status"`
	// Outputs are the values the step published, which are what the steps after the approval are
	// handed. They are part of what an approver releases, so they are part of the state bound.
	Outputs map[string]any `json:"outputs,omitempty"`
	// DryRunFindings is why the step's dry run was not change free, as the gate's scan read it. An
	// approver deciding after a dry run that was not change free is deciding on hosts that may have
	// changed, so it is part of the state bound. Omitted when empty, so a state with nothing found
	// digests exactly as it did before this was recorded.
	DryRunFindings []string `json:"dry_run_findings,omitempty"`
}

// StepState is what an approver of a workflow approval step decides on: the workflow's spec, how
// every step the approval waited behind ended and what each published, and which steps run on each
// answer. Its digest is what an approval of the step binds to, the way a run's approval binds to
// the digest of its spec, so a decision cannot be carried over to a workflow that arrived at the
// step differently from the one the approver looked at.
type StepState struct {
	// RunID is the workflow run.
	RunID string `json:"run_id"`
	// Step is the approval step's name.
	Step string `json:"step"`
	// StepRunID is the record of this approval step.
	StepRunID string `json:"step_run_id"`
	// SpecDigest is the workflow's spec digest, which covers every step it declares.
	SpecDigest string `json:"spec_digest"`
	// Upstream is every step the approval waited behind, in declaration order.
	Upstream []StepUpstream `json:"upstream,omitempty"`
	// OnApprove names the steps that run only because the step is approved, in declaration order.
	OnApprove []string `json:"on_approve,omitempty"`
	// OnDeny names the steps that run only because the step is denied or times out.
	OnDeny []string `json:"on_deny,omitempty"`
	// specBinding is the workflow's unredacted spec binding. It is folded into Binding so a resumed
	// workflow is held to the exact graph the approver released, not only to its redacted digest. It
	// is unexported so it never serializes into anything disclosed.
	specBinding string
}

// StepStateOf assembles the state an approval step's approver decides on, from the workflow's
// stored graph and its stored step records. Every step the approval waited behind has finished by
// the time the step asks, so the state is fixed from the moment the request is made, and rebuilding
// it later for a receipt produces the same bytes unless a record changed.
func StepStateOf(ctx context.Context, store run.Store, parent, node *run.Run) (*StepState, error) {
	specDigest, err := SpecDigest(parent)
	if err != nil {
		return nil, err
	}
	specBinding, err := SpecBinding(parent)
	if err != nil {
		return nil, err
	}
	children, err := store.Steps(ctx, parent.ID)
	if err != nil {
		return nil, err
	}
	latest := map[int]*run.Run{}
	for _, c := range children {
		if c.StepIndex == nil {
			continue
		}
		prev, ok := latest[*c.StepIndex]
		if !ok || c.Attempt > prev.Attempt ||
			(c.Attempt == prev.Attempt && c.CreatedAt.After(prev.CreatedAt)) {
			latest[*c.StepIndex] = c
		}
	}
	graph := run.GraphSteps(parent.Steps)
	at := -1
	for i, s := range graph {
		if s.Name == node.StepName {
			at = i
			break
		}
	}
	state := &StepState{RunID: parent.ID, Step: node.StepName, StepRunID: node.ID,
		SpecDigest: specDigest, specBinding: specBinding}
	if at < 0 {
		return state, nil
	}
	upstream := upstreamOf(graph, at)
	for i, s := range graph {
		if !upstream[i] {
			continue
		}
		u := StepUpstream{Name: s.Name, Status: "skipped"}
		if r, ok := latest[i]; ok {
			u.RunID, u.Status, u.Outputs = r.ID, string(r.Status), r.Outputs
			u.DryRunFindings = r.DryRunFindings()
		}
		state.Upstream = append(state.Upstream, u)
	}
	state.OnApprove, state.OnDeny = downstreamOf(graph, at)
	return state, nil
}

// upstreamOf marks every step the step at idx transitively waits behind, over both kinds of edge.
func upstreamOf(steps []run.PipelineStep, idx int) []bool {
	byName := make(map[string]int, len(steps))
	for i, s := range steps {
		byName[s.Name] = i
	}
	seen := make([]bool, len(steps))
	stack := []int{idx}
	for len(stack) > 0 {
		cur := stack[len(stack)-1]
		stack = stack[:len(stack)-1]
		for _, dep := range append(append([]string(nil), steps[cur].DependsOn...),
			steps[cur].IfDenied...) {
			j, ok := byName[dep]
			if !ok || seen[j] {
				continue
			}
			seen[j] = true
			stack = append(stack, j)
		}
	}
	return seen
}

// downstreamOf names the steps reached from the approval step at idx through an approval, and those
// reached through a denial: each first hop decides which answer a branch belongs to, and everything
// below that hop follows it.
func downstreamOf(steps []run.PipelineStep, idx int) (onApprove, onDeny []string) {
	name := steps[idx].Name
	reach := func(first func(run.PipelineStep) bool) []string {
		seen := make([]bool, len(steps))
		for i, s := range steps {
			if first(s) {
				seen[i] = true
			}
		}
		for changed := true; changed; {
			changed = false
			for i, s := range steps {
				if seen[i] {
					continue
				}
				for j, other := range steps {
					if seen[j] && (slices.Contains(s.DependsOn, other.Name) ||
						slices.Contains(s.IfDenied, other.Name)) {
						seen[i] = true
						changed = true
						break
					}
				}
			}
		}
		var out []string
		for i, s := range steps {
			if seen[i] {
				out = append(out, s.Name)
			}
		}
		return out
	}
	onApprove = reach(func(s run.PipelineStep) bool { return slices.Contains(s.DependsOn, name) })
	onDeny = reach(func(s run.PipelineStep) bool { return slices.Contains(s.IfDenied, name) })
	return onApprove, onDeny
}

// Digest returns the disclosed digest of the state, taken over its redacted canonical form the way
// a spec digest is, so a value an upstream step published that looks like a secret is never the
// input to a digest anybody is shown.
func (s *StepState) Digest() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	body, err := audit.CanonicalRedacted(raw)
	if err != nil {
		return "", err
	}
	return audit.UnkeyedDigestOfReduced(body), nil
}

// Binding returns the digest a resumed workflow holds an approved step to. It covers the state as
// written, with nothing redacted, because redaction is lossy and two different published values can
// reduce to the same bytes. It never leaves the process, for the reason a spec binding does not.
func (s *StepState) Binding() (string, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	h.Write(raw)
	h.Write([]byte("\n"))
	h.Write([]byte(s.specBinding))
	return "sha256:" + hex.EncodeToString(h.Sum(nil)), nil
}

// StepDecisionRecord is the canonical body an approval step's chain entry commits: which workflow,
// which step, the verdict, the workflow's spec digest, and the digest of the state decided on. The
// spec digest is the workflow's own, so the bundle verifier's rule that every decision on a run
// binds the spec its outcome commits holds for a step decision too.
type StepDecisionRecord struct {
	// RunID is the workflow run.
	RunID string `json:"run_id"`
	// Step is the approval step's name.
	Step string `json:"step"`
	// StepRunID is the record of this approval step.
	StepRunID string `json:"step_run_id"`
	// Verdict is requested, approved, rejected, or timed_out.
	Verdict string `json:"verdict"`
	// SpecDigest is the workflow's spec digest.
	SpecDigest string `json:"spec_digest"`
	// StateDigest is the digest of the state the approver was shown.
	StateDigest string `json:"state_digest"`
	// DecisionID, ReasonCommitment, and SeparationOfDuties are what a run's decision commits beside
	// its verdict, for the same reasons. Each is omitted when empty, so a step entry recorded
	// without them, a request or a timeout among them, reduces to the bytes it always did.
	DecisionID         string                       `json:"decision_id,omitempty"`
	ReasonCommitment   string                       `json:"reason_commitment,omitempty"`
	SeparationOfDuties *decision.SeparationOfDuties `json:"separation_of_duties,omitempty"`
}

// StepDecisionBody assembles the canonical record for one approval step entry and returns its JSON
// with the state digest it embeds. It is exported so a receipt can rebuild the same bytes.
func StepDecisionBody(state *StepState, verdict string) (body []byte, stateDigest string, err error) {
	return StepDecisionBodyWith(state, verdict, DecisionExtras{})
}

// StepDecisionBodyWith is StepDecisionBody for a step decision recorded with a decision record.
func StepDecisionBodyWith(state *StepState, verdict string, extras DecisionExtras) (body []byte,
	stateDigest string, err error) {
	stateDigest, err = state.Digest()
	if err != nil {
		return nil, "", err
	}
	body, err = json.Marshal(StepDecisionRecord{
		RunID: state.RunID, Step: state.Step, StepRunID: state.StepRunID, Verdict: verdict,
		SpecDigest: state.SpecDigest, StateDigest: stateDigest, DecisionID: extras.ID,
		ReasonCommitment: extras.ReasonCommitment, SeparationOfDuties: extras.SeparationOfDuties,
	})
	if err != nil {
		return nil, "", err
	}
	return body, stateDigest, nil
}

// StepDecisionPath is the chain path an approval step entry is recorded at. It names the workflow
// run first, so everything that collects a run's entries by its id collects these too, and the step
// record second, so two approval steps in one workflow are told apart.
func StepDecisionPath(runID, stepRunID, verdict string) string {
	return "/runs/" + runID + "/steps/" + stepRunID + "/decision/" + verdict
}

// ParseStepDecisionPath reads a path written by StepDecisionPath. It reports ok=false for anything
// else, including a run's own decision path.
func ParseStepDecisionPath(path string) (runID, stepRunID, verdict string, ok bool) {
	rest, found := strings.CutPrefix(path, "/runs/")
	if !found {
		return "", "", "", false
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 5 || parts[1] != "steps" || parts[3] != "decision" || parts[0] == "" ||
		parts[2] == "" {
		return "", "", "", false
	}
	switch parts[4] {
	case StepRequested, StepApproved, StepRejected, StepTimedOut:
	default:
		return "", "", "", false
	}
	return parts[0], parts[2], parts[4], true
}

// CommitStepDecision records one approval step event as a chain entry naming who caused it and
// committing the decision body, and returns the state digest it committed. A decision is appended
// before it takes effect, fail-closed, as a run's approval is.
func CommitStepDecision(ctx context.Context, audits audit.Store, state *StepState, verdict string,
	by Decider, now func() time.Time) (string, error) {
	return CommitStepDecisionWith(ctx, audits, state, verdict, by, now, DecisionExtras{})
}

// CommitStepDecisionWith is CommitStepDecision for a step decision recorded with a decision record.
// The entry takes the record's id, so the record and the entry that committed it name each other.
func CommitStepDecisionWith(ctx context.Context, audits audit.Store, state *StepState,
	verdict string, by Decider, now func() time.Time, extras DecisionExtras) (string, error) {
	entry, stateDigest, err := StepDecisionEntry(state, verdict, by, now, extras)
	if err != nil {
		return "", err
	}
	if err := audits.Append(ctx, entry); err != nil {
		return "", err
	}
	return stateDigest, nil
}

// StepDecisionEntry builds the chain entry CommitStepDecisionWith appends, without appending it,
// and returns it with the state digest its body commits, for a step decision that is claimed
// before it is recorded, as DecisionEntry is for a run's.
func StepDecisionEntry(state *StepState, verdict string, by Decider, now func() time.Time,
	extras DecisionExtras) (*audit.Entry, string, error) {
	body, stateDigest, err := StepDecisionBodyWith(state, verdict, extras)
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
		Method: audit.MethodDecision, Path: StepDecisionPath(state.RunID, state.StepRunID, verdict),
		ContentDigest: digest, Nonce: nonce,
	}, stateDigest, nil
}
