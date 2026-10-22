package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// AdoptionDeps is what adopting an install reads and writes, and who the chain names for it.
//
// Adoption seals, once per install, the rows a binary that held no binding key left behind, so a
// keyed process can hold them to seals the way it holds the rows it made itself. It is the one
// trusted moment in the life of an install's seals: everything it reads, it seals as found.
type AdoptionDeps struct {
	// Runs is the run store the rows are read from and sealed in.
	Runs run.Store
	// Audits is the chain the decision entries are read from, and the one the adoption record and
	// the RELEASE entries adoption owes are appended to.
	Audits audit.Store
	// Key is the install's binding key, which every seal is made under and which fixes the
	// adoption record's id.
	Key *binding.Key
	// Log receives each row adoption leaves unsealed and why. Nil logs nothing.
	Log *zap.Logger
	// Actor names who adopts. The adoption record names it, and each RELEASE entry adoption appends
	// names it acting on behalf of the workflow's own requester.
	Actor outcome.Decider
	// Now reads the time each RELEASE entry carries. Nil reads the wall clock.
	Now func() time.Time
}

// AdoptedRow is one row adoption seals, as the person running it is shown it.
type AdoptedRow struct {
	// ID is the row's id.
	ID string
	// Seal is the kind of seal written: a binding record kind, or binding.KindClaim for a decision
	// claim adoption completes.
	Seal string
	// Actor is who the row records as having asked for it.
	Actor string
	// SpecDigest is the row's spec digest as adoption read it.
	SpecDigest string
	// Release is the id of the RELEASE entry appended before the row is sealed, empty for none.
	Release string
	// DecidedBy names who made each decision the seal derives from, joined in the seal's order,
	// empty for none.
	DecidedBy string
	// DecisionEntry names the chain entries recording those decisions, joined in the same order,
	// empty for none.
	DecisionEntry string
	// Unrecorded reports a decision claim whose decision entry is not on the chain yet, so adoption
	// trusts a decision the chain does not hold.
	Unrecorded bool
}

// LeftRow is one row adoption leaves unsealed, which fails closed when a keyed process reaches it.
type LeftRow struct {
	// ID is the row's id.
	ID string
	// Reason says why it was left.
	Reason string
}

// AdoptionWork is what adopting an install would do, read from its rows and its chain and written
// nowhere: the rows it seals, the rows it leaves and why, and the rows that already carry a seal.
type AdoptionWork struct {
	// Rows are the rows adoption seals, in the order it seals them.
	Rows []AdoptedRow
	// Left are the rows adoption leaves unsealed, each with why.
	Left []LeftRow
	// Sealed are the ids of non-terminal rows that already carry a spec or result seal, sorted. On a
	// chain with no adoption record they mean the install was keyed before and its record is gone,
	// and Adopt refuses.
	Sealed []string
	// writes are the stored writes behind Rows, one per row in the same order.
	writes []adoptWrite
}

// Adopted is what one call of Adopt did.
type Adopted struct {
	// Record is the adoption record on the chain: the one this call appended, or the one an earlier
	// adoption appended, read back.
	Record *audit.Entry
	// Earlier reports that the chain already held the adoption record, so this call wrote nothing.
	Earlier bool
	// Work is what this call found to do, nil when Earlier.
	Work *AdoptionWork
	// Sealed are the ids of the rows this call sealed, in the order it sealed them.
	Sealed []string
	// Moved are the ids of the rows another process sealed, decided, or settled between this call's
	// read and its write, each left as that process left it.
	Moved []string
	// Left are the rows planned for a seal that this call could not write, each with why.
	Left []LeftRow
}

// adoptWrite is one row's stored write: the row as read and what SealAdopted stores on it, and for
// a step a decision released, the RELEASE entry appended first and what its seal is rebuilt from.
type adoptWrite struct {
	// row is the row as adoption read it.
	row *run.Run
	// adoption is what SealAdopted writes to the row.
	adoption run.Adoption
	// parent is the step's workflow, set only with release.
	parent *run.Run
	// release is the RELEASE entry appended before the seal, nil for none.
	release *audit.Entry
	// released names the decision entries the release derives from, by id and content digest.
	released []outcome.ReleaseRoot
	// roots name those decision entries, ahead of the release, in the order the step's seal does.
	roots []binding.Root
}

// adoptionBody is what the adoption record commits: the rows adoption gives a spec or result seal,
// the rows whose decision claim it completes, and the RELEASE entries it appends, each sorted.
type adoptionBody struct {
	// Runs are the ids of the rows given a spec or result seal.
	Runs []string `json:"runs"`
	// Claims are the ids of the rows whose decision claim was sealed.
	Claims []string `json:"claims"`
	// Releases are the ids of the RELEASE entries appended.
	Releases []string `json:"releases"`
}

