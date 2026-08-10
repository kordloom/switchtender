package server

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/loomseal/jcs"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestAuditBundleHandlerDownloadsAVerifiableBundle proves the audit view's bundle download serves a
// genuinely signed LoomSeal bundle: it comes back as an attachment and its ed25519 signature checks
// out against the producer key over the canonical form. A handler that re-encoded the signed bytes
// would fail this, which is the whole reason it writes them directly.
func TestAuditBundleHandlerDownloadsAVerifiableBundle(t *testing.T) {
	t.Parallel()
	audits := audit.NewMemStore()
	for i := 0; i < 3; i++ {
		if err := audits.Append(context.Background(), &audit.Entry{
			ID: audit.NewID(), At: time.Date(2026, 8, 10, 12, i, 0, 0, time.UTC),
			Actor: "alice", Method: "POST", Path: "/v1/runs",
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	h := auditBundleHandler(audits, &id, "v-test", zap.NewNop())

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if cd := rec.Header().Get("Content-Disposition"); cd == "" || !strings.Contains(cd, "loomseal.json") {
		t.Errorf("Content-Disposition = %q, want an attachment named for the bundle", cd)
	}

	// Verify the signature exactly as an offline verifier would: pull the signature, clear the
	// signatures array, canonicalize, and check ed25519 over that.
	parsed, err := jcs.Parse(rec.Body.Bytes())
	if err != nil {
		t.Fatalf("bundle is not valid JSON: %v", err)
	}
	obj, ok := parsed.(map[string]any)
	if !ok {
		t.Fatal("bundle is not a JSON object")
	}
	sigs, ok := obj["signatures"].([]any)
	if !ok || len(sigs) == 0 {
		t.Fatal("bundle carries no signature")
	}
	sigB64, _ := sigs[0].(map[string]any)["sig"].(string)
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		t.Fatalf("signature is not base64: %v", err)
	}
	obj["signatures"] = []any{}
	canonical, err := jcs.Serialize(obj)
	if err != nil {
		t.Fatalf("Serialize() error = %v", err)
	}
	if !ed25519.Verify(id.Public(), canonical, sig) {
		t.Error("the downloaded bundle's signature does not verify against the producer key")
	}
}

// TestAuditBundleHandlerRefusals proves the download fails closed: no producer identity is a 404 and
// an empty chain is a 409, so a caller is never handed an empty or unsigned artifact.
func TestAuditBundleHandlerRefusals(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}

	// No producer identity: bundle signing is off.
	rec := httptest.NewRecorder()
	auditBundleHandler(audit.NewMemStore(), nil, "v", zap.NewNop()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("no-producer status = %d, want 404", rec.Code)
	}

	// Empty chain: nothing to bundle.
	rec = httptest.NewRecorder()
	auditBundleHandler(audit.NewMemStore(), &id, "v", zap.NewNop()).
		ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle", nil))
	if rec.Code != http.StatusConflict {
		t.Errorf("empty-chain status = %d, want 409", rec.Code)
	}
}
