package relay_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dcadolph/switchtender/internal/event"
	"github.com/dcadolph/switchtender/internal/relay"
	"github.com/dcadolph/switchtender/internal/run"
)

// TestClientExecutionPath proves a run flows through the relay Client and its loopback transport into
// the backing store: a saved run claims, heartbeats, and records events and summaries, and the writes
// land in the backing store as if the worker held it directly.
func TestClientExecutionPath(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	c := relay.NewClient(relay.Loopback(backing))

	r := &run.Run{ID: "run_1", Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now()}
	if err := c.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	claimed, err := c.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claimed.ID != "run_1" {
		t.Fatalf("claimed %q, want run_1", claimed.ID)
	}
	if err := c.Heartbeat(ctx, "run_1", "worker-a"); err != nil {
		t.Fatalf("Heartbeat() error = %v", err)
	}
	if err := c.AppendEvents(ctx, "run_1", []event.Event{{Type: event.TypeRunnerOK, Host: "web01"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if err := c.SaveHostSummary(ctx, "run_1", []run.HostSummary{{Host: "web01", Changed: 1}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	got, err := c.Get(ctx, "run_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.ClaimedBy != "worker-a" {
		t.Errorf("claimed_by = %q, want worker-a", got.ClaimedBy)
	}
	// The writes reached the backing store through the relay.
	events, err := backing.Events(ctx, "run_1")
	if err != nil {
		t.Fatalf("backing Events() error = %v", err)
	}
	if len(events) != 1 {
		t.Errorf("backing events = %d, want 1", len(events))
	}
}

// TestClientRefusesControlNodeReads confirms a worker Client refuses the control-node-only queries.
func TestClientRefusesControlNodeReads(t *testing.T) {
	t.Parallel()
	c := relay.NewClient(relay.Loopback(run.NewMemStore()))
	if _, err := c.List(context.Background()); !errors.Is(err, relay.ErrUnsupported) {
		t.Errorf("List() error = %v, want ErrUnsupported", err)
	}
	if _, err := c.FleetHealth(context.Background(), 5); !errors.Is(err, relay.ErrUnsupported) {
		t.Errorf("FleetHealth() error = %v, want ErrUnsupported", err)
	}
}
