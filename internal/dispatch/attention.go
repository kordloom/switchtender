package dispatch

import (
	"context"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// The lease timing, exported so the attention dashboard measures a lost worker by the same clock
// the lease sweep reclaims by.
const (
	// LeaseTTL is how stale a lease may grow before the sweep treats its holder as dead.
	LeaseTTL = leaseTTL
	// HeartbeatInterval is how often an executing run renews its lease.
	HeartbeatInterval = watchInterval
	// SweepInterval is how often the lease sweep runs.
	SweepInterval = janitorInterval
)

// PresenceInterval is how often a dispatcher reports that it is polling for work. It runs on its
// own timer rather than on the claim loop, because a dispatcher with every slot busy stops
// claiming, and a full worker must not read as a missing one.
const PresenceInterval = 10 * time.Second

// PresenceRecorder records that an executor is polling for work. The attention store satisfies it.
type PresenceRecorder interface {
	// NoteWorker records that owner is polling for work, serving queues with slots runs at once.
	NoteWorker(ctx context.Context, owner string, queues []string, slots int) error
}

// WithPresence makes the dispatcher report, on a timer, that it is polling for work on its queues,
// so the dashboard can tell a queue nothing serves from one whose workers are busy.
func WithPresence(r PresenceRecorder) Option {
	return func(c *config) { c.presence = r }
}

// slotReporter is a store that sends this process's pool size with each claim, which is how a relay
// worker tells the control node it is full rather than gone. The relay Client satisfies it.
type slotReporter interface {
	// SetClaimSlots records how many runs this process executes at once.
	SetClaimSlots(n int)
}

// presenceLoop reports this process at once and then on every PresenceInterval until it closes.
func (d *Dispatcher) presenceLoop() {
	defer d.wg.Done()
	d.notePresence()
	ticker := time.NewTicker(PresenceInterval)
	defer ticker.Stop()
	for {
		select {
		case <-d.ctx.Done():
			return
		case <-ticker.C:
			d.notePresence()
		}
	}
}

// notePresence reports this process once. A process whose claim gate refuses work is not taking
// any, so it does not report itself as serving its queues while that lasts.
func (d *Dispatcher) notePresence() {
	if d.claimGate != nil && d.claimGate() != nil {
		return
	}
	ctx, cancel := context.WithTimeout(d.ctx, webhookTimeout)
	defer cancel()
	if err := d.presence.NoteWorker(ctx, d.owner, d.queues, cap(d.sem)); err != nil &&
		d.ctx.Err() == nil {
		d.log.Warn("dispatch: report worker presence: "+err.Error(), zap.String("owner", d.owner))
	}
}

// NotifyAttention tells the channels about work that has needed attention past its alert threshold.
// r carries the alert in its Attention field. A run without one is ignored. It satisfies the
// attention package's Alerter.
func (d *Dispatcher) NotifyAttention(r *run.Run) {
	if r == nil || r.Attention == nil || r.ParentID != nil {
		return
	}
	d.notifyAttention(r)
}
