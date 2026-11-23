package run

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/google/go-cmp/cmp"
)

// treeOf builds an in-memory tree from path and content pairs.
func treeOf(files map[string]string) fstest.MapFS {
	fsys := fstest.MapFS{}
	for name, body := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(body)}
	}
	return fsys
}

// scanned scans the playbook at name in a tree built from files and fails the test when the
// playbook itself cannot be read.
func scanned(t *testing.T, files map[string]string, name string) *PlaybookSignals {
	t.Helper()
	signals, err := ScanPlaybookIn(treeOf(files), name, "the project")
	if err != nil {
		t.Fatalf("ScanPlaybookIn(%q) error = %v", name, err)
	}
	return &signals
}

// gradeOf grades an Ansible run of the playbook at name in a tree built from files.
func gradeOf(t *testing.T, files map[string]string, name string) Reversibility {
	t.Helper()
	return AssessReversibilityFrom(&Run{Tool: ToolAnsible, Playbook: name},
		ReversibilityEvidence{Playbook: scanned(t, files, name)})
}

// TestAPlaybookIsGradedByWhatItDoes covers the gap that made reversibility blind to Ansible.
//
// The grade reads a command, and an Ansible run has none: the work is inside a file. A playbook
// that drops every database used to grade exactly like one that restarts a service, which is the
// flagship tool grading blindest.
func TestAPlaybookIsGradedByWhatItDoes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Playbook  string
		WantClass string
	}{{ // Test 0: A file removed is gone.
		Playbook: `
- hosts: all
  tasks:
    - name: Drop the archive
      ansible.builtin.file:
        path: /srv/archive
        state: absent
`,
		WantClass: Irreversible,
	}, { // Test 1: A database dropped is gone.
		Playbook: `
- hosts: all
  tasks:
    - community.mysql.mysql_db:
        name: prod
        state: absent
`,
		WantClass: Irreversible,
	}, { // Test 2: A package removed is a reinstall, not a loss.
		Playbook: `
- hosts: all
  tasks:
    - ansible.builtin.package:
        name: nginx
        state: absent
`,
		WantClass: ReversibleCostly,
	}, { // Test 3: A service restarted comes back.
		Playbook: `
- hosts: all
  tasks:
    - ansible.builtin.service:
        name: nginx
        state: restarted
`,
		WantClass: ReversibleCostly,
	}, { // Test 4: A destructive shell task inside a block is still found.
		Playbook: `
- hosts: all
  tasks:
    - block:
        - name: Wipe
          ansible.builtin.shell: rm -rf /var/lib/data
`,
		WantClass: Irreversible,
	}, { // Test 5: Making a filesystem destroys whatever was there.
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
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := gradeOf(t, map[string]string{"p.yml": test.Playbook}, "p.yml")
			if got.Class != test.WantClass {
				t.Errorf("class = %q, want %q. Reasons: %v", got.Class, test.WantClass, got.Reasons)
			}
		})
	}
}

// Playbook fragments the following tests assemble trees from.
const (
	// wipeTasks is a task file whose one task removes a directory for good.
	wipeTasks = "- ansible.builtin.file:\n    path: /srv/archive\n    state: absent\n"
	// dropTasks is a task file whose one task drops a database.
	dropTasks = "- community.mysql.mysql_db:\n    name: prod\n    state: absent\n"
	// restartTasks is a task file whose one task restarts a service, which comes back.
	restartTasks = "- ansible.builtin.service:\n    name: nginx\n    state: restarted\n"
)

