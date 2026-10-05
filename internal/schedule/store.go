package schedule

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/kordloom/switchtender/internal/util"
)

// Store persists schedules. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts or replaces the schedule identified by s.ID.
	Save(ctx context.Context, s *Schedule) error
	// Get returns the schedule with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Schedule, error)
	// List returns all schedules ordered by creation time, oldest first.
	List(ctx context.Context) ([]*Schedule, error)
	// Delete removes the schedule with the given id, or returns ErrNotFound.
	Delete(ctx context.Context, id string) error
	// Update replaces an existing schedule and returns ErrNotFound when the row is gone, so an edit
	// racing a delete cannot re-create what was deleted. Save stays create-or-replace for the
	// create path.
	Update(ctx context.Context, s *Schedule) error
	// RecordFire records that a schedule fired at the given time, the run it created, and why it
	// created none. An empty run id leaves the stored one alone, since a fire that failed created
	// nothing, and failure replaces the stored reason, so a fire that starts a run clears it. Either
	// way the fire was not skipped, so it clears the skip reason and the count of skips in a row. It
	// updates an existing row and never creates one, so a schedule deleted while its run was in
	// flight stays deleted. A missing row is not an error, because the record is a note about a run
	// that already happened rather than a change anyone is waiting on.
	RecordFire(ctx context.Context, id string, at time.Time, runID, failure string) error
	// RecordSkip records that a schedule's fire at the given time was skipped and why, adding one
	// to its count of skips in a row. It clears the stored failure, since a skip is not one, and
	// keeps the last run id. Like RecordFire it updates an existing row and never creates one, and a
	// missing row is not an error.
	RecordSkip(ctx context.Context, id string, at time.Time, reason string) error
	// ClaimDue atomically advances a schedule's next fire time from oldNext to newNext and
	// reports whether this caller won. Concurrent scheduler instances race on the same row; only
	// the winner fires, so a highly available pair never double-launches. A missing row loses
	// rather than erroring: the scheduler claims after listing, so a schedule deleted in that
	// window is a lost race, and the caller skips it exactly as it skips a row another node won.
	ClaimDue(ctx context.Context, id string, oldNext, newNext time.Time) (bool, error)
	// ClaimFinal atomically clears a schedule's next fire time when it still equals oldNext and
	// reports whether this caller won. It is ClaimDue for the last occurrence of a recurrence bounded
	// by COUNT or UNTIL: there is no next fire to advance to, so the row is claimed by clearing it,
	// which the scheduler reads as never due again. The same compare-and-set keeps a highly
	// available pair from both firing that last occurrence.
	ClaimFinal(ctx context.Context, id string, oldNext time.Time) (bool, error)
	// Release hands back an occurrence a claim took and whose fire started no run: it sets the next
	// fire time back to due when the row still holds what the claim wrote, claimed for ClaimDue and
	// nil for ClaimFinal, and reports whether it did. It is the same compare-and-set as a claim, so
	// an edit, a delete, or another server's claim made in between wins, and a missing row reports
	// false without an error.
	Release(ctx context.Context, id string, claimed *time.Time, due time.Time) (bool, error)
	// ClaimFire is ClaimDue, or ClaimFinal when next is nil, for an occurrence the caller is about
	// to fire: in the same write it marks oldNext in flight on the row, stamped with the store's
	// clock, so a server that stops before the fire's run exists leaves the occurrence marked
	// rather than lost. A row that still marks another occurrence in flight is not claimed, which
	// the caller reads as a lost race, so the claim of the next occurrence never unmarks one whose
	// fire is still unaccounted for. The mark is cleared by SettleInFlight and outlives every other
	// write but Delete.
	ClaimFire(ctx context.Context, id string, oldNext time.Time, next *time.Time) (bool, error)
	// TakeInFlight takes up every occurrence marked in flight for at least grace by the store's
	// clock, re-marking each one now so another server's sweep leaves it alone for another grace,
	// and returns them, oldest occurrence first. Two servers sweeping at once never take the same
	// occurrence.
	TakeInFlight(ctx context.Context, grace time.Duration) ([]InFlight, error)
	// SettleInFlight clears the in-flight mark when the row still marks occurrence, once its fire is
	// accounted for: its run exists, or the fire recorded why it started none. A row that is gone,
	// or that marks no occurrence or another one, is left as it is without an error.
	SettleInFlight(ctx context.Context, id string, occurrence time.Time) error
}

// InFlight is an occurrence claimed for a fire that is not known to have created its run.
type InFlight struct {
	// ScheduleID is the schedule.
	ScheduleID string
	// Occurrence is the fire time the claim took, which names the run's idempotency key.
	Occurrence time.Time
}

// inFlightMark is the in-memory store's in-flight marker on one schedule.
type inFlightMark struct {
	// occurrence is the fire time the claim took.
	occurrence time.Time
	// since is when the occurrence was last marked.
	since time.Time
}

// ClaimFire claims an occurrence for a fire and marks it in flight, unless another occurrence is.
func (m *memStore) ClaimFire(_ context.Context, id string, oldNext time.Time, next *time.Time) (bool,
	error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.schedules[id]
	if !ok || sc.NextRunAt == nil || !sc.NextRunAt.Equal(oldNext) {
		return false, nil
	}
	if mark, marked := m.inflight[id]; marked && !mark.occurrence.Equal(oldNext) {
		return false, nil
	}
	sc.NextRunAt = nil
	if next != nil {
		to := *next
		sc.NextRunAt = &to
	}
	m.inflight[id] = inFlightMark{occurrence: oldNext, since: time.Now()}
	return true, nil
}