// AdoptionPlan reads what adopting the install would do and writes nothing: every non-terminal row,
// and every finished step and approval step a non-terminal workflow reads forward, each given the
// one seal its kind is held to, from the row as found and the chain entries it names. A row whose
// decision entry is missing or records something else, or whose parent is gone or has ended, is
// left and listed. A store or chain read that fails ends the plan with its error, since a row left
// for a read that failed could never be adopted later.
func AdoptionPlan(ctx context.Context, deps AdoptionDeps) (*AdoptionWork, error) {
	deps.require()
	rows, err := deps.Runs.NonTerminal(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the rows to adopt: %w", err)
	}
	slices.SortFunc(rows, func(a, b *run.Run) int { return strings.Compare(a.ID, b.ID) })
	p := &adoptPlanner{deps: deps, live: map[string]*run.Run{}, steps: map[string][]*run.Run{},
		work: &AdoptionWork{}}
	for _, r := range rows {
		p.live[r.ID] = r
		if r.SpecSeal != "" || r.ResultSeal != "" {
			p.work.Sealed = append(p.work.Sealed, r.ID)
		}
	}
	for _, r := range rows {
		if err := p.row(ctx, r); err != nil {
			return nil, err
		}
	}
	for _, r := range rows {
		if r.ParentID != nil || r.Kind != run.KindPipeline {
			continue
		}
		steps, err := p.stepsOf(ctx, r)
		if err != nil {
			return nil, err
		}
		for _, s := range steps {
			if err := p.finished(ctx, r, s); err != nil {
				return nil, err
			}
		}
	}
	return p.work, nil
}

// Adopt adopts the install once. It reads the rows and computes every seal first, appends the
// adoption record under the id the key fixes, and only then writes, through SealAdopted, so a
// process that dies after the record leaves rows unsealed, which fail closed, and never a second
// trusted moment. A chain that already holds the record is reported as Earlier and nothing is
// written, which is also what a second process racing this one gets. A chain that holds no record
// beside rows that already carry a seal is refused, with nothing written, as binding.ErrDrift.
//
// A RELEASE entry a released step is owed is appended before that step's seal is written, as a
// launch appends it before saving the step. A row a write could not land on is listed in Left, and
// the errors are joined into the error returned beside the result.
func Adopt(ctx context.Context, deps AdoptionDeps) (*Adopted, error) {
	deps.require()
	if held, err := adoptionRecord(ctx, deps); err != nil || held != nil {
		return earlier(held, err)
	}
	work, err := AdoptionPlan(ctx, deps)
	if err != nil {
		return nil, err
	}
	if len(work.Sealed) > 0 {
		// A process adopting beside this one appends the record before it seals anything, so seals
		// found with no record are first checked against the chain again.
		if held, err := adoptionRecord(ctx, deps); err != nil || held != nil {
			return earlier(held, err)
		}
		return nil, fmt.Errorf("%w: the chain holds no adoption record, and %d non-terminal rows "+
			"already carry a seal (%s): restore the chain from the backup taken before the upgrade "+
			"rather than adopt again", binding.ErrDrift, len(work.Sealed),
			strings.Join(work.Sealed, ", "))
	}
	record, err := work.record(deps)
	if err != nil {
		return nil, err
	}
	mine, err := appendRecord(ctx, deps, record)
	if err != nil {
		return nil, err
	}
	if !mine {
		held, err := adoptionRecord(ctx, deps)
		return earlier(held, err)
	}
	w := &adoptWriter{deps: deps, res: &Adopted{Record: record, Work: work}}
	var errs []error
	for i := range work.writes {
		if err := w.write(ctx, &work.writes[i]); err != nil {
			errs = append(errs, err)
		}
	}
	return w.res, errors.Join(errs...)
}

// ErrAdoptionPending is returned by RecordEmpty for an install with rows adoption would seal, which
// only an operator running adopt may trust.
var ErrAdoptionPending = errors.New("rows wait to be adopted")

// RecordEmpty appends an adoption record that seals nothing, for a keyed install whose start finds
// nothing to adopt. The record extends trust to no row. It closes the install's one trusted moment,
// so a row a writer later strips of its seal is refused when it is reached rather than offered to
// adopt as work an earlier release left. A chain that already holds the record, including one a
// second process racing this one appended, is left as it is. It refuses, writing nothing, an
// install with rows adoption would seal (ErrAdoptionPending) and one whose non-terminal rows carry
// a seal with no record beside them (binding.ErrDrift), since every keyed install records adoption
// at its first start and such a record was removed.
func RecordEmpty(ctx context.Context, deps AdoptionDeps) error {
	deps.require()
	if held, err := adoptionRecord(ctx, deps); err != nil || held != nil {
		return err
	}
	work, err := AdoptionPlan(ctx, deps)
	if err != nil {
		return err
	}
	switch {
	case len(work.Sealed) > 0:
		return fmt.Errorf("%w: the chain holds no adoption record, and %d non-terminal rows "+
			"already carry a seal (%s)", binding.ErrDrift, len(work.Sealed),
			strings.Join(work.Sealed, ", "))
	case len(work.Rows) > 0:
		return fmt.Errorf("%w: %d rows", ErrAdoptionPending, len(work.Rows))
	}
	record, err := work.record(deps)
	if err != nil {
		return err
	}
	_, err = appendRecord(ctx, deps, record)
	return err
}

// Recorded reports whether the chain holds the adoption record deps.Key fixes the id of.
func Recorded(ctx context.Context, deps AdoptionDeps) (bool, error) {
	held, err := adoptionRecord(ctx, deps)
	return held != nil, err
}

// NotAdoptedText is the failure every row FailUnsealed ends records.
const NotAdoptedText = "not adopted: this install shows keyed operation began, so this row was " +
	"not trusted"

// failUnsealedOwner is the lease FailUnsealed takes a row under to end it.
const failUnsealedOwner = "switchtender adopt --fail-unsealed"

