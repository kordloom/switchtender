package pgstore

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
)

// audTwoReplicas opens two handles on a PostgreSQL database of this test's own, the way two serve
// processes share one database, and closes both when the test ends.
func audTwoReplicas(t *testing.T) (*DB, *DB) {
	t.Helper()
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	a, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() replica a error = %v", err)
	}
	t.Cleanup(func() { _ = a.Close() })
	b, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() replica b error = %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return a, b
}

// TestASpanBeatNeverLandsBehindTheChainHeadOnPostgres is the beat-behind-head defect across two
// replicas on one PostgreSQL chain. Replica b, whose clock reads ten seconds ahead of replica a's,
// appends an ordinary entry between replica a's beats.
//
// The append lock serializes the two, and the ordinary path pins a behind-clock time forward, but a
// beat is held against the previous beat only. Replica a's second beat is therefore written five
// seconds behind the head, and the shared chain's recorded times run backward with nothing
// tampered with, which every bundle over the pair reports as a time problem.
func TestASpanBeatNeverLandsBehindTheChainHeadOnPostgres(t *testing.T) {
	t.Parallel()
	a, b := audTwoReplicas(t)
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if _, err := a.Audits().AppendSpanBeat(ctx, base, 60); err != nil {
		t.Fatalf("AppendSpanBeat() first beat error = %v", err)
	}
	if err := b.Audits().Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: base.Add(10 * time.Second), Actor: "admin", ActorType: "token",
		Method: "POST", Path: "/v1/templates",
	}); err != nil {
		t.Fatalf("Append() on replica b error = %v", err)
	}
	_, err := a.Audits().AppendSpanBeat(ctx, base.Add(5*time.Second), 60)
	if err != nil && !errors.Is(err, audit.ErrClockBehind) {
		t.Fatalf("AppendSpanBeat() second beat error = %v", err)
	}
	chain, err := a.Audits().Chain(ctx)
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
