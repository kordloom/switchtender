package audit

import (
	"context"
	"fmt"
	"slices"
	"sort"
	"sync"
	"time"
)

// memStore is an in-memory audit Store guarded by a mutex.
type memStore struct {
	// mu guards entries and anchors.
	mu sync.RWMutex
	// entries holds every appended entry in chain order.
	entries []*Entry
	// installID stamps every appended entry, empty when no identity is bound.
	installID string
	// anchors holds every recorded anchor.
	anchors []*Anchor
}

// SaveAnchor records one anchor.
func (m *memStore) SaveAnchor(_ context.Context, a *Anchor) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := *a
	m.anchors = append(m.anchors, &cp)
	return nil
}

// Anchors returns every anchor at or below seq, oldest first. A seq of zero or less returns all.
func (m *memStore) Anchors(_ context.Context, seq int64) ([]*Anchor, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Anchor, 0, len(m.anchors))
	for _, a := range m.anchors {
		if seq > 0 && a.Seq > seq {
			continue
		}
		cp := *a
		out = append(out, &cp)
	}
	// Ties break on id, matching both SQL stores, so all three return the same order.
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq == out[j].Seq {
			return out[i].ID < out[j].ID
		}
		return out[i].Seq < out[j].Seq
	})
	return out, nil
}

// DeleteAnchor removes the anchor with the given id, or reports ErrAnchorNotFound.
func (m *memStore) DeleteAnchor(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i, a := range m.anchors {
		if a.ID == id {
			m.anchors = append(m.anchors[:i], m.anchors[i+1:]...)
			return nil
		}
	}
	return fmt.Errorf("delete anchor %s: %w", id, ErrAnchorNotFound)
}

// NewMemStore returns an empty in-memory audit Store.
func NewMemStore() Store {
	return &memStore{}
}

// Append records one entry, linking it to the current head so the chain stays intact. A span
// marker entry is refused: only AppendSpanBeat mints beats.
func (m *memStore) Append(_ context.Context, e *Entry) error {
	if IsSpanMarker(e) {
		return fmt.Errorf("append audit entry: %w", ErrReservedSpan)
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	BindEntryInstall(e, m.installID)
	var prev *Entry
	if n := len(m.entries); n > 0 {
		prev = m.entries[n-1]
	}
	cp := *e
	Link(prev, &cp)
	m.entries = append(m.entries, &cp)
	*e = cp
	return nil
}

// AppendSpanBeat mints and appends the next span beat under the append mutex, so the beat read and
// the append are one atomic step and concurrent callers cannot mint the same beat. A time that does
// not advance past the newest beat is refused with ErrClockBehind and nothing is written: a beat's
// time is a signed claim, so writing a time the clock did not read would be a false statement in an
// attestation. The skipped beat surfaces as a reported gap, and its number waits for the next beat
// the chain accepts.
func (m *memStore) AppendSpanBeat(_ context.Context, at time.Time, cadenceS int) (*Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var prev *Entry
	var headSeq int64
	if n := len(m.entries); n > 0 {
		prev = m.entries[n-1]
		headSeq = prev.Seq
	}
	var lastSpanSeq, lastSpanBeat int64
	var lastSpanAt time.Time
	for i := len(m.entries) - 1; i >= 0; i-- {
		e := m.entries[i]
		if e.Actor != SpanActor || e.Method != SpanMethod {
			continue
		}
		// A span-marked entry whose path does not round-trip is an ordinary entry that merely
		// wears the actor, so it is skipped rather than trusted for a beat number.
		if b, _, _, ok := ParseSpanPath(e.Path); ok {
			lastSpanSeq, lastSpanBeat, lastSpanAt = e.Seq, b, e.At
			break
		}
	}
	beat, count := NextSpanBeat(headSeq, lastSpanSeq, lastSpanBeat)
	// The time is checked against the same newest beat the numbering came from, under the same lock,
	// so a clock that stepped backward skips the beat instead of minting one that fails every bundle
	// covering the pair. See CheckBeatAdvance.
	if err := CheckBeatAdvance(at, lastSpanAt, beat); err != nil {
		return nil, fmt.Errorf("append span beat: %w", err)
	}
	e := NewSpanEntry(at, beat, count, cadenceS)
	BindEntryInstall(e, m.installID)
	Link(prev, e)
	m.entries = append(m.entries, e)
	cp := *e
	return &cp, nil
}

// SpanBeats returns the newest limit span beat entries, oldest first. Near-miss entries that wear
// the span actor without a round-tripping path are ordinary entries and are excluded.
func (m *memStore) SpanBeats(_ context.Context, limit int) ([]*Entry, error) {
	if limit < 1 {
		limit = 1
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := []*Entry{}
	for i := len(m.entries) - 1; i >= 0 && len(out) < limit; i-- {
		if !IsSpanMarker(m.entries[i]) {
			continue
		}
		cp := *m.entries[i]
		out = append(out, &cp)
	}
	slices.Reverse(out)
	return out, nil
}

// List returns up to limit entries, newest first.
func (m *memStore) List(_ context.Context, limit int) ([]*Entry, error) {
	if limit < 1 {
		limit = 1
	}
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Entry, 0, len(m.entries))
	for _, e := range m.entries {
		cp := *e
		out = append(out, &cp)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Seq == out[j].Seq {
			return out[i].ID > out[j].ID
		}
		return out[i].Seq > out[j].Seq
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// Chain returns every entry in chain order, oldest first, for verification.
func (m *memStore) Chain(_ context.Context) ([]*Entry, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]*Entry, 0, len(m.entries))
	for _, e := range m.entries {
		cp := *e
		out = append(out, &cp)
	}
	return out, nil
}

// ChainScan streams every entry above afterSeq in chain order, oldest first. Entries are copied
// before the scan so fn runs without the lock and may keep what it is handed.
func (m *memStore) ChainScan(ctx context.Context, afterSeq int64, fn func(*Entry) error) error {
	entries, err := m.Chain(ctx)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if e.Seq <= afterSeq {
			continue
		}
		if err := fn(e); err != nil {
			return err
		}
	}
	return nil
}

// BindInstall sets the install every later append is stamped with.
func (m *memStore) BindInstall(installID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.installID = installID
}