// UnsealedRow is one row FailUnsealed ends, as the person running it is shown it.
type UnsealedRow struct {
	// ID is the row's id.
	ID string
	// Status is the status the row was read in.
	Status run.Status
	// Actor is who the row records as having asked for it.
	Actor string
	// parked reports a workflow held at an approval step after it started, which is taken under a
	// lease to end it rather than settled as a held run.
	parked bool
	// top reports a row with no parent, whose outcome is committed once it ends.
	top bool
}

// UnsealedWork is what FailUnsealed would do, read from the install's rows and written nowhere.
type UnsealedWork struct {
	// Fail are the unsealed non-terminal rows FailUnsealed ends failed, sorted by id.
	Fail []UnsealedRow
	// Left are the unsealed non-terminal rows it leaves as they are, each with why.
	Left []LeftRow
	// Sealed are the ids of non-terminal rows that carry a seal, sorted. They are left for a keyed
	// server to verify when it reaches them.
	Sealed []string
}

// Unadopted is what one call of FailUnsealed did.
type Unadopted struct {
	// Record is the adoption record on the chain: the one this call appended, or the one an earlier
	// adoption appended, read back.
	Record *audit.Entry
	// Earlier reports that the chain already held the adoption record, so this call wrote nothing.
	Earlier bool
	// Work is what this call found to do, nil when Earlier.
	Work *UnsealedWork
	// Failed are the ids of the rows this call ended, in the order it ended them.
	Failed []string
	// Moved are the ids of the rows another process moved between this call's read and its write,
	// each left as that process left it.
	Moved []string
	// Left are the rows planned for failing that this call could not end, each with why.
	Left []LeftRow
}

// UnsealedPlan reads what FailUnsealed would do and writes nothing: which unsealed non-terminal rows
// it ends, which it leaves and why, and which carry a seal a keyed server checks itself.
func UnsealedPlan(ctx context.Context, deps AdoptionDeps) (*UnsealedWork, error) {
	deps.require()
	rows, err := deps.Runs.NonTerminal(ctx)
	if err != nil {
		return nil, fmt.Errorf("list the unfinished rows: %w", err)
	}
	slices.SortFunc(rows, func(a, b *run.Run) int { return strings.Compare(a.ID, b.ID) })
	work := &UnsealedWork{}
	for _, r := range rows {
		if CarriesSeal(r) {
			work.Sealed = append(work.Sealed, r.ID)
			continue
		}
		why := ""
		switch {
		case r.Status == run.StatusRunning:
			why = "running under a lease: the lease sweep ends it, and a server of this release " +
				"refuses it as carrying no seal if it reaches it first"
		case r.InFlight():
			why = "a decision is in flight on it, which a server of this release refuses as " +
				"carrying no seal"
		case r.CancelRequested:
			why = "a cancel was requested, which ends it"
		case r.Status == run.StatusPendingApproval && r.ClaimedBy != "":
			why = "held under a lease, which the lease sweep ends"
		}
		if why != "" {
			work.Left = append(work.Left, LeftRow{ID: r.ID, Reason: why})
			continue
		}
		row := UnsealedRow{ID: r.ID, Status: r.Status, Actor: r.Actor, top: r.ParentID == nil}
		if r.Status == run.StatusPendingApproval && r.Kind == run.KindPipeline {
			if row.parked, err = run.Started(ctx, deps.Runs, r.ID); err != nil {
				return nil, fmt.Errorf("read the steps of workflow %s: %w", r.ID, err)
			}
		}
		work.Fail = append(work.Fail, row)
	}
	return work, nil
}

// FailUnsealed closes an install's adoption while trusting no row, for an install adopt refuses
// because it shows keyed operation began but whose unsealed rows lock its keyed servers out. It
// appends the adoption record with nothing adopted, then ends every unsealed non-terminal row it
// can failed with NotAdoptedText, each in one write fenced on the status it was read in. Nothing is
// sealed, so a row a writer planted a seal on is left for a keyed server to verify. A chain that
// already holds the record is reported as Earlier and nothing is written. A row a write could not
// land on is listed in Left, and the errors are joined into the error returned beside the result.
func FailUnsealed(ctx context.Context, deps AdoptionDeps) (*Unadopted, error) {
	deps.require()
	if held, err := adoptionRecord(ctx, deps); err != nil || held != nil {
		return unadoptedEarlier(held, err)
	}
	work, err := UnsealedPlan(ctx, deps)
	if err != nil {
		return nil, err
	}
	record, err := (&AdoptionWork{}).record(deps)
	if err != nil {
		return nil, err
	}
	mine, err := appendRecord(ctx, deps, record)
	if err != nil {
		return nil, err
	}
	if !mine {
		held, err := adoptionRecord(ctx, deps)
		return unadoptedEarlier(held, err)
	}
	res := &Unadopted{Record: record, Work: work}
	var errs []error
	for _, row := range work.Fail {
		moved, err := failUnsealedRow(ctx, deps, row)
		switch {
		case err != nil:
			res.Left = append(res.Left, LeftRow{ID: row.ID, Reason: err.Error()})
			errs = append(errs, fmt.Errorf("fail row %s: %w", row.ID, err))
		case !moved:
			res.Moved = append(res.Moved, row.ID)
		default:
			res.Failed = append(res.Failed, row.ID)
			commitFailedOutcome(ctx, deps, row)
		}
	}
	return res, errors.Join(errs...)
}

