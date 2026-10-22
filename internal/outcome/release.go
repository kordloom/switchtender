package outcome

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// ReleaseRecord is the canonical body a RELEASE entry commits: which step record of which workflow
// a decision released, at which position and attempt, the decisions it derives from, and digests of
// the spec and the inputs the step was built with. A receipt rebuilds it from the stored step, so a
// step rewritten after its release rebuilds to a body the committed digest refuses.
type ReleaseRecord struct {
	// RunID is the workflow run.
	RunID string `json:"run_id"`
	// StepRunID is the step record released.
	StepRunID string `json:"step_run_id"`
	// Step is the step's name in the graph.
	Step string `json:"step"`
	// Index is the step's position in the graph.
	Index int `json:"index"`
	// Attempt is which try of the step was released.
	Attempt int `json:"attempt"`
	// Roots are the decision entries that released the step, each by its id and the content digest
	// it committed, sorted by id, the order the release entry's id hashes the ids in.
	Roots []ReleaseRoot `json:"roots"`
	// SpecDigest is the step's spec digest, the value its workflow's outcome names for the step, so
	// a reader holds what ran to what was released.
	SpecDigest string `json:"spec_digest"`
	// InputsDigest is the digest of the vars the step was handed: its workflow's vars with the
	// outputs of the steps it waited behind layered on top. It is taken over their redacted
	// canonical form, so a value an upstream step published that looks like a secret is never the
	// input to a digest anybody is shown.
	InputsDigest string `json:"inputs_digest"`
}

// ReleaseRoot names one decision entry a step's release derives from: the entry's id, and the
// content digest the entry committed. A chain claim carries its content digest and no entry id, so
// the digest is what a reader of a receipt finds the decision by, and a decision entry rewritten on
// the chain after the release no longer matches the body the release committed.
type ReleaseRoot struct {
	// ID is the decision entry's id.
	ID string `json:"id"`
	// ContentDigest is the content digest the decision entry committed. It is empty when the entry
	// could not be found, so a body rebuilt without it fails its digest rather than being withheld.
	ContentDigest string `json:"content_digest"`
}

// RootsOf names each decision entry ids lists as a release root, with the content digest digest
// reads for its id, in the order ids lists them. digest must not be nil, and reports an empty
// string for an entry it cannot find, which the root then carries. ids is not modified.
func RootsOf(ids []string, digest func(id string) string) []ReleaseRoot {
	roots := make([]ReleaseRoot, len(ids))
	for i, id := range ids {
		roots[i] = ReleaseRoot{ID: id, ContentDigest: digest(id)}
	}
	return roots
}

// rootIDs returns the ids roots names, in their order.
func rootIDs(roots []ReleaseRoot) []string {
	ids := make([]string, len(roots))
	for i, root := range roots {
		ids[i] = root.ID
	}
	return ids
}

// compareRoots orders two release roots by id, then by content digest, so roots in any order
// encode to one body.
func compareRoots(a, b ReleaseRoot) int {
	return cmp.Or(strings.Compare(a.ID, b.ID), strings.Compare(a.ContentDigest, b.ContentDigest))
}

// ReleaseBody assembles the canonical release record for child, a workflow step, released by the
// decision entries roots names, and returns its JSON. It is exported so a receipt can rebuild the
// same bytes from the stored step. The roots may arrive in any order, and roots is not modified.
func ReleaseBody(child *run.Run, roots []ReleaseRoot) ([]byte, error) {
	if child.ParentID == nil || child.StepIndex == nil {
		return nil, fmt.Errorf("run %s is not a workflow step, so no decision releases it", child.ID)
	}
	specDigest, err := SpecDigest(child)
	if err != nil {
		return nil, err
	}
	inputsDigest, err := varsDigest(child.ExtraVars)
	if err != nil {
		return nil, err
	}
	sorted := slices.SortedFunc(slices.Values(roots), compareRoots)
	if sorted == nil {
		sorted = []ReleaseRoot{}
	}
	return json.Marshal(ReleaseRecord{
		RunID: *child.ParentID, StepRunID: child.ID, Step: child.StepName, Index: *child.StepIndex,
		Attempt: child.Attempt, Roots: sorted, SpecDigest: specDigest, InputsDigest: inputsDigest,
	})
}

// ReleaseEntry builds the RELEASE entry a step launch appends before the step's record is saved,
// without appending it. It commits the release body under a content digest, names who launched the
// step, and takes the id audit.ReleaseID fixes from the roots' ids, the workflow, the step's index,
// and its attempt, so a second launcher of the same attempt builds an entry the chain refuses as a
// duplicate. The id leaves the roots' digests out, so a launcher that read another digest for one
// of the decisions still meets the first launcher's entry rather than recording a second release.
// now reads the entry's time, and a nil function falls back to time.Now.
func ReleaseEntry(child *run.Run, roots []ReleaseRoot, by Decider,
	now func() time.Time) (*audit.Entry, error) {
	body, err := ReleaseBody(child, roots)
	if err != nil {
		return nil, err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return nil, err
	}
	if now == nil {
		now = time.Now
	}
	id := audit.ReleaseID(rootIDs(roots), *child.ParentID, *child.StepIndex, child.Attempt)
	return &audit.Entry{
		ID: id, At: now(),
		Actor: by.Name, ActorType: by.Type, OnBehalfOf: by.OnBehalfOf,
		Method: audit.MethodRelease, Path: audit.ReleasePath(*child.ParentID, child.ID),
		ContentDigest: digest, Nonce: nonce,
	}, nil
}

// ReleaseRoots returns the ids of the decision entries a workflow step derives from, read from the
// stored rows: the workflow's own decision when one won it, then, in declaration order, the
// decision that won each approval step the step waits behind, directly or through other steps over
// either kind of edge, read from that approval step's latest record. steps are the workflow's step
// records as the store lists them. A child outside the workflow's graph derives from the
// workflow's decision alone, and a step nothing decided on derives from nothing.
//
// It is the one rule both the launch that records a release and the receipt that rebuilds the
// release read, so the two cannot name different decisions. RootsOf adds the digest each decision
// committed.
func ReleaseRoots(parent, child *run.Run, steps []*run.Run) []string {
	var roots []string
	if parent.DecisionID != "" {
		roots = append(roots, parent.DecisionID)
	}
	graph := run.GraphSteps(parent.Steps)
	if child.StepIndex == nil || *child.StepIndex < 0 || *child.StepIndex >= len(graph) {
		return roots
	}
	latest := latestByIndex(parent.ID, steps)
	upstream := upstreamOf(graph, *child.StepIndex)
	for i, s := range graph {
		if !upstream[i] || !s.IsApproval() {
			continue
		}
		node, ok := latest[i]
		if !ok || node.Kind != run.KindApproval || node.DecisionID == "" {
			continue
		}
		roots = append(roots, node.DecisionID)
	}
	return roots
}

// latestByIndex returns the latest record of each step of the workflow parentID among steps, by
// step index: the highest attempt, and the one created last within an attempt, the record a walk
// settles a step from.
func latestByIndex(parentID string, steps []*run.Run) map[int]*run.Run {
	latest := map[int]*run.Run{}
	for _, c := range steps {
		if c == nil || c.StepIndex == nil || c.ParentID == nil || *c.ParentID != parentID {
			continue
		}
		prev, ok := latest[*c.StepIndex]
		if !ok || c.Attempt > prev.Attempt ||
			(c.Attempt == prev.Attempt && c.CreatedAt.After(prev.CreatedAt)) {
			latest[*c.StepIndex] = c
		}
	}
	return latest
}
