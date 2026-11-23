package dispatch

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/run"
)

// agentModulesConfig calls one registry module and one local module.
const agentModulesConfig = `module "vpc" {
  source  = "terraform-aws-modules/vpc/aws"
  version = "5.0.0"
}

module "net" {
  source = "./modules/net"
}
`

// agentModulesFiles is a project holding agentModulesConfig and its local module.
var agentModulesFiles = map[string]string{"main.tf": agentModulesConfig, "modules/net/main.tf": ""}

// localOnlyFiles is a project whose configuration calls only a local module.
var localOnlyFiles = map[string]string{
	"main.tf":             "module \"net\" {\n  source = \"./modules/net\"\n}\n",
	"modules/net/main.tf": "",
}

// agentModulesSubmit submits a Terraform or OpenTofu run of files as opts says and returns it.
func agentModulesSubmit(t *testing.T, tool string, files map[string]string, dryRun bool,
	opts ...run.SubmitOption) *run.Run {
	t.Helper()
	p := dispatchtest.LocalProject(t, files)
	store := run.NewMemStore()
	d := New(store, dispatchtest.PlanRunner("Plan: 0 to add, 0 to change, 0 to destroy"), zap.NewNop(),
		WithNoJanitor(), WithProjects(p.Store, p.Syncer))
	t.Cleanup(d.Close)
	p.Fetch(t)
	got, err := d.Submit(context.Background(), "", "", append([]run.SubmitOption{
		run.WithTool(tool), run.WithCommand("."), run.WithProject(p.ID), run.WithDryRun(dryRun)},
		opts...)...)
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	return got
}

// TestAgentModulesAreNamedOnTheHold drives an agent's apply that nothing has planned, from a
// configuration calling a registry module and a local one.
//
// The apply is held before it plans, and the run's own init fetches the registry module only once a
// person releases it. The hold note never said that module existed, so the approver read the
// configuration's local modules as everything that would run. Now the note names the registry
// module and says the run's init resolves it, and leaves the local module out, since the gate read
// it. An agent's dry run records the module as unread with the same point.
func TestAgentModulesAreNamedOnTheHold(t *testing.T) {
	t.Parallel()
	const sentence = "The run's own init resolves these when it starts."
	const vpc = `module.vpc from "terraform-aws-modules/vpc/aws"`
	for testNum, tool := range []string{run.ToolTerraform, run.ToolOpenTofu} {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := agentModulesSubmit(t, tool, agentModulesFiles, false, dispatchtest.AgentOpts()...)
			if got.Status != run.StatusPendingApproval {
				t.Fatalf("status = %s, want held", got.Status)
			}
			for _, want := range []string{"before anything plans", vpc, sentence} {
				if !strings.Contains(got.HoldNote, want) {
					t.Errorf("hold note %q does not say %q", got.HoldNote, want)
				}
			}
			if strings.Contains(got.HoldNote, "module.net") {
				t.Errorf("hold note %q names the local module the gate read", got.HoldNote)
			}
			if len(got.DryRunScans) != 0 {
				t.Errorf("an apply recorded dry run scans: %v", got.DryRunScans)
			}
		})
	}
}

// TestAgentModulesDryRunSaysInitResolvesThem confirms an agent's dry run lists the registry module
// as unread, saying the run's own init resolves it.
func TestAgentModulesDryRunSaysInitResolvesThem(t *testing.T) {
	t.Parallel()
	got := agentModulesSubmit(t, run.ToolTerraform, agentModulesFiles, true, dispatchtest.AgentOpts()...)
	var unread []string
	for _, scan := range got.DryRunScans {
		unread = append(unread, scan.Unread...)
	}
	want := `module.vpc from "terraform-aws-modules/vpc/aws" (not downloaded, since an agent asked ` +
		`for this run and nothing is fetched for it before a person approves, so the run's own ` +
		`init resolves it when the run starts)`
	if !strings.Contains(strings.Join(unread, "\n"), want) {
		t.Errorf("unread = %q, want an entry %q", unread, want)
	}
}

// TestAgentModulesControls pins what the change leaves alone: a configuration of local modules gets
// the note it got before, byte for byte, which is the note of a configuration with no modules, and
// a person's apply is not held and carries no such line.
func TestAgentModulesControls(t *testing.T) {
	t.Parallel()
	local := agentModulesSubmit(t, run.ToolTerraform, localOnlyFiles, false, dispatchtest.AgentOpts()...)
	plain := agentModulesSubmit(t, run.ToolTerraform,
		map[string]string{"main.tf": dispatchtest.CleanConfig}, false, dispatchtest.AgentOpts()...)
	if local.HoldNote == "" || local.HoldNote != plain.HoldNote {
		t.Errorf("a configuration of local modules got note %q, want the unchanged note %q",
			local.HoldNote, plain.HoldNote)
	}
	person := agentModulesSubmit(t, run.ToolTerraform, agentModulesFiles, false,
		dispatchtest.PersonOpts()...)
	if person.Status == run.StatusPendingApproval || strings.Contains(person.HoldNote, "module") {
		t.Errorf("a person's apply is %s with note %q, want it not held and no module line",
			person.Status, person.HoldNote)
	}
}