// unadoptedEarlier reports an adoption the chain already records, or the error the read of it
// ended on.
func unadoptedEarlier(held *audit.Entry, err error) (*Unadopted, error) {
	if err != nil {
		return nil, err
	}
	return &Unadopted{Record: held, Earlier: true}, nil
}

// failUnsealedRow ends row failed with NotAdoptedText, reporting false when the row was no longer in
// the status it was read in. A queued run and a workflow parked at an approval step are taken under
// a lease and ended under it, as a refused run is, so a stale claimant's start finds it gone. A run
// held before it started is settled as a held run. A row taken under the lease whose end then fails
// is left running under a lease nobody renews, which the lease sweep ends.
func failUnsealedRow(ctx context.Context, deps AdoptionDeps, row UnsealedRow) (bool, error) {
	fin := run.Finalization{Status: run.StatusFailed, Error: NotAdoptedText, EndedAt: deps.now()}
	if row.Status == run.StatusPendingApproval && !row.parked {
		return deps.Runs.SettleHeld(ctx, row.ID, fin)
	}
	ok, err := deps.Runs.TransitionStatusAndClaim(ctx, row.ID, row.Status, run.StatusRunning,
		failUnsealedOwner, time.Time{})
	if err != nil || !ok {
		return false, err
	}
	fin.Owner = failUnsealedOwner
	moved, err := deps.Runs.FinalizeRunning(ctx, row.ID, fin)
	if err == nil && !moved {
		err = errors.New("taken under a lease but not ended, so the lease sweep ends it")
	}
	return moved, err
}

// commitFailedOutcome commits the outcome of a top-level row FailUnsealed ended. A commit that
// fails leaves the outcome owed, which the janitor of the next server to start commits.
func commitFailedOutcome(ctx context.Context, deps AdoptionDeps, row UnsealedRow) {
	if !row.top {
		return
	}
	r, err := deps.Runs.Get(ctx, row.ID)
	if err == nil {
		err = outcome.CommitOwed(ctx, deps.Audits, deps.Runs, r, deps.Actor.Name, deps.now)
	}
	if err != nil {
		deps.log().Warn("adopt: commit the outcome of a row failed as not adopted, the janitor "+
			"commits it: "+err.Error(), zap.String("run_id", row.ID))
	}
}

// require panics on deps that cannot adopt anything, which is a caller's mistake.
func (deps AdoptionDeps) require() {
	if deps.Runs == nil || deps.Audits == nil || deps.Key == nil {
		panic("dispatch: adoption needs a run store, a chain, and a binding key")
	}
}

// log returns deps' logger, a no-op one when none was given.
func (deps AdoptionDeps) log() *zap.Logger {
	if deps.Log == nil {
		return zap.NewNop()
	}
	return deps.Log
}

// now reads deps' clock, the wall clock when none was given.
func (deps AdoptionDeps) now() time.Time {
	if deps.Now == nil {
		return time.Now()
	}
	return deps.Now()
}

// earlier reports an adoption the chain already records, or the error the read of it ended on.
func earlier(held *audit.Entry, err error) (*Adopted, error) {
	if err != nil {
		return nil, err
	}
	return &Adopted{Record: held, Earlier: true}, nil
}

// adoptionRecord reads the adoption record by the id deps.Key fixes, nil when the chain holds none.
func adoptionRecord(ctx context.Context, deps AdoptionDeps) (*audit.Entry, error) {
	if deps.Audits == nil || deps.Key == nil {
		panic("dispatch: adoption needs a chain and a binding key")
	}
	var held *audit.Entry
	err := withRetries(func() error {
		var rerr error
		held, rerr = chainEntryOf(deps.Audits)(ctx, deps.Key.AdoptionID())
		return rerr
	})
	if err != nil {
		return nil, fmt.Errorf("read the adoption record: %w", err)
	}
	return held, nil
}

// appendRecord appends the adoption record and reports whether this call's record is the one the
// chain holds. An append refused as a duplicate is read back: the same commitment is an earlier
// try of this append whose answer was lost, and anything else is another adoption's record.
func appendRecord(ctx context.Context, deps AdoptionDeps, record *audit.Entry) (bool, error) {
	dup := false
	err := withRetries(func() error {
		aerr := deps.Audits.Append(ctx, record)
		if errors.Is(aerr, audit.ErrDuplicateID) {
			dup = true
			return nil
		}
		return aerr
	})
	if err != nil {
		return false, fmt.Errorf("record the adoption on the chain: %w", err)
	}
	if !dup {
		return true, nil
	}
	held, err := adoptionRecord(ctx, deps)
	if err != nil {
		return false, err
	}
	return held != nil && held.ContentDigest == record.ContentDigest, nil
}

// record builds the adoption record for w under deps: a CLI entry at audit.AdoptedPath, under the
// id deps.Key fixes, naming deps.Actor, committing the body that lists what w seals and releases.
// Its time is left for the chain to stamp as it appends.
func (w *AdoptionWork) record(deps AdoptionDeps) (*audit.Entry, error) {
	body := adoptionBody{Runs: []string{}, Claims: []string{}, Releases: []string{}}
	for _, write := range w.writes {
		if write.adoption.Claim != "" {
			body.Claims = append(body.Claims, write.row.ID)
		} else {
			body.Runs = append(body.Runs, write.row.ID)
		}
		if write.release != nil {
			body.Releases = append(body.Releases, write.release.ID)
		}
	}
	slices.Sort(body.Runs)
	slices.Sort(body.Claims)
	slices.Sort(body.Releases)
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("encode the adoption record: %w", err)
	}
	digest, nonce, err := audit.ContentDigestOf(raw)
	if err != nil {
		return nil, fmt.Errorf("commit the adoption record: %w", err)
	}
	return &audit.Entry{ID: deps.Key.AdoptionID(), Actor: deps.Actor.Name,
		ActorType: deps.Actor.Type, OnBehalfOf: deps.Actor.OnBehalfOf, Method: audit.MethodCLI,
		Path: audit.AdoptedPath, ContentDigest: digest, Nonce: nonce}, nil
}

