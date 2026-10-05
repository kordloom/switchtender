package run

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// checkPlay is a playbook header the check_mode cases below hang their tasks under.
const checkPlay = "- hosts: all\n  tasks:\n"

// TestCheckModeForcedIsFound covers the scan that decides whether an Ansible dry run is a preview.
//
// Ansible runs any play, block, task, role, or include that sets check_mode to something other than
// true for real under --check. A rule excluding dry runs exempted every dry run on its flag, so a
// playbook with one such task changed hosts while the rule waved it through. Each place the keyword
// can sit is read, and a value the scan cannot prove true counts as forcing.
//
//nolint:funlen // Test function.
func TestCheckModeForcedIsFound(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files      map[string]string
		WantForced []string
	}{{ // Test 0: A task that sets check_mode to false.
		Files: map[string]string{"site.yml": checkPlay +
			"    - name: Restart web\n      ansible.builtin.service: name=web state=restarted\n" +
			"      check_mode: false\n"},
		WantForced: []string{`site.yml: task "Restart web" sets check_mode to false`},
	}, { // Test 1: A play that sets it covers every task under it.
		Files: map[string]string{"site.yml": "- name: Deploy\n  hosts: all\n  check_mode: false\n" +
			"  tasks:\n    - ansible.builtin.ping:\n"},
		WantForced: []string{`site.yml: play "Deploy" sets check_mode to false`},
	}, { // Test 2: A block that sets it, written as no.
		Files: map[string]string{"site.yml": checkPlay +
			"    - name: Migrate\n      check_mode: no\n      block:\n" +
			"        - ansible.builtin.command: /usr/bin/migrate\n"},
		WantForced: []string{`site.yml: block "Migrate" sets check_mode to "no"`},
	}, { // Test 3: A role entry in the play's roles list that sets it.
		Files: map[string]string{
			"site.yml":                 "- hosts: all\n  roles:\n    - role: web\n      check_mode: false\n",
			"roles/web/tasks/main.yml": "- ansible.builtin.ping:\n",
		},
		WantForced: []string{`site.yml: role "web" sets check_mode to false`},
	}, { // Test 4: A task inside a role sets it, found by following the role.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  roles:\n    - web\n",
			"roles/web/tasks/main.yml": "- name: Reload\n  ansible.builtin.command: /bin/reload\n" +
				"  check_mode: false\n",
		},
		WantForced: []string{`roles/web/tasks/main.yml: task "Reload" sets check_mode to false`},
	}, { // Test 5: A task in an included file sets it.
		Files: map[string]string{
			"site.yml": checkPlay + "    - ansible.builtin.include_tasks: more.yml\n",
			"more.yml": "- name: Write\n  ansible.builtin.copy: dest=/etc/x content=y\n" +
				"  check_mode: no\n",
		},
		WantForced: []string{`more.yml: task "Write" sets check_mode to "no"`},
	}, { // Test 6: An include that applies it to everything it pulls in.
		Files: map[string]string{
			"site.yml": checkPlay + "    - name: Pull in\n      ansible.builtin.include_tasks:\n" +
				"        file: more.yml\n        apply:\n          check_mode: false\n",
			"more.yml": "- ansible.builtin.ping:\n",
		},
		WantForced: []string{`site.yml: the apply of task "Pull in" sets check_mode to false`},
	}, { // Test 7: An import task that sets it on itself, which the imported tasks inherit.
		Files: map[string]string{
			"site.yml": checkPlay + "    - name: Import\n      ansible.builtin.import_tasks: more.yml\n" +
				"      check_mode: false\n",
			"more.yml": "- ansible.builtin.ping:\n",
		},
		WantForced: []string{`site.yml: task "Import" sets check_mode to false`},
	}, { // Test 8: A templated value is only decided at run time, so it counts.
		Files: map[string]string{"site.yml": checkPlay +
			"    - name: Maybe\n      ansible.builtin.ping:\n      check_mode: \"{{ live }}\"\n"},
		WantForced: []string{`site.yml: task "Maybe" sets check_mode to "{{ live }}", which is only ` +
			`decided at run time`},
	}, { // Test 9: True, in every spelling Ansible reads as true, forces nothing.
		Files: map[string]string{"site.yml": checkPlay +
			"    - ansible.builtin.ping:\n      check_mode: true\n" +
			"    - ansible.builtin.ping:\n      check_mode: yes\n" +
			"    - ansible.builtin.ping:\n      check_mode: \"True\"\n" +
			"    - ansible.builtin.ping:\n      check_mode: 1\n"},
		WantForced: nil,
	}, { // Test 10: A playbook with no check_mode anywhere forces nothing.
		Files:      map[string]string{"site.yml": checkPlay + "    - ansible.builtin.ping:\n"},
		WantForced: nil,
	}, { // Test 11: An unnamed task is named by its module, so it can still be found.
		Files: map[string]string{"site.yml": checkPlay +
			"    - ansible.builtin.shell: echo hi\n      check_mode: false\n"},
		WantForced: []string{
			`site.yml: an unnamed "ansible.builtin.shell" task sets check_mode to false`,
		},
	}, { // Test 12: An empty value is not true.
		Files: map[string]string{"site.yml": checkPlay +
			"    - name: Empty\n      ansible.builtin.ping:\n      check_mode:\n"},
		WantForced: []string{`site.yml: task "Empty" sets check_mode to nothing, which is not true`},
	}, { // Test 13: A handler that sets it runs for real when notified.
		Files: map[string]string{"site.yml": "- hosts: all\n  handlers:\n" +
			"    - name: Bounce\n      ansible.builtin.service: name=web state=restarted\n" +
			"      check_mode: false\n"},
		WantForced: []string{`site.yml: task "Bounce" sets check_mode to false`},
	}, { // Test 14: A role dependency that sets it, read from the role's metadata.
		Files: map[string]string{
			"site.yml":                  "- hosts: all\n  roles:\n    - web\n",
			"roles/web/meta/main.yml":   "dependencies:\n  - role: base\n    check_mode: false\n",
			"roles/base/tasks/main.yml": "- ansible.builtin.ping:\n",
		},
		WantForced: []string{`roles/web/meta/main.yml: role dependency "base" sets check_mode to false`},
	}, { // Test 15: An imported playbook that sets it on its own play.
		Files: map[string]string{
			"site.yml":  "- ansible.builtin.import_playbook: other.yml\n",
			"other.yml": "- name: Other\n  hosts: all\n  check_mode: false\n  tasks: []\n",
		},
		WantForced: []string{`other.yml: play "Other" sets check_mode to false`},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := scanned(t, test.Files, "site.yml").DryRunScan()
			if diff := cmp.Diff(test.WantForced, got.Entries(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("DryRunScan().Entries() mismatch (-want +got):\n%s", diff)
			}
			wantClass := DryRunChangeFree
			if len(test.WantForced) > 0 {
				wantClass = DryRunNotChangeFree
			}
			if got.Classification != wantClass || got.Scanner != CheckModeScanner {
				t.Errorf("scan = %s classified %q, want %s classified %q", got.Scanner,
					got.Classification, CheckModeScanner, wantClass)
			}
		})
	}
}

