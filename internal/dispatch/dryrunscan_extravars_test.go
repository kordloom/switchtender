package dispatch

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDryRunExtraVarsThatNameAConnectionVariableAreHeld drives an operator's Ansible dry run of a
// clean playbook through the gate under a rule that excludes dry runs.
//
// Extra vars outrank every other variable, and a connection or interpreter variable names a program
// Ansible starts on the controller, under --check as much as without it. So a dry run whose playbook
// was clean ran a command for real while the rule waved it through as change free. Template code in
// an extra var does the same through a lookup. The controls are the variables a dry run sets every
// day, which say who and where and run nothing.
func TestDryRunExtraVarsThatNameAConnectionVariableAreHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Vars        map[string]any
		WantHeld    bool
		WantFinding string
	}{{ // Test 0: The audit's case, a proxy command on every SSH connection.
		Vars:     map[string]any{"ansible_ssh_common_args": "-o ProxyCommand=true"},
		WantHeld: true, WantFinding: `the extra var "ansible_ssh_common_args" sets an ansible_ variable`,
	}, { // Test 1: A connection plugin.
		Vars:     map[string]any{"ansible_connection": "local"},
		WantHeld: true, WantFinding: `the extra var "ansible_connection"`,
	}, { // Test 2: An interpreter is a program the controller starts on each host, or itself.
		Vars:     map[string]any{"ansible_python_interpreter": "/tmp/x"},
		WantHeld: true, WantFinding: `the extra var "ansible_python_interpreter"`,
	}, { // Test 3: Template code nested in an ordinary variable.
		Vars: map[string]any{"app": map[string]any{"tags": []any{"web",
			"{{ lookup('pipe', 'id') }}"}}},
		WantHeld: true, WantFinding: `the extra var "app" carries template code`,
	}, { // Test 4: A name in another case is still the same family.
		Vars:     map[string]any{"ANSIBLE_SHELL_EXECUTABLE": "/tmp/sh"},
		WantHeld: true, WantFinding: `the extra var "ANSIBLE_SHELL_EXECUTABLE"`,
	}, { // Test 5: Control, an ordinary variable.
		Vars: map[string]any{"greeting": "hello"}, WantHeld: false,
	}, { // Test 6: Control, who and where.
		Vars: map[string]any{"ansible_user": "deploy", "ansible_host": "10.0.0.5"}, WantHeld: false,
	}, { // Test 7: Control, the rest of the allowed list.
		Vars: map[string]any{"ansible_port": 2222, "ansible_become": true,
			"ansible_become_user": "root", "ansible_password": "pw", "ansible_ssh_pass": "pw",
			"ansible_become_password": "pw"},
		WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := dispatchtest.LocalProject(t, map[string]string{"site.yml": cleanPlaybook})
			store := run.NewMemStore()
			d := New(store, dispatchtest.OKRunner(), zap.NewNop(), WithPolicies(excludeDryRuns(t)),
				WithNoJanitor(), WithProjects(p.Store, p.Syncer))
			t.Cleanup(d.Close)

			got, err := d.Submit(ctx, "site.yml", "hosts.ini", run.WithTool(run.ToolAnsible),
				run.WithDryRun(true), run.WithProject(p.ID), run.WithExtraVars(test.Vars))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held = %v, want %v. Recorded: %q", held, test.WantHeld, got.DryRunFindings())
			}
			stored, err := store.Get(ctx, got.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			risk := run.AssessRisk(stored)
			if !test.WantHeld {
				if len(stored.DryRunFindings()) != 0 {
					t.Errorf("a dry run with ordinary extra vars recorded findings: %q",
						stored.DryRunFindings())
				}
				return
			}
			if !strings.Contains(strings.Join(stored.DryRunFindings(), "\n"), test.WantFinding) {
				t.Errorf("recorded %q, want an entry naming %q", stored.DryRunFindings(), test.WantFinding)
			}
			if slices.Contains(risk.Reasons, "dry run, makes no changes") {
				t.Errorf("the risk grade still reads the dry run as making no changes: %v", risk.Reasons)
			}
		})
	}
}
