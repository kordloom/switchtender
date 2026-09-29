package dispatch

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"time"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
	"go.uber.org/zap"
)

// claimLoop leases pending runs from the store and executes them, one claim per free worker slot,
// until the dispatcher closes. Every process running a dispatcher takes part, so a lone server
// executes its own queue and added workers simply compete for the same leases.
func (d *Dispatcher) claimLoop() {
	defer d.wg.Done()
	idle := 0
	for {
		select {
		case d.sem <- struct{}{}:
		case <-d.ctx.Done():
			return
		}

		r, err := d.store.Claim(d.ctx, d.owner, d.queues)
		if err != nil {
			<-d.sem
			if !errors.Is(err, run.ErrNonePending) && d.ctx.Err() == nil {
				d.log.Error("dispatch: claim: " + err.Error())
			}
			idle++
			timer := time.NewTimer(d.idleWait(idle))
			select {
			case <-timer.C:
			case <-d.wakeCh:
				// Work arrived. Start over at the base interval, since a controller that just
				// took a submission is not idle any more.
				timer.Stop()
				idle = 0
			case <-d.ctx.Done():
				timer.Stop()
				return
			}
			continue
		}
		idle = 0

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			defer func() { <-d.sem }()
			d.executeLeased(d.ctx, r)
		}()
	}
}

// wake asks the claim loop to poll immediately rather than wait out its idle backoff.
//
// The backoff exists so idle dispatchers stop competing for the store's single writer, but nothing
// told the loop when work arrived, so its whole wait landed on submit-to-start latency: a run
// submitted to a quiet controller waited a measured 1.7 seconds on average and 2.75 at worst, from
// 250ms before the backoff existed. A submitting caller signals here and the loop starts at once,
// which keeps the backoff's benefit without paying for it on the first run after an idle spell.
//
// It never blocks and it is safe to call for a run that turns out not to be claimable. A spurious
// wake costs one empty claim, after which the loop backs off again; a missed wake costs a user
// seconds of waiting. Over-signaling is deliberately the cheaper mistake, which is why every submit
// path calls this rather than only the ones that provably created claimable work.
func (d *Dispatcher) wake() {
	select {
	case d.wakeCh <- struct{}{}:
	default:
	}
}

// idleWait returns how long to wait before the next claim, given how many consecutive claims came
// back empty. The wait doubles toward a ceiling so a dispatcher with nothing to do stops competing
// for the store's single writer with the runs that are actually executing, and it carries jitter so
// several dispatchers sharing a store spread their polls out instead of arriving together. One
// claim resets the count, so work is never picked up on a stale backoff.
func (d *Dispatcher) idleWait(idle int) time.Duration {
	wait := d.claimInterval << min(max(idle-1, 0), idleBackoffShift)
	// Half the wait, plus a random share of it: the mean stays at wait and no two idle dispatchers
	// stay in step.
	return wait/2 + time.Duration(rand.Int64N(int64(wait)))
}

// settledReporter is a store whose sweep can name the top-level runs it settled, so their outcomes
// reach the chain. A store that cannot, such as the relay client, leaves the reporting to whichever
// process owns the sweep.
type settledReporter interface {
	// ReclaimStaleSettled sweeps like ReclaimStale and also returns the ids of the top-level runs the
	// sweep drove to a terminal state.
	ReclaimStaleSettled(ctx context.Context, ttl time.Duration) (int, []string, error)
}

// commitSettled records the outcome of every run the sweep drove to a terminal state.
//
// The sweep is a bulk update in the store rather than a pass through finalize, so these runs used to
// end with no chain entry at all: not receiptable, and absent from their own dossiers. A run whose
// worker died mid-change is precisely the incident somebody asks about afterward, so it is the last
// run that should have no record. The commit is best effort, as it is on the relay's terminal save
// and for the same reason: the run has already happened, and refusing to record it would not unhappen
// it. A failure is logged where an operator can find it.
func (d *Dispatcher) commitSettled(ids []string) {
	if d.audits == nil || len(ids) == 0 {
		return
	}
	for _, id := range ids {
		r, err := d.store.Get(d.ctx, id)
		if err != nil {
			if d.ctx.Err() == nil {
				d.log.Error("dispatch: read settled run: "+err.Error(), zap.String("run_id", id))
			}
			continue
		}
		if err := outcome.Commit(d.ctx, d.audits, d.store, r, "system:janitor", d.now); err != nil {
			if d.ctx.Err() == nil {
				d.log.Error("dispatch: commit settled outcome: "+err.Error(), zap.String("run_id", id))
			}
		}
	}
}

// settleOverrunning ends a run that has been running past its own timeout, whatever its executor says.
//
// The lease sweep above reclaims on a stale heartbeat, so it never touches a worker that keeps
// heartbeating. A run's timeout was enforced only inside the executing process, which means it bound
// a cooperative executor and nothing else: a relay that claimed work and kept the lease fresh held it
// forever, and nothing on the control node ever timed it out. One compromised relay could therefore
// take the queue and stall the estate indefinitely, and even an honest worker that wedged held its
// run until somebody noticed.
//
// The grace is deliberate. The executor should be the one to end its own run, with the real exit code
// and output, so this only acts once the executor has clearly failed to.
func (d *Dispatcher) settleOverrunning() {
	running, err := d.store.ListPage(d.ctx, run.ListFilter{Status: string(run.StatusRunning)},
		overrunScan, 0)
	if err != nil {
		if d.ctx.Err() == nil {
			d.log.Error("dispatch: list running: " + err.Error())
		}
		return
	}
	now := d.now()
	for _, r := range running {
		if r.Timeout <= 0 || r.StartedAt == nil {
			continue
		}
		deadline := r.StartedAt.Add(time.Duration(r.Timeout)*time.Second + overrunGrace)
		if now.Before(deadline) {
			continue
		}
		fin := run.Finalization{
			Status: run.StatusFailed,
			Error: fmt.Sprintf("timed out: still running %s after its %ds timeout, so the control "+
				"node ended it", now.Sub(*r.StartedAt).Round(time.Second), r.Timeout),
			EndedAt: now,
		}
		moved, ferr := d.store.FinalizeRunning(d.ctx, r.ID, fin)
		if ferr != nil {
			if d.ctx.Err() == nil {
				d.log.Error("dispatch: settle overrunning run: "+ferr.Error(), zap.String("run_id", r.ID))
			}
			continue
		}
		if moved {
			d.log.Warn("dispatch: ended a run past its timeout", zap.String("run_id", r.ID),
				zap.Int("timeout_seconds", r.Timeout))
			d.commitSettled([]string{r.ID})
		}
	}
}

// janitor sweeps stale leases so runs owned by dead processes requeue or resolve. It runs once
// immediately, covering restarts, then on an interval.
func (d *Dispatcher) janitor() {
	defer d.wg.Done()
	sweep := func() {
		var n int
		var settled []string
		var err error
		if reporter, ok := d.store.(settledReporter); ok {
			n, settled, err = reporter.ReclaimStaleSettled(d.ctx, leaseTTL)
		} else {
			n, err = d.store.ReclaimStale(d.ctx, leaseTTL)
		}
		if err != nil {
			if d.ctx.Err() == nil {
				d.log.Error("dispatch: reclaim stale: " + err.Error())
			}
			return
		}
		if n > 0 {
			d.log.Info("dispatch: reclaimed stale runs", zap.Int("count", n))
		}
		d.commitSettled(settled)
		d.settleOverrunning()
	}
	sweep()
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
}