// adoptPlanner walks an install's rows once and builds its adoption.
type adoptPlanner struct {
	// deps is what the walk reads and seals with.
	deps AdoptionDeps
	// live holds every non-terminal row by id, as listed once at the start.
	live map[string]*run.Run
	// steps holds each workflow's step records by the workflow's id, read once.
	steps map[string][]*run.Run
	// work is the adoption being built.
	work *AdoptionWork
	// decided are the decision entries the row being planned derives from, in its seal's order,
	// which the row is shown with once it is added.
	decided []*audit.Entry
	// unrecorded reports that the row being planned is a decision claim whose decision entry is not
	// on the chain.
	unrecorded bool
}

// row plans one non-terminal row by the rule its kind is held to.
func (p *adoptPlanner) row(ctx context.Context, r *run.Run) error {
	p.decided, p.unrecorded = nil, false
	if r.ParentID == nil {
		return p.top(ctx, r)
	}
	parent, why, err := p.parentOf(ctx, r)
	switch {
	case err != nil:
		return err
	case why != "":
		p.leave(r.ID, why)
		return nil
	case r.Kind == run.KindApproval:
		return p.node(ctx, parent, r)
	case r.StepIndex == nil:
		p.shard(parent, r)
		return nil
	}
	return p.step(ctx, parent, r)
}

// top plans a top-level row: its decision claim when one is in flight, which is completed and
// nothing else, and otherwise a seal over the row as submitted, or as approved by the DECISION
// entry it names.
func (p *adoptPlanner) top(ctx context.Context, r *run.Run) error {
	switch {
	case r.Kind == run.KindApproval:
		p.leave(r.ID, "it is an approval step with no workflow")
		return nil
	case r.InFlight():
		return p.claim(ctx, nil, r)
	case r.SpecSeal != "":
		return nil
	case r.DecisionID == "":
		submitted, err := p.asSubmitted(ctx, r)
		if err != nil {
			return err
		}
		p.seal(r, binding.KindSubmit, func() (binding.Record, error) {
			return binding.SubmitRecord(submitted)
		})
		return nil
	}
	root, why, err := p.root(ctx, r.DecisionID, func(e *audit.Entry) bool {
		return e.Path == "/runs/"+r.ID+"/decision/"+outcome.StepApproved
	})
	switch {
	case err != nil:
		return err
	case why != "":
		p.leave(r.ID, why)
		return nil
	}
	p.seal(r, binding.KindRun, func() (binding.Record, error) { return binding.RunRecord(r, root) })
	return nil
}

// asSubmitted returns r as it read when it was submitted, as far as its seal marks it held. A
// workflow waiting at one of its approval steps reads as held, but it was submitted queued and
// parked once its walk reached the step, so it is sealed as queued, the way a workflow the live
// submit path builds is. A workflow held at submission has no steps until a decision releases it,
// and a released one names that decision, so a held workflow with steps is one parked at a step.
// r is not changed.
func (p *adoptPlanner) asSubmitted(ctx context.Context, r *run.Run) (*run.Run, error) {
	if r.Kind != run.KindPipeline || r.Status != run.StatusPendingApproval {
		return r, nil
	}
	steps, err := p.stepsOf(ctx, r)
	if err != nil || len(steps) == 0 {
		return r, err
	}
	parked := *r
	parked.Status = run.StatusPending
	return &parked, nil
}

// node plans an approval step waiting for a decision: its claim when one is in flight, and
// otherwise the seal it is created with, which names no chain entry.
func (p *adoptPlanner) node(ctx context.Context, parent, n *run.Run) error {
	switch {
	case parent.Kind != run.KindPipeline:
		p.leave(n.ID, "its parent "+parent.ID+" is not a workflow")
	case n.Status != run.StatusPendingApproval:
		p.leave(n.ID, fmt.Sprintf("it is an approval step that is %s", n.Status))
	case n.InFlight():
		return p.claim(ctx, parent, n)
	case n.SpecSeal == "":
		p.seal(n, binding.KindStep, func() (binding.Record, error) {
			return binding.StepRecord(parent, n, nil)
		})
	}
	return nil
}

// shard plans a shard of a split, sealed to its split as found.
func (p *adoptPlanner) shard(parent, s *run.Run) {
	switch {
	case parent.Kind != run.KindSplit:
		p.leave(s.ID, "its parent "+parent.ID+" is not a split")
	case s.SpecSeal == "":
		p.seal(s, binding.KindShard, func() (binding.Record, error) {
			return binding.ShardRecord(parent, s)
		})
	}
}

