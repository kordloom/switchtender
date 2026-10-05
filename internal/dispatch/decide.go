package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/run"
)

// errDecisionUnrecorded marks a decision that won its run and could not yet be recorded on the
// chain. Nothing has taken effect: the run stays claimed by the decision, and the janitor records
// and settles it once the chain accepts the entry.
var errDecisionUnrecorded = errors.New("the decision is claimed and not yet recorded: it takes " +
	"effect once the audit trail accepts it")

// decisionClaim is a decision fixed before it takes effect: the chain entry that records it and
// how it settles the run it decides.
//
// A decision used to be appended to the chain first and applied second, by a compare-and-set that
// decided who had won only after both had written. A decider that lost, to a second approver, a
// rejection, a cancel, or the timeout sweep on another replica, was answered 409 and stayed on the
// chain anyway, after the outcome of a run it never released, which the bundle verifier reads as a
// gate bypass on an honest install. Now the compare-and-set comes first: the decision claims its
// run, storing this, and only the winner is ever recorded. Whoever finishes a claimed decision, the
// process that made it or a janitor after that process died, appends exactly this entry under
// exactly this id and settles exactly this way, so a crash at any step finishes the same decision
// rather than leaving a second one possible.
type decisionClaim struct {
	// ID is the decision's id: its decision record's, and the id its chain entry is appended under.
	ID string `json:"id"`
	// Verdict is approved, rejected, or timed_out.
	Verdict string `json:"verdict"`
	// Step marks a decision on a workflow approval step, whose workflow resumes once it settles.
	Step bool `json:"step,omitempty"`
	// Entry is the chain entry recording the decision, nil on a dispatcher that keeps no chain.
	Entry *claimEntry `json:"entry,omitempty"`
	// Status is where settling moves the run: pending or running for an approved run, and a
	// terminal status for a rejected run or a decided step.
	Status run.Status `json:"status"`
	// Error is the failure text a rejected run or a denied or timed-out step records.
	Error string `json:"error,omitempty"`
	// Finalize leaves the run's end time, its failure text, and its outcome to finalize once the
	// decision settles it, the way a rejected run is completed. A decided step is settled whole.
	Finalize bool `json:"finalize,omitempty"`
	// Digest and Binding are stamped on the run before it settles, so execution is held to the
	// spec or the state the decider was shown. Both are empty when nothing is stamped.
	Digest string `json:"digest,omitempty"`
	// Binding is the unredacted binding stamped beside Digest.
	Binding string `json:"binding,omitempty"`
}

// claimEntry is the chain entry a decision claim carries, with the nonce its content digest is
// keyed by, which an exported entry never carries. It lives only on the claimed run's row.
type claimEntry struct {
	// ID is the entry's id, the decision's.
	ID string `json:"id"`
	// At is when the decision was made.
	At time.Time `json:"at"`
	// Actor names the decider.
	Actor string `json:"actor"`
	// ActorType is how the decider authenticated.
	ActorType string `json:"actor_type,omitempty"`
	// OnBehalfOf is the account whose authority the decider used.
	OnBehalfOf string `json:"on_behalf_of,omitempty"`
	// Method is the chain method, DECISION.
	Method string `json:"method"`
	// Path names the run or step decided and the verdict.
	Path string `json:"path"`
	// ContentDigest commits the decision's body.
	ContentDigest string `json:"content_digest,omitempty"`
	// Nonce keys ContentDigest.
	Nonce string `json:"nonce,omitempty"`
}

// claimEntryOf returns the claim form of a prepared chain entry.
func claimEntryOf(e *audit.Entry) *claimEntry {
	return &claimEntry{ID: e.ID, At: e.At, Actor: e.Actor, ActorType: e.ActorType,
		OnBehalfOf: e.OnBehalfOf, Method: e.Method, Path: e.Path, ContentDigest: e.ContentDigest,
		Nonce: e.Nonce}
}

// entry returns the chain entry a claim appends, freshly built so each append starts clean.
func (c *claimEntry) entry() *audit.Entry {
	return &audit.Entry{ID: c.ID, At: c.At, Actor: c.Actor, ActorType: c.ActorType,
		OnBehalfOf: c.OnBehalfOf, Method: c.Method, Path: c.Path, ContentDigest: c.ContentDigest,
		Nonce: c.Nonce}
}