// TestCheckModeFailsClosedOnWhatItCannotRead covers the half of the scan that cannot prove a
// negative. A dry run is exempt only when nothing in it forces real work, and a file the scan did
// not read proves nothing, so a role outside the tree, an include named at run time, and a scan
// that stopped at its bounds all leave the run treated as forcing.
func TestCheckModeFailsClosedOnWhatItCannotRead(t *testing.T) {
	t.Parallel()
	deep := map[string]string{"site.yml": checkPlay + "    - ansible.builtin.include_tasks: t0.yml\n"}
	for i := range maxScanDepth + 2 {
		deep[fmt.Sprintf("t%d.yml", i)] = fmt.Sprintf("- ansible.builtin.include_tasks: t%d.yml\n", i+1)
	}
	wide := map[string]string{"site.yml": checkPlay}
	for i := range maxScanFiles + 4 {
		wide["site.yml"] += fmt.Sprintf("    - ansible.builtin.include_tasks: w%d.yml\n", i)
		wide[fmt.Sprintf("w%d.yml", i)] = "- ansible.builtin.ping:\n"
	}
	tests := []struct {
		Files    map[string]string
		WantText string
	}{{ // Test 0: A role that is not in the tree.
		Files:    map[string]string{"site.yml": "- hosts: all\n  roles:\n    - missing\n"},
		WantText: `could not read role "missing" (not in the project)`,
	}, { // Test 1: An include named at run time.
		Files: map[string]string{
			"site.yml": checkPlay + "    - ansible.builtin.include_tasks: \"{{ f }}\"\n",
		},
		WantText: "named at run time",
	}, { // Test 2: Includes nested past the depth one scan follows.
		Files:    deep,
		WantText: fmt.Sprintf("nested more than %d deep", maxScanDepth),
	}, { // Test 3: More files than one scan reads.
		Files:    wide,
		WantText: fmt.Sprintf("past the %d files", maxScanFiles),
	}, { // Test 4: An included file that is not a list of tasks.
		Files: map[string]string{
			"site.yml": checkPlay + "    - ansible.builtin.include_tasks: odd.yml\n",
			"odd.yml":  "key: value\n",
		},
		WantText: "not a list of tasks",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			scan := scanned(t, test.Files, "site.yml").DryRunScan()
			forced := scan.Entries()
			if len(forced) == 0 || scan.ChangeFree() {
				t.Fatal("the scan found nothing, so a dry run of a playbook the scan could not " +
					"read in full would be exempt")
			}
			if !strings.Contains(strings.Join(forced, "\n"), test.WantText) {
				t.Errorf("Entries() = %q, want an entry mentioning %q", forced, test.WantText)
			}
		})
	}
}

