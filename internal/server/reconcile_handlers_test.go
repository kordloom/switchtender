package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/paid"
	"github.com/kordloom/switchtender/internal/run"
)

// testReconciler stands in for the paid reconciler. It knows none of the reconcile rules: it
// records the request and returns Spec, or reruns the check as it ran when Spec is nil, so the
// handler's own behavior can be tested on a build that has no reconciler.
type testReconciler struct {
	// mu guards got and calls.
	mu sync.Mutex
	// got is the last request Propose saw.
	got paid.ReconcileRequest
	// calls counts Propose calls.
	calls int
	// Spec is what Propose returns, the check's own execution options when nil.
	Spec []run.SubmitOption
	// Err is returned from Propose when set.
	Err error
}

// Propose records req and returns the configured spec.
func (r *testReconciler) Propose(_ context.Context, req paid.ReconcileRequest) ([]run.SubmitOption, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got, r.calls = req, r.calls+1
	if r.Err != nil {
		return nil, r.Err
	}
	if r.Spec != nil {
		return r.Spec, nil
	}
	return req.Check.ExecutionOptions(), nil
}

// last returns the last request and the number of calls.
func (r *testReconciler) last() (paid.ReconcileRequest, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.got, r.calls
}

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
	// A Terraform or OpenTofu check that found drift keeps the plan it saved, as a real check does.
	kind := run.NormalizeTool(tool)
	if (kind == run.ToolTerraform || kind == run.ToolOpenTofu) && changed > 0 {
		if err := store.KeepDriftPlan(ctx, runID, "sealed-plan-"+runID); err != nil {
			t.Fatalf("KeepDriftPlan() error = %v", err)
		}
	}
	done := check.Clone()
	done.Status = run.StatusSucceeded
	if err := store.Save(ctx, done); err != nil {
		t.Fatalf("Save() finalize error = %v", err)
	}
}

// TestReconcileDrift covers what the public handler owns around the paid proposal builder: a drifted
// host reaches the reconciler with its check, host, and no plan for Ansible, the proposal is held,
// attributed, and sourced whatever the spec says, an in-sync target is refused, an unknown target is
// not found, a tool with no reconcile is refused, an empty target is invalid, and a Terraform check
// reaches the reconciler with the plan it kept.
func TestReconcileDrift(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	seedDrift(t, store, "chk_web", "", "web01", 3)
	seedDrift(t, store, "chk_db", "", "db01", 0)
	seedDrift(t, store, "chk_bash", "bash", "job01", 2)
	seedDrift(t, store, "chk_tf", run.ToolTerraform, "infra/network", 5)

	fake := &fakeSubmitter{run: &run.Run{ID: "run_prop", Status: run.StatusPendingApproval}}
	// The spec tries to release its own proposal and to claim another source. The handler applies
	// the hold and the attribution after it, so neither takes.
	rec0 := &testReconciler{Spec: []run.SubmitOption{
		run.WithLimit("from-spec"), run.WithRequireApproval(false),
		run.WithSource("spec", "elsewhere"), run.WithProposedFrom("elsewhere"),
	}}
	handler := New(store, fake, zap.NewNop(), WithReconciler(rec0)).Handler()
	post := func(body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/drift/reconcile",
			strings.NewReader(body)))
		return rec
	}

	// Test 0: A drifted host reaches the reconciler and its spec is submitted held.
	rec := post(`{"host":"web01"}`)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("reconcile status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	req, calls := rec0.last()
	if calls != 1 || req.Check == nil || req.Check.ID != "chk_web" || req.Host != "web01" ||
		req.PlanFile != "" {
		t.Errorf("reconciler saw %+v after %d calls, want chk_web, web01, no plan, once", req, calls)
	}
	if fake.gotPlaybook != "site.yml" || fake.gotInventory != "hosts.ini" {
		t.Errorf("submitted %q %q, want the check run's playbook and inventory",
			fake.gotPlaybook, fake.gotInventory)
	}
	got := fake.gotRun
	if got.Limit != "from-spec" {
		t.Errorf("limit = %q, want the reconciler's spec applied", got.Limit)
	}
	if got.Status != run.StatusPendingApproval {
		t.Errorf("status = %q, want pending_approval whatever the spec says", got.Status)
	}
	if got.ProposedFrom != "chk_web" || got.Source != "reconcile" {
		t.Errorf("proposal from %q with source %q, want chk_web and reconcile", got.ProposedFrom,
			got.Source)
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
	if _, calls := rec0.last(); calls != 1 {
		t.Errorf("reconciler called %d times, want only the one valid request", calls)
	}

	// Test 5: A Terraform check reaches the reconciler with the plan it kept.
	if rec := post(`{"host":"infra/network"}`); rec.Code != http.StatusAccepted {
		t.Fatalf("terraform reconcile status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if req, _ := rec0.last(); req.Check.ID != "chk_tf" || req.PlanFile != "sealed-plan-chk_tf" {
		t.Errorf("terraform reconciler request = %s with plan %q, want chk_tf with its kept plan",
			req.Check.ID, req.PlanFile)
	}
	if tf := fake.gotRun; tf.Status != run.StatusPendingApproval || tf.ProposedFrom != "chk_tf" {
		t.Errorf("terraform proposal = status %q from %q, want pending_approval from chk_tf",
			tf.Status, tf.ProposedFrom)
	}
}

// TestAReconcileWithNoKeptPlanIsRefused pins the Terraform and OpenTofu side the handler owns. A
// check that kept no plan, here because a later check dropped it, has nothing to carry, so the
// request is refused with what to do next before the reconciler is asked, rather than proposing an
// apply that would plan again.
func TestAReconcileWithNoKeptPlanIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	seedDrift(t, store, "chk_old", run.ToolOpenTofu, "infra/storage", 2)
	newer := &run.Run{ID: "chk_newer", Tool: run.ToolOpenTofu, Command: "infra/storage",
		ProjectID: "proj1", DryRun: true, Status: run.StatusRunning, CreatedAt: time.Now()}
	if err := store.Save(ctx, newer); err != nil {
		t.Fatalf("Save(newer) error = %v", err)
	}
	if err := store.KeepDriftPlan(ctx, newer.ID, ""); err != nil {
		t.Fatalf("KeepDriftPlan(newer) error = %v", err)
	}

	fake := &fakeSubmitter{run: &run.Run{ID: "run_prop", Status: run.StatusPendingApproval}}
	reconciler := &testReconciler{}
	handler := New(store, fake, zap.NewNop(), WithReconciler(reconciler)).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/drift/reconcile",
		strings.NewReader(`{"host":"infra/storage"}`)))
	if rec.Code != http.StatusConflict ||
		!strings.Contains(rec.Body.String(), "Run the drift check again") {
		t.Errorf("a check with no kept plan answered %d (%s), want 409 saying to run it again",
			rec.Code, rec.Body.String())
	}
	if _, calls := reconciler.last(); calls != 0 || fake.gotRun != nil {
		t.Errorf("a check with no kept plan reached the reconciler %d times and submitted %v",
			calls, fake.gotRun)
	}
}