// TestTheGradeFollowsWhatAPlaybookPullsIn is the reason the scan follows at all.
//
// A real playbook is a few lines that name roles, and the roles hold the work. Reading only the
// playbook graded every such run on what its top file happened to say, which for a role-based
// project was nothing, so a teardown role and a restart role graded the same. Each case puts the
// destructive work one hop away, where a scan of the playbook alone cannot see it, and each would
// grade costly if that hop were not followed.
func TestTheGradeFollowsWhatAPlaybookPullsIn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files      map[string]string
		Playbook   string
		WantClass  string
		WantReason string
	}{{ // Test 0: A role's tasks, the common case.
		Files: map[string]string{
			"site.yml":                 "- hosts: all\n  roles:\n    - db\n",
			"roles/db/tasks/main.yml":  dropTasks,
			"roles/web/tasks/main.yml": restartTasks,
		},
		WantClass:  Irreversible,
		WantReason: "a task in roles/db/tasks/main.yml removes a mysql_db",
	}, { // Test 1: A role's handlers run real work when notified.
		Files: map[string]string{
			"site.yml":                   "- hosts: all\n  roles:\n    - { role: db }\n",
			"roles/db/tasks/main.yml":    restartTasks,
			"roles/db/handlers/main.yml": "- name: purge\n  ansible.builtin.shell: rm -rf /var/lib/db\n",
		},
		WantClass:  Irreversible,
		WantReason: "a shell task in roles/db/handlers/main.yml",
	}, { // Test 2: A role's dependencies run before it whether or not the playbook names them.
		Files: map[string]string{
			"site.yml":                     "- hosts: all\n  roles:\n    - app\n",
			"roles/app/tasks/main.yml":     restartTasks,
			"roles/app/meta/main.yml":      "dependencies:\n  - role: cleanup\n",
			"roles/cleanup/tasks/main.yml": wipeTasks,
		},
		WantClass:  Irreversible,
		WantReason: "roles/cleanup/tasks/main.yml",
	}, { // Test 3: An include_tasks resolves beside the file doing the including.
		Files: map[string]string{
			"site.yml":       "- hosts: all\n  tasks:\n    - include_tasks: tasks/wipe.yml\n",
			"tasks/wipe.yml": wipeTasks,
		},
		WantClass:  Irreversible,
		WantReason: "a task in tasks/wipe.yml",
	}, { // Test 4: An import_tasks inside a role resolves in the role's tasks directory.
		Files: map[string]string{
			"site.yml":                "- hosts: all\n  roles:\n    - db\n",
			"roles/db/tasks/main.yml": "- ansible.builtin.import_tasks: drop.yml\n",
			"roles/db/tasks/drop.yml": dropTasks,
		},
		WantClass:  Irreversible,
		WantReason: "roles/db/tasks/drop.yml",
	}, { // Test 5: An include_role with tasks_from reads the file it names.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - include_role:\n" +
				"        name: db\n        tasks_from: teardown\n",
			"roles/db/tasks/main.yml":     restartTasks,
			"roles/db/tasks/teardown.yml": dropTasks,
		},
		WantClass:  Irreversible,
		WantReason: "roles/db/tasks/teardown.yml",
	}, { // Test 6: An imported playbook resolves against its own directory, roles and all.
		Files: map[string]string{
			"site.yml":                         "- import_playbook: plays/db.yml\n",
			"plays/db.yml":                     "- hosts: db\n  roles:\n    - purge\n",
			"plays/roles/purge/tasks/main.yml": wipeTasks,
		},
		WantClass:  Irreversible,
		WantReason: "plays/roles/purge/tasks/main.yml",
	}, { // Test 7: A collection role, installed where a sync puts a project's dependencies.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  roles:\n    - acme.storage.wipe\n",
			".galaxy/collections/ansible_collections/acme/storage/roles/wipe/tasks/main.yml": wipeTasks,
		},
		WantClass:  Irreversible,
		WantReason: "ansible_collections/acme/storage/roles/wipe/tasks/main.yml",
	}, { // Test 8: A Galaxy role, installed where a sync puts a project's roles.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  roles:\n    - geerlingguy.mysql\n",
			".galaxy/roles/geerlingguy.mysql/tasks/main.yml": dropTasks,
		},
		WantClass:  Irreversible,
		WantReason: ".galaxy/roles/geerlingguy.mysql/tasks/main.yml",
	}, { // Test 9: The roles path ansible.cfg sets, for a playbook in a subdirectory.
		Files: map[string]string{
			"ansible.cfg": "[defaults]\n" +
				"roles_path = ./shared/roles:/etc/ansible/roles\n",
			"playbooks/site.yml":                "- hosts: all\n  roles:\n    - purge\n",
			"shared/roles/purge/tasks/main.yml": wipeTasks,
		},
		Playbook:   "playbooks/site.yml",
		WantClass:  Irreversible,
		WantReason: "shared/roles/purge/tasks/main.yml",
	}, { // Test 10: A short role name from a collection the play names.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  collections:\n    - acme.storage\n  roles:\n    - wipe\n",
			".galaxy/collections/ansible_collections/acme/storage/roles/wipe/tasks/main.yml": wipeTasks,
		},
		WantClass:  Irreversible,
		WantReason: "acme/storage/roles/wipe/tasks/main.yml",
	}, { // Test 11: An include written in the old inline form still resolves.
		Files: map[string]string{
			"site.yml":       "- hosts: all\n  tasks:\n    - include: tasks/wipe.yml\n",
			"tasks/wipe.yml": wipeTasks,
		},
		WantClass:  Irreversible,
		WantReason: "a task in tasks/wipe.yml",
	}, { // Test 12: A role named by path from the root the run starts in.
		Files: map[string]string{
			"playbooks/site.yml":                 "- hosts: all\n  roles:\n    - roles/purge\n",
			"roles/purge/tasks/main.yml":         wipeTasks,
			"playbooks/roles/web/tasks/main.yml": restartTasks,
		},
		Playbook:   "playbooks/site.yml",
		WantClass:  Irreversible,
		WantReason: "roles/purge/tasks/main.yml",
	}, { // Test 13: A nested include inside a role resolves against the role's tasks directory.
		Files: map[string]string{
			"site.yml":                     "- hosts: all\n  roles:\n    - db\n",
			"roles/db/tasks/main.yml":      "- include_tasks: steps/run.yml\n",
			"roles/db/tasks/steps/run.yml": "- include_tasks: drop.yml\n",
			"roles/db/tasks/drop.yml":      dropTasks,
		},
		WantClass:  Irreversible,
		WantReason: "a task in roles/db/tasks/drop.yml",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			name := test.Playbook
			if name == "" {
				name = "site.yml"
			}
			got := gradeOf(t, test.Files, name)
			if got.Class != test.WantClass {
				t.Errorf("class = %q, want %q. Reasons: %v", got.Class, test.WantClass, got.Reasons)
			}
			if said := strings.Join(got.Reasons, "\n"); !strings.Contains(said, test.WantReason) {
				t.Errorf("reasons do not name %q, so an approver cannot find the task:\n%s",
					test.WantReason, said)
			}
		})
	}
}

