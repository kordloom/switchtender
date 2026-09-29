package relay

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
)

// TestFactsAreBoundToTheHostsTheRunRecorded covers the one relay write that reached outside the run it
// belongs to. Every report call is checked against the worker's lease for that run, which is right, and
// the host facts table is keyed on the host alone: whatever host name the body carried was the row that
// got replaced. A worker holding a legitimate lease for its own one-host run could therefore rewrite the
// recorded facts for any machine in the fleet, or invent machines that do not exist, and the control node
// stored it as gathered evidence.
//
// Facts are now bounded by the hosts the run has recorded results for. That does not make a worker
// trustworthy, since a worker also authors the results, and it is not meant to: it makes a fabrication
// attributable. A worker that wants to write facts for a machine must first say, on its own run's record
// and in the dossier and fleet page drawn from it, that the run touched that machine. Nothing can be
// written about a host from nowhere.
func TestFactsAreBoundToTheHostsTheRunRecorded(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	claimed := time.Now()
	if err := store.Save(ctx, &run.Run{
		ID: "run_1", Status: run.StatusRunning, CreatedAt: claimed, ClaimedBy: "worker-1",
		ClaimSecret: "lease-1", Queue: "default", Tool: "ansible", Playbook: "site.yml",
	}); err != nil {
		t.Fatalf("Save run: %v", err)
	}

	handler := NewHandler(store, SinglePool("ymt_worker"), zap.NewNop(), nil, nil)
	post := func(path, body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		r.Header.Set("Authorization", "Bearer ymt_worker")
		r.Header.Set(leaseHeader, "lease-1")
		r.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, r)
		return rec
	}

	// Test 0: Facts for a host this run has recorded nothing about are refused, and nothing lands.
	rec := post("/relay/v1/runs/run_1/host-facts",
		`[{"run_id":"run_1","host":"db-prod-1","facts":{"os":"totally-fine"}}]`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("facts for an unrecorded host = %d, want 403 (body %s)", rec.Code, rec.Body.String())
	}
	if facts, err := store.HostFactsFor(ctx, "db-prod-1"); err == nil && len(facts.Facts) > 0 {
		t.Error("the fabricated facts were stored, so the fleet record describes a machine no run " +
			"claims to have touched")
	}

	// The run reports the host it ran against, which is the claim the facts then hang off.
	if rec := post("/relay/v1/runs/run_1/host-summary",
		`[{"run_id":"run_1","host":"web-1","ok":1,"worst":"ok"}]`); rec.Code != http.StatusNoContent {
		t.Fatalf("host summary = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}

	// Test 1: Facts for that host are now stored.
	if rec := post("/relay/v1/runs/run_1/host-facts",
		`[{"run_id":"run_1","host":"web-1","facts":{"os":"debian"}}]`); rec.Code != http.StatusNoContent {
		t.Errorf("facts for its own host = %d, want 204 (body %s)", rec.Code, rec.Body.String())
	}
	if facts, err := store.HostFactsFor(ctx, "web-1"); err != nil || len(facts.Facts) == 0 {
		t.Errorf("the run's own host facts did not land: %v", err)
	}

	// Test 2: A mixed batch is refused whole rather than partly applied, so a worker cannot smuggle an
	// unrecorded host in behind a legitimate one.
	rec = post("/relay/v1/runs/run_1/host-facts",
		`[{"run_id":"run_1","host":"web-1","facts":{"os":"debian"}},`+
			`{"run_id":"run_1","host":"db-prod-1","facts":{"os":"smuggled"}}]`)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mixed batch = %d, want 403", rec.Code)
	}
	if facts, err := store.HostFactsFor(ctx, "db-prod-1"); err == nil && len(facts.Facts) > 0 {
		t.Error("the smuggled host's facts landed anyway")
	}

	// Test 3: The refusal says what to do, since the ordinary cause is a report arriving out of order
	// rather than an attack.
	if !strings.Contains(rec.Body.String(), "db-prod-1") {
		t.Errorf("refusal %q does not name the host it refused", rec.Body.String())
	}
}