// TakeInFlight takes up the occurrences marked in flight for at least grace.
func (m *memStore) TakeInFlight(_ context.Context, grace time.Duration) ([]InFlight, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := time.Now()
	out := []InFlight{}
	for id, mark := range m.inflight {
		if now.Sub(mark.since) < grace {
			continue
		}
		m.inflight[id] = inFlightMark{occurrence: mark.occurrence, since: now}
		out = append(out, InFlight{ScheduleID: id, Occurrence: mark.occurrence})
	}
	SortInFlight(out)
	return out, nil
}

// SettleInFlight clears the in-flight mark when it still marks occurrence.
func (m *memStore) SettleInFlight(_ context.Context, id string, occurrence time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if mark, ok := m.inflight[id]; ok && mark.occurrence.Equal(occurrence) {
		delete(m.inflight, id)
	}
	return nil
}

// SortInFlight orders occurrences oldest first, then by schedule, the order TakeInFlight returns
// them in.
func SortInFlight(list []InFlight) {
	sort.Slice(list, func(i, j int) bool {
		if !list[i].Occurrence.Equal(list[j].Occurrence) {
			return list[i].Occurrence.Before(list[j].Occurrence)
		}
		return list[i].ScheduleID < list[j].ScheduleID
	})
}

// Release sets a schedule's next fire time back to due when it still holds claimed, nil meaning no
// next fire time at all, and reports whether it did.
func (m *memStore) Release(_ context.Context, id string, claimed *time.Time, due time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.schedules[id]
	if !ok {
		return false, nil
	}
	switch {
	case claimed == nil && sc.NextRunAt != nil:
		return false, nil
	case claimed != nil && (sc.NextRunAt == nil || !sc.NextRunAt.Equal(*claimed)):
		return false, nil
	}
	back := due
	sc.NextRunAt = &back
	return true, nil
}

// ClaimFinal clears a schedule's next fire time when it still equals oldNext and reports whether
// this caller won. A row that is gone loses without an error, the same as ClaimDue.
func (m *memStore) ClaimFinal(_ context.Context, id string, oldNext time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.schedules[id]
	if !ok || sc.NextRunAt == nil || !sc.NextRunAt.Equal(oldNext) {
		return false, nil
	}
	sc.NextRunAt = nil
	return true, nil
}

// ClaimDue atomically advances a schedule's next fire time and reports whether this caller won. A
// row that is gone loses without an error, the same answer the SQL backends give, since neither can
// tell a deleted schedule from one another node already advanced and the caller treats both alike.
func (m *memStore) ClaimDue(_ context.Context, id string, oldNext, newNext time.Time) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.schedules[id]
	if !ok {
		return false, nil
	}
	if sc.NextRunAt == nil || !sc.NextRunAt.Equal(oldNext) {
		return false, nil
	}
	next := newNext
	sc.NextRunAt = &next
	return true, nil
}

// memStore is an in-memory Store guarded by a read-write mutex.
type memStore struct {
	// mu guards schedules and inflight.
	mu sync.RWMutex
	// schedules maps schedule id to the stored schedule.
	schedules map[string]*Schedule
	// inflight maps a schedule id to the occurrence a claim marked in flight, kept apart from the
	// schedule itself the way the database stores keep it out of every read.
	inflight map[string]inFlightMark
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{schedules: make(map[string]*Schedule), inflight: map[string]inFlightMark{}}
}

// Save inserts or replaces the schedule identified by s.ID.
func (m *memStore) Save(_ context.Context, s *Schedule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.schedules[s.ID] = s.Clone()
	return nil
}

// Update replaces an existing schedule, or returns ErrNotFound when it is gone.
func (m *memStore) Update(_ context.Context, s *Schedule) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.schedules[s.ID]; !ok {
		return ErrNotFound
	}
	m.schedules[s.ID] = s.Clone()
	return nil
}

// RecordFire records a fire against an existing schedule, touching only what the fire owns.
func (m *memStore) RecordFire(_ context.Context, id string, at time.Time, runID, failure string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.schedules[id]
	if !ok {
		return nil
	}
	when := at
	sc.LastRunAt = &when
	if runID != "" {
		sc.LastRunID = runID
	}
	sc.LastError = util.SafeText(failure)
	sc.LastSkip = ""
	sc.SkippedFires = 0
	return nil
}

// RecordSkip records a skipped fire against an existing schedule, touching only what a fire owns.
func (m *memStore) RecordSkip(_ context.Context, id string, at time.Time, reason string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	sc, ok := m.schedules[id]
	if !ok {
		return nil
	}
	when := at
	sc.LastRunAt = &when
	sc.LastError = ""
	sc.LastSkip = util.SafeText(reason)
	sc.SkippedFires++
	return nil
}

// Get returns the schedule with the given id, or ErrNotFound.
func (m *memStore) Get(_ context.Context, id string) (*Schedule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	s, ok := m.schedules[id]
	if !ok {
		return nil, ErrNotFound
	}
	return s.Clone(), nil
}

// List returns all schedules ordered by creation time, oldest first.
func (m *memStore) List(_ context.Context) ([]*Schedule, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Schedule, 0, len(m.schedules))
	for _, s := range m.schedules {
		out = append(out, s.Clone())
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].ID < out[j].ID
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

// Delete removes the schedule with the given id, or returns ErrNotFound.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.schedules[id]; !ok {
		return ErrNotFound
	}
	delete(m.schedules, id)
	delete(m.inflight, id)
	return nil
}
