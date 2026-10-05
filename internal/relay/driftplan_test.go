package relay_test

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestADriftCheckHandsItsPlanToTheControlNode pins the route a worker's drift check keeps its plan
// through. The control node seals the plan with its own key and keeps it on the check, and only for
// the run the worker holds the lease on, when that run is a Terraform or OpenTofu dry run still
// executing, so a worker cannot attach a plan to a run a reconcile was never meant to come from.
func TestADriftCheckHandsItsPlanToTheControlNode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Shape changes the seeded check.
		Shape func(*run.Run)
		// Lease is the capability presented in place of the check's own, when set.
		Lease string
		// Body is the request body.
		Body string
		// WantStatus is the answer.
		WantStatus int
		// WantKept is the sealed plan the check keeps afterward.
		WantKept string
	}{{ // Test 0: A running check keeps its plan, sealed by the control node.
		Name: "kept", Body: `{"plan_file":"cGxhbg=="}`, WantStatus: http.StatusNoContent,
		WantKept: "sealed:cGxhbg==",
	}, { // Test 1: A check that found no drift keeps nothing.
		Name: "no drift", Body: `{}`, WantStatus: http.StatusNoContent,
	}, { // Test 2: Another capability is refused.
		Name: "wrong lease", Lease: "not-the-capability", Body: `{"plan_file":"cGxhbg=="}`,
		WantStatus: http.StatusForbidden,
	}, { // Test 3: An apply is not a drift check.
		Name: "apply", Shape: func(r *run.Run) { r.DryRun = false }, Body: `{"plan_file":"cGxhbg=="}`,
		WantStatus: http.StatusConflict,
	}, { // Test 4: A finished check keeps nothing now.
		Name: "finished", Shape: func(r *run.Run) { r.Status = run.StatusSucceeded },
		Body: `{"plan_file":"cGxhbg=="}`, WantStatus: http.StatusConflict,
	}, { // Test 5: An Ansible check saves no plan file.
		Name: "ansible", Shape: func(r *run.Run) { r.Tool = run.ToolAnsible },
		Body: `{"plan_file":"cGxhbg=="}`, WantStatus: http.StatusConflict,
	}, { // Test 6: A body that does not decode.
		Name: "bad body", Body: `{"plan_file":`, WantStatus: http.StatusBadRequest,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fixture := newRelayFixture(t, nil, nil)
			lease := seedPlan(t, fixture.Store, "run_check", func(r *run.Run) {
				r.DryRun = true
				if test.Shape != nil {
					test.Shape(r)
				}
			})
			if test.Lease != "" {
				lease = test.Lease
			}
			status, body := leasedPost(t, fixture.URL, "/relay/v1/runs/run_check/drift-plan", lease,
				test.Body)
			if status != test.WantStatus {
				t.Fatalf("status = %d (%s), want %d", status, body, test.WantStatus)
			}
			kept, err := fixture.Store.DriftPlan(ctx, "run_check")
			if err != nil {
				t.Fatalf("DriftPlan() error = %v", err)
			}
			if kept != test.WantKept {
				t.Errorf("kept plan = %q, want %q", kept, test.WantKept)
			}
		})
	}
}

// TestAWorkerRefusesAPlanPastTheRelayLimit pins that a drift check's plan file larger than the
// relay carries is refused on the worker, with the limit stated, rather than sent for the control
// node to cut off partway through the upload.
func TestAWorkerRefusesAPlanPastTheRelayLimit(t *testing.T) {
	t.Parallel()
	transport := relay.NewHTTPTransport("http://127.0.0.1:1", testWorkerToken, nil)
	err := transport.KeepDriftPlanFile(context.Background(), "run_check",
		make([]byte, relay.MaxPlanFileBytes+1))
	if !errors.Is(err, relay.ErrPlanFileTooLarge) || !strings.Contains(err.Error(), "19 MiB") {
		t.Errorf("KeepDriftPlanFile() error = %v, want ErrPlanFileTooLarge stating the limit", err)
	}
}

// TestAWorkerDriftCheckKeepsItsPlanOverTheRelay drives a Terraform drift check on a worker across
// the relay. The worker has no key to seal the plan with, so it hands the plan file to the control
// node, which seals it and keeps it on the check for the reconcile to carry.
func TestAWorkerDriftCheckKeepsItsPlanOverTheRelay(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	backing := run.NewMemStore()
	control := dispatch.New(backing, roundhouse.RunnerFunc(nil), zaptest.NewLogger(t),
		dispatch.WithNoJanitor(),
		dispatch.WithClaimGate(func() error { return errors.New("closed for this test") }))
	t.Cleanup(control.Close)
	ts := httptest.NewServer(relay.NewHandler(backing, relay.SinglePool(testWorkerToken), nil, nil,
		nil, relay.WithPlanSealer(control)))
	t.Cleanup(ts.Close)
	transport := relay.NewHTTPTransport(ts.URL, testWorkerToken, nil)
	planner := roundhouse.RunnerFunc(
		func(_ context.Context, spec roundhouse.Spec, out io.Writer) (roundhouse.Result, error) {
			const summary = "Plan: 0 to add, 1 to change, 0 to destroy."
			_, err := io.WriteString(out, summary+"\n")
			if !spec.DryRun || spec.PlanOut == "" {
				return roundhouse.Result{ExitCode: 1}, errors.New("the check was not a plan saving its file")
			}
			res := plantest.Result(summary)
			res.PlanFile = []byte("plan the worker saved")
			return res, err
		})
	worker := dispatch.New(relay.NewClient(transport), planner, zaptest.NewLogger(t),
		dispatch.WithWorkers(1), dispatch.WithNoJanitor(), dispatch.WithOwner("worker-a"),
		dispatch.WithClaimInterval(10*time.Millisecond), dispatch.WithRunFilesRoot(t.TempDir()))
	t.Cleanup(worker.Close)
	seed := &run.Run{ID: "run_worker_check", Tool: run.ToolTerraform, Command: "infra/prod",
		DryRun: true, Status: run.StatusPending, CreatedAt: time.Now()}
	if err := backing.Save(ctx, seed); err != nil {
		t.Fatalf("seed Save() error = %v", err)
	}
	check := waitTerminal(t, backing, "run_worker_check")
	if check.Status != run.StatusSucceeded || check.Warning != "" {
		t.Fatalf("check = %q (%s, warning %q), want succeeded with its plan kept", check.Status,
			check.Error, check.Warning)
	}
	kept, err := backing.DriftPlan(ctx, check.ID)
	if err != nil {
		t.Fatalf("DriftPlan() error = %v", err)
	}
	encoded, ok := strings.CutPrefix(kept, "plain:")
	if !ok {
		t.Fatalf("kept plan = %q, want the control node's sealed form", kept)
	}
	plan, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || string(plan) != "plan the worker saved" {
		t.Errorf("the kept plan is %q, %v, want the plan the worker's check saved", plan, err)
	}
}
