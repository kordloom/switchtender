package dispatch

import (
	"context"
	"fmt"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/util"
)

// Write health settings.
const (
	// degradedWindow is how long readiness reports degraded after a write on the run path failed.
	degradedWindow = 5 * time.Minute
	// maxOwedEnds bounds the run ends one process holds for a store that refused them.
	maxOwedEnds = 1024
)

// Health is this process's view of whether it can record what its runs do. It is per process on
// purpose: a failed write is this process's view of its store and disk, and recording it in the
// store is the write that just failed.
type Health struct {
	// Degraded reports a recent write failure or an end the store has not yet taken.
	Degraded bool
	// Reasons says why, one line each.
	Reasons []string
	// LastWriteFailure is when a write on the run path last failed, zero when none has.
	LastWriteFailure time.Time
	// OwedEnds counts runs whose end this process holds and the store has not yet taken.
	OwedEnds int
}

// owedEnd is a run's terminal record the store refused, held until it lands.
type owedEnd struct {
	// run is the run as the executor left it, which the record lands on.
	run *run.Run
	// fin is the terminal record the store refused.
	fin run.Finalization
	// refused is the store's error, named in the run's warning once the record lands.
	refused string
}

// writeHealth records the failed writes on the run path and the run ends owed to the store. The zero
// value is ready to use.
type writeHealth struct {
	// mu guards every field below.
	mu sync.Mutex
	// last is when a write last failed.
	last time.Time
	// lastErr is that write's error.
	lastErr string
	// owed holds the ends the store refused, by run id.
	owed map[string]owedEnd
	// order lists owed run ids oldest first, for eviction.
	order []string
}

// noteFailure stamps a failed write at now.
func (h *writeHealth) noteFailure(now time.Time, err error) {
	if h == nil || err == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	h.last, h.lastErr = now, err.Error()
}

// owe holds a run's refused terminal record and returns the id of the oldest end it dropped to stay
// within maxOwedEnds, empty when it dropped none.
func (h *writeHealth) owe(r *run.Run, fin run.Finalization, refused error) string {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.owed == nil {
		h.owed = map[string]owedEnd{}
	}
	if _, held := h.owed[r.ID]; !held {
		h.order = append(h.order, r.ID)
	}
	h.owed[r.ID] = owedEnd{run: r.Clone(), fin: fin, refused: refused.Error()}
	if len(h.order) <= maxOwedEnds {
		return ""
	}
	dropped := h.order[0]
	h.order = h.order[1:]
	delete(h.owed, dropped)
	return dropped
}

// pending returns the owed ends, oldest first.
func (h *writeHealth) pending() []owedEnd {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]owedEnd, 0, len(h.order))
	for _, id := range h.order {
		out = append(out, h.owed[id])
	}
	return out
}

// settle forgets a run's owed end.
func (h *writeHealth) settle(id string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if _, held := h.owed[id]; !held {
		return
	}
	delete(h.owed, id)
	for i, o := range h.order {
		if o == id {
			h.order = append(h.order[:i], h.order[i+1:]...)
			break
		}
	}
}

// snapshot reports the health at now.
func (h *writeHealth) snapshot(now time.Time) Health {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := Health{LastWriteFailure: h.last, OwedEnds: len(h.order)}
	if !h.last.IsZero() && now.Sub(h.last) < degradedWindow {
		out.Reasons = append(out.Reasons, "a write to the run store failed at "+
			h.last.UTC().Format(time.RFC3339)+": "+h.lastErr)
	}
	if n := len(h.order); n > 0 {
		out.Reasons = append(out.Reasons, fmt.Sprintf("the store has not yet taken the end of %d %s, "+
			"which %s as running until it does", n, util.Plural(n, "run", "runs"),
			util.Plural(n, "reads", "read")))
	}
	out.Degraded = len(out.Reasons) > 0
	return out
}

// Health reports whether this process can record what its runs do, for the readiness probe.
func (d *Dispatcher) Health() Health {
	return d.health.snapshot(d.now())
}

// landOwed retries owed run ends on the janitor's interval until the dispatcher closes. It runs on
// every dispatcher, a relay worker's included, which has no janitor. An end landed twice meets the
// terminal fence the second time and is dropped, so it is committed and announced once.
func (d *Dispatcher) landOwed() {
	defer d.wg.Done()
	ticker := time.NewTicker(janitorInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.landOwedEnds()
		}
	}
}

// landOwedEnds retries every end the store refused. Each owed run's lease is renewed first, so
// another replica's janitor does not mark interrupted a run whose true end this process holds. An
// end that lands gains a warning saying it was recorded late and why, and is then committed and
// announced the way every end is. One a fence refuses, because the run ended or moved on in the
// meantime, is dropped.
func (d *Dispatcher) landOwedEnds() {
	for _, o := range d.health.pending() {
		if o.run.ClaimedBy != "" {
			_ = d.store.Heartbeat(context.Background(), o.run.ID, o.run.ClaimedBy)
		}
		r := o.run.Clone()
		addWarning(r, "its end was recorded late, after the store refused the write: "+o.refused)
		fin := o.fin
		fin.Warning = r.Warning
		stored, recorded, err := d.recordTerminal(r, fin)
		if err != nil {
			continue
		}
		d.health.settle(r.ID)
		if !recorded {
			d.log.Warn("dispatch: owed run end dropped, the run moved on",
				zap.String("run_id", r.ID), zap.String("stored", string(stored)))
			continue
		}
		d.afterTerminal(r)
	}
}