// decide claims the run target for the decision c describes and finishes it, keeping rec, the
// decision's record, when there is one. The record is kept first, so the reason's text and random
// value exist before anything commits to them. It reports whether this call settled the run, which
// is what tells the caller to start what the decision released. A decision that lost the claim
// returns ErrNotPendingApproval, having recorded nothing, and its record is withdrawn.
func (d *Dispatcher) decide(ctx context.Context, target string, rec *decision.Record,
	c *decisionClaim) (bool, error) {
	if rec != nil {
		if err := d.decisions.Save(ctx, rec); err != nil {
			return false, fmt.Errorf("keep the decision record: %w", err)
		}
	}
	raw, err := json.Marshal(c)
	if err != nil {
		d.withdrawRecord(rec)
		return false, fmt.Errorf("encode the decision: %w", err)
	}
	won, err := d.store.ClaimDecision(ctx, target, c.ID, string(raw))
	if err != nil {
		// The claim may have landed with its answer lost. The row says whether it did, and a
		// record is withdrawn only once it is known to belong to a decision that lost.
		cur, gerr := d.storeGetWithRetries(context.WithoutCancel(ctx), target)
		if gerr != nil {
			return false, fmt.Errorf("claim the decision: %w", err)
		}
		if cur.DecisionID != c.ID {
			d.withdrawRecord(rec)
			return false, fmt.Errorf("claim the decision: %w", err)
		}
		won = true
	}
	if !won {
		d.withdrawRecord(rec)
		return false, d.claimRefusal(ctx, target)
	}
	return d.finishDecision(ctx, target, c)
}

// claimRefusal says why a decision lost its claim: another decision is being recorded on the run,
// or the run stopped waiting for one.
func (d *Dispatcher) claimRefusal(ctx context.Context, id string) error {
	cur, err := d.store.Get(ctx, id)
	switch {
	case err != nil:
		return ErrNotPendingApproval
	case cur.InFlight():
		return fmt.Errorf("%w: another decision on it is being recorded", ErrNotPendingApproval)
	default:
		return fmt.Errorf("%w: it is %s", ErrNotPendingApproval, cur.Status)
	}
}

// withdrawRecord removes the record of a decision that lost its claim, so no record stands for a
// decision that never took effect. A record left behind by a failed removal is never credited,
// because every reader credits only the decision the run stores as its winner.
func (d *Dispatcher) withdrawRecord(rec *decision.Record) {
	if rec == nil {
		return
	}
	if err := d.decisions.Delete(context.Background(), rec.ID); err != nil &&
		!errors.Is(err, decision.ErrNotFound) {
		d.log.Error("dispatch: withdraw the record of a decision that did not take effect: "+
			err.Error(), zap.String("decision_id", rec.ID))
	}
}

// finishDecision records a claimed decision on the chain, stamps what it was decided on, and
// settles its run, reporting whether this call is the one that settled it. Any number of
// processes may finish one decision at once, the decider and every replica's janitor: the entry is
// appended once under its id, and the settle is a compare-and-set on the claim, so exactly one of
// them settles the run and goes on to start what the decision released.
func (d *Dispatcher) finishDecision(ctx context.Context, id string, c *decisionClaim) (bool,
	error) {
	if d.audits != nil && c.Entry != nil {
		err := withRetries(func() error {
			aerr := d.audits.Append(ctx, c.Entry.entry())
			// Already on the chain: a second finisher, or an append whose answer was lost.
			if errors.Is(aerr, audit.ErrDuplicateID) {
				return nil
			}
			return aerr
		})
		if err != nil {
			return false, fmt.Errorf("%w: %w", errDecisionUnrecorded, err)
		}
	}
	if c.Digest != "" || c.Binding != "" {
		if err := withRetries(func() error {
			return d.store.StampApprovedSpec(ctx, id, c.Digest, c.Binding)
		}); err != nil {
			return false, fmt.Errorf("stamp the state the decision was made on: %w", err)
		}
	}
	settle := run.DecisionSettle{Status: c.Status}
	switch {
	case c.Status == run.StatusRunning:
		settle.Owner = d.owner
	case c.Status.Terminal() && !c.Finalize:
		settle.Error, settle.EndedAt = c.Error, d.now()
	}
	ok, err := d.store.SettleDecision(ctx, id, c.ID, settle)
	if err == nil {
		return ok, nil
	}
	// The settle may have landed with its answer lost. A run this process now holds running is one
	// this call released, and is started here. Anything else is left to whichever finisher's settle
	// the store reports, since starting it twice is worse than the janitor finding it settled.
	cur, gerr := d.storeGetWithRetries(context.WithoutCancel(ctx), id)
	if gerr != nil || cur.InFlight() || cur.DecisionID != c.ID {
		return false, fmt.Errorf("settle the decision: %w", err)
	}
	return cur.Status == run.StatusRunning && cur.ClaimedBy == d.owner, nil
}

