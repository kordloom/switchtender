package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestRunReceiptEndpointServesAVerifiableReceipt proves the product's headline evidence artifact is
// reachable over the API, not only from a shell on the server, and that what it serves actually
// verifies. Until this endpoint existed, the run an operator was looking at could be read, exported
// as a dossier, and streamed, but could not be turned into the one file an auditor checks without
// trusting the install.
func TestRunReceiptEndpointServesAVerifiableReceipt(t *testing.T) {
	// No t.Parallel: this test sets an environment variable, which the identity loader reads.
	ctx := context.Background()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	runs := run.NewMemStore()
	audits := audit.NewMemStore()

	at := time.Date(2026, 8, 17, 9, 0, 0, 0, time.FixedZone("CST", -6*60*60))
	creation := &audit.Entry{
		ID: audit.NewID(), At: at, Actor: "operator-jane", ActorType: "session",
		Method: http.MethodPost, Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	r := &run.Run{
		ID: "run_receipted", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "operator-jane", ActorType: "session", AuditReceipt: audit.Receipt(creation),
		CreatedAt: at, StartedAt: &at,
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	ended := at.Add(time.Minute)
	r.Status, r.EndedAt = run.StatusSucceeded, &ended
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	if err := outcome.Commit(ctx, audits, runs, r, "system:dispatcher"); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}

	handler := New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithAudit(audits), WithProducerIdentity(&id, "v-test")).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_receipted/receipt", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("receipt status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Switchtender-Key-Id"); got != id.KeyID() {
		t.Errorf("key fingerprint header = %q, want %q", got, id.KeyID())
	}
	if cd := rec.Header().Get("Content-Disposition"); !strings.Contains(cd, "run_receipted") {
		t.Errorf("Content-Disposition = %q, want it to name the run", cd)
	}

	// The bytes it served have to verify on their own, pinned to this install's key.
	report, err := audit.VerifyBundle(rec.Body.Bytes(), id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !report.OK() {
		t.Errorf("the served receipt does not verify: %+v", report)
	}
	if !report.OutcomePresent || !report.OutcomeDigestOK {
		t.Errorf("the served receipt does not disclose a verified outcome: %+v", report)
	}

	// A run with nothing to attest yet is a 409 that says so, not a server error.
	if err := runs.Save(ctx, &run.Run{ID: "run_live", Playbook: "site.yml", Status: run.StatusRunning}); err != nil {
		t.Fatalf("Save(live) error = %v", err)
	}
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_live/receipt", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("receipt of an unfinished run = %d, want 409", rec.Code)
	}

	// The sparse shape is reachable and verifies too.
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_receipted/receipt?sparse=1", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("sparse receipt status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
	}
	sparse, err := audit.VerifyBundle(rec.Body.Bytes(), id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle(sparse) error = %v", err)
	}
	if !sparse.OK() {
		t.Errorf("the served sparse receipt does not verify: %+v", sparse)
	}
}
