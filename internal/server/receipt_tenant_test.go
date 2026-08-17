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
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestAReceiptDoesNotCarryAnotherTenantsAuditTrail covers what a non-admin can walk away with.
//
// There are two shapes of receipt. The contiguous one carries the chain segment from the request that
// created the run through the entry recording what it did, which lets it disclose the outcome, and on a
// shared install that segment also holds whatever else was recorded in between: other organizations'
// actors, methods, and request paths, with the object ids embedded in them. The sparse one carries only
// this run's own entries, each proved to belong to the log, which is why it exists.
//
// The contiguous shape was the default for everyone. The audit trail itself is admin-only for exactly
// the reason that it is cross-tenant management data, so an operator who could not read a single line of
// /v1/audit could read a slice of it by asking for a receipt of their own run, signed by the install and
// free to pass on. A non-admin now receives the sparse shape whatever they ask for, and an admin, who
// may read the trail directly, still gets the contiguous one.
func TestAReceiptDoesNotCarryAnotherTenantsAuditTrail(t *testing.T) {
	// No t.Parallel: this test sets an environment variable, which the identity loader reads.
	ctx := context.Background()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()

	// The operator's own run is created first, so its creation entry opens the window.
	at := time.Date(2026, 8, 17, 9, 0, 0, 0, time.UTC)
	creation := &audit.Entry{
		ID: audit.NewID(), At: at, Actor: "jane", ActorType: "session",
		Method: http.MethodPost, Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}

	// Another tenant's work, recorded while the run was held and then executing. These are the entries
	// the contiguous window sweeps up, and each one names a person and an object.
	const otherActor = "rival-admin"
	const otherPath = "/v1/credentials/cred_rival_prod_root"
	for _, e := range []*audit.Entry{{
		ID: audit.NewID(), At: at.Add(time.Second), Actor: otherActor, ActorType: "session",
		Method: http.MethodPost, Path: otherPath,
	}, {
		ID: audit.NewID(), At: at.Add(2 * time.Second), Actor: otherActor, ActorType: "session",
		Method: http.MethodPost, Path: "/v1/orgs/org_rival/members",
	}} {
		if err := audits.Append(ctx, e); err != nil {
			t.Fatalf("Append(other tenant) error = %v", err)
		}
	}

	r := &run.Run{
		ID: "run_jane", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "jane", ActorType: "session", AuditReceipt: audit.Receipt(creation),
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

	// jane: a real, non-admin operator, and the actor of the run, which is what the route admits.
	jane, err := user.New("jane", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, jane); err != nil {
		t.Fatalf("Save(user) error = %v", err)
	}
	janePlain, janeToken, err := auth.New("jane-token")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	janeToken.UserID = jane.ID
	if err := tokens.Save(ctx, janeToken); err != nil {
		t.Fatalf("Save(token) error = %v", err)
	}
	// The run has to name the token's actor for denyUnlessAdminOrActor to admit it.
	r.Actor = janeToken.Name
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(actor) error = %v", err)
	}

	root, err := user.New("root", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New(admin) error = %v", err)
	}
	if err := users.Save(ctx, root); err != nil {
		t.Fatalf("Save(admin) error = %v", err)
	}
	rootPlain, rootToken, err := auth.New("root-token")
	if err != nil {
		t.Fatalf("auth.New(admin) error = %v", err)
	}
	rootToken.UserID = root.ID
	if err := tokens.Save(ctx, rootToken); err != nil {
		t.Fatalf("Save(admin token) error = %v", err)
	}

	handler := New(runs, &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithAudit(audits), WithProducerIdentity(&id, "v-test"),
		WithTokens(tokens), WithUsers(users)).Handler()

	fetch := func(t *testing.T, bearer, path string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("%s = %d, want 200 (body %s)", path, rec.Code, rec.Body.String())
		}
		return rec
	}

	// The operator's default receipt must verify and must not name the other tenant.
	rec := fetch(t, janePlain, "/v1/runs/run_jane/receipt")
	body := rec.Body.String()
	report, err := audit.VerifyBundle(rec.Body.Bytes(), id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !report.OK() {
		t.Errorf("the operator's receipt does not verify: %+v", report)
	}
	for _, leaked := range []string{otherActor, otherPath, "org_rival"} {
		if strings.Contains(body, leaked) {
			t.Errorf("a non-admin's receipt of their own run discloses another tenant's audit entry "+
				"(%q), which /v1/audit would refuse them", leaked)
		}
	}

	// Asking for the contiguous shape does not get it either, since the query is the caller's choice
	// and the disclosure is not.
	if body := fetch(t, janePlain, "/v1/runs/run_jane/receipt?sparse=").Body.String(); strings.Contains(body, otherActor) {
		t.Errorf("a non-admin asked for the contiguous shape and received another tenant's entries")
	}

	// The receipt still proves what it is for: this run's own entries, each shown to belong to the log.
	// The outcome body is not reproduced, because a tree leaf's hash covers its claim's whole payload
	// and adding one would make the leaf disagree with the root, so the sparse shape names the outcome
	// entry and the digest it committed instead.
	if report.ClaimCount < 2 {
		t.Errorf("claims = %d, want the run's creation and its outcome at least", report.ClaimCount)
	}
	if !strings.Contains(body, "/outcome/") {
		t.Error("the receipt does not carry the run's outcome entry, so it attests nothing about the " +
			"run ending")
	}

	// An admin, who may read the trail directly, still gets the contiguous shape.
	adminBody := fetch(t, rootPlain, "/v1/runs/run_jane/receipt").Body.String()
	if !strings.Contains(adminBody, otherActor) {
		t.Error("an admin's contiguous receipt no longer carries the chain segment around the run, " +
			"so the narrowing was applied to the wrong caller")
	}
	adminReport, err := audit.VerifyBundle([]byte(adminBody), id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle(admin) error = %v", err)
	}
	if !adminReport.OK() {
		t.Errorf("the admin's receipt does not verify: %+v", adminReport)
	}
}
