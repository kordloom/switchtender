package server

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

// seedImagedDrift stores a finished, containerized dry-run check that reports drift on host, pinning
// an execution image, a pull credential, and a timeout so a reconcile can be checked for carrying
// them over.
func seedImagedDrift(t *testing.T, store run.Store, runID, host, image, pullCred string, timeout int) {
	t.Helper()
	ctx := context.Background()
	at := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	check := &run.Run{
		ID: runID, Playbook: "site.yml", Inventory: "hosts.ini", DryRun: true,
		Status: run.StatusRunning, CreatedAt: at,
		ProjectID: "proj1", InventoryID: "inv1", CredentialIDs: []string{"cred1"}, Queue: "dmz",
		Image: image, PullCredentialID: pullCred, Timeout: timeout,
	}
	if err := store.Save(ctx, check); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sums := []run.HostSummary{{Host: host, Changed: 3, Worst: "changed", RanAt: at}}
	if err := store.SaveHostSummary(ctx, runID, sums); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	done := check.Clone()
	done.Status = run.StatusSucceeded
	if err := store.Save(ctx, done); err != nil {
		t.Fatalf("Save() finalize error = %v", err)
	}
}

// TestReconcileCarriesExecutionImage proves a reconcile runs inside the pinned image the check ran
// under, carrying the image, its pull credential, and the timeout, and applying for real rather than
// in check mode. Hand-building the submit options dropped all three, so a reconcile of a containerized
// check escaped its image onto the host under the default timeout.
func TestReconcileCarriesExecutionImage(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	seedImagedDrift(t, store, "chk_img", "web01", "registry.example.com/ee:1.2@sha256:abc", "cred_pull", 3600)

	fake := &fakeSubmitter{run: &run.Run{ID: "run_prop", Status: run.StatusPendingApproval}}
	handler := New(store, fake, zap.NewNop()).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/drift/reconcile",
		strings.NewReader(`{"host":"web01"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reconcile status = %d, want 202 (body %s)", rec.Code, rec.Body.String())
	}

	got := fake.gotRun
	if got == nil {
		t.Fatal("no run submitted")
	}
	if got.Image != "registry.example.com/ee:1.2@sha256:abc" {
		t.Errorf("image = %q, want the check run's pinned image", got.Image)
	}
	if got.PullCredentialID != "cred_pull" {
		t.Errorf("pull credential = %q, want cred_pull", got.PullCredentialID)
	}
	if got.Timeout != 3600 {
		t.Errorf("timeout = %d, want the check run's 3600", got.Timeout)
	}
	if got.DryRun {
		t.Error("reconcile is a dry run, want a real apply")
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("status = %q, want pending_approval", got.Status)
	}
}
