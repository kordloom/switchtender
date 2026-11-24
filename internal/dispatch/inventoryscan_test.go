package dispatch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/inventorytest"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// TestADryRunsProjectInventoryIsRead pins that the gate reads the inventory a project dry run
// names, at the commit the run executes: an inventory file the commit stores as executable is a
// program Ansible runs on the controller, so the dry run is held, and the same bytes stored as a
// plain file are read as data and leave it exempt. The commit's own mode decides, which is why the
// commit filesystem reports the executable bit.
func TestADryRunsProjectInventoryIsRead(t *testing.T) {
	t.Parallel()
	const bytes = "[web]\nweb01\n"
	tests := []struct {
		// Inventory is the inventory file's content.
		Inventory string
		// Executable stores the file in the commit as executable.
		Executable bool
		// WantHeld is whether the dry run is held.
		WantHeld bool
		// WantNote is a fragment the hold note holds.
		WantNote string
	}{{ // Test 0: Mode 100755 is a program.
		Inventory: bytes, Executable: true, WantHeld: true,
		WantNote: `the inventory file "inv/hosts" is executable`,
	}, { // Test 1: The same bytes at 100644 are data.
		Inventory: bytes,
	}, { // Test 2: A host line with ssh arguments.
		Inventory: "web01 ansible_ssh_common_args='-o ProxyCommand=nc'\n", WantHeld: true,
		WantNote: "remove each ansible_ setting it names",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			repo := newTreeRepo(t, map[string]string{"site.yml": cleanPlaybook,
				"inv/hosts": test.Inventory})
			if test.Executable {
				gitIn(t, repo, "update-index", "--chmod=+x", "inv/hosts")
				gitIn(t, repo, "commit", "-m", "make the inventory a script")
			}
			syncer, err := project.NewSyncer(t.TempDir())
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			projects := project.NewMemStore()
			p := &project.Project{ID: "proj_inv", Name: "infra", RepoURL: repo, Branch: "main"}
			if err := projects.Save(ctx, p); err != nil {
				t.Fatalf("Save(project) error = %v", err)
			}
			d := New(run.NewMemStore(), dispatchtest.OKRunner(), zap.NewNop(),
				WithProjects(projects, syncer), WithPolicies(excludeDryRuns(t)), WithNoJanitor())
			t.Cleanup(d.Close)
			got, err := d.Submit(ctx, "site.yml", "inv/hosts", run.WithProject(p.ID),
				run.WithTool(run.ToolAnsible), run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			checkHeld(t, got, test.WantHeld, test.WantNote)
		})
	}
}

// TestADryRunsStoredInventoryIsRead pins the stored inventory sources of the pass: the snapshot the
// run executes is read, a plain inventory, a dynamic source, and a composed inventory's result
// alike, and what it sets decides the dry run's exemption the way the playbook does. Nothing is
// refused at save, so an imported inventory with an interpreter still lands and is held only when
// a rule exempting dry runs meets it.
//
//nolint:funlen // Test table.
func TestADryRunsStoredInventoryIsRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Inventory is the stored inventory the run targets.
		Inventory *inventory.Inventory
		// Input replaces the content of the composed inventory's input inv_a, when set.
		Input string
		// WantHeld is whether the dry run is held.
		WantHeld bool
		// WantNote is a fragment the hold note holds.
		WantNote string
	}{{ // Test 0: Bastion arguments on every host, under a rule exempting dry runs.
		Inventory: &inventory.Inventory{ID: "inv_x", Name: "bastioned",
			Content: "[all:vars]\nansible_ssh_common_args=-o ProxyCommand=nc\n[web]\nweb01\n"},
		WantHeld: true, WantNote: `the stored inventory "bastioned": group "all" sets ` +
			`"ansible_ssh_common_args"`,
	}, { // Test 1: Its clean twin starts unheld.
		Inventory: &inventory.Inventory{ID: "inv_x", Name: "bastioned",
			Content: "[web]\nweb01 ansible_user=deploy\n"},
	}, { // Test 2: A dynamic source is unread, so the dry run is held.
		Inventory: &inventory.Inventory{ID: "inv_x", Name: "cloud",
			Content: "plugin: amazon.aws.aws_ec2\nregions: [us-east-1]\n"},
		WantHeld: true, WantNote: "stored inventory that syncs from a source",
	}, { // Test 3: A composed inventory is read as the result it composed.
		Inventory: &inventory.Inventory{ID: "inv_x", Name: "web everywhere", OrgID: "org_x",
			Kind: inventory.KindSmart, HostFilter: "groups__name=web"},
		Input: inventorytest.Listing(map[string][]string{"web": {"web1"}},
			map[string]map[string]any{"web1": {"ansible_python_interpreter": "/tmp/x"}}),
		WantHeld: true, WantNote: `sets "ansible_python_interpreter"`,
	}, { // Test 4: Its clean twin starts unheld.
		Inventory: &inventory.Inventory{ID: "inv_x", Name: "web everywhere", OrgID: "org_x",
			Kind: inventory.KindSmart, HostFilter: "groups__name=web"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			invs := composeInventories(t)
			if test.Input != "" {
				in, err := invs.Get(ctx, "inv_a")
				if err != nil {
					t.Fatalf("Get(inv_a) error = %v", err)
				}
				in.Content = test.Input
				if err := invs.Save(ctx, in); err != nil {
					t.Fatalf("Save(inv_a) error = %v", err)
				}
			}
			if err := invs.Save(ctx, test.Inventory); err != nil {
				t.Fatalf("Save(inventory) error = %v", err)
			}
			projects, from := heldFrom(t)
			d := New(run.NewMemStore(), dispatchtest.OKRunner(), zap.NewNop(), projects,
				WithInventories(invs), WithPolicies(excludeDryRuns(t)), WithRunFilesRoot(t.TempDir()),
				WithNoJanitor())
			t.Cleanup(d.Close)
			got, err := d.Submit(ctx, "site.yml", "", from, run.WithInventory("inv_x"),
				run.WithTool(run.ToolAnsible), run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			checkHeld(t, got, test.WantHeld, test.WantNote)
		})
	}
}