// step plans a workflow step not yet finished. A step no decision released is sealed to its
// workflow alone. A step a decision released is owed the RELEASE entry its launch would have
// appended, under the id audit.ReleaseID fixes, and is sealed to its workflow, to the decision
// entries read by id, and to that entry, which is appended before the seal is written.
func (p *adoptPlanner) step(ctx context.Context, parent, s *run.Run) error {
	switch {
	case parent.Kind != run.KindPipeline:
		p.leave(s.ID, "its parent "+parent.ID+" is not a workflow")
		return nil
	case s.SpecSeal != "":
		return nil
	}
	steps, err := p.stepsOf(ctx, parent)
	if err != nil {
		return err
	}
	ids := outcome.ReleaseRoots(parent, s, steps)
	if len(ids) == 0 {
		p.seal(s, binding.KindStep, func() (binding.Record, error) {
			return binding.StepRecord(parent, s, nil)
		})
		return nil
	}
	roots := make([]binding.Root, 0, len(ids))
	for _, id := range ids {
		root, why, err := p.root(ctx, id, func(e *audit.Entry) bool { return decides(e, parent) })
		switch {
		case err != nil:
			return err
		case why != "":
			p.leave(s.ID, why)
			return nil
		}
		roots = append(roots, root)
	}
	by := outcome.Decider{Name: p.deps.Actor.Name, Type: p.deps.Actor.Type, OnBehalfOf: parent.Actor}
	released := releaseRootsOf(roots)
	entry, err := outcome.ReleaseEntry(s, released, by, p.deps.Now)
	if err != nil {
		p.leave(s.ID, "its release could not be built: "+err.Error())
		return nil
	}
	rec, err := binding.StepRecord(parent, s,
		append(slices.Clone(roots), binding.PreparedRootOf(entry)))
	if err != nil {
		p.leave(s.ID, "its record could not be built: "+err.Error())
		return nil
	}
	p.add(adoptWrite{row: s, adoption: run.Adoption{SpecSeal: p.deps.Key.Seal(rec)}, parent: parent,
		release: entry, released: released, roots: roots}, binding.KindStep)
	return nil
}

// finished plans a step of the non-terminal workflow parent that has ended, which a resumed walk
// reads forward: a succeeded step's status and outputs under a result seal, and an approved
// approval step under the seal over the state it was approved on and its DECISION entry. Nothing
// reads a seal from a step that ended any other way.
func (p *adoptPlanner) finished(ctx context.Context, parent, s *run.Run) error {
	p.decided, p.unrecorded = nil, false
	if _, live := p.live[s.ID]; live || s.Status != run.StatusSucceeded {
		return nil
	}
	if s.Kind == run.KindApproval {
		if s.DecisionID == "" || s.SpecSeal != "" {
			return nil
		}
		return p.gate(ctx, parent, s)
	}
	if s.ResultSeal != "" {
		return nil
	}
	// The outputs are cleaned once on a copy, the way the store cleaned them, so the seal covers
	// what a walk reads back.
	ended := *s
	seal, err := SealResult(p.deps.Key, &ended, run.StatusSucceeded)
	if err != nil {
		p.leave(s.ID, "its result could not be sealed: "+err.Error())
		return nil
	}
	p.add(adoptWrite{row: s, adoption: run.Adoption{ResultSeal: seal}}, binding.KindResult)
	return nil
}

// gate plans an approved approval step: the workflow state rebuilt now must bind to what the step
// was stamped as approved on, and its DECISION entry must record that approval.
func (p *adoptPlanner) gate(ctx context.Context, parent, n *run.Run) error {
	state, err := outcome.StepStateOf(ctx, p.deps.Runs, parent, n)
	if err != nil {
		p.leave(n.ID, "the workflow state it was decided on could not be rebuilt: "+err.Error())
		return nil
	}
	stateBinding, err := state.Binding()
	switch {
	case err != nil:
		p.leave(n.ID, "its workflow state could not be bound: "+err.Error())
		return nil
	case stateBinding != n.ApprovedSpecBinding:
		p.leave(n.ID, "the workflow no longer binds to the state the step was approved on")
		return nil
	}
	path := outcome.StepDecisionPath(parent.ID, n.ID, outcome.StepApproved)
	root, why, err := p.root(ctx, n.DecisionID, func(e *audit.Entry) bool { return e.Path == path })
	switch {
	case err != nil:
		return err
	case why != "":
		p.leave(n.ID, why)
		return nil
	}
	p.seal(n, binding.KindGate, func() (binding.Record, error) {
		return binding.GateRecord(parent, n, stateBinding, root)
	})
	return nil
}

