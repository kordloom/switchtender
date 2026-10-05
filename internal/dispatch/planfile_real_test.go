package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// planFileConfig is a configuration of one terraform_data resource per name, held in a local state
// file outside its working directory. terraform_data is built into both tools, so planning and
// applying it needs no provider download and no network.
const planFileConfig = `terraform {
  backend "local" {
    path = %q
  }
}

variable "names" {
  type = list(string)
}

resource "terraform_data" "r" {
  for_each = toset(var.names)
  input    = each.key
}
`

// realPlanDir writes planFileConfig into a working directory with its state beside it and returns
// both paths.
func realPlanDir(t *testing.T) (workdir, state string) {
	t.Helper()
	root := t.TempDir()
	workdir = filepath.Join(root, "infra")
	state = filepath.Join(root, "state.tfstate")
	if err := os.MkdirAll(workdir, 0o700); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(workdir, "main.tf"),
		[]byte(fmt.Sprintf(planFileConfig, state)), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return workdir, state
}

// stateInstances counts the resource instances a local state file holds.
func stateInstances(t *testing.T, state string) int {
	t.Helper()
	b, err := os.ReadFile(state)
	if err != nil {
		t.Fatalf("read state: %v", err)
	}
	var doc struct {
		// Resources are the state's resources.
		Resources []struct {
			// Instances are each resource's instances.
			Instances []json.RawMessage `json:"instances"`
		} `json:"resources"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("decode state: %v", err)
	}
	n := 0
	for _, r := range doc.Resources {
		n += len(r.Instances)
	}
	return n
}

// applyDirect applies the configuration with names straight through the runner, the way a change
// made outside the gate would.
func applyDirect(t *testing.T, runner roundhouse.Runner, tool, workdir string, names ...string) {
	t.Helper()
	res, err := runner.Run(context.Background(), roundhouse.Spec{Tool: tool, Command: workdir,
		ExtraVars: map[string]any{"names": names}}, io.Discard)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("direct apply of %v = %+v, %v", names, res, err)
	}
}

// TestAGatedApplyCarriesOutTheApprovedPlan drives the plan gate with the real tool, Terraform and
// OpenTofu each. A rule holds any apply destroying more than one resource, or, for a blanket rule,
// any apply at all, or no rule is in force and the apply's own submission asks for approval. The
// gate plans, saves the plan file, measures the destroys from the saved plan, and holds the apply
// carrying that plan. Once approved, the apply carries out exactly the saved plan. When the state
// moved after the plan was made, by a change applied around the gate, the tool itself refuses the
// stale plan, nothing the approver did not see is destroyed, and the run says the apply has to be
// proposed again.
func TestAGatedApplyCarriesOutTheApprovedPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Binary is the tool's executable, Tool its run tool.
		Binary, Tool string
		// Moved applies a change around the gate after the plan and before the approval.
		Moved bool
		// Blanket holds every apply rather than only one past a destroy limit.
		Blanket bool
		// Requested runs with no rules in force and asks for approval on the submission itself.
		Requested bool
		// WantStatus is how the approved apply ends.
		WantStatus run.Status
		// WantInstances is how many resources the state holds afterward.
		WantInstances int
	}{{ // Test 0: Terraform applies the approved plan.
		Name: "terraform approved", Binary: "terraform", Tool: run.ToolTerraform,
		WantStatus: run.StatusSucceeded, WantInstances: 1,
	}, { // Test 1: Terraform refuses the approved plan once the state moved.
		Name: "terraform stale", Binary: "terraform", Tool: run.ToolTerraform, Moved: true,
		WantStatus: run.StatusFailed, WantInstances: 4,
	}, { // Test 2: OpenTofu applies the approved plan.
		Name: "tofu approved", Binary: "tofu", Tool: run.ToolOpenTofu,
		WantStatus: run.StatusSucceeded, WantInstances: 1,
	}, { // Test 3: OpenTofu refuses the approved plan once the state moved.
		Name: "tofu stale", Binary: "tofu", Tool: run.ToolOpenTofu, Moved: true,
		WantStatus: run.StatusFailed, WantInstances: 4,
	}, { // Test 4: A blanket rule holds the planned apply, and Terraform applies its plan.
		Name: "terraform blanket approved", Binary: "terraform", Tool: run.ToolTerraform, Blanket: true,
		WantStatus: run.StatusSucceeded, WantInstances: 1,
	}, { // Test 5: A blanket rule's planned apply is refused by OpenTofu once the state moved.
		Name: "tofu blanket stale", Binary: "tofu", Tool: run.ToolOpenTofu, Blanket: true, Moved: true,
		WantStatus: run.StatusFailed, WantInstances: 4,
	}, { // Test 6: A request that asks for approval plans first, and Terraform applies its plan.
		Name: "terraform requested approved", Binary: "terraform", Tool: run.ToolTerraform,
		Requested: true, WantStatus: run.StatusSucceeded, WantInstances: 1,
	}, { // Test 7: A requested apply's plan is refused by OpenTofu once the state moved.
		Name: "tofu requested stale", Binary: "tofu", Tool: run.ToolOpenTofu, Requested: true,
		Moved: true, WantStatus: run.StatusFailed, WantInstances: 4,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if _, err := exec.LookPath(test.Binary); err != nil {
				standDownOrFail(t, "%s is not on PATH, so the real plan cannot run", test.Binary)
			}
			ctx := context.Background()
			runner := roundhouse.NewAnsibleRunner()
			workdir, state := realPlanDir(t)
			applyDirect(t, runner, test.Tool, workdir, "a", "b", "c")

			policies := policy.NewMemStore()
			rule := &policy.Policy{ID: policy.NewID(), Name: "destroys need a person", Tool: test.Tool,
				MaxDestroy: 1}
			if test.Blanket {
				rule.Name, rule.MaxDestroy = "applies need a person", policy.DisabledMaxDestroy
			}
			if err := policies.Save(ctx, rule); err != nil {
				t.Fatalf("Save(policy) error = %v", err)
			}
			store := run.NewMemStore()
			opts := []Option{WithAudits(audit.NewMemStore()),
				WithCredentials(credential.NewMemStore(), snapshotSealer), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor()}
			if test.Requested {
				rule.Name = holdRequested
			} else {
				opts = append(opts, WithPolicies(policies))
			}
			d := New(store, runner, nil, opts...)
			t.Cleanup(d.Close)

			plan, err := d.Submit(ctx, "", "", run.WithTool(test.Tool), run.WithCommand(workdir),
				run.WithExtraVars(map[string]any{"names": []string{"a"}}),
				run.WithRequireApproval(test.Requested))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if done := waitTerminal(t, store, plan.ID); done.Status != run.StatusSucceeded {
				t.Fatalf("plan status = %q (%s), want succeeded", done.Status, done.Error)
			}
			proposal := waitProposal(t, store, plan.ID)
			held, err := store.Get(ctx, proposal.ID)
			if err != nil {
				t.Fatalf("Get(proposal) error = %v", err)
			}
			if held.Status != run.StatusPendingApproval || held.PlanDestroys == nil ||
				*held.PlanDestroys != 2 || !strings.HasPrefix(held.HeldByPolicy, rule.Name) {
				t.Fatalf("proposal status %q destroys %v held by %q, want held by %q with the saved "+
					"plan's 2", held.Status, held.PlanDestroys, held.HeldByPolicy, rule.Name)
			}
			if held.PlanSHA256 == "" || held.PlanSealed == "" ||
				strings.HasPrefix(held.PlanSealed, plainPrefix) {
				t.Fatalf("the held apply carries no sealed plan file: digest %q", held.PlanSHA256)
			}
			if stateInstances(t, state) != 3 {
				t.Fatal("planning changed the state")
			}
			if test.Moved {
				applyDirect(t, runner, test.Tool, workdir, "a", "b", "c", "d")
			}
			if _, err := d.Approve(ctx, held.ID, decider("approver-1", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			final := waitTerminal(t, store, held.ID)
			if final.Status != test.WantStatus {
				log, _ := store.Log(ctx, held.ID)
				t.Fatalf("apply ended %q (%s), want %q:\n%s", final.Status, final.Error, test.WantStatus, log)
			}
			if got := stateInstances(t, state); got != test.WantInstances {
				t.Errorf("state holds %d resources, want %d", got, test.WantInstances)
			}
			if test.Moved {
				log, err := store.Log(ctx, held.ID)
				if err != nil {
					t.Fatalf("Log() error = %v", err)
				}
				if !strings.Contains(string(log), "Saved plan is stale") {
					t.Errorf("the apply did not refuse a stale plan, it ended:\n%s", log)
				}
				if final.Error != staleRefusal(final) {
					t.Errorf("error = %q, want it to say the apply has to be proposed again", final.Error)
				}
			}
			if final.PlanSealed != "" {
				t.Error("the ended apply still carries its sealed plan file")
			}
		})
	}
}

// TestAReconcileCarriesOutThePlanItsCheckSaved drives a drift reconcile with the real tool,
// Terraform and OpenTofu each. A drift check plans, saves its plan file, and keeps it sealed. The
// reconcile is built the way the reconcile request builds it, carrying that plan, and held. Once
// approved, it carries out exactly the plan the check made, and the next check finds the directory
// in sync. When the state moved after the check, the tool refuses the stale plan, nothing is
// applied, and the run says to run the check again.
func TestAReconcileCarriesOutThePlanItsCheckSaved(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Binary is the tool's executable, Tool its run tool.
		Binary, Tool string
		// Moved applies a change after the check and before the approval.
		Moved bool
		// WantStatus is how the approved reconcile ends.
		WantStatus run.Status
		// WantInstances is how many resources the state holds afterward.
		WantInstances int
	}{{ // Test 0: Terraform applies the plan the check saved.
		Name: "terraform approved", Binary: "terraform", Tool: run.ToolTerraform,
		WantStatus: run.StatusSucceeded, WantInstances: 1,
	}, { // Test 1: Terraform refuses the check's plan once the state moved.
		Name: "terraform stale", Binary: "terraform", Tool: run.ToolTerraform, Moved: true,
		WantStatus: run.StatusFailed, WantInstances: 4,
	}, { // Test 2: OpenTofu applies the plan the check saved.
		Name: "tofu approved", Binary: "tofu", Tool: run.ToolOpenTofu,
		WantStatus: run.StatusSucceeded, WantInstances: 1,
	}, { // Test 3: OpenTofu refuses the check's plan once the state moved.
		Name: "tofu stale", Binary: "tofu", Tool: run.ToolOpenTofu, Moved: true,
		WantStatus: run.StatusFailed, WantInstances: 4,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if _, err := exec.LookPath(test.Binary); err != nil {
				standDownOrFail(t, "%s is not on PATH, so the real plan cannot run", test.Binary)
			}
			ctx := context.Background()
			runner := roundhouse.NewAnsibleRunner()
			workdir, state := realPlanDir(t)
			applyDirect(t, runner, test.Tool, workdir, "a", "b", "c")

			store := run.NewMemStore()
			d := New(store, runner, nil, WithAudits(audit.NewMemStore()),
				WithCredentials(credential.NewMemStore(), snapshotSealer), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor())
			t.Cleanup(d.Close)

			check, err := d.Submit(ctx, "", "", run.WithTool(test.Tool), run.WithCommand(workdir),
				run.WithDryRun(true), run.WithExtraVars(map[string]any{"names": []string{"a"}}))
			if err != nil {
				t.Fatalf("Submit(check) error = %v", err)
			}
			if done := waitTerminal(t, store, check.ID); done.Status != run.StatusSucceeded {
				t.Fatalf("check status = %q (%s), want succeeded", done.Status, done.Error)
			}
			sealed, err := store.DriftPlan(ctx, check.ID)
			if err != nil || sealed == "" || strings.HasPrefix(sealed, plainPrefix) {
				t.Fatalf("the check kept no sealed plan: %v", err)
			}
			opts := append(check.ExecutionOptions(), run.WithDryRun(false), run.WithRequireApproval(true),
				run.WithProposedFrom(check.ID), run.WithSource("reconcile", check.ID),
				run.WithPlanFile(sealed))
			reconcile, err := d.Submit(ctx, "", "", opts...)
			if err != nil {
				t.Fatalf("Submit(reconcile) error = %v", err)
			}
			if reconcile.Status != run.StatusPendingApproval ||
				reconcile.PlanSHA256 != run.SealedBlobSHA256(sealed) {
				t.Fatalf("reconcile = %q binding %q, want held binding the check's plan",
					reconcile.Status, reconcile.PlanSHA256)
			}
			if stateInstances(t, state) != 3 {
				t.Fatal("the check changed the state")
			}
			if test.Moved {
				applyDirect(t, runner, test.Tool, workdir, "a", "b", "c", "d")
			}
			if _, err := d.Approve(ctx, reconcile.ID, decider("approver-1", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			final := waitTerminal(t, store, reconcile.ID)
			if final.Status != test.WantStatus {
				log, _ := store.Log(ctx, reconcile.ID)
				t.Fatalf("reconcile ended %q (%s), want %q:\n%s", final.Status, final.Error,
					test.WantStatus, log)
			}
			if got := stateInstances(t, state); got != test.WantInstances {
				t.Errorf("state holds %d resources, want %d", got, test.WantInstances)
			}
			if test.Moved && !strings.HasSuffix(final.Error,
				"Run the drift check again and propose the reconcile again.") {
				t.Errorf("error = %q, want it to say to run the check again", final.Error)
			}
			if final.PlanSealed != "" {
				t.Error("the ended reconcile still carries its sealed plan file")
			}
			if test.Moved {
				return
			}
			// The directory is back where the check's plan put it, so the next check finds nothing,
			// and the Drift page shows it in sync rather than the drift the first check saw.
			again, err := d.Submit(ctx, "", "", run.WithTool(test.Tool), run.WithCommand(workdir),
				run.WithDryRun(true), run.WithExtraVars(map[string]any{"names": []string{"a"}}))
			if err != nil {
				t.Fatalf("Submit(check again) error = %v", err)
			}
			if done := waitTerminal(t, store, again.ID); done.Status != run.StatusSucceeded {
				t.Fatalf("second check status = %q (%s), want succeeded", done.Status, done.Error)
			}
			rows, err := store.DriftStatus(ctx)
			if err != nil || len(rows) != 1 || rows[0].RunID != again.ID || rows[0].DriftedTasks != 0 {
				t.Errorf("Drift page after the reconcile = %+v, %v, want %s in sync", rows, err, workdir)
			}
		})
	}
}
