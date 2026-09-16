package dispatch

import (
	"os"
	"path/filepath"
	"testing"

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

	// MaxDestroy must be the disabled value, as the file loader sets for any rule without a
	// plan-content threshold. Left at the zero value, Requiring skips the rule entirely.
	gate := &policy.Policy{Name: "hold the permanent", Effect: policy.EffectRequireApproval,
		Reversibility: run.Irreversible, MaxDestroy: policy.DisabledMaxDestroy}

	wipe := &run.Run{Tool: run.ToolAnsible, Playbook: destructive}
	if p := policy.Requiring([]*policy.Policy{gate}, graded(wipe)); p == nil {
		t.Error("a playbook that deletes an archive was not held by an irreversible rule, so the " +
			"gate is still blind to what an Ansible run actually does")
	}

	restart := &run.Run{Tool: run.ToolAnsible, Playbook: benign}
	if p := policy.Requiring([]*policy.Policy{gate}, graded(restart)); p != nil {
		t.Error("a service restart was held by an irreversible rule. A gate that fires on " +
			"recoverable work is one an operator switches off")
	}

	// The grade rides on a copy. The run about to be persisted and folded into the audit chain's
	// content digest must not gain a derived field, or the receipt commits to something computed.
	if wipe.Reversibility != nil {
		t.Error("grading mutated the run, so a computed field would reach storage and the receipt")
	}
}
