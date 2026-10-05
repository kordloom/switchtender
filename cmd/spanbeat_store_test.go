package cmd

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/spanbeat"
)

// The emitter times a beat with the chain's clock only through this interface, so the adapter has
// to keep satisfying it.
var _ spanbeat.ClockedStore = auditBeatStore{}

// TestAuditBeatStoreTimesBeatsWithTheChainClock verifies the beat the emitter appends through the
// adapter is timed by the chain under its append lock, and lands after an entry another request
// appended moments before, where a beat timed before that entry is refused.
func TestAuditBeatStoreTimesBeatsWithTheChainClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	chain := audit.NewMemStore()
	store := auditBeatStore{store: chain}
	before := time.Now()
	entry := &audit.Entry{ID: audit.NewID(), Actor: "admin", ActorType: "token", Method: "POST",
		Path: "/v1/templates"}
	if err := chain.Append(ctx, entry); err != nil {
		t.Fatalf("Append() error = %v", err)
	}

	if _, err := store.AppendSpanBeat(ctx, before, 60); !errors.Is(err, audit.ErrClockBehind) {
		t.Errorf("AppendSpanBeat() timed before the newest entry error = %v, want "+
			"audit.ErrClockBehind", err)
	}
	beat, err := store.AppendSpanBeatNow(ctx, 60)
	if err != nil {
		t.Fatalf("AppendSpanBeatNow() error = %v", err)
	}
	if beat.Beat != 1 || beat.At.Before(entry.At) {
		t.Errorf("AppendSpanBeatNow() = beat %d at %s, want beat 1 at or after the newest entry's %s",
			beat.Beat, beat.At, entry.At)
	}
}
