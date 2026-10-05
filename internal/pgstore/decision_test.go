package pgstore_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/decisiontest"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDecisionStoreContract runs the decision record contract against PostgreSQL.
func TestDecisionStoreContract(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	decisiontest.Contract(t, func() decision.Store {
		truncateTable(t, dsn, "run_decisions")
		return db.Decisions()
	})
}

// TestRetentionTakesAReasonWithItsRun pins on PostgreSQL that an approver's reason is held exactly
// as long as its run: purging a run past retention removes its decision records, and a run still
// kept keeps its own.
func TestRetentionTakesAReasonWithItsRun(t *testing.T) {
	dsn := testDSN(t)
	db, err := pgstore.Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	truncateAll(t, dsn)
	ctx := context.Background()
	old := time.Now().Add(-90 * 24 * time.Hour)
	ended := old.Add(time.Minute)
	for _, r := range []*run.Run{
		{ID: "run_old", Playbook: "a.yml", Status: run.StatusRejected, CreatedAt: old, EndedAt: &ended},
		{ID: "run_new", Playbook: "a.yml", Status: run.StatusRejected, CreatedAt: time.Now(),
			EndedAt: &ended},
	} {
		if err := db.Runs().Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
		rec := &decision.Record{ID: "aud_" + r.ID, Kind: decision.KindDecision,
			DecisionID: "aud_" + r.ID, RunID: r.ID, Verdict: "rejected", At: r.CreatedAt,
			Actor: "ops-admin", Reason: &decision.Reason{Text: "not today",
				Random: "00", Commitment: "sha256:00"}}
		if err := db.Decisions().Save(ctx, rec); err != nil {
			t.Fatalf("Save(decision) error = %v", err)
		}
	}
	if _, err := db.Runs().PurgeRunsBefore(ctx, time.Now().Add(-30*24*time.Hour)); err != nil {
		t.Fatalf("PurgeRunsBefore() error = %v", err)
	}
	if _, err := db.Decisions().Get(ctx, "aud_run_old"); !errors.Is(err, decision.ErrNotFound) {
		t.Errorf("the purged run's decision record: Get() error = %v, want ErrNotFound", err)
	}
	if _, err := db.Decisions().Get(ctx, "aud_run_new"); err != nil {
		t.Errorf("the kept run's decision record: Get() error = %v, want it kept", err)
	}
}
