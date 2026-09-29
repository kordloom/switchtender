package run

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"
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