// TestTheGradeReadsOnlyWhatRuns holds the scan to precision as well as reach.
//
// A role's other task files run only when something names them, and a role on disk runs only when
// a play pulls it in. Grading a restart as irreversible because a teardown file sits beside it is
// the false alarm that gets an irreversibility rule switched off.
func TestTheGradeReadsOnlyWhatRuns(t *testing.T) {
	t.Parallel()
	got := gradeOf(t, map[string]string{
		"site.yml":                     "- hosts: all\n  roles:\n    - web\n",
		"roles/web/tasks/main.yml":     "- include_tasks: install.yml\n",
		"roles/web/tasks/install.yml":  restartTasks,
		"roles/web/tasks/teardown.yml": wipeTasks,
		"roles/unused/tasks/main.yml":  dropTasks,
	}, "site.yml")
	if got.Class != ReversibleCostly {
		t.Errorf("class = %q, want %q: a file nothing runs raised the grade. Reasons: %v",
			got.Class, ReversibleCostly, got.Reasons)
	}
	want := "the playbook and the 2 files it pulls in were read"
	if said := strings.Join(got.Reasons, "\n"); !strings.Contains(said, want) {
		t.Errorf("reasons do not say %q:\n%s", want, said)
	}
}

// TestWhatTheScanCannotReadIsNamed covers the scan's honesty about its reach. Each case pulls in
// something the scan cannot read, and the grade must name it and why rather than silently grading
// what it could see as the whole run.
func TestWhatTheScanCannotReadIsNamed(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files      map[string]string
		WantReason string
	}{{ // Test 0: A name only known at run time.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - include_tasks: \"{{ step }}.yml\"\n",
		},
		WantReason: `could not follow include_tasks "{{ step }}.yml" (named at run time)`,
	}, { // Test 1: A role that is not in the project.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  roles:\n    - missing\n",
		},
		WantReason: `could not follow role "missing" (not in the project)`,
	}, { // Test 2: An include that climbs out of the project.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - import_tasks: ../../etc/tasks.yml\n",
		},
		WantReason: `could not follow import_tasks "../../etc/tasks.yml" (outside the project)`,
	}, { // Test 3: An include named by absolute path.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - include_tasks: /etc/ansible/tasks.yml\n",
		},
		WantReason: `could not follow include_tasks "/etc/ansible/tasks.yml" (outside the project)`,
	}, { // Test 4: A task file that is not a list of tasks.
		Files: map[string]string{
			"site.yml":   "- hosts: all\n  tasks:\n    - include_tasks: broken.yml\n",
			"broken.yml": "not: [a, task, list\n",
		},
		WantReason: `could not follow include_tasks "broken.yml" (not a list of tasks)`,
	}, { // Test 5: A role's tasks named at run time.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - include_role:\n" +
				"        name: db\n        tasks_from: \"{{ action }}\"\n",
			"roles/db/tasks/main.yml": restartTasks,
		},
		WantReason: `could not follow include_role "db" (its tasks are named at run time)`,
	}, { // Test 6: A tasks_from naming a file the role does not have.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - import_role:\n" +
				"        name: db\n        tasks_from: nothere\n",
			"roles/db/tasks/main.yml": restartTasks,
		},
		WantReason: `import_role "db" tasks "nothere" (not in the project)`,
	}, { // Test 7: More than two unread names are counted, and the first two named.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  roles:\n    - one\n    - two\n    - three\n",
		},
		WantReason: `could not follow 3 roles and includes, among them role "one" (not in the ` +
			`project) and role "two" (not in the project), so the grade covers only what was read`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := gradeOf(t, test.Files, "site.yml")
			if got.Class != ReversibleCostly {
				t.Errorf("class = %q, want %q", got.Class, ReversibleCostly)
			}
			if said := strings.Join(got.Reasons, "\n"); !strings.Contains(said, test.WantReason) {
				t.Errorf("reasons do not say %q:\n%s", test.WantReason, said)
			}
		})
	}
}