// claimOf reads the decision a run's claim describes.
func claimOf(r *run.Run) (*decisionClaim, error) {
	var c decisionClaim
	if err := json.Unmarshal([]byte(r.DecisionClaim), &c); err != nil {
		return nil, fmt.Errorf("read the decision claimed on %s: %w", r.ID, err)
	}
	if c.ID == "" || c.ID != r.DecisionID {
		return nil, fmt.Errorf("read the decision claimed on %s: it names %q, the run %q", r.ID,
			c.ID, r.DecisionID)
	}
	return &c, nil
}

// finishClaimed finishes the decision that claimed r and has not settled it, and starts what it
// released when this process settled it. A step's workflow is resumed only when resume is set: a
// workflow that is ending finishes its claimed steps to record them before its own outcome, and
// is not resumed.
func (d *Dispatcher) finishClaimed(ctx context.Context, r *run.Run, resume bool) {
	c, err := claimOf(r)
	if err != nil {
		d.log.Error("dispatch: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	mine, err := d.finishDecision(ctx, r.ID, c)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("dispatch: finish a claimed decision: "+err.Error(),
				zap.String("run_id", r.ID), zap.String("decision_id", c.ID))
		}
		return
	}
	if c.Step {
		if resume && r.ParentID != nil {
			d.resumeParked(d.ctx, *r.ParentID)
		}
		return
	}
	if !mine {
		return
	}
	settled, err := d.storeGetWithRetries(ctx, r.ID)
	if err != nil {
		d.log.Error("dispatch: read a decided run: "+err.Error(), zap.String("run_id", r.ID))
		return
	}
	d.afterRunDecision(ctx, settled, c)
}

// afterRunDecision starts what a decision on a whole held run released, on the process that
// settled it: an approved run is offered to the claim loop, an approved split or workflow is
// coordinated here, where its lease was taken as it settled, and a rejected run has its split's
// shards settled and its outcome committed.
//
// A cancel requested while the decision was being recorded is honored once it settles. Releasing
// the run without it would leave it pending with a cancel flag the claim loop skips, a run that
// never executes and never ends. A split or workflow carries the flag to its coordinator, whose
// watch stops it.
func (d *Dispatcher) afterRunDecision(ctx context.Context, r *run.Run, c *decisionClaim) {
	if c.Status == run.StatusRejected {
		reason := c.Error
		if r.Kind == run.KindSplit {
			d.rejectShards(ctx, r, reason)
		}
		d.finalize(r, run.StatusRejected, nil, reason)
		return
	}
	switch r.Kind {
	case run.KindPipeline:
		d.startPipeline(r)
	case run.KindSplit:
		// The dispatcher's own context, not the request's: releasing the shards is the
		// coordinator's work, and the request context ends when the approval is answered.
		d.startSplit(d.ctx, r)
	default:
		if r.CancelRequested {
			if _, err := d.CancelWaiting(context.WithoutCancel(ctx), r.ID); err != nil {
				d.log.Error("dispatch: cancel a run released while a cancel was requested: "+
					err.Error(), zap.String("run_id", r.ID))
			}
			return
		}
		d.wake()
	}
}

// finishClaimedSteps finishes every approval step of a workflow that a decision claimed and has
// not settled, recording each decision before anything records how the workflow ended. Every path
// that ends a workflow, a cancel, a park that failed, a walk that stopped, and the sweep that
// interrupts a workflow whose coordinator died, calls it before the workflow's outcome, so a step
// decision that took effect is on the chain ahead of that outcome and never reads as an approval
// recorded after the run it named.
func (d *Dispatcher) finishClaimedSteps(ctx context.Context, parentID string) {
	steps, err := run.ApprovalSteps(ctx, d.store, parentID)
	if err != nil {
		if ctx.Err() == nil {
			d.log.Error("dispatch: list a workflow's approval steps: "+err.Error(),
				zap.String("run_id", parentID))
		}
		return
	}
	for _, s := range steps {
		if s.InFlight() {
			d.finishClaimed(ctx, s, false)
		}
	}
}

// decisionError reports a decision that did not take effect in the words of what was being decided,
// keeping ErrNotPendingApproval recognizable for the caller that answers 409.
func decisionError(what string, err error) error {
	if errors.Is(err, ErrNotPendingApproval) {
		return err
	}
	return fmt.Errorf("record the %s: %w", what, err)
}