// claim plans the completion of a decision claim a process with no binding key stored on r and
// never settled: the seal decide would have put in it, over what an approval releases and the
// entry the claim carries, and the claim seal over the result, which every finisher checks before
// it records the decision. The row's own seal is left empty, since the decision stamps it when it
// settles. A claim with a claim seal was made by a keyed process and is left as it is. The row is
// shown with the decision its claim carries, and whether the chain holds that decision yet.
func (p *adoptPlanner) claim(ctx context.Context, parent, r *run.Run) error {
	c, err := claimOf(r)
	if err != nil {
		p.leave(r.ID, err.Error())
		return nil
	}
	if c.ClaimSeal != "" {
		return nil
	}
	var held *audit.Entry
	err = withRetries(func() error {
		var rerr error
		held, rerr = chainEntryOf(p.deps.Audits)(ctx, c.ID)
		return rerr
	})
	if err != nil {
		return fmt.Errorf("read chain entry %s: %w", c.ID, err)
	}
	p.unrecorded = held == nil
	if c.Entry != nil {
		p.decided = []*audit.Entry{c.Entry.entry()}
	}
	path := "/runs/" + r.ID + "/decision/" + c.Verdict
	if parent != nil {
		path = outcome.StepDecisionPath(parent.ID, r.ID, c.Verdict)
	}
	if c.Step != (parent != nil) || (c.Entry != nil && (c.Entry.ID != c.ID ||
		c.Entry.Method != audit.MethodDecision || c.Entry.Path != path)) {
		p.leave(r.ID, "its decision claim records a decision on something else")
		return nil
	}
	c.Seal = ""
	if c.Verdict == outcome.StepApproved && c.Entry != nil {
		root := binding.PreparedRootOf(c.Entry.entry())
		var rec binding.Record
		// The claim stamps its digest and binding when it settles, so the seal covers the row as
		// it reads then.
		stamped := binding.AsStamped(r, c.Digest, c.Binding)
		if parent == nil {
			rec, err = binding.RunRecord(stamped, root)
		} else {
			rec, err = binding.GateRecord(parent, stamped, c.Binding, root)
		}
		if err != nil {
			p.leave(r.ID, "its record could not be built: "+err.Error())
			return nil
		}
		c.Seal = p.deps.Key.Seal(rec)
	}
	raw, err := claimBytes(c)
	if err != nil {
		p.leave(r.ID, "its decision claim could not be encoded: "+err.Error())
		return nil
	}
	c.ClaimSeal = p.deps.Key.SealClaim(r.ID, raw)
	sealed, err := json.Marshal(c)
	if err != nil {
		p.leave(r.ID, "its decision claim could not be encoded: "+err.Error())
		return nil
	}
	p.add(adoptWrite{row: r, adoption: run.Adoption{Claim: string(sealed),
		PrevClaim: r.DecisionClaim}}, binding.KindClaim)
	return nil
}

// seal plans a spec seal of kind over the record build returns for r.
func (p *adoptPlanner) seal(r *run.Run, kind string, build func() (binding.Record, error)) {
	rec, err := build()
	if err != nil {
		p.leave(r.ID, "its record could not be built: "+err.Error())
		return
	}
	p.add(adoptWrite{row: r, adoption: run.Adoption{SpecSeal: p.deps.Key.Seal(rec)}}, kind)
}

// add plans w, shown as a seal of kind.
func (p *adoptPlanner) add(w adoptWrite, kind string) {
	digest, err := outcome.SpecDigest(w.row)
	if err != nil {
		p.leave(w.row.ID, "its spec could not be digested: "+err.Error())
		return
	}
	shown := AdoptedRow{ID: w.row.ID, Seal: kind, Actor: w.row.Actor, SpecDigest: digest,
		Unrecorded: p.unrecorded}
	if w.release != nil {
		shown.Release = w.release.ID
	}
	by := make([]string, 0, len(p.decided))
	ids := make([]string, 0, len(p.decided))
	for _, e := range p.decided {
		by = append(by, e.Actor)
		ids = append(ids, e.ID)
	}
	shown.DecidedBy, shown.DecisionEntry = strings.Join(by, ", "), strings.Join(ids, ", ")
	p.decided, p.unrecorded = nil, false
	p.work.Rows = append(p.work.Rows, shown)
	p.work.writes = append(p.work.writes, w)
}

// leave lists the row id as left unsealed for why.
func (p *adoptPlanner) leave(id, why string) {
	p.decided, p.unrecorded = nil, false
	p.work.Left = append(p.work.Left, LeftRow{ID: id, Reason: why})
	p.deps.log().Warn("dispatch: adoption leaves a row unsealed: "+why, zap.String("run_id", id))
}

// parentOf returns r's parent, from the rows already listed or read from the store, with why r is
// left instead when its parent is gone, has ended, or is itself a child.
func (p *adoptPlanner) parentOf(ctx context.Context, r *run.Run) (*run.Run, string, error) {
	id := *r.ParentID
	parent, ok := p.live[id]
	if !ok {
		err := withRetries(func() error {
			var gerr error
			parent, gerr = p.deps.Runs.Get(ctx, id)
			if errors.Is(gerr, run.ErrNotFound) {
				parent, gerr = nil, nil
			}
			return gerr
		})
		if err != nil {
			return nil, "", fmt.Errorf("read the parent %s of %s: %w", id, r.ID, err)
		}
	}
	switch {
	case parent == nil:
		return nil, "its parent " + id + " is not in the store", nil
	case parent.ParentID != nil:
		return nil, "its parent " + id + " is itself a child of another run", nil
	case parent.Status.Terminal():
		return nil, fmt.Sprintf("its parent %s is %s", id, parent.Status), nil
	}
	return parent, "", nil
}

// stepsOf returns the step records of the workflow parent, read once.
func (p *adoptPlanner) stepsOf(ctx context.Context, parent *run.Run) ([]*run.Run, error) {
	if steps, ok := p.steps[parent.ID]; ok {
		return steps, nil
	}
	var steps []*run.Run
	err := withRetries(func() error {
		var serr error
		steps, serr = p.deps.Runs.Steps(ctx, parent.ID)
		return serr
	})
	if err != nil {
		return nil, fmt.Errorf("read the steps of workflow %s: %w", parent.ID, err)
	}
	p.steps[parent.ID] = steps
	return steps, nil
}

