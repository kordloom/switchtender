package sqlitestore_test

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestASpanBeatNeverLandsBehindTheChainHeadOnSQLite is the SQLite form of the beat-behind-head
// defect, with the ordinary entry written through a second handle on the same file, the way a
// command-line change or a second process appends beside a running server's beat loop.
//
// CheckBeatAdvance holds a beat against the previous beat only. A beat whose time is after the last
// beat but before the head is written behind the entry ahead of it, so the chain's recorded times
// run backward and every bundle over the pair reports a time problem on an untampered install.
func TestASpanBeatNeverLandsBehindTheChainHeadOnSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "switchtender.db")
	server, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = server.Close() })
	other, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() second handle error = %v", err)
	}
	t.Cleanup(func() { _ = other.Close() })

	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := server.Audits().AppendSpanBeat(ctx, base, 60); err != nil {
		t.Fatalf("AppendSpanBeat() first beat error = %v", err)
	}
	if err := other.Audits().Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base.Add(10 * time.Second), Actor: "cli:operator",
		ActorType: "cli", Method: audit.MethodCLI, Path: "/cli/token/new",
	}); err != nil {
		t.Fatalf("Append() through the second handle error = %v", err)
	}
	_, err = server.Audits().AppendSpanBeat(ctx, base.Add(5*time.Second), 60)
	if err != nil && !errors.Is(err, audit.ErrClockBehind) {
		t.Fatalf("AppendSpanBeat() second beat error = %v", err)
	}
	chain, err := server.Audits().Chain(ctx)
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