// TestAServerPathInventoryIsRead pins the server path source of the pass: an inventory a dry run
// names by a path on this server is read in place, with the group_vars beside it, and a link is
// reported as one rather than followed.
func TestAServerPathInventoryIsRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Files is the directory the inventory sits in.
		Files map[string]string
		// Link makes hosts.ini a link to real.ini.
		Link bool
		// WantFinding is a fragment one entry holds, empty for a change-free dry run.
		WantFinding string
	}{{ // Test 0: A clean inventory.
		Files: map[string]string{"hosts.ini": "[web]\nweb01\n"},
	}, { // Test 1: group_vars beside it naming an interpreter.
		Files: map[string]string{"hosts.ini": "[web]\nweb01\n",
			"group_vars/web.yml": "ansible_python_interpreter: /tmp/x\n"},
		WantFinding: `sets "ansible_python_interpreter"`,
	}, { // Test 2: A link to a clean inventory.
		Files: map[string]string{"real.ini": "[web]\nweb01\n"}, Link: true,
		WantFinding: "is a symbolic link",
	}, { // Test 3: A file the server does not hold.
		Files: map[string]string{"other.ini": "[web]\nweb01\n"}, WantFinding: "not in",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			// The playbook sits apart from the inventory, so the playbook scan's walk of its own
			// directory does not reach the variables beside the inventory.
			plays, dir := t.TempDir(), t.TempDir()
			writeTree(t, dir, test.Files)
			writeTree(t, plays, map[string]string{"site.yml": cleanPlaybook})
			if test.Link {
				if err := os.Symlink("real.ini", filepath.Join(dir, "hosts.ini")); err != nil {
					t.Fatalf("Symlink() error = %v", err)
				}
			}
			r := &run.Run{ID: "run_path", Tool: run.ToolAnsible, DryRun: true,
				Playbook: filepath.Join(plays, "site.yml"), Inventory: filepath.Join(dir, "hosts.ini")}
			scans := gradedLocally(r).DryRunScans
			if len(scans) != 1 {
				t.Fatalf("scans = %+v, want one", scans)
			}
			entries := strings.Join(scans[0].Entries(), "\n")
			if test.WantFinding == "" {
				if !scans[0].ChangeFree() {
					t.Errorf("a clean server path inventory was not change free: %s", entries)
				}
				return
			}
			if scans[0].ChangeFree() || !strings.Contains(entries, test.WantFinding) {
				t.Errorf("entries = %q, want one holding %q", entries, test.WantFinding)
			}
		})
	}
}

// checkHeld checks that got is held, or not, and that a held run's hold note holds wantNote.
func checkHeld(t *testing.T, got *run.Run, wantHeld bool, wantNote string) {
	t.Helper()
	held := got.Status == run.StatusPendingApproval
	if held != wantHeld {
		t.Fatalf("held = %t, want %t. Recorded: %q", held, wantHeld, got.DryRunFindings())
	}
	if wantHeld && !strings.Contains(got.HoldNote, wantNote) {
		t.Errorf("hold note %q does not say %q", got.HoldNote, wantNote)
	}
}