// root reads the chain entry id and returns the root naming it, with why the row naming it is left
// instead when the chain holds no such entry or it is not a DECISION that ok accepts.
func (p *adoptPlanner) root(ctx context.Context, id string, ok func(e *audit.Entry) bool) (
	binding.Root, string, error) {
	var e *audit.Entry
	err := withRetries(func() error {
		var rerr error
		e, rerr = chainEntryOf(p.deps.Audits)(ctx, id)
		return rerr
	})
	switch {
	case err != nil:
		return binding.Root{}, "", fmt.Errorf("read chain entry %s: %w", id, err)
	case e == nil:
		return binding.Root{}, "the decision entry " + id + " it names is not on the chain", nil
	case e.Method != audit.MethodDecision || !ok(e):
		return binding.Root{}, "chain entry " + id + " is not the decision it names", nil
	}
	p.decided = append(p.decided, e)
	return binding.RootOf(e), "", nil
}

// decides reports whether e is a decision that can release a step of the workflow parent: the
// workflow's own approval, or a verdict on one of its approval steps.
func decides(e *audit.Entry, parent *run.Run) bool {
	if e.Path == "/runs/"+parent.ID+"/decision/"+outcome.StepApproved {
		return true
	}
	runID, _, verdict, ok := outcome.ParseStepDecisionPath(e.Path)
	return ok && runID == parent.ID && verdict != outcome.StepRequested
}

// adoptWriter performs an adoption's writes once its record is on the chain.
type adoptWriter struct {
	// deps is what the writes go to.
	deps AdoptionDeps
	// res is what the writes did.
	res *Adopted
}

// write performs one row's write: its RELEASE entry first when it is owed one, with the step's
// seal rebuilt over the entry the chain holds, and then the fenced seal.
func (w *adoptWriter) write(ctx context.Context, aw *adoptWrite) error {
	a := aw.adoption
	if aw.release != nil {
		root, why, err := w.release(ctx, aw)
		if err != nil {
			w.leave(aw.row.ID, "its RELEASE entry could not be recorded on the chain")
			return fmt.Errorf("release step %s: %w", aw.row.ID, err)
		}
		if why != "" {
			w.leave(aw.row.ID, why)
			return nil
		}
		rec, err := binding.StepRecord(aw.parent, aw.row, append(slices.Clone(aw.roots), root))
		if err != nil {
			w.leave(aw.row.ID, "its record could not be built: "+err.Error())
			return fmt.Errorf("seal step %s: %w", aw.row.ID, err)
		}
		a.SpecSeal = w.deps.Key.Seal(rec)
	}
	return w.seal(ctx, aw.row.ID, a)
}

// release appends the RELEASE entry aw is owed and returns the root naming the entry the chain
// holds under its id. An append refused as a duplicate is read back and kept only when it releases
// this step's record under the same decisions, the way a launcher reads back a release it lost;
// another entry under the id leaves the row, with why.
func (w *adoptWriter) release(ctx context.Context, aw *adoptWrite) (binding.Root, string, error) {
	dup := false
	err := withRetries(func() error {
		aerr := w.deps.Audits.Append(ctx, aw.release)
		if errors.Is(aerr, audit.ErrDuplicateID) {
			dup = true
			return nil
		}
		return aerr
	})
	if err != nil {
		return binding.Root{}, "", err
	}
	if !dup {
		return binding.RootOf(aw.release), "", nil
	}
	var held *audit.Entry
	err = withRetries(func() error {
		var rerr error
		held, rerr = chainEntryOf(w.deps.Audits)(ctx, aw.release.ID)
		return rerr
	})
	switch {
	case err != nil:
		return binding.Root{}, "", err
	case held == nil || !releases(held, aw.row, aw.released):
		return binding.Root{}, "the RELEASE entry " + aw.release.ID + " on the chain releases " +
			"another record", nil
	}
	return binding.RootOf(held), "", nil
}

// seal writes a to the row id through the fenced store write. A row the fence refuses was sealed,
// decided, or settled by another process after it was read, and is left as that process left it,
// unless the row reads back holding exactly a, which is an earlier try of this write whose answer
// was lost.
func (w *adoptWriter) seal(ctx context.Context, id string, a run.Adoption) error {
	changed, gone := false, false
	err := withRetries(func() error {
		var serr error
		changed, serr = w.deps.Runs.SealAdopted(ctx, id, a)
		if errors.Is(serr, run.ErrNotFound) {
			gone, serr = true, nil
		}
		return serr
	})
	switch {
	case err != nil:
		w.leave(id, "its seal could not be written")
		return fmt.Errorf("seal run %s: %w", id, err)
	case changed || (!gone && w.holds(ctx, id, a)):
		w.res.Sealed = append(w.res.Sealed, id)
	default:
		w.res.Moved = append(w.res.Moved, id)
	}
	return nil
}

// holds reports whether the row id reads back with every column a names holding what a writes.
func (w *adoptWriter) holds(ctx context.Context, id string, a run.Adoption) bool {
	r, err := w.deps.Runs.Get(ctx, id)
	if err != nil {
		return false
	}
	return (a.SpecSeal == "" || r.SpecSeal == a.SpecSeal) &&
		(a.ResultSeal == "" || r.ResultSeal == a.ResultSeal) &&
		(a.Claim == "" || r.DecisionClaim == a.Claim)
}

// leave lists the row id as planned and not sealed, for why.
func (w *adoptWriter) leave(id, why string) {
	w.res.Left = append(w.res.Left, LeftRow{ID: id, Reason: why})
	w.deps.log().Warn("dispatch: adoption leaves a row unsealed: "+why, zap.String("run_id", id))
}
