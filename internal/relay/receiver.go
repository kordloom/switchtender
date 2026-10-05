package relay

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/run"
)

const (
	// openedWindow is how long a worker remembers a delivery it opened, to refuse it if it is ever
	// presented again. A delivery is bound to one claim's lease, and a lease is not renewed past the
	// run it belongs to, so a day is far past the point where a replay could open anyway.
	openedWindow = 24 * time.Hour
	// maxOpened bounds how many opened deliveries a worker remembers, so a long-lived worker's
	// memory of them cannot grow without limit. Past it the oldest are forgotten first.
	maxOpened = 1 << 16
)

// deliveryHolder is the part of a transport that keeps what each claim delivered.
type deliveryHolder interface {
	// takeDelivery removes and returns what the run's claim delivered.
	takeDelivery(runID string) (heldDelivery, bool)
	// dropDelivery wipes and forgets what the run's claim delivered.
	dropDelivery(runID string)
}

// Receiver opens, on a relay worker, the secrets the control node sealed to the worker's pool with
// each claim. It is the worker's half of sealed delivery, and the dispatcher reaches it through
// dispatch.SecretDelivery: a run's delivery is opened in memory when the run first needs a secret,
// once, and only under the claim it was sealed for.
type Receiver struct {
	// holder is the transport keeping each claim's delivery.
	holder deliveryHolder
	// ring holds the pool keys this worker opens deliveries with, empty when it holds none.
	ring *handoff.KeyRing
	// mu guards opened.
	mu sync.Mutex
	// opened records, by delivery id, when each delivery this worker opened was opened.
	opened map[string]time.Time
	// now reads the clock the opened record is aged by.
	now func() time.Time
}

// compile-time proof that a Receiver is what a dispatcher takes deliveries from.
var _ dispatch.SecretDelivery = (*Receiver)(nil)

// NewReceiver returns the receiver for deliveries arriving over t, opened with the keys in ring. A
// nil or empty ring is allowed: a worker whose pool registered no key still needs every delivery a
// claim carries, refused or not, turned into a clear failure rather than ignored. It panics when t
// keeps no deliveries, which is a wiring error.
func NewReceiver(t Transport, ring *handoff.KeyRing) *Receiver {
	holder, ok := t.(deliveryHolder)
	if !ok {
		panic("relay: the transport keeps no claim deliveries")
	}
	if ring == nil {
		ring, _ = handoff.NewKeyRing()
	}
	return &Receiver{holder: holder, ring: ring, opened: map[string]time.Time{}, now: time.Now}
}

// Receive opens what the control node delivered with r's claim. It returns nothing for a claim that
// carried no delivery, the reason for one the control node refused, and for a sealed one, the
// opened payload or why it does not open: sealed to a key this worker does not hold, for another
// claim, or already opened once. The ciphertext is wiped either way.
func (rc *Receiver) Receive(_ context.Context, r *run.Run) (*handoff.Payload, error) {
	held, ok := rc.holder.takeDelivery(r.ID)
	if !ok || held.delivery == nil {
		return nil, nil
	}
	defer held.wipe()
	env := held.delivery.Sealed
	if env == nil {
		reason := held.delivery.Refused
		if reason == "" {
			reason = "it gave no reason"
		}
		return nil, fmt.Errorf("%w: %s", ErrDeliveryRefused, reason)
	}
	if rc.ring.Len() == 0 {
		return nil, fmt.Errorf("%w: the control node sealed this run's secrets to delivery key %s, "+
			"and this worker was started with no --delivery-key to open them", ErrNoDeliveryKey,
			env.KeyID)
	}
	if !rc.markOpened(env.DeliveryID) {
		return nil, fmt.Errorf("%w: delivery %s for run %s is refused", ErrDeliveryReplay,
			env.DeliveryID, r.ID)
	}
	return handoff.Open(rc.ring, handoff.Binding{RunID: r.ID, Lease: held.lease, Owner: held.owner},
		env)
}

// Discard wipes whatever the run's claim delivered that its executor never took.
func (rc *Receiver) Discard(runID string) {
	rc.holder.dropDelivery(runID)
}

// markOpened records a delivery as opened and reports whether it was new. It is marked before the
// delivery is decrypted, so two attempts on the same delivery cannot both open it, and a delivery
// that fails to open is not tried again either.
func (rc *Receiver) markOpened(id string) bool {
	rc.mu.Lock()
	defer rc.mu.Unlock()
	now := rc.now()
	if at, seen := rc.opened[id]; seen && now.Sub(at) < openedWindow {
		return false
	}
	if len(rc.opened) >= maxOpened {
		rc.forgetOldest(now)
	}
	rc.opened[id] = now
	return true
}

// forgetOldest drops every remembered delivery older than the window and then, oldest first, as
// many more as it takes to leave room for one, so the record never grows past maxOpened. The caller
// holds mu.
func (rc *Receiver) forgetOldest(now time.Time) {
	type opened struct {
		// id is the delivery id.
		id string
		// at is when it was opened.
		at time.Time
	}
	kept := make([]opened, 0, len(rc.opened))
	for id, at := range rc.opened {
		if now.Sub(at) >= openedWindow {
			delete(rc.opened, id)
			continue
		}
		kept = append(kept, opened{id: id, at: at})
	}
	excess := len(rc.opened) - maxOpened + 1
	if excess <= 0 {
		return
	}
	slices.SortFunc(kept, func(a, b opened) int { return a.at.Compare(b.at) })
	for _, o := range kept[:excess] {
		delete(rc.opened, o.id)
	}
}
