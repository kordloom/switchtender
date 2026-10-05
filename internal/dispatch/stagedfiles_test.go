package dispatch

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/factcache"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRunFilesGoneBeforeASetupFailureIsRecorded is the setup half of the run-files order. A run
// whose setup fails after its stored inventory was written, here because the project it names
// cannot be checked out on this executor, is recorded failed by the same terminal write a finished
// play gets, and its inventory directory, with the host list's secret variables in it, must be gone
// by then too. The run is queued straight into the store, as one submitted on another node is,
// since a submission here would refuse the project before anything was staged.
func TestRunFilesGoneBeforeASetupFailureIsRecorded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Project    string
		WantError  string
		WantStatus run.Status
	}{{ // Test 0: The project cannot be checked out once the inventory is in place.
		Name: "project unavailable", Project: "proj_gone",
		WantError: "project", WantStatus: run.StatusFailed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			root := t.TempDir()
			inventories := inventory.NewMemStore()
			if err := inventories.Save(ctx, &inventory.Inventory{ID: "inv_1", Name: "fleet",
				Content:   "[web]\nweb01 ansible_password=staged-host-secret-81d2\n",
				CreatedAt: time.Now()}); err != nil {
				t.Fatal(err)
			}
			store := &rlyFinalizeSnapshot{Store: run.NewMemStore(), root: root}
			ran := false
			runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec, io.Writer) (
				roundhouse.Result, error) {
				ran = true
				return roundhouse.Result{}, nil
			})
			d := New(store, runner, nil, WithNoJanitor(), WithInventories(inventories),
				WithFactCache(factcache.NewMemStore()), WithRunFilesRoot(root))
			defer d.Close()
			r := &run.Run{ID: fmt.Sprintf("run_setup_%d", testNum), Playbook: "site.yml",
				Status: run.StatusPending, InventoryID: "inv_1", ProjectID: test.Project,
				UseFactCache: true, CreatedAt: time.Now()}
			// A run submitted on another node arrives carrying the inventory snapshot its submission
			// took, which execution requires before it stages anything.
			if err := d.snapshotInventory(ctx, r); err != nil {
				t.Fatalf("snapshotInventory() error = %v", err)
			}
			if err := store.Save(ctx, r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			d.wake()
			done := waitTerminal(t, store, r.ID)
			if done.Status != test.WantStatus || !strings.Contains(done.Error, test.WantError) {
				t.Fatalf("run = %s (%s), want %s naming %q", done.Status, done.Error,
					test.WantStatus, test.WantError)
			}
			if ran {
				t.Fatal("the tool ran, so the setup did not fail where this test needs it to")
			}
			if diff := cmp.Diff([]string(nil), store.leftAtFinalize(),
				cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("run directories present when the failed setup was recorded "+
					"(-want +got):\n%s", diff)
			}
		})
	}
}

// TestStagedFilesRemoveEachOnceNewestFirst pins the helper the run-files order rests on: every
// staged removal runs exactly once, whichever call reaches it, and the most recent first.
func TestStagedFilesRemoveEachOnceNewestFirst(t *testing.T) {
	t.Parallel()
	var order []string
	s := &stagedFiles{}
	s.add(func() { order = append(order, "inventory") })
	s.add(nil)
	s.add(func() { order = append(order, "facts") })
	s.removeAll()
	s.removeAll()
	if diff := cmp.Diff([]string{"facts", "inventory"}, order); diff != "" {
		t.Errorf("removals (-want +got):\n%s", diff)
	}
}
