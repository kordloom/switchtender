package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// slowGateRunner executes nothing and holds the gate's module download until it is released, the
// way a slow registry holds it.
type slowGateRunner struct {
	// Runner executes the runs a test launches.
	roundhouse.Runner
	// release is closed to let every download finish.
	release chan struct{}
	// mu guards started.
	mu sync.Mutex
	// started counts the downloads begun.
	started int
}

// FetchModules waits for release, then reports a download that completed.
func (s *slowGateRunner) FetchModules(ctx context.Context, _ roundhouse.Spec,
	_ io.Writer) (roundhouse.Result, error) {
	s.mu.Lock()
	s.started++
	s.mu.Unlock()
	select {
	case <-s.release:
		return roundhouse.Result{ExitCode: 0}, nil
	case <-ctx.Done():
		return roundhouse.Result{ExitCode: -1}, ctx.Err()
	}
}

// downloads returns how many downloads began.
func (s *slowGateRunner) downloads() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.started
}

// slowHook is a server whose push trigger fires a Terraform plan the gate must download modules
// for, with the download held by a slowGateRunner.
type slowHook struct {
	// handler serves the API.
	handler http.Handler
	// srv is the server.
	srv *Server
	// runs holds the runs.
	runs run.Store
	// audits holds the chain.
	audits audit.Store
	// gate holds the downloads.
	gate *slowGateRunner
	// token is the trigger's webhook token.
	token string
	// secret signs deliveries.
	secret string
	// triggerID is the trigger's id.
	triggerID string
}

// newSlowHook builds the server, with rule as its only approval rule.
func newSlowHook(t *testing.T, rule *policy.Policy) *slowHook {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(
		"module \"net\" {\n  source = \"registry.example.test/acme/net/null\"\n}\n"), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{ID: "tpl_1", Name: "plan",
		Tool: run.ToolTerraform, Command: dir, DryRun: true}); err != nil {
		t.Fatalf("save template: %v", err)
	}
	policies := policy.NewMemStore()
	if err := policies.Save(ctx, rule); err != nil {
		t.Fatalf("save policy: %v", err)
	}
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	gate := &slowGateRunner{Runner: roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec,
		io.Writer) (roundhouse.Result, error) {
		return roundhouse.Result{ExitCode: 0}, nil
	}), release: make(chan struct{})}
	disp := dispatch.New(runs, gate, zap.NewNop(), dispatch.WithPolicies(policies),
		dispatch.WithRunFilesRoot(t.TempDir()), dispatch.WithNoJanitor())
	sealer := testSealer()
	triggers := trigger.NewMemStore()
	srv := New(runs, disp, zap.NewNop(), WithTriggers(triggers, sealer), WithTemplates(templates),
		WithAudit(audits), WithHookAnswerWithin(50*time.Millisecond))
	h := &slowHook{handler: srv.Handler(), srv: srv, runs: runs, audits: audits, gate: gate}
	h.token, h.secret = seedSignedTrigger(t, triggers, sealer, true)
	tg, err := triggers.FindByTokenHash(ctx, trigger.HashToken(h.token))
	if err != nil {
		t.Fatalf("find trigger: %v", err)
	}
	h.triggerID = tg.ID
	t.Cleanup(func() {
		h.releaseGate()
		srv.WaitForHooks(context.Background())
		disp.Close()
	})
	return h
}

// releaseGate lets every held download finish, once.
func (h *slowHook) releaseGate() {
	select {
	case <-h.gate.release:
	default:
		close(h.gate.release)
	}
}

// deliver sends the same signed push event and returns the answer's fields, failing the test when
// no answer comes. The gate's download stays blocked until the test releases it, so an answer that
// arrives at all arrived before the download finished. The deadline only bounds an answer that will
// never come, and is long enough that a busy machine never reaches it.
func (h *slowHook) deliver(t *testing.T) map[string]any {
	t.Helper()
	body := []byte(`{"ref":"refs/heads/main","after":"c0ffee"}`)
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		req := httptest.NewRequest(http.MethodPost, "/hooks/"+h.token, bytes.NewReader(body))
		req.Header.Set("X-Hub-Signature-256", trigger.SignBody(h.secret, body))
		rec := httptest.NewRecorder()
		h.handler.ServeHTTP(rec, req)
		answered <- rec
	}()
	select {
	case rec := <-answered:
		if rec.Code != http.StatusAccepted {
			t.Fatalf("webhook = %d %s, want 202", rec.Code, rec.Body.String())
		}
		var got map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode answer: %v", err)
		}
		return got
	case <-time.After(time.Minute):
		t.Fatal("the sender was not answered while the gate's download was still running")
		return nil
	}
}

