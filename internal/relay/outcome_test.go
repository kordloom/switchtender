package relay_test

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRelayCommitsRunOutcome proves a run executed on a relay worker is receiptable too: when the
// worker reports it finished, the control node commits the same outcome entry an in-process run gets,
// and the committed digest verifies against the run's evidence. Only the control node holds the chain,
// so this is the one place a relay run's outcome can be recorded.
func TestRelayCommitsRunOutcome(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	audits := audit.NewMemStore()
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil, audits))
	t.Cleanup(ts.Close)
	c := relay.NewClient(relay.NewHTTPTransport(ts.URL, testWorkerToken, nil))

	if err := backing.Save(ctx, &run.Run{
		ID: "run_relay", Playbook: "site.yml", Inventory: "prod", Status: run.StatusPending,
		Actor: "alice", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}

	claimed, err := c.Claim(ctx, "worker-a", []string{""})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if err := c.AppendLog(ctx, "run_relay", []byte("ok: [web01]\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	if err := c.SaveHostSummary(ctx, "run_relay",
		[]run.HostSummary{{Host: "web01", OK: 1, Worst: "ok"}}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	claimed.Status = run.StatusSucceeded
	if err := c.Save(ctx, claimed); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var outcomeEntry *audit.Entry
	for _, e := range chain {
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/run_relay/outcome/") {
			outcomeEntry = e
			break
		}
	}
	if outcomeEntry == nil {
		t.Fatal("the control node committed no outcome entry for the relay run")
	}
	if outcomeEntry.Path != "/runs/run_relay/outcome/succeeded" {
		t.Errorf("outcome path = %q, want the succeeded outcome", outcomeEntry.Path)
	}
	if outcomeEntry.Actor != "system:relay" {
		t.Errorf("outcome actor = %q, want system:relay", outcomeEntry.Actor)
	}
	if outcomeEntry.OnBehalfOf != "alice" {
		t.Errorf("outcome on_behalf_of = %q, want alice", outcomeEntry.OnBehalfOf)
	}

	// The digest must verify against the run's real outcome as the control node stored it.
	got, err := backing.Get(ctx, "run_relay")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	body, err := outcome.Body(ctx, backing, got)
	if err != nil {
		t.Fatalf("outcome.Body() error = %v", err)
	}
	if !audit.VerifyContentDigest(outcomeEntry.ContentDigest, outcomeEntry.Nonce, body) {
		t.Error("committed relay outcome digest does not verify against the run's evidence")
	}
}