// TestAnUnreadableIncludeNeverLowersTheGrade holds the direction the scan is allowed to move a
// grade. It may only raise it, so work it cannot see can never make a run look safer than it is.
func TestAnUnreadableIncludeNeverLowersTheGrade(t *testing.T) {
	t.Parallel()
	got := gradeOf(t, map[string]string{"site.yml": `
- hosts: all
  roles:
    - wipe-everything
  tasks:
    - ansible.builtin.file:
        path: /srv/archive
        state: absent
`}, "site.yml")
	if got.Class != Irreversible {
		t.Errorf("class = %q, want %q", got.Class, Irreversible)
	}
	want := `could not follow role "wipe-everything" (not in the project), so there may be more`
	if said := strings.Join(got.Reasons, "\n"); !strings.Contains(said, want) {
		t.Errorf("the grade does not say what it could not follow, so its scope is invisible:\n%s",
			said)
	}
}

// TestAScanOfAHostileTreeEnds covers the shapes a repository can take that would otherwise keep a
// scan on the submit path running: includes that loop, roles that depend on each other, and more
// files than one grade should read. Each must end, and a bound that stopped the scan must say so.
func TestAScanOfAHostileTreeEnds(t *testing.T) {
	t.Parallel()
	t.Run("includes that loop", func(t *testing.T) {
		t.Parallel()
		got := scanned(t, map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - include_tasks: a.yml\n",
			"a.yml":    "- include_tasks: b.yml\n",
			"b.yml":    "- include_tasks: a.yml\n" + wipeTasks,
		}, "site.yml")
		if len(got.Permanent) != 1 {
			t.Errorf("permanent = %v, want the one finding in b.yml, found once", got.Permanent)
		}
	})
	t.Run("roles that depend on each other", func(t *testing.T) {
		t.Parallel()
		got := scanned(t, map[string]string{
			"site.yml":               "- hosts: all\n  roles:\n    - a\n",
			"roles/a/meta/main.yml":  "dependencies: [b]\n",
			"roles/b/meta/main.yml":  "dependencies: [a]\n",
			"roles/b/tasks/main.yml": wipeTasks,
		}, "site.yml")
		if len(got.Permanent) != 1 {
			t.Errorf("permanent = %v, want the one finding in role b", got.Permanent)
		}
	})
	t.Run("includes nested deeper than a grade follows", func(t *testing.T) {
		t.Parallel()
		files := map[string]string{"site.yml": "- hosts: all\n  tasks:\n    - include_tasks: t0.yml\n"}
		for i := 0; i < maxScanDepth+10; i++ {
			files[fmt.Sprintf("t%d.yml", i)] = fmt.Sprintf("- include_tasks: t%d.yml\n", i+1)
		}
		got := scanned(t, files, "site.yml")
		want := fmt.Sprintf("nested more than %d deep", maxScanDepth)
		if said := strings.Join(got.Unread, "\n"); !strings.Contains(said, want) {
			t.Errorf("unread does not say %q, so the stop is invisible:\n%s", want, said)
		}
	})
	t.Run("more files than a grade reads", func(t *testing.T) {
		t.Parallel()
		var play strings.Builder
		play.WriteString("- hosts: all\n  tasks:\n")
		files := map[string]string{}
		for i := 0; i < maxScanFiles+10; i++ {
			fmt.Fprintf(&play, "    - include_tasks: t%d.yml\n", i)
			files[fmt.Sprintf("t%d.yml", i)] = restartTasks
		}
		files["site.yml"] = play.String()
		got := scanned(t, files, "site.yml")
		if got.Files > maxScanFiles {
			t.Errorf("read %d files, more than the %d a grade reads", got.Files, maxScanFiles)
		}
		want := fmt.Sprintf("past the %d files", maxScanFiles)
		if said := strings.Join(got.Unread, "\n"); !strings.Contains(said, want) {
			t.Errorf("unread does not say %q, so the stop is invisible:\n%s", want, said)
		}
	})
	t.Run("a file larger than a grade reads", func(t *testing.T) {
		t.Parallel()
		got := scanned(t, map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - include_tasks: huge.yml\n",
			"huge.yml": "# " + strings.Repeat("x", maxScanFileBytes) + "\n",
		}, "site.yml")
		if said := strings.Join(got.Unread, "\n"); !strings.Contains(said, "larger than the 1 MiB") {
			t.Errorf("unread does not say the file was too large:\n%s", said)
		}
	})
}

