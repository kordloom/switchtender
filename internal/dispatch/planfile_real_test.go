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
// OpenTofu each. A rule holds any apply destroying more than one resource. The gate plans, saves the
// plan file, measures the destroys from the saved plan, and holds the apply carrying that plan. Once
// approved, the apply carries out exactly the saved plan. When the state moved after the plan was
// made, by a change applied around the gate, the tool itself refuses the stale plan and nothing the
// approver did not see is destroyed.
func TestAGatedApplyCarriesOutTheApprovedPlan(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Binary is the tool's executable, Tool its run tool.
		Binary, Tool string
		// Moved applies a change around the gate after the plan and before the approval.
		Moved bool
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
			if err := policies.Save(ctx, &policy.Policy{ID: policy.NewID(), Name: "destroys need a person",
				Tool: test.Tool, MaxDestroy: 1}); err != nil {
				t.Fatalf("Save(policy) error = %v", err)
			}
			store := run.NewMemStore()
			d := New(store, runner, nil, WithPolicies(policies), WithAudits(audit.NewMemStore()),
				WithCredentials(credential.NewMemStore(), snapshotSealer), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor())
			t.Cleanup(d.Close)

			plan, err := d.Submit(ctx, "", "", run.WithTool(test.Tool), run.WithCommand(workdir),
				run.WithExtraVars(map[string]any{"names": []string{"a"}}))
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
				*held.PlanDestroys != 2 {
				t.Fatalf("proposal status %q destroys %v, want held with the saved plan's 2",
					held.Status, held.PlanDestroys)
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
			}
			if final.PlanSealed != "" {
				t.Error("the ended apply still carries its sealed plan file")
			}
		})
	}
}
