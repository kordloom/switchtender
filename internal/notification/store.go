package notification

import (
	"context"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory Store guarded by a read-write mutex.
type memStore struct {
	// mu guards every map below.
	mu sync.RWMutex
	// targets maps target id to the stored target.
	targets map[string]*Notification
	// attachments maps attachment id to the stored attachment.
	attachments map[string]*Attachment
	// events maps a run id to its recorded events, in sequence order.
	events map[string][]*RunEvent
	// deliveries maps a delivery's key to the stored delivery.
	deliveries map[string]*Delivery
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{targets: map[string]*Notification{}, attachments: map[string]*Attachment{},
		events: map[string][]*RunEvent{}, deliveries: map[string]*Delivery{}}
}

// Save inserts or replaces the target identified by n.ID.
func (m *memStore) Save(_ context.Context, n *Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.targets[n.ID] = n.Clone()
	return nil
}

// Update replaces an existing target, or returns ErrNotFound when it is gone.
func (m *memStore) Update(_ context.Context, n *Notification) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.targets[n.ID]; !ok {
		return ErrNotFound
	}
	m.targets[n.ID] = n.Clone()
	return nil
}

// Get returns the target with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Notification, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	n, ok := m.targets[id]
	if !ok {
		return nil, ErrNotFound
	}
	return n.Clone(), nil
}

// List returns every target ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Notification, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Notification, 0, len(m.targets))
	for _, n := range m.targets {
		out = append(out, n.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the target and its attachments, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.targets[id]; !ok {
		return ErrNotFound
	}
	delete(m.targets, id)
	for aid, a := range m.attachments {
		if a.NotificationID == id {
			delete(m.attachments, aid)
		}
	}
	return nil
}

// Attach stores an attachment, or returns ErrDuplicate when an equal one exists.
func (m *memStore) Attach(_ context.Context, a *Attachment) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, have := range m.attachments {
		if have.NotificationID == a.NotificationID && have.ObjectKind == a.ObjectKind &&
			have.ObjectID == a.ObjectID && have.Event == a.Event {
			return ErrDuplicate
		}
	}
	m.attachments[a.ID] = a.Clone()
	return nil
}

// Detach removes the attachment with the given id, or returns ErrAttachmentNotFound.
func (m *memStore) Detach(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.attachments[id]; !ok {
		return ErrAttachmentNotFound
	}
	delete(m.attachments, id)
	return nil
}

// Attachments returns a target's attachments, oldest first.
func (m *memStore) Attachments(_ context.Context, notificationID string) ([]*Attachment, error) {
	return m.filter(func(a *Attachment) bool { return a.NotificationID == notificationID }), nil
}

// AttachedTo returns the attachments on one object, oldest first.
func (m *memStore) AttachedTo(_ context.Context, kind, objectID string) ([]*Attachment, error) {
	return m.filter(func(a *Attachment) bool {
		return a.ObjectKind == kind && a.ObjectID == objectID
	}), nil
}