// TestAnIdempotentRunHasNothingToUndo covers the strongest evidence there is.
//
// Ansible is idempotent, and a finished run reports how many tasks actually changed a host. None
// means nothing happened, so a run that could have been destructive provably was not. That beats
// any prediction, including a playbook scan that found something permanent.
func TestAnIdempotentRunHasNothingToUndo(t *testing.T) {
	t.Parallel()
	destructive := scanned(t, map[string]string{"site.yml": "- hosts: all\n  tasks:\n" +
		"    - ansible.builtin.file:\n        path: /srv/archive\n        state: absent\n"}, "site.yml")
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

// TestTheGradeSaysWhichCommitItRead covers the source line. A project's files have many versions,
// and a grade that does not say which one it read cannot be checked against the run it describes.
func TestTheGradeSaysWhichCommitItRead(t *testing.T) {
	t.Parallel()
	signals := scanned(t, map[string]string{"site.yml": "- hosts: all\n  tasks: []\n"}, "site.yml")
	signals.Source = "read at commit 0123456789ab, the commit this run is pinned to"
	got := AssessReversibilityFrom(&Run{Tool: ToolAnsible, Playbook: "site.yml"},
		ReversibilityEvidence{Playbook: signals})
	if said := strings.Join(got.Reasons, "\n"); !strings.Contains(said, signals.Source) {
		t.Errorf("reasons do not carry the source %q:\n%s", signals.Source, said)
	}
}

// TestTheScanDeniesEveryLookupItCannotClear covers the scan as a default deny. Each of the first
// five forms ran a command on the controller under ansible-playbook --check, and a scan that
// matched the pipe lookup by name classified every one of them change free. A dry run is change free only
// when every lookup names, as a quoted literal, a plugin the scan knows runs nothing, every with_
// loop does too, and the lookup names appear nowhere else.
//
//nolint:funlen // Test function.
func TestTheScanDeniesEveryLookupItCannotClear(t *testing.T) {
	t.Parallel()
	task := func(body string) string {
		return checkPlay + "    - name: Probe\n" + body
	}
	tests := []struct {
		Playbook       string
		WantChangeFree bool
	}{{ // Test 0: The lines lookup runs its argument as a command.
		Playbook: task("      ansible.builtin.debug:\n" +
			"        msg: \"{{ lookup('lines', 'id') }}\"\n"),
	}, { // Test 1: The lookup function called through parentheses.
		Playbook: task("      ansible.builtin.debug:\n" +
			"        msg: \"{{ (lookup)('pipe', 'id') }}\"\n"),
	}, { // Test 2: The plugin named through a variable.
		Playbook: "- hosts: all\n  vars:\n    plugin_name: pipe\n  tasks:\n" +
			"    - ansible.builtin.debug:\n        msg: \"{{ lookup(plugin_name, 'id') }}\"\n",
	}, { // Test 3: The plugin's name built from pieces.
		Playbook: task("      ansible.builtin.debug:\n" +
			"        msg: \"{{ lookup('pi' ~ 'pe', 'id') }}\"\n"),
	}, { // Test 4: The loop form of the lines lookup.
		Playbook: task("      ansible.builtin.debug:\n        msg: \"{{ item }}\"\n" +
			"      with_lines: id\n"),
	}, { // Test 5: A plain playbook looping with with_items stays change free.
		Playbook: task("      ansible.builtin.debug:\n        msg: \"{{ item }}\"\n" +
			"      with_items:\n        - one\n        - two\n"),
		WantChangeFree: true,
	}, { // Test 6: Every lookup naming a cleared plugin as a literal stays change free.
		Playbook: task("      ansible.builtin.debug:\n" +
			"        msg: \"{{ lookup('vars', 'x') }} {{ query(\\\"ansible.builtin.items\\\", a) }} " +
			"{{ q('sequence', 'start=1 end=3') }}\"\n"),
		WantChangeFree: true,
	}, { // Test 7: A command's -q flag is a word, not the q function.
		Playbook:       task("      ansible.builtin.command: grep -q web /etc/hosts\n"),
		WantChangeFree: true,
	}, { // Test 8: The q function handed on through parentheses.
		Playbook: task("      ansible.builtin.debug:\n" +
			"        msg: \"{{ (q)('pipe', 'id') }}\"\n"),
	}, { // Test 9: A literal joined to the next one by Jinja names a plugin only known at run time.
		Playbook: task("      ansible.builtin.debug:\n" +
			"        msg: \"{{ query('vars' 'x', 'id') }}\"\n"),
	}, { // Test 10: The word lookup anywhere else, a task's name included, costs the classification.
		Playbook: checkPlay + "    - name: lookup the host\n      ansible.builtin.ping:\n",
	}, { // Test 11: A lookup in the play's hosts is templated on the controller.
		Playbook: "- hosts: \"{{ lookup('file', 'hosts.txt') }}\"\n  tasks:\n" +
			"    - ansible.builtin.ping:\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			scan := scanned(t, map[string]string{"site.yml": test.Playbook}, "site.yml").DryRunScan()
			if diff := cmp.Diff(test.WantChangeFree, scan.ChangeFree()); diff != "" {
				t.Errorf("ChangeFree() mismatch (-want +got):\n%s\nfindings=%q", diff, scan.Findings)
			}
		})
	}
}

