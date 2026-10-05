package audit

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestASpanBeatNeverLandsBehindTheChainHead appends a beat, then an ordinary entry stamped ten
// seconds later, then a beat whose time is after the first beat but before that entry.
//
// The later entry is what a concurrent request looks like when it takes the append lock between
// the beat loop reading its clock and the beat reaching the store, or when the beat loop's tick sat
// in the channel while the previous beat's anchor request was in flight, or simply a replica whose
// clock is a few seconds ahead. StampAppendTime says the span beat refuses a behind-clock append
// before reaching the pin, but CheckBeatAdvance compares the beat with the previous beat only,
// never with the head. The beat is written behind the entry before it, so the chain's recorded
// times run backward on an install where nothing was tampered with, and every bundle over it
// reports a time problem, the signature the pin exists to keep off honest records.
func TestASpanBeatNeverLandsBehindTheChainHead(t *testing.T) {
	t.Parallel()
	audSpanBehindHead(t, NewMemStore())
}

// audSpanBehindHead drives the beat, entry, beat sequence against store and fails when the chain's
// recorded times run backward.
func audSpanBehindHead(t *testing.T, store Store) {
	t.Helper()
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := store.AppendSpanBeat(ctx, base, 60); err != nil {
		t.Fatalf("AppendSpanBeat() first beat error = %v", err)
	}
	if err := store.Append(ctx, &Entry{
		ID: NewID(), At: base.Add(10 * time.Second), Actor: "admin", ActorType: "token",
		Method: "POST", Path: "/v1/templates",
	}); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	_, err := store.AppendSpanBeat(ctx, base.Add(5*time.Second), 60)
	if err != nil && !errors.Is(err, ErrClockBehind) {
		t.Fatalf("AppendSpanBeat() second beat error = %v", err)
	}
	chain, err := store.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for i := 1; i < len(chain); i++ {
		if chain[i].At.Before(chain[i-1].At) {
			t.Errorf("entry %d (%s %s) is dated %s, before entry %d at %s: a beat was written "+
				"behind the chain head", chain[i].Seq, chain[i].Method, chain[i].Path,
				chain[i].At.Format(time.RFC3339), chain[i-1].Seq,
				chain[i-1].At.Format(time.RFC3339))
		}
	}
}
