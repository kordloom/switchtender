package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/run"
)

// TestExplainAHeldTerraformApply pins what an approver's Explain is built from when the held run is
// a terraform or opentofu apply. Both the plan gate and a terraform drift reconcile hold an apply
// proposed from another run, and both reached the reconcile prompt written for Ansible: it
// described the apply as running an empty playbook against an empty host and never showed the plan,
// so the one run whose plan says exactly what it will destroy was explained without it.
func TestExplainAHeldTerraformApply(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says where the held apply came from.
		Name string
		// Tool is the apply's tool.
		Tool string
		// SourceDryRun reports whether the run it was proposed from was a dry-run check.
		SourceDryRun bool
		// Plan is the output of the run it was proposed from.
		Plan string
		// WantLine is the line of the plan an approver must see.
		WantLine string
		// HeldBy is the reason the apply is held.
		HeldBy string
	}{{ // Test 0: The plan gate held an apply whose plan destroys past the limit.
		Name: "plan gate", Tool: run.ToolTerraform,
		Plan: "  # null_resource.legacy_subnet[0] will be destroyed\n" +
			"Plan: 0 to add, 0 to change, 3 to destroy.\n",
		WantLine: "Plan: 0 to add, 0 to change, 3 to destroy.",
		HeldBy:   "terraform destroys need a second approver (plan destroys 3, limit 0)",
	}, { // Test 1: A drift check found a change and proposed the apply that makes it.
		Name: "drift reconcile", Tool: run.ToolTerraform, SourceDryRun: true,
		Plan: "  # aws_security_group.web will be updated in-place\n" +
			"Plan: 0 to add, 1 to change, 0 to destroy.\n",
		WantLine: "aws_security_group.web will be updated in-place",
		HeldBy:   "requested at submission",
	}, { // Test 2: The same for OpenTofu.
		Name: "opentofu", Tool: run.ToolOpenTofu,
		Plan:     "Plan: 2 to add, 0 to change, 1 to destroy.\n",
		WantLine: "Plan: 2 to add, 0 to change, 1 to destroy.",
		HeldBy:   "tofu destroys need a person (plan destroys 1, limit 0)",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			source := &run.Run{
				ID: "run_plan", Tool: test.Tool, Command: "infra/legacy-network",
				DryRun: test.SourceDryRun, Status: run.StatusRunning, CreatedAt: time.Now(),
			}
			if err := store.Save(ctx, source); err != nil {
				t.Fatalf("Save(source) error = %v", err)
			}
			// Logs are written while the run executes; the store refuses them once it is terminal.
			if err := store.AppendLog(ctx, source.ID, []byte(test.Plan)); err != nil {
				t.Fatalf("AppendLog() error = %v", err)
			}
			done := source.Clone()
			done.Status = run.StatusSucceeded
			if err := store.Save(ctx, done); err != nil {
				t.Fatalf("Save(source done) error = %v", err)
			}
			if err := store.Save(ctx, &run.Run{
				ID: "run_apply", Tool: test.Tool, Command: "infra/legacy-network",
				Status: run.StatusPendingApproval, ProposedFrom: source.ID, HeldByPolicy: test.HeldBy,
				CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("Save(apply) error = %v", err)
			}

			var gotSystem, gotUser string
			provider := ai.ProviderFunc(func(_ context.Context, system, user string) (string, error) {
				gotSystem, gotUser = system, user
				return "Approving destroys the legacy subnets.", nil
			})
			handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithAI(provider)).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_apply/explain",
				nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("explain status = %d, want 200: %s", rec.Code, rec.Body.String())
			}
			if !strings.Contains(gotSystem, "Terraform or OpenTofu apply") {
				t.Errorf("system prompt = %q, want the apply reviewer framing", gotSystem)
			}
			for _, want := range []string{test.WantLine, "infra/legacy-network", test.HeldBy} {
				if !strings.Contains(gotUser, want) {
					t.Errorf("prompt missing %q:\n%s", want, gotUser)
				}
			}
			if strings.Contains(gotUser, "playbook") {
				t.Errorf("prompt describes a terraform apply as a playbook:\n%s", gotUser)
			}
		})
	}
}
