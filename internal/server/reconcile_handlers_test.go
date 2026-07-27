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

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/run"
)

// seedDrift stores a check run and a host summary so DriftStatus reports the host.
func seedDrift(t *testing.T, store run.Store, runID, tool, host string, changed int, events ...event.Event) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	// The summary and events are written while the check is still running, as a real run does, then
	// it finalizes, since the store fences auxiliary writes to a terminal run.
	check := &run.Run{
		ID: runID, Playbook: "site.yml", Inventory: "hosts.ini", Tool: tool, Command: host, DryRun: true,
		Status: run.StatusRunning, CreatedAt: at,
		ProjectID: "proj1", InventoryID: "inv1", CredentialIDs: []string{"cred1"}, Queue: "dmz",
	}
	if err := store.Save(ctx, check); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sums := []run.HostSummary{{Host: host, Changed: changed, Worst: "changed", RanAt: at}}
	if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	if len(events) > 0 {
		if err := store.AppendEvents(ctx, runID, events); err != nil {
			t.Fatalf("AppendEvents() error = %v", err)
		}
	}
	done := check.Clone()
	done.Status = run.StatusSucceeded
	if err := store.Save(ctx, done); err != nil {
		t.Fatalf("Save() finalize error = %v", err)
	}
}

// TestReconcileDrift covers the proposal builder: an Ansible check clones held and limited to the
// host, a Terraform check applies its working directory, an in-sync target is refused, an unknown
// target is not found, a tool with no reconcile is refused, and an empty target is invalid.
func TestReconcileDrift(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	seedDrift(t, store, "chk_web", "", "web01", 3)
	seedDrift(t, store, "chk_db", "", "db01", 0)
	seedDrift(t, store, "chk_bash", "bash", "job01", 2)
	seedDrift(t, store, "chk_tf", run.ToolTerraform, "infra/network", 5)

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

	// Test 3: A tool with no reconcile, such as bash, is refused with 400.
	if rec := post(`{"host":"job01"}`); rec.Code != http.StatusBadRequest {
		t.Errorf("bash reconcile status = %d, want 400", rec.Code)
	}

	// Test 4: An empty host is invalid.
	if rec := post(`{"host":"  "}`); rec.Code != http.StatusBadRequest {
		t.Errorf("empty host status = %d, want 400", rec.Code)
	}

	// Test 5: A Terraform drift check applies its working directory, held for approval, with no host
	// limit, run for real rather than as a plan.
	rec = post(`{"host":"infra/network"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("terraform reconcile status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	tf := fake.gotRun
	if run.NormalizeTool(tf.Tool) != run.ToolTerraform || tf.Command != "infra/network" {
		t.Errorf("terraform proposal tool/command = %q/%q, want terraform applying infra/network",
			tf.Tool, tf.Command)
	}
	if tf.Limit != "" {
		t.Errorf("terraform proposal limit = %q, want none", tf.Limit)
	}
	if tf.DryRun {
		t.Error("terraform proposal is a dry run, want a real apply")
	}
	if tf.Status != run.StatusPendingApproval || tf.ProposedFrom != "chk_tf" {
		t.Errorf("terraform proposal = status %q from %q, want pending_approval from chk_tf",
			tf.Status, tf.ProposedFrom)
	}
}

// TestExplainProposal proves a held reconcile proposal is explainable: the prompt carries the
// proposal target and only the target host's drifted tasks, under the proposal system prompt.
func TestExplainProposal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	seedDrift(t, store, "chk_web", "", "web01", 2,
		event.Event{Type: event.TypeRunnerOK, Play: "site", Task: "template nginx.conf", Host: "web01", Changed: true, Message: "would rewrite"},
		event.Event{Type: event.TypeRunnerOK, Play: "site", Task: "install nginx", Host: "web02", Changed: true},
		event.Event{Type: event.TypeRunnerOK, Play: "site", Task: "gather facts", Host: "web01", Changed: false})
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