// TestTheScanDeniesConnectionVariables covers a play, role, task, or set_fact that sets an Ansible
// connection or interpreter variable. Each names a program the controller starts, under --check as
// much as without it, so the dry run is not change free. The variables that only say who and where
// stay allowed.
func TestTheScanDeniesConnectionVariables(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files          map[string]string
		WantChangeFree bool
		WantFinding    string
	}{{ // Test 0: A play var naming the shell.
		Files: map[string]string{"site.yml": "- hosts: all\n  vars:\n    ansible_shell_executable: /tmp/sh\n" +
			"  tasks:\n    - ansible.builtin.ping:\n"},
		WantFinding: `sets "ansible_shell_executable"`,
	}, { // Test 1: A task var naming the interpreter.
		Files: map[string]string{"site.yml": checkPlay + "    - name: Probe\n      ansible.builtin.ping:\n" +
			"      vars:\n        ansible_python_interpreter: /tmp/py\n"},
		WantFinding: `task "Probe" sets "ansible_python_interpreter"`,
	}, { // Test 2: A fact set for the tasks after it.
		Files: map[string]string{"site.yml": checkPlay + "    - name: Switch\n      ansible.builtin.set_fact:\n" +
			"        ansible_connection: local\n"},
		WantFinding: `task "Switch" sets "ansible_connection"`,
	}, { // Test 3: A role parameter.
		Files: map[string]string{
			"site.yml":                 "- hosts: all\n  roles:\n    - role: web\n      ansible_ssh_common_args: -o x\n",
			"roles/web/tasks/main.yml": "- ansible.builtin.ping:\n",
		},
		WantFinding: `role "web" sets "ansible_ssh_common_args"`,
	}, { // Test 4: Control, who and where on the play.
		Files: map[string]string{"site.yml": "- hosts: all\n  vars:\n    ansible_user: deploy\n" +
			"    ansible_port: 2222\n  tasks:\n    - ansible.builtin.ping:\n"},
		WantChangeFree: true,
	}, { // Test 5: Control, an ordinary fact.
		Files:          map[string]string{"site.yml": checkPlay + "    - ansible.builtin.set_fact:\n        tier: web\n"},
		WantChangeFree: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			scan := scanned(t, test.Files, "site.yml").DryRunScan()
			if diff := cmp.Diff(test.WantChangeFree, scan.ChangeFree()); diff != "" {
				t.Errorf("ChangeFree() mismatch (-want +got):\n%s\nfindings=%q", diff, scan.Findings)
			}
			if test.WantFinding != "" && !strings.Contains(strings.Join(scan.Findings, "\n"), test.WantFinding) {
				t.Errorf("findings %q, want one naming %q", scan.Findings, test.WantFinding)
			}
		})
	}
}

