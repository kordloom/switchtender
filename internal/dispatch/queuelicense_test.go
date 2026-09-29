package dispatch

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAQueueInheritedFromAnInventoryIsLicensed covers the door the request-side gate cannot see.
//
// A named queue restricts a run to workers serving that name, and workers are Team, so on an
// install without them nothing can ever claim such a run. The server refuses a request that names
// a queue for exactly that reason, and says which tier it needs, because the alternative is a run
// that sits pending forever with no error anywhere to explain it.
//
// A queue does not only arrive in the request. It also comes from the launched template, and after
// that from the run's stored inventory, and the inventory's is filled in by the dispatcher after
// the handler has looked at an empty request field and waved the run through. A Community install
// with a queue on an inventory therefore accepted every run against that inventory and stranded
// every one of them, which is the precise failure the gate was written to prevent, reached through
// a door the gate is not standing in.
//
// A lapsed Team install reaches the same state by a different route, which is why the lapse is
// covered here as well.
func TestAQueueInheritedFromAnInventoryIsLicensed(t *testing.T) {
	tests := []struct {
		Name    string
		License *license.License
		WantErr bool
	}{{ // Test 0: No license at all. Nothing can serve the queue.
		Name: "community", License: nil, WantErr: true,
	}, { // Test 1: A lapsed Team license. The workers it paid for have stopped.
		Name: "lapsed", WantErr: true,
		License: &license.License{Claims: license.Claims{
			V: 1, ID: "lic_lapsed", Org: "Example", Tier: license.TierTeam,
			Issued: "2026-01-01T00:00:00Z", Expires: "2026-02-01T00:00:00Z",
		}},
	}, { // Test 2: A live Team license. The queue is exactly what was bought.
		Name: "team", WantErr: false,
		License: &license.License{Claims: license.Claims{
			V: 1, ID: "lic_live", Org: "Example", Tier: license.TierTeam,
			Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
		}},
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			// license.Set is process-wide, so these cannot run in parallel.
			held := license.Current()
			license.Set(test.License)
			t.Cleanup(func() { license.Set(held) })

			ctx := context.Background()
			invStore := inventory.NewMemStore()
			if err := invStore.Save(ctx, &inventory.Inventory{
				ID: "inv_dmz", Name: "dmz", Content: "[web]\nhost1", Queue: "dmz",
			}); err != nil {
				t.Fatalf("test %d: inventory Save() error = %v", testNum, err)
			}
			runner := roundhouse.RunnerFunc(
				func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
					return roundhouse.Result{ExitCode: 0}, nil
				})
			d := New(run.NewMemStore(), runner, nil, WithInventories(invStore))
			defer d.Close()

			// The request names no queue. The handler's gate sees an empty field and passes.
			created, err := d.Submit(ctx, "play.yml", "", run.WithInventory("inv_dmz"))

			if !test.WantErr {
				if err != nil {
					t.Fatalf("test %d: Submit() error = %v, want the run accepted", testNum, err)
				}
				if created.Queue != "dmz" {
					t.Errorf("test %d: Queue = %q, want %q: a licensed install must still inherit "+
						"the inventory's queue", testNum, created.Queue, "dmz")
				}
				return
			}
			if !errors.Is(err, ErrQueueUnlicensed) {
				t.Fatalf("test %d: Submit() error = %v, want %v.\nThe run was accepted and pinned "+
					"to a queue no worker serves, so it sits pending forever with nothing to "+
					"explain it", testNum, err, ErrQueueUnlicensed)
			}
		})
	}
}

// TestAnUnservableQueueIsNotSilentlyRunOnTheControlNode pins which of the two wrong answers was
// rejected, since the other one is the tempting fix.
//
// Dropping the queue and letting the work run on the server's own pool keeps the install moving
// and reads as the gentler option. It is the dangerous one: a queue is usually drawn around a
// network somebody meant to keep separate, so silently running the work somewhere else crosses the
// boundary the queue exists to hold. Refusing is loud and safe.
func TestAnUnservableQueueIsNotSilentlyRunOnTheControlNode(t *testing.T) {
	held := license.Current()
	license.Set(nil)
	t.Cleanup(func() { license.Set(held) })

	ctx := context.Background()
	invStore := inventory.NewMemStore()
	if err := invStore.Save(ctx, &inventory.Inventory{
		ID: "inv_dmz", Name: "dmz", Content: "[web]\nhost1", Queue: "dmz",
	}); err != nil {
		t.Fatalf("inventory Save() error = %v", err)
	}
	var ran bool
	runner := roundhouse.RunnerFunc(
		func(_ context.Context, _ roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
			ran = true
			return roundhouse.Result{ExitCode: 0}, nil
		})
	store := run.NewMemStore()
	d := New(store, runner, nil, WithInventories(invStore))
	defer d.Close()

	created, err := d.Submit(ctx, "play.yml", "", run.WithInventory("inv_dmz"))
	if err == nil {
		t.Fatalf("Submit() accepted a run for a queue nothing can serve and created %s", created.ID)
	}
	if ran {
		t.Error("the work ran on the control node's own pool, crossing the boundary the queue was " +
			"drawn for")
	}
}