// TestAPushTriggerAnswersBeforeTheGateFinishes covers a push trigger whose launch waits on the
// gate's module download. The sender is answered within the bound, told the delivery was accepted,
// and the launch carries on after the answer. A redelivery while it runs joins it rather than
// downloading or launching again, and one after it collapses onto the run it made.
func TestAPushTriggerAnswersBeforeTheGateFinishes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newSlowHook(t, &policy.Policy{ID: "pol_plans", Name: "prod infra", Tool: run.ToolTerraform,
		ExcludeDryRun: true, Effect: policy.EffectRequireApproval, MaxDestroy: policy.DisabledMaxDestroy})
	first := h.deliver(t)
	if first["accepted"] == nil || first["run"] != nil {
		t.Fatalf("answer = %v, want the delivery accepted before any run exists", first)
	}
	if again := h.deliver(t); again["accepted"] == nil {
		t.Errorf("redelivery while the launch runs = %v, want it accepted too", again)
	}
	if n := h.gate.downloads(); n != 1 {
		t.Errorf("the gate downloaded %d times for one delivery and its redelivery, want 1", n)
	}
	h.releaseGate()
	if !h.srv.WaitForHooks(ctx) {
		t.Fatal("the launch did not finish")
	}
	list, err := h.runs.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("runs = %d (%v), want the one the delivery launched", len(list), err)
	}
	if later := h.deliver(t); later["id"] != list[0].ID {
		t.Errorf("redelivery after the launch = %v, want the run it made, %s", later, list[0].ID)
	}
	if !chainHasPath(t, h.audits, "/hooks/"+h.triggerID+"/fired") {
		t.Error("the fire was not recorded before the sender was answered")
	}
}

// TestALaunchThatFailsAfterTheAnswerIsRecorded covers a push trigger's launch failing after its
// sender was told the delivery was accepted. The answer that would have carried the failure reached
// nobody, so the failure is recorded on the chain.
func TestALaunchThatFailsAfterTheAnswerIsRecorded(t *testing.T) {
	t.Parallel()
	h := newSlowHook(t, &policy.Policy{ID: "pol_deny", Name: "no plans", Tool: run.ToolTerraform,
		Effect: policy.EffectDeny, MaxDestroy: policy.DisabledMaxDestroy})
	if got := h.deliver(t); got["accepted"] == nil {
		t.Fatalf("answer = %v, want the delivery accepted", got)
	}
	h.releaseGate()
	if !h.srv.WaitForHooks(context.Background()) {
		t.Fatal("the launch did not finish")
	}
	if !chainHasPath(t, h.audits, "/hooks/"+h.triggerID+"/failed") {
		t.Error("a launch refused after the sender was answered left nothing on the chain")
	}
	if list, err := h.runs.List(context.Background()); err != nil || len(list) != 0 {
		t.Errorf("a refused launch left %d runs (%v)", len(list), err)
	}
	if !strings.Contains(strings.Join(chainPaths(t, h.audits), " "), "/fired") {
		t.Error("the fire itself was not recorded")
	}
}

// chainPaths returns the path of every entry on the chain.
func chainPaths(t *testing.T, audits audit.Store) []string {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var out []string
	for _, e := range chain {
		out = append(out, e.Path)
	}
	return out
}

// TestAReviewAnswersBeforeTheGateFinishes covers a pull request review whose pre-check waits on the
// gate's module download. The forge is answered within the bound with the delivery recorded, and
// the pull request still gets its status once the pre-check finishes after the answer.
func TestAReviewAnswersBeforeTheGateFinishes(t *testing.T) {
	t.Parallel()
	gate := &slowGateRunner{Runner: planRunner(&specLog{}), release: make(chan struct{})}
	rs := newReviewServer(t, reviewSetup{Runner: gate, AnswerWithin: 50 * time.Millisecond,
		PRFiles: map[string]string{"infra/plan.txt": "Plan: 1 to add, 0 to change, 0 to destroy.\n",
			"infra/main.tf": "module \"net\" {\n  source = \"registry.example.test/acme/net/null\"\n}\n"}})
	answered := make(chan *httptest.ResponseRecorder, 1)
	go func() { answered <- rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "") }()
	// The download stays blocked until the test releases it, so an answer that arrives at all
	// arrived before the pre-check finished, and the deadline only bounds one that never comes.
	var rec *httptest.ResponseRecorder
	select {
	case rec = <-answered:
	case <-time.After(time.Minute):
		close(gate.release)
		t.Fatal("the forge was not answered while the pre-check's download was still running")
	}
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "being prepared") {
		t.Errorf("webhook = %d %s, want the delivery accepted", rec.Code, rec.Body.String())
	}
	if !chainHasPath(t, rs.audits, "/hooks/"+rs.triggerID+"/review/7/accepted") {
		t.Error("the delivery was answered before it was recorded")
	}
	close(gate.release)
	rs.srv.WaitForHooks(context.Background())
	// The download installed nothing, so the pre-check could not read the module and the plan is
	// refused, which the pull request is told after the forge was answered.
	rs.waitStatus(t, rs.headSHA, "error")
}