// TestTheScanReadsVariableFilesAndTemplates covers the variable files and templates a playbook's
// tasks reach. A variable is templated on the controller where a task uses it, and a template is
// rendered whole there, so a lookup in a role's defaults, a vars_files entry, group_vars, or a
// template source runs under --check as much as one written in the task. The scan read only tasks
// and play vars, so each of these was classified change free. A file it cannot read as plain data
// leaves the scan incomplete.
//
//nolint:funlen // Test function.
func TestTheScanReadsVariableFilesAndTemplates(t *testing.T) {
	t.Parallel()
	const lookup = "\"{{ lookup('pipe', 'id') }}\""
	cleanSite := "- hosts: all\n  roles:\n    - web\n"
	webTasks := "- ansible.builtin.debug:\n    msg: \"{{ banner }}\"\n"
	manyGroupVars := map[string]string{"site.yml": checkPlay + "    - ansible.builtin.ping:\n"}
	for i := range maxScanFiles + 4 {
		manyGroupVars[fmt.Sprintf("group_vars/g%03d.yml", i)] = "tier: web\n"
	}
	tests := []struct {
		Files          map[string]string
		WantChangeFree bool
		WantFinding    string
		WantUnread     string
	}{{ // Test 0: A role's defaults set the variable a task prints to a pipe lookup.
		Files: map[string]string{"site.yml": cleanSite, "roles/web/tasks/main.yml": webTasks,
			"roles/web/defaults/main.yml": "banner: " + lookup + "\n"},
		WantFinding: `roles/web/defaults/main.yml: a variable file calls the lookup plugin "pipe"`,
	}, { // Test 1: A vars_files entry holds the lookup.
		Files: map[string]string{"site.yml": "- hosts: all\n  vars_files:\n    - vars/extra.yml\n" +
			"  tasks:\n    - ansible.builtin.debug:\n        msg: \"{{ banner }}\"\n",
			"vars/extra.yml": "banner: " + lookup + "\n"},
		WantFinding: `vars/extra.yml: a variable file calls the lookup plugin "pipe"`,
	}, { // Test 2: A template task's source holds the lookup.
		Files: map[string]string{"site.yml": cleanSite,
			"roles/web/tasks/main.yml":    "- ansible.builtin.template:\n    src: motd.j2\n    dest: /etc/motd\n",
			"roles/web/templates/motd.j2": "Welcome {{ lookup('pipe', 'id') }}\n"},
		WantFinding: `roles/web/templates/motd.j2: a template calls the lookup plugin "pipe"`,
	}, { // Test 3: group_vars that set the interpreter.
		Files: map[string]string{"site.yml": checkPlay + "    - ansible.builtin.ping:\n",
			"group_vars/all.yml": "ansible_python_interpreter: /tmp/py\n"},
		WantFinding: `group_vars/all.yml: a variable file sets "ansible_python_interpreter"`,
	}, { // Test 4: A vault-encrypted group_vars file cannot be read.
		Files: map[string]string{"site.yml": checkPlay + "    - ansible.builtin.ping:\n",
			"group_vars/all/vault.yml": "$ANSIBLE_VAULT;1.1;AES256\n6162636465\n"},
		WantUnread: `variable file "group_vars/all/vault.yml" (vault encrypted`,
	}, { // Test 5: One vault-encrypted value in a plain file cannot be read either.
		Files: map[string]string{"site.yml": checkPlay + "    - ansible.builtin.ping:\n",
			"host_vars/web1.yml": "db_password: !vault |\n  $ANSIBLE_VAULT;1.1;AES256\n  6162\n"},
		WantUnread: `variable file "host_vars/web1.yml" (holds a vault-encrypted value`,
	}, { // Test 6: Past the read cap the rest is unread.
		Files:      manyGroupVars,
		WantUnread: "past the 256 files",
	}, { // Test 7: An include_vars target holds the lookup.
		Files: map[string]string{"site.yml": checkPlay + "    - ansible.builtin.include_vars: more.yml\n",
			"vars/more.yml": "x: " + lookup + "\n"},
		WantFinding: `vars/more.yml: a variable file calls the lookup plugin "pipe"`,
	}, { // Test 8: Control, a plain with_items playbook with a plain role.
		Files: map[string]string{"site.yml": cleanSite,
			"roles/web/tasks/main.yml": "- ansible.builtin.debug:\n    msg: \"{{ item }}\"\n" +
				"  with_items:\n    - one\n    - two\n",
			"roles/web/defaults/main.yml": "banner: hello\n",
			"roles/web/templates/motd.j2": "Welcome {{ banner }}\n"},
		WantChangeFree: true,
	}, { // Test 9: Control, the word query in a name and lookup in a comment.
		Files: map[string]string{"site.yml": checkPlay + "    - ansible.builtin.ping:\n",
			"group_vars/all.yml": "# lookup the timeout below\nquery_timeout: 30\n" +
				"ansible_user: deploy\n"},
		WantChangeFree: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			scan := scanned(t, test.Files, "site.yml").DryRunScan()
			if diff := cmp.Diff(test.WantChangeFree, scan.ChangeFree()); diff != "" {
				t.Errorf("ChangeFree() mismatch (-want +got):\n%s\nfindings=%q\nunread=%q", diff,
					scan.Findings, scan.Unread)
			}
			if test.WantFinding != "" && !strings.Contains(strings.Join(scan.Findings, "\n"), test.WantFinding) {
				t.Errorf("findings %q, want one naming %q", scan.Findings, test.WantFinding)
			}
			if test.WantUnread != "" && !strings.Contains(strings.Join(scan.Unread, "\n"), test.WantUnread) {
				t.Errorf("unread %q, want one naming %q", scan.Unread, test.WantUnread)
			}
		})
	}
}

