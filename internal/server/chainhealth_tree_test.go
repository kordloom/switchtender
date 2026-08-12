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
)

// seedTreeAnchoredChain returns a store holding an intact chain and one tree anchor fixing the
// Merkle root over the whole of it, computed for the given install.
func seedTreeAnchoredChain(t *testing.T, installID string, n int) audit.Store {
	t.Helper()
	ctx := context.Background()
	audits := audit.NewMemStore()
	for i := 0; i < n; i++ {
		if err := audits.Append(ctx, &audit.Entry{
			ID: audit.NewID(), At: time.Date(2026, 8, 11, 12, i, 0, 0, time.UTC),
			Actor: "alice", Method: "POST", Path: "/v1/runs",
		}); err != nil {
			t.Fatalf("Append() error = %v", err)
		}
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	size, root, err := audit.TreeHead(chain, installID)
	if err != nil {
		t.Fatalf("TreeHead() error = %v", err)
	}
	if err := audits.(audit.AnchorStore).SaveAnchor(ctx, &audit.Anchor{
		ID: audit.NewAnchorID(), Type: audit.AnchorHTTPS, Shape: audit.AnchorShapeTree,
		Seq: size, Link: root, At: time.Now().UTC(), Ref: "https://anchors.example/head",
	}); err != nil {
		t.Fatalf("SaveAnchor() error = %v", err)
	}
	return audits
}

// TestChainHealthAcceptsATreeAnchor proves the metrics view does not page over a healthy chain. A
// tree anchor fixes a Merkle root at a tree size, not an entry hash at a sequence, so the health
// walk must recompute the tree to check it; holding the root against the linear hash map reported
// every tree-anchored install as tampered forever.
func TestChainHealthAcceptsATreeAnchor(t *testing.T) {
	t.Parallel()
	const installID = "inst_health"
	audits := seedTreeAnchoredChain(t, installID, 4)

	health := newChainHealth(audits, installID)
	g := health.snapshot(context.Background())
	if !g.Verified || g.Stale {
		t.Fatalf("snapshot verified=%v stale=%v, want a verified fresh reading", g.Verified, g.Stale)
	}
	if g.AnchorsTotal != 1 {
		t.Errorf("AnchorsTotal = %d, want 1", g.AnchorsTotal)
	}
	if g.AnchorProblems != 0 {
		t.Errorf("AnchorProblems = %d over an intact tree-anchored chain, want 0: the health view "+
			"reports a healthy chain as tampered", g.AnchorProblems)
	}
}

// TestAuditBundleHandlerExportsOverATreeAnchor proves the bundle download is not blocked by a tree
// anchor: the export must come back 200 and verify, rather than 409 claiming the chain cannot
// satisfy its anchors.
func TestAuditBundleHandlerExportsOverATreeAnchor(t *testing.T) {
	t.Parallel()
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	audits := seedTreeAnchoredChain(t, id.InstallID, 3)

	h := auditBundleHandler(audits, &id, "v-test", zap.NewNop())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/bundle", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	rep, err := audit.VerifyBundle(rec.Body.Bytes(), "")
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OK() {
		t.Errorf("the exported bundle does not verify: %+v", rep)
	}
}

// TestAuditVerifyHandlerAcceptsATreeAnchor proves GET /v1/audit/verify reports an intact
// tree-anchored chain as OK with no anchor problems.
func TestAuditVerifyHandlerAcceptsATreeAnchor(t *testing.T) {
	t.Parallel()
	const installID = "inst_verify"
	audits := seedTreeAnchoredChain(t, installID, 5)

	h := auditVerifyHandler(audits, installID, zap.NewNop())
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/audit/verify", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{`"ok":true`, `"anchored":1`} {
		if !strings.Contains(body, want) {
			t.Errorf("verify response missing %s:\n%s", want, body)
		}
	}
	if strings.Contains(body, "anchor_problems") {
		t.Errorf("verify response reports anchor problems over an intact chain:\n%s", body)
	}
}
