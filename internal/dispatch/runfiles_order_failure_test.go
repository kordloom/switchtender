package dispatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// rlyFinalizeSnapshot is a run store that lists the run directories under root at the instant a
// run is recorded as finished, which is the moment the documentation says they are already gone.
type rlyFinalizeSnapshot struct {
	run.Store
	// root is the run-files root to list.
	root string
	// mu guards left.
	mu sync.Mutex
	// left holds the run directories present when the run was recorded as finished.
	left []string
}

// FinalizeRunning lists the run directories under root, then records the run as finished.
func (p *rlyFinalizeSnapshot) FinalizeRunning(ctx context.Context, id string,
	fin run.Finalization) (bool, error) {
	entries, _ := os.ReadDir(p.root)
	p.mu.Lock()
	for _, e := range entries {
		if e.IsDir() {
			p.left = append(p.left, e.Name())
		}
	}
	p.mu.Unlock()
	return p.Store.FinalizeRunning(ctx, id, fin)
}

// leftAtFinalize returns the run directories present when the run was recorded as finished.
func (p *rlyFinalizeSnapshot) leftAtFinalize() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.left...)
}

// TestRunFilesGoneBeforeTheRunIsRecordedFinished runs a playbook against a stored inventory whose
// hosts carry a secret variable, with the fact cache on, and lists the run-files root at the
// instant the run is recorded as finished.
//
// The run-files and secrets pages promise that a run's directory is removed before the run is
// recorded as finished, on every way a run ends, and both list the stored inventory and the run's
// copy of the fact cache among what a run stages. Only the credential directory keeps that order.
// The inventory's directory and the fact cache's directory are removed by deferred calls that run
// after the terminal record is written, so a process killed in that window leaves a finished run's
// secret host variables and cached facts on disk for the sweep to find minutes later, or for good
// on a host whose last process just died.
func TestRunFilesGoneBeforeTheRunIsRecordedFinished(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		ExitCode   int
		WantStatus run.Status
	}{{ // Test 0: The play succeeds.
		Name: "succeeded", ExitCode: 0, WantStatus: run.StatusSucceeded,
	}, { // Test 1: The play fails on a host.
		Name: "failed", ExitCode: 2, WantStatus: run.StatusFailed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root := t.TempDir()
			inventories := inventory.NewMemStore()
			if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
				Content:   "[web]\nweb01 ansible_password=rly-host-secret-5c1e\n",
				CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			store := &rlyFinalizeSnapshot{Store: run.NewMemStore(), root: root}
			runner := roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
				_ io.Writer) (roundhouse.Result, error) {
				if spec.FactCacheDir == "" {
					return roundhouse.Result{ExitCode: 1}, fmt.Errorf("no fact cache directory")
				}
				return roundhouse.Result{ExitCode: test.ExitCode}, nil
			})
			d := New(store, runner, nil, WithNoJanitor(), WithInventories(inventories),
				WithFactCache(factcache.NewMemStore()), WithRunFilesRoot(root))
			defer d.Close()
			r, err := d.Submit(ctx, "site.yml", "", run.WithInventory("inv_1"),
				run.WithFactCache(true, 0))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			done := waitTerminal(t, store, r.ID)
			if done.Status != test.WantStatus {
				t.Fatalf("run = %s (%s), want %s", done.Status, done.Error, test.WantStatus)
			}
			if diff := cmp.Diff([]string(nil), store.leftAtFinalize(),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("run directories present when the run was recorded as finished "+
					"(-want +got):\n%s", diff)
			}
		})
	}
}