// TestChangeFreeReadsTheRecord covers the one judgment every grader shares: a dry run changes
// nothing only when the gate found nothing it forces.
func TestChangeFreeReadsTheRecord(t *testing.T) {
	t.Parallel()
	forced := []string{`site.yml: task "Restart web" sets check_mode to false`}
	tests := []struct {
		Run            *Run
		WantChangeFree bool
		WantClass      string
	}{{ // Test 0: A clean dry run changes nothing.
		Run:            &Run{Tool: ToolAnsible, Playbook: "site.yml", DryRun: true},
		WantChangeFree: true, WantClass: Reversible,
	}, { // Test 1: A dry run that forces real work is graded as the change it is.
		Run: &Run{Tool: ToolAnsible, Playbook: "site.yml", DryRun: true, Limit: "web01",
			DryRunScans: []DryRunScan{{Tool: ToolAnsible, Findings: forced}}},
		WantChangeFree: false, WantClass: ReversibleCostly,
	}, { // Test 2: A real run is never change free.
		Run:            &Run{Tool: ToolAnsible, Playbook: "site.yml"},
		WantChangeFree: false, WantClass: ReversibleCostly,
	}, { // Test 3: Nil is no run at all.
		Run: nil, WantChangeFree: false, WantClass: Reversible,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Run.ChangeFree(); got != test.WantChangeFree {
				t.Errorf("ChangeFree() = %v, want %v", got, test.WantChangeFree)
			}
			undo := AssessReversibility(test.Run)
			if undo.Class != test.WantClass {
				t.Errorf("reversibility = %q, want %q: %v", undo.Class, test.WantClass, undo.Reasons)
			}
			if test.Run != nil && len(test.Run.DryRunScans) > 0 {
				risk := AssessRisk(test.Run)
				if !strings.Contains(strings.Join(risk.Reasons, "\n"), "Restart web") ||
					!strings.Contains(strings.Join(undo.Reasons, "\n"), "Restart web") {
					t.Errorf("the grades do not name the forcing task: risk %v, undo %v",
						risk.Reasons, undo.Reasons)
				}
				if slices.Contains(risk.Reasons, "dry run, makes no changes") {
					t.Errorf("a forcing dry run is graded as making no changes: %v", risk.Reasons)
				}
			}
		})
	}
}
