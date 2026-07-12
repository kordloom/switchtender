package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/ai"
	"github.com/dcadolph/yardmaster/internal/event"
	"github.com/dcadolph/yardmaster/internal/run"
)

// seedDrift stores a check run and a host summary so DriftStatus reports the host.
func seedDrift(t *testing.T, store run.Store, runID, tool, host string, changed int) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &run.Run{
		ID: runID, Playbook: "site.yml", Inventory: "hosts.ini", Tool: tool, DryRun: true,
		Status: run.StatusSucceeded, CreatedAt: at,
		ProjectID: "proj1", InventoryID: "inv1", CredentialIDs: []string{"cred1"}, Queue: "dmz",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sums := []run.HostSummary{{Host: host, Changed: changed, Worst: "changed", RanAt: at}}
	if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
}

// TestReconcileDrift covers the proposal builder: the happy path clones the check run held for
// approval and limited to the host, an in-sync host is refused, an unknown host is not found, a
// non-ansible check is refused, and an empty host is invalid.
func TestReconcileDrift(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	seedDrift(t, store, "chk_web", "", "web01", 3)
	seedDrift(t, store, "chk_db", "", "db01", 0)
	seedDrift(t, store, "chk_bash", "bash", "job01", 2)

	fake := &fakeSubmitter{run: &run.Run{ID: "run_prop", Status: run.StatusPendingApproval}}
	handler := New(store, fake, zap.NewNop()).Handler()
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/drift/reconcile",
			strings.NewReader(body)))
		return rec
	}

	// Test 0: A drifted host produces a held proposal cloned from its check run.
	rec := post(`{"host":"web01"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reconcile status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if fake.gotPlaybook != "site.yml" || fake.gotInventory != "hosts.ini" {
		t.Errorf("submitted %q %q, want the check run's playbook and inventory",
			fake.gotPlaybook, fake.gotInventory)
	}
	got := fake.gotRun
	if got.Limit != "web01" {
		t.Errorf("limit = %q, want web01", got.Limit)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("status = %q, want pending_approval", got.Status)
	}
	if got.ProposedFrom != "chk_web" {
		t.Errorf("proposed_from = %q, want chk_web", got.ProposedFrom)
	}
	if got.DryRun {
		t.Error("proposal is a dry run, want a real run")
	}
	if got.ProjectID != "proj1" || got.InventoryID != "inv1" || got.Queue != "dmz" ||
		len(got.CredentialIDs) != 1 || got.CredentialIDs[0] != "cred1" {
		t.Errorf("proposal did not clone the check run's objects: %+v", got)
	}
	var resp run.Run
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.ID != "run_prop" {
		t.Errorf("response run = %q, want the submitted proposal", resp.ID)
	}

	// Test 1: An in-sync host is refused with 409.
	if rec := post(`{"host":"db01"}`); rec.Code != http.StatusConflict {
		t.Errorf("in-sync reconcile status = %d, want 409", rec.Code)
	}

	// Test 2: An unknown host is 404.
	if rec := post(`{"host":"ghost"}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown host status = %d, want 404", rec.Code)
	}

	// Test 3: A non-ansible check is refused with 400.
	if rec := post(`{"host":"job01"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("non-ansible reconcile status = %d, want 400", rec.Code)
	}

	// Test 4: An empty host is invalid.
	if rec := post(`{"host":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty host status = %d, want 400", rec.Code)
	}
}

// TestExplainProposal proves a held reconcile proposal is explainable: the prompt carries the
// proposal target and only the target host's drifted tasks, under the proposal system prompt.
func TestExplainProposal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	seedDrift(t, store, "chk_web", "", "web01", 2)
	if err := store.AppendEvents(ctx, "chk_web", []event.Event{
		{Type: event.TypeRunnerOK, Play: "site", Task: "template nginx.conf", Host: "web01", Changed: true, Message: "would rewrite"},
		{Type: event.TypeRunnerOK, Play: "site", Task: "install nginx", Host: "web02", Changed: true},
		{Type: event.TypeRunnerOK, Play: "site", Task: "gather facts", Host: "web01", Changed: false},
	}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if err := store.Save(ctx, &run.Run{
		ID: "run_prop", Playbook: "site.yml", Status: run.StatusPendingApproval,
		ProposedFrom: "chk_web", Limit: "web01",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	var gotSystem, gotUser string
	provider := ai.ProviderFunc(func(_ context.Context, system, user string) (string, error) {
		gotSystem, gotUser = system, user
		return "Approving resets the nginx config on web01.", nil
	})
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_prop/explain", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("explain proposal status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(gotSystem, "reviewing a proposed reconcile") {
		t.Errorf("system prompt = %q, want the proposal reviewer framing", gotSystem)
	}
	for _, want := range []string{"limited to host web01", "template nginx.conf", "would rewrite"} {
		if !strings.Contains(gotUser, want) {
			t.Errorf("prompt missing %q:\n%s", want, gotUser)
		}
	}
	if strings.Contains(gotUser, "install nginx") {
		t.Errorf("prompt carries another host's drift:\n%s", gotUser)
	}
}
