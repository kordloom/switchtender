package relay

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
)

// PresenceRecorder records which workers are polling for work. The attention store satisfies it.
type PresenceRecorder interface {
	// NoteWorker records that owner is polling for work, serving queues with slots runs at once.
	NoteWorker(ctx context.Context, owner string, queues []string, slots int) error
	// TouchWorker records that owner is still reporting, leaving what it serves alone.
	TouchWorker(ctx context.Context, owner string) error
}

// WithPresence records each relay worker that claims or renews a lease, so the dashboard counts a
// worker across the relay as connected the way it counts one on the database. A worker is recorded
// at most once per dispatch.PresenceInterval, since a claim loop polls far more often than that.
func WithPresence(rec PresenceRecorder) HandlerOption {
	return func(s *relayServer) {
		if rec != nil {
			s.presence = &presenceNotes{rec: rec, log: s.log, last: map[string]time.Time{},
				every: dispatch.PresenceInterval}
		}
	}
}

// presenceEvery sets how often one worker's report is recorded, for a test that cannot wait out
// the interval. It applies after WithPresence.
func presenceEvery(d time.Duration) HandlerOption {
	return func(s *relayServer) {
		if s.presence != nil {
			s.presence.every = d
		}
	}
}

// presenceNotes throttles the reports the relay records for its workers.
type presenceNotes struct {
	// rec records a report.
	rec PresenceRecorder
	// log records a report that could not be written.
	log *zap.Logger
	// mu guards last.
	mu sync.Mutex
	// last maps an owner to when its report was last recorded.
	last map[string]time.Time
	// every is the shortest time between two recorded reports from one worker.
	every time.Duration
}

// due reports whether owner's report is due and, when it is, marks it recorded now.
func (p *presenceNotes) due(owner string) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	if at, ok := p.last[owner]; ok && now.Sub(at) < p.every {
		return false
	}
	// A worker that stopped coming back is forgotten once its report could not be throttled again,
	// so the map stays the size of the fleet that is actually connected.
	for name, at := range p.last {
		if now.Sub(at) > 3*p.every {
			delete(p.last, name)
		}
	}
	p.last[owner] = now
	return true
}

// note records a claiming worker's queues and slots. A nil receiver records nothing.
func (p *presenceNotes) note(ctx context.Context, owner string, queues []string, slots int) {
	if p == nil || !p.due(owner) {
		return
	}
	if err := p.rec.NoteWorker(ctx, owner, queues, slots); err != nil {
		p.log.Warn("relay: record worker presence: "+err.Error(), zap.String("owner", owner))
	}
}

// touch records that a worker renewing a lease is still there, so a worker too busy to claim does
// not read as gone. A nil receiver records nothing.
func (p *presenceNotes) touch(ctx context.Context, owner string) {
	if p == nil || !p.due(owner) {
		return
	}
	if err := p.rec.TouchWorker(ctx, owner); err != nil {
		p.log.Warn("relay: record worker presence: "+err.Error(), zap.String("owner", owner))
	}
}

// SetClaimSlots tells the transport how many runs this worker executes at once, which it sends with
// each claim.
func (t *httpTransport) SetClaimSlots(n int) {
	t.slots.Store(int64(n))
}

// SetClaimSlots passes how many runs this worker executes at once to a transport that sends it with
// each claim. The dispatcher calls it with its pool size.
func (c *Client) SetClaimSlots(n int) {
	if s, ok := c.t.(interface{ SetClaimSlots(int) }); ok {
		s.SetClaimSlots(n)
	}
}
