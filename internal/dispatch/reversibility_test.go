package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheGateSeesWhatThePlaybookDoes is the reason the reversibility grade exists.
//
// A rule written to hold anything that cannot be undone has to fire before the run executes, and at
// that moment there is no outcome to read. The only evidence is the playbook, whose text the run
// names but does not carry, so a grade computed from the run alone is blind to it: a playbook that
// drops every database looked exactly like one that restarts a service, and the gate let it past.
func TestTheGateSeesWhatThePlaybookDoes(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	destructive := filepath.Join(dir, "wipe.yml")
	if err := os.WriteFile(destructive, []byte(`
- hosts: all
  tasks:
    - ansible.builtin.file:
        path: /srv/archive
        state: absent
`), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	benign := filepath.Join(dir, "restart.yml")
	if err := os.WriteFile(benign, []byte(`
- hosts: all
  tasks:
    - ansible.builtin.service:
        name: nginx
        state: restarted
`), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}

	// The gate's floors read this grade. The comparison is the paid rule engine's, so this pins what
	// every build computes and hands to it: the run page, the dossier, and the Rego input read the
	// same grade.
	wipe := &run.Run{Tool: run.ToolAnsible, Playbook: destructive}
	if g := gradedLocally(wipe).Reversibility; g == nil || g.Class != run.Irreversible {
		t.Errorf("a playbook that deletes an archive graded %+v, want irreversible, so the gate is "+
			"still blind to what an Ansible run actually does", g)
	}

	restart := &run.Run{Tool: run.ToolAnsible, Playbook: benign}
	if g := gradedLocally(restart).Reversibility; g != nil && g.Class == run.Irreversible {
		t.Error("a service restart graded irreversible. A gate that fires on recoverable work is " +
			"one an operator switches off")
	}

	// The grade rides on a copy. The run about to be persisted and folded into the audit chain's
	// content digest must not gain a derived field, or the receipt commits to something computed.
	if wipe.Reversibility != nil {
		t.Error("grading mutated the run, so a computed field would reach storage and the receipt")
	}
}

// TestAnAgentsRunCannotTellAMissingPathFromAnUnreadableOne covers what the gate's record of a path
// on this server tells the one who asked. The record goes back to the submitter, and for an agent
// two reasons, one for a path that is not there and one for a path that is there but cannot be
// read, would make every run a probe of which paths exist on this server. An agent's playbook and
// working directory record one reason for both. A person's keep both, which say what to fix.
//
//nolint:funlen // Test function.
func TestAnAgentsRunCannotTellAMissingPathFromAnUnreadableOne(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("root reads a file whatever its mode, so nothing here is unreadable")
	}
	tests := []struct {
		// Tool is the tool of the dry run.
		Tool string
		// Missing names a path that is not there, rather than one that cannot be read.
		Missing bool
		// Agent has an agent ask for the run.
		Agent bool
		// WantReason is the reason the record gives.
		WantReason string
	}{{ // Test 0: An agent's playbook that is not there.
		Tool: run.ToolAnsible, Missing: true, Agent: true, WantReason: agentLocalUnread,
	}, { // Test 1: An agent's playbook that cannot be read gives the same reason.
		Tool: run.ToolAnsible, Agent: true, WantReason: agentLocalUnread,
	}, { // Test 2: An agent's working directory that is not there.
		Tool: run.ToolTerraform, Missing: true, Agent: true, WantReason: agentLocalUnread,
	}, { // Test 3: An agent's working directory that cannot be read gives the same reason.
		Tool: run.ToolTerraform, Agent: true, WantReason: agentLocalUnread,
	}, { // Test 4: A person's working directory that is not there says so.
		Tool: run.ToolTerraform, Missing: true, WantReason: "not on this server",
	}, { // Test 5: A person's that cannot be read says that, the control for tests 2 and 3.
		Tool: run.ToolTerraform, WantReason: "it could not be read or parsed",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			target := filepath.Join(dir, "absent")
			if !test.Missing {
				target = filepath.Join(dir, "locked")
				if test.Tool == run.ToolAnsible {
					target = filepath.Join(dir, "locked.yml")
					if err := os.WriteFile(target, []byte("- hosts: all\n"), 0o600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(target, 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(target, 0); err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = os.Chmod(target, 0o700) })
			}
			r := &run.Run{ID: "run_path", Tool: test.Tool, DryRun: true}
			if test.Tool == run.ToolAnsible {
				r.Playbook = target
			} else {
				r.Command = target
			}
			if test.Agent {
				r.ActorType = policy.ActorKindAgent
			}
			d := New(run.NewMemStore(), dispatchtest.OKRunner(), zap.NewNop(), WithNoJanitor())
			t.Cleanup(d.Close)
			gr := d.graded(r)
			if len(gr.DryRunScans) != 1 {
				t.Fatalf("scans = %+v, want the gate's one scan", gr.DryRunScans)
			}
			unread := strings.Join(gr.DryRunScans[0].Unread, "\n")
			if !strings.Contains(unread, "("+test.WantReason+")") {
				t.Errorf("unread = %q, want the path unread saying %q", unread, test.WantReason)
			}
			if test.Agent && (strings.Contains(unread, "not on this server") ||
				strings.Contains(unread, "could not be read or parsed")) {
				t.Errorf("unread = %q tells an agent which paths exist on this server", unread)
			}
		})
	}
}