// TestACommunityReconcileRefusesNamingTeam covers a build with no reconciler on an install that
// holds a Team license, which is the one case the license gate alone lets through: the stub
// refuses with 403 in the Community line, and nothing is submitted. A reconciler failing for any
// other reason is a server error, not a refusal.
func TestACommunityReconcileRefusesNamingTeam(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	seedDrift(t, store, "chk_web", "", "web01", 3)

	tests := []struct {
		Reconciler paid.Reconciler
		WantStatus int
		WantText   string
	}{{ // Test 0: The Community stub, as New installs it on a build with no paid module.
		Reconciler: paid.CurrentReconciler(), WantStatus: http.StatusForbidden,
		WantText: license.Unlicensed(license.FeatureReconcile).Error(),
	}, { // Test 1: A reconciler that fails is a 500 with a generic line.
		Reconciler: &testReconciler{Err: errors.New("boom")},
		WantStatus: http.StatusInternalServerError, WantText: "could not build the reconcile proposal",
	}}
	for testNum, test := range tests {
		fake := &fakeSubmitter{run: &run.Run{ID: "run_prop", Status: run.StatusPendingApproval}}
		handler := New(store, fake, zap.NewNop(), WithReconciler(test.Reconciler)).Handler()
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/drift/reconcile",
			strings.NewReader(`{"host":"web01"}`)))
		if rec.Code != test.WantStatus || !strings.Contains(rec.Body.String(), test.WantText) {
			t.Errorf("test %d: reconcile = %d %s, want %d containing %q", testNum, rec.Code,
				rec.Body.String(), test.WantStatus, test.WantText)
		}
		if fake.gotRun != nil {
			t.Errorf("test %d: a refused reconcile submitted %+v", testNum, fake.gotRun)
		}
	}
}

// TestNewUsesTheRegisteredReconciler pins the default: a server built without WithReconciler asks
// the seam, which on a build with no paid module is the Community stub.
func TestNewUsesTheRegisteredReconciler(t *testing.T) {
	t.Parallel()
	srv := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop())
	if srv.reconciler != paid.CurrentReconciler() {
		t.Errorf("reconciler = %T, want the registered one, %T", srv.reconciler,
			paid.CurrentReconciler())
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
