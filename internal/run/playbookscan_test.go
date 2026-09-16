package run

import (
	"fmt"
	"strings"
	"testing"
)

// TestAPlaybookIsGradedByWhatItDoes covers the gap that made reversibility blind to Ansible.
//
// The grade reads a command, and an Ansible run has none: the work is inside a file. A playbook
// that drops every database used to grade exactly like one that restarts a service, which is the
// flagship tool grading blindest.
func TestAPlaybookIsGradedByWhatItDoes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Playbook  string
		WantClass string
	}{{
		Name: "a file removed is gone",
		Playbook: `
- hosts: all
  tasks:
    - name: Drop the archive
      ansible.builtin.file:
        path: /srv/archive
        state: absent
`,
		WantClass: Irreversible,
	}, {
		Name: "a database dropped is gone",
		Playbook: `
- hosts: all
  tasks:
    - community.mysql.mysql_db:
        name: prod
        state: absent
`,
		WantClass: Irreversible,
	}, {
		Name: "a package removed is a reinstall, not a loss",
		Playbook: `
- hosts: all
  tasks:
    - ansible.builtin.package:
        name: nginx
        state: absent
`,
		WantClass: ReversibleCostly,
	}, {
		Name: "a service restarted comes back",
		Playbook: `
- hosts: all
  tasks:
    - ansible.builtin.service:
        name: nginx
        state: restarted
`,
		WantClass: ReversibleCostly,
	}, {
		Name: "a destructive shell task inside a block is still found",
		Playbook: `
- hosts: all
  tasks:
    - block:
        - name: Wipe
          ansible.builtin.shell: rm -rf /var/lib/data
`,
		WantClass: Irreversible,
	}, {
		Name: "making a filesystem destroys whatever was there",
		Playbook: `
- hosts: all
  tasks:
    - community.general.filesystem:
        fstype: ext4
        dev: /dev/sdb1
`,
		WantClass: Irreversible,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := AssessReversibilityFrom(
				&Run{Tool: ToolAnsible, Playbook: "/srv/p.yml"},
				ReversibilityEvidence{Playbook: []byte(test.Playbook)})
			if got.Class != test.WantClass {
				t.Errorf("class = %q, want %q. Reasons: %v", got.Class, test.WantClass, got.Reasons)
			}
		})
	}
}

// TestAnUnreadableIncludeNeverLowersTheGrade holds the direction the scan is allowed to move a
// grade. It may only raise it, so work it cannot see can never make a run look safer than it is.
func TestAnUnreadableIncludeNeverLowersTheGrade(t *testing.T) {
	t.Parallel()
	withRoles := `
- hosts: all
  roles:
    - wipe-everything
  tasks:
    - ansible.builtin.file:
        path: /srv/archive
        state: absent
`
	got := AssessReversibilityFrom(&Run{Tool: ToolAnsible},
		ReversibilityEvidence{Playbook: []byte(withRoles)})
	if got.Class != Irreversible {
		t.Errorf("class = %q, want %q", got.Class, Irreversible)
	}
	var said bool
	for _, r := range got.Reasons {
		if strings.Contains(r, "not followed") {
			said = true
		}
	}
	if !said {
		t.Errorf("the grade does not say roles were not followed, so its scope is invisible: %v",
			got.Reasons)
	}
}

// TestAnIdempotentRunHasNothingToUndo covers the strongest evidence there is.
//
// Ansible is idempotent, and a finished run reports how many tasks actually changed a host. None
// means nothing happened, so a run that could have been destructive provably was not. That beats
// any prediction, including a playbook scan that found something permanent.
func TestAnIdempotentRunHasNothingToUndo(t *testing.T) {
	t.Parallel()
	destructive := []byte(`
- hosts: all
  tasks:
    - ansible.builtin.file:
        path: /srv/archive
        state: absent
`)
	finished := &Run{Tool: ToolAnsible, Status: StatusSucceeded}

	nothingChanged := AssessReversibilityFrom(finished, ReversibilityEvidence{
		Playbook: destructive,
		Hosts:    []HostSummary{{Host: "a", OK: 3, Changed: 0}, {Host: "b", OK: 3, Changed: 0}},
	})
	if nothingChanged.Class != Reversible {
		t.Errorf("a run that changed nothing graded %q, want %q. The file was already gone, so "+
			"nothing was destroyed: %v", nothingChanged.Class, Reversible, nothingChanged.Reasons)
	}

	somethingChanged := AssessReversibilityFrom(finished, ReversibilityEvidence{
		Playbook: destructive,
		Hosts:    []HostSummary{{Host: "a", OK: 3, Changed: 0}, {Host: "b", OK: 2, Changed: 1}},
	})
	if somethingChanged.Class != Irreversible {
		t.Errorf("a run that removed a file on one host graded %q, want %q: %v",
			somethingChanged.Class, Irreversible, somethingChanged.Reasons)
	}
}
