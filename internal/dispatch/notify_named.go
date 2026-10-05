package dispatch

import (
	"context"

	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
)

// NotificationRouter finds the named notification targets attached to what a run came from, for the
// event the run is at: started, held for approval, succeeded, or failed. The notification package's
// Router satisfies it. A target's secrets are opened only here, at delivery, so they never ride on
// the run record, its receipt, or its evidence the way a template's inline targets do.
type NotificationRouter interface {
	// Targets returns the channel configuration of every target attached for the run's event.
	Targets(ctx context.Context, r *run.Run) []run.NotifyTarget
}

// NotificationRouterFunc adapts a function to a NotificationRouter.
type NotificationRouterFunc func(ctx context.Context, r *run.Run) []run.NotifyTarget

// Targets calls f.
func (f NotificationRouterFunc) Targets(ctx context.Context, r *run.Run) []run.NotifyTarget {
	return f(ctx, r)
}

// WithNotificationRouter delivers each top-level run to the named notification targets attached to
// what it came from, beside the server-wide channels and the run's own targets.
func WithNotificationRouter(router NotificationRouter) Option {
	return func(c *config) { c.router = router }
}

// notifyNamed delivers a top-level run to the named targets attached for the event it is at.
func (d *Dispatcher) notifyNamed(r *run.Run) {
	d.notifyNamedOn(r, named.Branch{})
}

// notifyNamedOn delivers the event a top-level run is at to its named targets, the event placed
// within the workflow by branch, or in the run's own lifecycle when branch is the zero Branch.
//
// With an outbox the event is recorded on the caller's path, in the order the run reaches it, and
// delivered from there in that order. Without one the targets are looked up off the caller's path,
// since the lookup reads the store and the caller is often the executor finishing a run, and
// delivered at once, best effort. The run is copied first either way, because the executor keeps
// changing it: a started notification read after the run moved on would report the wrong state.
func (d *Dispatcher) notifyNamedOn(r *run.Run, branch named.Branch) {
	if r == nil || r.ParentID != nil {
		return
	}
	if d.outbox != nil && r.ID != "" {
		_ = d.recordNamed(r, branch)
		return
	}
	if d.router == nil {
		return
	}
	snap := r.Clone()
	d.notifyWG.Add(1)
	go func() {
		defer d.notifyWG.Done()
		ctx, cancel := context.WithTimeout(context.Background(), webhookTimeout)
		targets := d.router.Targets(ctx, snap)
		cancel()
		d.deliverTargets(snap, targets)
	}()
}

// startedSentence is what every channel says after its own rendering of a run's label when the run
// begins, so a start reads as a start rather than as a failure, which is how any status other than
// succeeded rendered before a channel could hear about one.
const startedSentence = "started."

// notifyStarted tells the named targets attached for the started event that a top-level run began
// executing, and settles the start the store marked owed. Only named targets hear it: the
// server-wide channels and a template's inline targets have always reported outcomes and holds, and
// a start they were never configured for would arrive in every channel at once.
func (d *Dispatcher) notifyStarted(r *run.Run) {
	if r.ParentID != nil || r.Status != run.StatusRunning {
		return
	}
	d.announceOwed(r, run.OwedStart)
}
