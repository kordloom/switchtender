package decision

import (
	"context"
	"sort"
	"sync"
)

// Store persists decision records. Implementations must be safe for concurrent use.
type Store interface {
	// Save inserts a record. A record is written once, so an id already held is ErrExists.
	Save(ctx context.Context, r *Record) error
	// Get returns the record with the given id, or ErrNotFound.
	Get(ctx context.Context, id string) (*Record, error)
	// ForRun returns the records of a run, decisions and corrections on its workflow approval steps
	// included, ordered by time and then id.
	ForRun(ctx context.Context, runID string) ([]*Record, error)
	// Redact removes a record's reason text and random value together and records why. It returns
	// ErrNotFound for an unknown id, ErrNoReason for a record given no reason, and ErrRedacted for one
	// already redacted. The commitment is kept.
	Redact(ctx context.Context, id string, red Redaction) error
	// Delete removes a record. It exists for one purpose: withdrawing the record of a decision or
	// correction whose chain entry could not be appended, so no record stands for something the chain
	// does not hold. It returns ErrNotFound for an unknown id.
	Delete(ctx context.Context, id string) error
	// FinishRedaction records that the pending redaction of record id, the one recorded under the
	// chain entry entryID, is on the chain. It is nil when that redaction is already finished, and
	// returns ErrNotFound for an unknown id and ErrRedacted when the record holds a different
	// redaction or none.
	FinishRedaction(ctx context.Context, id, entryID string) error
	// PendingRedactions returns every record whose redaction is still pending, for the janitor that
	// finishes a redaction its process left behind.
	PendingRedactions(ctx context.Context) ([]*Record, error)
}

// memStore is an in-memory Store.
type memStore struct {
	// mu guards records.
	mu sync.RWMutex
	// records holds every record by id.
	records map[string]*Record
}

// NewMemStore returns an empty in-memory Store.
func NewMemStore() Store {
	return &memStore{records: map[string]*Record{}}
}

// Save inserts a record, refusing an id already held.
func (m *memStore) Save(_ context.Context, r *Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[r.ID]; ok {
		return ErrExists
	}
	m.records[r.ID] = r.Clone()
	return nil
}

// Get returns a copy of the record with the given id.
func (m *memStore) Get(_ context.Context, id string) (*Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	r, ok := m.records[id]
	if !ok {
		return nil, ErrNotFound
	}
	return r.Clone(), nil
}

// ForRun returns copies of a run's records, oldest first.
func (m *memStore) ForRun(_ context.Context, runID string) ([]*Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Record
	for _, r := range m.records {
		if r.RunID == runID {
			out = append(out, r.Clone())
		}
	}
	SortRecords(out)
	return out, nil
}

// Redact clears a record's reason text and random value and records the redaction.
func (m *memStore) Redact(_ context.Context, id string, red Redaction) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	switch {
	case !ok:
		return ErrNotFound
	case !r.HasReason():
		return ErrNoReason
	case r.Reason.Redacted != nil:
		return ErrRedacted
	}
	r.Reason.Text, r.Reason.Random = "", ""
	r.Reason.Redacted = &red
	return nil
}

// FinishRedaction clears the pending mark of the named redaction.
func (m *memStore) FinishRedaction(_ context.Context, id, entryID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.records[id]
	switch {
	case !ok:
		return ErrNotFound
	case !r.HasReason() || r.Reason.Redacted == nil || r.Reason.Redacted.EntryID != entryID:
		return ErrRedacted
	}
	r.Reason.Redacted.Pending = false
	return nil
}

// PendingRedactions returns copies of the records whose redaction is pending.
func (m *memStore) PendingRedactions(_ context.Context) ([]*Record, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	var out []*Record
	for _, r := range m.records {
		if r.HasReason() && r.Reason.Redacted != nil && r.Reason.Redacted.Pending {
			out = append(out, r.Clone())
		}
	}
	SortRecords(out)
	return out, nil
}

// Delete removes a record.
func (m *memStore) Delete(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.records[id]; !ok {
		return ErrNotFound
	}
	delete(m.records, id)
	return nil
}

// SortRecords orders records by time, then id, the order a thread of decisions and corrections
// reads in. It is the one ordering every store returns, so a thread cannot read differently by
// backend.
func SortRecords(records []*Record) {
	sort.Slice(records, func(i, j int) bool {
		a, b := records[i].At, records[j].At
		if !a.Equal(b) {
			return a.Before(b)
		}
		return records[i].ID < records[j].ID
	})
}
