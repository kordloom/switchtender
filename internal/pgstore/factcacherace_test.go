package pgstore

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
)

// TestFactSaveRacingAnInventoryDeleteLeavesNoFacts saves a run's facts while a delete of their
// inventory is under way on another connection: the delete has removed the inventory and its facts
// and has not committed.
//
// A save that checked for the inventory without a lock saw it, since the delete had not committed,
// and wrote facts the delete's own statement could no longer see. Both committed, and the deleted
// inventory's facts were back with nothing left to delete them through. The save reads the
// inventory under a key share lock, so it waits for the delete and then finds the inventory gone.
func TestFactSaveRacingAnInventoryDeleteLeavesNoFacts(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	ctx := context.Background()
	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Inventories().Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: "web01", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	raw := rawHandle(t, dsn)
	del, err := raw.BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin the delete: %v", err)
	}
	defer func() { _ = del.Rollback() }()
	for _, q := range []string{"DELETE FROM inventories WHERE id='inv_1'",
		"DELETE FROM host_fact_cache WHERE inventory_id='inv_1'"} {
		if _, err := del.Exec(q); err != nil {
			t.Fatalf("%s: %v", q, err)
		}
	}

	done := make(chan error, 1)
	go func() {
		done <- db.FactCache().SaveFacts(ctx, []factcache.Entry{{InventoryID: "inv_1",
			Host: "web01", Facts: json.RawMessage(`{"ansible_env":{"HOME":"/root"}}`),
			RunID: "run_1", ModifiedAt: time.Now()}})
	}()
	// The save either finishes on its own, which a save that takes no lock does, or waits on the
	// delete's row lock. Either way the delete then commits.
	var saved error
	finished := false
	deadline := time.Now().Add(30 * time.Second)
	for !finished {
		select {
		case saved = <-done:
			finished = true
			continue
		default:
		}
		var waiting int
		if err := raw.QueryRow(`SELECT count(*) FROM pg_stat_activity
WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&waiting); err != nil {
			t.Fatalf("read pg_stat_activity: %v", err)
		}
		if waiting > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the save neither finished nor waited on the delete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := del.Commit(); err != nil {
		t.Fatalf("commit the delete: %v", err)
	}
	if !finished {
		select {
		case saved = <-done:
		case <-time.After(30 * time.Second):
			t.Fatal("the save did not finish after the delete committed")
		}
	}
	if saved != nil {
		t.Fatalf("SaveFacts() error = %v, want the deleted inventory's entry dropped quietly", saved)
	}
	left, err := db.FactCache().List(ctx, "inv_1", false)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var hosts []string
	for _, e := range left {
		hosts = append(hosts, e.Host)
	}
	if diff := cmp.Diff([]string(nil), hosts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("cached facts for the deleted inventory (-want +got):\n%s", diff)
	}
}
