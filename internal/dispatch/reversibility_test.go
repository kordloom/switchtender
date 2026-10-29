package dispatch

import (
	"os"
	"path/filepath"
	"testing"

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