// TestTheScanDeniesWhatTheTreeLoads covers what Ansible loads from the tree without any playbook
// naming it. A plugin directory, a module directory, an ansible.cfg, or a requirements file that
// installs collections can bring code onto the controller that no task names, so wherever one sits
// the dry run is not change free.
func TestTheScanDeniesWhatTheTreeLoads(t *testing.T) {
	t.Parallel()
	const play = checkPlay + "    - ansible.builtin.ping:\n"
	tests := []struct {
		Files          map[string]string
		WantChangeFree bool
		WantText       string
	}{{ // Test 0: A filter plugin directory beside the playbook.
		Files:    map[string]string{"filter_plugins/evil.py": "x = 1\n"},
		WantText: "filter_plugins: a filter_plugins directory",
	}, { // Test 1: A lookup plugin directory inside a role.
		Files:    map[string]string{"roles/web/lookup_plugins/evil.py": "x = 1\n"},
		WantText: "roles/web/lookup_plugins",
	}, { // Test 2: A module directory.
		Files:    map[string]string{"library/mod.py": "x = 1\n"},
		WantText: "library",
	}, { // Test 3: A module_utils directory.
		Files:    map[string]string{"module_utils/util.py": "x = 1\n"},
		WantText: "module_utils",
	}, { // Test 4: An ansible.cfg in a subdirectory.
		Files:    map[string]string{"deploy/ansible.cfg": "[defaults]\n"},
		WantText: "deploy/ansible.cfg",
	}, { // Test 5: A collections requirements file.
		Files:    map[string]string{"collections/requirements.yml": "collections:\n  - a.b\n"},
		WantText: "collections/requirements.yml",
	}, { // Test 6: A root requirements file asking for collections.
		Files:    map[string]string{"requirements.yml": "collections:\n  - a.b\n"},
		WantText: "requirements.yml: a requirements file that installs collections",
	}, { // Test 7: A requirements file asking for roles alone, and plain files, change nothing.
		Files: map[string]string{"requirements.yml": "- src: geerlingguy.nginx\n",
			"docs/plugins.md": "notes\n", "files/library.txt": "a file, not a directory\n"},
		WantChangeFree: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			files := map[string]string{"site.yml": play}
			for name, body := range test.Files {
				files[name] = body
			}
			scan := scanned(t, files, "site.yml").DryRunScan()
			if diff := cmp.Diff(test.WantChangeFree, scan.ChangeFree()); diff != "" {
				t.Errorf("ChangeFree() mismatch (-want +got):\n%s\nfindings=%q", diff, scan.Findings)
			}
			if !strings.Contains(strings.Join(scan.Findings, "\n"), test.WantText) {
				t.Errorf("Findings = %q, want one naming %q", scan.Findings, test.WantText)
			}
		})
	}
}