// filter returns copies of the attachments keep accepts, oldest first.
func (m *memStore) filter(keep func(*Attachment) bool) []*Attachment {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*Attachment{}
	for _, a := range m.attachments {
		if keep(a) {
			out = append(out, a.Clone())
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out
}

// ListAttachments returns every attachment, oldest first.
func (m *memStore) ListAttachments(_ context.Context) ([]*Attachment, error) {
	return m.filter(func(*Attachment) bool { return true }), nil
}

// DetachObject removes every attachment on one object and summarizes what it removed.
func (m *memStore) DetachObject(_ context.Context, kind, objectID string) (Cleanup, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var targets []string
	for id, a := range m.attachments {
		if a.ObjectKind == kind && a.ObjectID == objectID {
			targets = append(targets, a.NotificationID)
			delete(m.attachments, id)
		}
	}
	return Summarize(targets), nil
}

// Record appends the event to its run's sequence and queues a delivery to each recipient, unless
// the run already holds the same moment, or already holds its end and this is an end too.
func (m *memStore) Record(_ context.Context, ev *RunEvent, recipients []Recipient) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := ev.DedupeKey()
	for _, have := range m.events[ev.RunID] {
		if have.DedupeKey() == key || (ev.End() && have.End()) {
			return false, nil
		}
	}
	stored := *ev
	stored.CreatedAt = Millis(ev.CreatedAt)
	stored.Seq = int64(len(m.events[ev.RunID]) + 1)
	stored.Snapshot = append([]byte(nil), ev.Snapshot...)
	stored.Follows = DecodeFollows(EncodeFollows(ev.Follows))
	m.events[ev.RunID] = append(m.events[ev.RunID], &stored)
	ev.Seq = stored.Seq
	for _, rc := range recipients {
		d := newDelivery(&stored, rc)
		if _, dup := m.deliveries[d.Key()]; dup {
			continue
		}
		m.deliveries[d.Key()] = d
	}
	return true, nil
}

// newDelivery builds the delivery an event queues for one recipient: pending and due at once, or
// skipped and finished with the reason it was skipped.
func newDelivery(ev *RunEvent, rc Recipient) *Delivery {
	d := &Delivery{
		NotificationID: rc.NotificationID, RunID: ev.RunID, Seq: ev.Seq, Event: ev.Event,
		Branch: ev.Branch, Follows: ev.Follows, TargetName: rc.Name, TargetKind: rc.Kind,
		Status:        DeliveryPending,
		NextAttemptAt: ev.CreatedAt, CreatedAt: ev.CreatedAt,
	}
	if rc.Skip != "" {
		at := ev.CreatedAt
		d.Status, d.LastError, d.FinishedAt = DeliverySkipped, rc.Skip, &at
	}
	return d
}

// Claim leases the due deliveries whose earlier deliveries to the same target for the same run have
// finished, at most perTarget to one target counting what owner already holds when perTarget is
// above zero.
func (m *memStore) Claim(_ context.Context, owner string, now time.Time, lease time.Duration,
	limit, perTarget int) ([]*Delivery, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	held := map[string]int{}
	var due []*Delivery
	for _, d := range m.deliveries {
		if d.Status != DeliveryPending {
			continue
		}
		if d.ClaimUntil != nil && !d.ClaimUntil.Before(now) {
			if d.ClaimedBy == owner {
				held[d.NotificationID]++
			}
			continue
		}
		if d.NextAttemptAt.After(now) || m.blocked(d) {
			continue
		}
		due = append(due, d)
	}
	sort.Slice(due, func(i, j int) bool {
		a, b := due[i], due[j]
		if !a.NextAttemptAt.Equal(b.NextAttemptAt) {
			return a.NextAttemptAt.Before(b.NextAttemptAt)
		}
		if a.RunID != b.RunID {
			return a.RunID < b.RunID
		}
		if a.Seq != b.Seq {
			return a.Seq < b.Seq
		}
		return a.NotificationID < b.NotificationID
	})
	out := make([]*Delivery, 0, len(due))
	until := Millis(now.Add(lease))
	for _, d := range due {
		if limit > 0 && len(out) == limit {
			break
		}
		if perTarget > 0 && held[d.NotificationID] >= perTarget {
			continue
		}
		held[d.NotificationID]++
		d.ClaimedBy, d.ClaimUntil = owner, &until
		c := d.Clone()
		c.Snapshot = append([]byte(nil), m.event(d.RunID, d.Seq).Snapshot...)
		c.EarlierFailed = m.earlierFailed(d)
		out = append(out, c)
	}
	return out, nil
}

// blocked reports whether an earlier delivery to the same target for the same run is still pending
// and orders before d: one of the run's own lifecycle, or one on d's own step or a step d follows.
func (m *memStore) blocked(d *Delivery) bool {
	for _, o := range m.deliveries {
		if o.NotificationID == d.NotificationID && o.RunID == d.RunID && o.Seq < d.Seq &&
			o.Status == DeliveryPending && Ordered(d.Branch, d.Follows, o.Branch) {
			return true
		}
	}
	return false
}

// earlierFailed reports whether an earlier delivery to the same target for the same run failed.
func (m *memStore) earlierFailed(d *Delivery) bool {
	for _, o := range m.deliveries {
		if o.NotificationID == d.NotificationID && o.RunID == d.RunID && o.Seq < d.Seq &&
			o.Status == DeliveryFailed {
			return true
		}
	}
	return false
}

// event returns the recorded event a delivery carries, or an empty one when it is gone.
func (m *memStore) event(runID string, seq int64) *RunEvent {
	for _, ev := range m.events[runID] {
		if ev.Seq == seq {
			return ev
		}
	}
	return &RunEvent{}
}

// Finish records an attempt's outcome on a delivery owner holds and releases the claim.
func (m *memStore) Finish(_ context.Context, d *Delivery, owner string, out Outcome) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	have, ok := m.deliveries[d.Key()]
	if !ok || have.Status != DeliveryPending || have.ClaimedBy != owner {
		return ErrDeliveryLost
	}
	have.Attempts++
	have.LastError, have.Note = out.Error, out.Note
	have.ClaimedBy, have.ClaimUntil = "", nil
	switch out.Status {
	case DeliveryDelivered, DeliveryFailed, DeliverySkipped:
		at := Millis(out.At)
		have.Status, have.FinishedAt = out.Status, &at
	default:
		have.NextAttemptAt = Millis(out.NextAttemptAt)
	}
	return nil
}

// Release lets go of owner's claim on a delivery without counting an attempt, leaving it due as it
// was.
func (m *memStore) Release(_ context.Context, d *Delivery, owner string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	have, ok := m.deliveries[d.Key()]
	if !ok || have.Status != DeliveryPending || have.ClaimedBy != owner {
		return ErrDeliveryLost
	}
	have.ClaimedBy, have.ClaimUntil = "", nil
	return nil
}

// Deliveries lists the deliveries a filter selects, newest first.
func (m *memStore) Deliveries(_ context.Context, f DeliveryFilter) ([]*Delivery, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*Delivery{}
	for _, d := range m.deliveries {
		if (f.RunID != "" && d.RunID != f.RunID) ||
			(f.NotificationID != "" && d.NotificationID != f.NotificationID) ||
			(f.Status != "" && d.Status != f.Status) ||
			(!f.Since.IsZero() && d.CreatedAt.Before(f.Since)) {
			continue
		}
		c := d.Clone()
		c.Snapshot = nil
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return deliveryNewer(out[i], out[j]) })
	if len(out) > f.EffectiveLimit() {
		out = out[:f.EffectiveLimit()]
	}
	return out, nil
}

// deliveryNewer orders deliveries newest first: by when they were recorded, then by run, sequence,
// and target, so a listing is stable.
func deliveryNewer(a, b *Delivery) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	if a.RunID != b.RunID {
		return a.RunID > b.RunID
	}
	if a.Seq != b.Seq {
		return a.Seq > b.Seq
	}
	return a.NotificationID < b.NotificationID
}
