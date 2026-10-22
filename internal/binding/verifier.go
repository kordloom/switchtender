package binding

import (
	"context"
	"errors"
	"fmt"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// readAttempts is how many times a store read is tried before the verifier reports that it could
// not verify. Attempts follow each other at once: no check reads a clock or waits on one.
const readAttempts = 3

// SealFunc returns the seal stored on a run, empty when it carries none.
type SealFunc func(r *run.Run) string

// EntryFunc reads one chain entry by id. It returns a nil entry and a nil error when the chain
// holds no entry with that id, and an error only for a read it could not make.
type EntryFunc func(ctx context.Context, id string) (*audit.Entry, error)

// RootsFunc returns the ids of the chain entries a workflow step derives from, in the order its
// seal names them. It returns an error only for a read it could not make.
type RootsFunc func(ctx context.Context, parent, child *run.Run) ([]string, error)

// Verifier holds the rows the executor acts on, and the rows a person decides on, to the seals the
// process made over them and to the chain entries those seals name. It is the one place a seal is
// recomputed. Every method returns nil, an error wrapping ErrDrift that names the row and what
// moved, or an error wrapping ErrUnverified that carries the store error a read ended on.
//
// A Verifier with a nil Key checks no seal, since none was minted, and still requires each chain
// entry a row names to exist and to record what the row says it does.
type Verifier struct {
	// Runs is the run store a child's parent is read from.
	Runs run.Store
	// Audits is the chain the roots are read from.
	Audits audit.Store
	// Key is the binding key, nil on an install with no producer identity.
	Key *Key
	// SpecSeal returns the spec seal stored on a run.
	SpecSeal SealFunc
	// ResultSeal returns the result seal stored on a finished run.
	ResultSeal SealFunc
	// Entry reads one entry of Audits by id.
	Entry EntryFunc
	// StepRoots returns the ids of the chain entries a workflow step derives from. Nil names none,
	// so a step sealed with roots is refused until it is set.
	StepRoots RootsFunc
}

// Executable checks a row the claim loop or the relay is about to execute. A child's parent is read
// through Runs and checked first. A top-level run that names a decision is held to that decision's
// entry, a shard to its parent, and a step to its parent and to the entries that released it. An
// approval step never executes, so one presented here is refused.
func (v *Verifier) Executable(ctx context.Context, r *run.Run) error {
	subject := "run " + r.ID
	if r.Kind == run.KindApproval {
		return drift(subject, "an approval step records a decision and never executes")
	}
	if r.ParentID == nil {
		return v.top(ctx, r, subject)
	}
	parent, err := v.parent(ctx, r, subject)
	if err != nil {
		return err
	}
	if r.StepIndex == nil {
		rec, err := ShardRecord(parent, r)
		if err != nil {
			return unverified(subject, "build its record", err)
		}
		return v.check(subject, rec, v.specSeal(r))
	}
	roots, err := v.stepRoots(ctx, parent, r, subject)
	if err != nil {
		return err
	}
	rec, err := StepRecord(parent, r, roots)
	if err != nil {
		return unverified(subject, "build its record", err)
	}
	return v.check(subject, rec, v.specSeal(r))
}

// Gate checks a workflow approval step a resumed walk is about to treat as approved. state is the
// workflow state rebuilt from the store, which must bind to what the approver decided on. The
// workflow is checked first, then the step's DECISION entry, which must record an approval of this
// step of this workflow.
func (v *Verifier) Gate(ctx context.Context, parent, node *run.Run, state *outcome.StepState) error {
	subject := fmt.Sprintf("approval step %q (run %s)", node.StepName, node.ID)
	if node.ParentID == nil || *node.ParentID != parent.ID {
		return drift(subject, "it is not a step of workflow "+parent.ID)
	}
	if parent.ParentID != nil {
		return drift(subject, "its workflow "+parent.ID+" is itself a child of another run")
	}
	if err := v.top(ctx, parent, subject+": workflow run "+parent.ID); err != nil {
		return err
	}
	if node.DecisionID == "" {
		return drift(subject, "it carries no decision")
	}
	root, err := v.root(ctx, subject, node.DecisionID, audit.MethodDecision,
		outcome.StepDecisionPath(parent.ID, node.ID, outcome.StepApproved))
	if err != nil {
		return err
	}
	if state == nil {
		return unverified(subject, "rebuild the workflow state", errors.New("no state given"))
	}
	stateBinding, err := state.Binding()
	if err != nil {
		return unverified(subject, "bind the workflow state", err)
	}
	rec, err := GateRecord(parent, node, stateBinding, root)
	if err != nil {
		return unverified(subject, "build its record", err)
	}
	return v.check(subject, rec, v.specSeal(node))
}

// Held checks a top-level run waiting for a decision, before anything about a decision on it is
// recorded. Its approval fields decide who may decide it, and an approval seals the run as it
// reads, so it is held to the seal it was submitted with, or to its decision's entry when it names
// one, the way Executable holds a top-level run. A child or an approval step is decided some other
// way, so one presented here is refused.
func (v *Verifier) Held(ctx context.Context, r *run.Run) error {
	subject := "run " + r.ID
	switch {
	case r.Kind == run.KindApproval:
		return drift(subject, "an approval step is decided as a step of its workflow")
	case r.ParentID != nil:
		return drift(subject, "a child is decided through its parent")
	}
	return v.top(ctx, r, subject)
}

// Pending checks a workflow approval step waiting for a decision, before anything about a decision
// on it is recorded. The step's approval fields decide who may decide it, and an approval seals the
// step as it reads, so the workflow is held to its own seal first and the step then to the one it
// was created with, which names no chain entry because nothing released it.
func (v *Verifier) Pending(ctx context.Context, parent, node *run.Run) error {
	subject := fmt.Sprintf("approval step %q (run %s)", node.StepName, node.ID)
	switch {
	case node.Kind != run.KindApproval:
		return drift(subject, "it is not an approval step")
	case node.ParentID == nil || *node.ParentID != parent.ID:
		return drift(subject, "it is not a step of workflow "+parent.ID)
	case parent.ParentID != nil:
		return drift(subject, "its workflow "+parent.ID+" is itself a child of another run")
	}
	if err := v.top(ctx, parent, subject+": workflow run "+parent.ID); err != nil {
		return err
	}
	rec, err := StepRecord(parent, node, nil)
	if err != nil {
		return unverified(subject, "build its record", err)
	}
	return v.check(subject, rec, v.specSeal(node))
}

// Result checks a finished step whose status and outputs a walk is about to feed forward. Only a
// succeeded step is held to a result seal: a sweep ends a step interrupted or canceled with no
// outputs and no key, and a step that did not succeed feeds nothing forward.
func (v *Verifier) Result(_ context.Context, child *run.Run) error {
	if child.Status != run.StatusSucceeded {
		return nil
	}
	subject := "run " + child.ID
	rec, err := ResultRecord(child)
	if err != nil {
		return unverified(subject, "build its record", err)
	}
	return v.check(subject, rec, v.resultSeal(child))
}

// top checks a top-level run: against its DECISION entry when it names one, as submitted when it
// does not.
func (v *Verifier) top(ctx context.Context, r *run.Run, subject string) error {
	if r.DecisionID == "" {
		rec, err := SubmitRecord(r)
		if err != nil {
			return unverified(subject, "build its record", err)
		}
		return v.check(subject, rec, v.specSeal(r))
	}
	root, err := v.root(ctx, subject, r.DecisionID, audit.MethodDecision,
		"/runs/"+r.ID+"/decision/"+outcome.StepApproved)
	if err != nil {
		return err
	}
	rec, err := RunRecord(r, root)
	if err != nil {
		return unverified(subject, "build its record", err)
	}
	return v.check(subject, rec, v.specSeal(r))
}

// parent reads and checks r's parent. A parent is a top-level run in every shape the dispatcher
// builds, so a parent that is itself a child is refused rather than followed.
func (v *Verifier) parent(ctx context.Context, r *run.Run, subject string) (*run.Run, error) {
	id := *r.ParentID
	var parent *run.Run
	err := retry(ctx, func() error {
		var gerr error
		parent, gerr = v.Runs.Get(ctx, id)
		return gerr
	})
	switch {
	case errors.Is(err, run.ErrNotFound):
		return nil, drift(subject, "its parent "+id+" is not in the store")
	case err != nil:
		return nil, unverified(subject, "read its parent "+id, err)
	case parent.ParentID != nil:
		return nil, drift(subject, "its parent "+id+" is itself a child of another run")
	}
	if err := v.top(ctx, parent, subject+": parent run "+id); err != nil {
		return nil, err
	}
	return parent, nil
}

// stepRoots reads each chain entry a step derives from.
func (v *Verifier) stepRoots(ctx context.Context, parent, child *run.Run, subject string) ([]Root,
	error) {
	if v.StepRoots == nil {
		return nil, nil
	}
	ids, err := v.StepRoots(ctx, parent, child)
	if err != nil {
		return nil, unverified(subject, "name the entries that released it", err)
	}
	roots := make([]Root, 0, len(ids))
	for _, id := range ids {
		root, err := v.root(ctx, subject, id, "", "")
		if err != nil {
			return nil, err
		}
		roots = append(roots, root)
	}
	return roots, nil
}

// root reads the chain entry id and returns the root naming it. A non-empty method or path must
// match the entry's, so an entry recording something else is refused by name even where no key
// checks the seal that would also catch it.
func (v *Verifier) root(ctx context.Context, subject, id, method, path string) (Root, error) {
	if v.Entry == nil {
		return Root{}, unverified(subject, "read chain entry "+id, errors.New("no chain reader"))
	}
	var e *audit.Entry
	err := retry(ctx, func() error {
		var rerr error
		e, rerr = v.Entry(ctx, id)
		return rerr
	})
	switch {
	case err != nil:
		return Root{}, unverified(subject, "read chain entry "+id, err)
	case e == nil:
		return Root{}, drift(subject, "chain entry "+id+" it derives from is not on the chain")
	case method != "" && e.Method != method, path != "" && e.Path != path:
		return Root{}, drift(subject, "chain entry "+id+" is not the decision this row names")
	}
	return RootOf(e), nil
}

// check compares the stored seal with the one rec makes under v's key. With no key there is no
// seal to compare.
func (v *Verifier) check(subject string, rec Record, stored string) error {
	if v.Key == nil {
		return nil
	}
	if stored == "" {
		return drift(subject, "it carries no seal, so this process did not build or release it")
	}
	if !v.Key.Verify(rec, stored) {
		return drift(subject, moved(rec.Kind))
	}
	return nil
}

// specSeal returns r's stored spec seal.
func (v *Verifier) specSeal(r *run.Run) string {
	if v.SpecSeal == nil {
		return ""
	}
	return v.SpecSeal(r)
}

// resultSeal returns r's stored result seal.
func (v *Verifier) resultSeal(r *run.Run) string {
	if v.ResultSeal == nil {
		return ""
	}
	return v.ResultSeal(r)
}

// moved says what a seal mismatch means for a record of kind.
func moved(kind string) string {
	switch kind {
	case KindSubmit:
		return "its spec or its approval fields changed after it was submitted"
	case KindRun:
		return "its spec, its approval fields, or its decision changed after it was approved"
	case KindGate:
		return "the workflow state or the decision changed after the step was approved"
	case KindShard:
		return "it or its split changed after the split was built"
	case KindStep:
		return "it, its workflow, or the entries that released it changed after it was built"
	case KindResult:
		return "its status or its outputs changed after it finished"
	}
	return "it changed after it was sealed"
}

// retry runs a store read up to readAttempts times, stopping early on success, on a run that is
// not found, which a retry cannot change, or on a context that has ended.
func retry(ctx context.Context, read func() error) error {
	var err error
	for range readAttempts {
		if err = read(); err == nil || errors.Is(err, run.ErrNotFound) || ctx.Err() != nil {
			return err
		}
	}
	return err
}

// drift returns an ErrDrift naming subject and what moved.
func drift(subject, what string) error {
	return fmt.Errorf("%w: %s: %s", ErrDrift, subject, what)
}

// unverified returns an ErrUnverified naming subject, the read that failed, and its error.
func unverified(subject, what string, err error) error {
	return fmt.Errorf("%w: %s: %s: %w", ErrUnverified, subject, what, err)
}
