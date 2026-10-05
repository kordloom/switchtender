package dispatch_test

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// schWaitDone polls the run store until the run is terminal or the budget runs out.
func schWaitDone(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	for {
		r, err := store.Get(context.Background(), id)
		if err == nil && r.Status.Terminal() {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach a terminal state", id)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestFactCacheInventoryDeletedDuringARunGetsItsFactsBack deletes a stored inventory through the
// real SQLite store, which removes the inventory and its cached facts in one transaction, while a
// run of a fact caching template against it is still executing, and then lets the run finish.
//
// The API reference promises that deleting an inventory deletes every fact cached for its hosts,
// and the store deletes both together because facts left behind would be served to a later
// inventory that reused the id, such as one a backup restores. The run's collection writes what it
// gathered with an upsert that never asks whether the inventory still exists, and nothing ties a
// cached fact to a live inventory, so the deleted inventory's facts are back the moment the run
// ends, holding whatever the hosts reported about themselves, with no inventory left to delete
// them through.
func TestFactCacheInventoryDeletedDuringARunGetsItsFactsBack(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := db.Inventories().Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
		Content: "[web]\nweb01\n", CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	gathered := make(chan struct{})
	release := make(chan struct{})
	runner := roundhouse.RunnerFunc(func(ctx context.Context, spec roundhouse.Spec,
		_ io.Writer) (roundhouse.Result, error) {
		if spec.FactCacheDir == "" {
			return roundhouse.Result{ExitCode: 1}, nil
		}
		if err := os.WriteFile(filepath.Join(spec.FactCacheDir, "web01"),
			[]byte(`{"ansible_env":{"HOME":"/root"}}`), 0o600); err != nil {
			return roundhouse.Result{ExitCode: 1}, err
		}
		close(gathered)
		select {
		case <-release:
		case <-ctx.Done():
			return roundhouse.Result{ExitCode: 1}, ctx.Err()
		}
		return roundhouse.Result{}, nil
	})
	runs := db.Runs()
	d := dispatch.New(runs, runner, nil, dispatch.WithNoJanitor(),
		dispatch.WithInventories(db.Inventories()), dispatch.WithFactCache(db.FactCache()))
	defer d.Close()
	r, err := d.Submit(ctx, "site.yml", "", run.WithInventory("inv_1"), run.WithFactCache(true, 0))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	select {
	case <-gathered:
	case <-time.After(60 * time.Second):
		t.Fatal("the run never gathered")
	}
	if err := db.Inventories().Delete(ctx, "inv_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	close(release)
	schWaitDone(t, runs, r.ID)

	left, err := db.FactCache().List(ctx, "inv_1", false)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var hosts []string
	for _, e := range left {
		hosts = append(hosts, e.Host)
	}
	if diff := cmp.Diff([]string(nil), hosts, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("cached facts for the deleted inventory inv_1 after the run ended (-want +got):\n%s",
			diff)
	}
}
