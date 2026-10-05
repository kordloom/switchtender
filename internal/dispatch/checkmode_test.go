package dispatch

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// Playbooks the check_mode gate tests submit as dry runs.
const (
	// cleanPlaybook changes nothing under --check.
	cleanPlaybook = "- hosts: all\n  tasks:\n    - name: Look\n      ansible.builtin.ping:\n"
	// forcedTaskPlaybook has one task Ansible runs for real under --check.
	forcedTaskPlaybook = "- hosts: all\n  tasks:\n    - name: Restart web\n" +
		"      ansible.builtin.service: name=web state=restarted\n      check_mode: false\n"
)

// excludeDryRuns returns a policy store holding one rule that holds every Ansible run except a dry
// run, which is the rule the bypass walked through.
func excludeDryRuns(t *testing.T) policy.Store {
	t.Helper()
	rules := policy.NewMemStore()
	if err := rules.Save(context.Background(), &policy.Policy{
		ID: "pol_prod", Name: "prod ansible", Tool: run.ToolAnsible, ExcludeDryRun: true,
		Effect: policy.EffectRequireApproval, MaxDestroy: policy.DisabledMaxDestroy,
	}); err != nil {
		t.Fatalf("Save(policy) error = %v", err)
	}
	return rules
}

// writeFiles writes files under dir, creating the directories they sit in.
func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		full := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}

// specRecorder is a runner that records the specs it executes.
type specRecorder struct {
	// mu guards specs.
	mu sync.Mutex
	// specs are the specs executed, in order.
	specs []roundhouse.Spec
}

// Run records spec and succeeds.
func (s *specRecorder) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.specs = append(s.specs, spec)
	return roundhouse.Result{ExitCode: 0}, nil
}

// executed returns a copy of the specs run so far.
func (s *specRecorder) executed() []roundhouse.Spec {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]roundhouse.Spec(nil), s.specs...)
}

// TestADryRunThatForcesRealTasksIsHeld drives the bypass through the dispatcher, the path every
// submission takes.
//
// A rule that excluded dry runs exempted any run submitted with the dry-run flag, and Ansible runs
// a play, block, task, role, or include that sets check_mode to false for real under --check. So a
// dry run of such a playbook changed hosts while the rule waved it through. Each place the keyword
// can sit is driven here, beside the clean dry run the rule must still exempt.
//
//nolint:funlen // Test function.
func TestADryRunThatForcesRealTasksIsHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Files      map[string]string
		WantHeld   bool
		WantForced string
	}{{ // Test 0: A clean dry run is still exempt.
		Files: map[string]string{"site.yml": cleanPlaybook}, WantHeld: false,
	}, { // Test 1: A task-level check_mode false is held, naming the file and the task.
		Files: map[string]string{"site.yml": forcedTaskPlaybook}, WantHeld: true,
		WantForced: `site.yml: task "Restart web" sets check_mode to false`,
	}, { // Test 2: A play-level check_mode false.
		Files: map[string]string{"site.yml": "- name: Deploy\n  hosts: all\n  check_mode: false\n" +
			"  tasks:\n    - ansible.builtin.ping:\n"},
		WantHeld: true, WantForced: `site.yml: play "Deploy" sets check_mode to false`,
	}, { // Test 3: A block-level check_mode no.
		Files: map[string]string{"site.yml": "- hosts: all\n  tasks:\n    - name: Migrate\n" +
			"      check_mode: no\n      block:\n        - ansible.builtin.command: /bin/migrate\n"},
		WantHeld: true, WantForced: `site.yml: block "Migrate" sets check_mode to "no"`,
	}, { // Test 4: A role that sets it on its own task.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  roles:\n    - web\n",
			"roles/web/tasks/main.yml": "- name: Reload\n  ansible.builtin.command: /bin/reload\n" +
				"  check_mode: false\n",
		},
		WantHeld: true, WantForced: `roles/web/tasks/main.yml: task "Reload" sets check_mode to false`,
	}, { // Test 5: A role entry that sets it for the whole role.
		Files: map[string]string{
			"site.yml":                 "- hosts: all\n  roles:\n    - role: web\n      check_mode: no\n",
			"roles/web/tasks/main.yml": "- ansible.builtin.ping:\n",
		},
		WantHeld: true, WantForced: `site.yml: role "web" sets check_mode to "no"`,
	}, { // Test 6: A task in an included file.
		Files: map[string]string{
			"site.yml": "- hosts: all\n  tasks:\n    - ansible.builtin.include_tasks: more.yml\n",
			"more.yml": "- name: Write\n  ansible.builtin.copy: dest=/etc/x content=y\n" +
				"  check_mode: false\n",
		},
		WantHeld: true, WantForced: `more.yml: task "Write" sets check_mode to false`,
	}, { // Test 7: A templated value, decided only after the gate passed the run.
		Files: map[string]string{"site.yml": "- hosts: all\n  tasks:\n    - name: Maybe\n" +
			"      ansible.builtin.ping:\n      check_mode: \"{{ live | bool }}\"\n"},
		WantHeld: true, WantForced: `site.yml: task "Maybe" sets check_mode to "{{ live | bool }}"`,
	}, { // Test 8: check_mode true forces nothing, so the dry run stays exempt.
		Files: map[string]string{"site.yml": "- hosts: all\n  check_mode: true\n  tasks:\n" +
			"    - name: Look\n      ansible.builtin.ping:\n      check_mode: yes\n"},
		WantHeld: false,
	}, { // Test 9: A role the scan cannot find fails closed.
		Files:    map[string]string{"site.yml": "- hosts: all\n  roles:\n    - from.galaxy.only\n"},
		WantHeld: true, WantForced: `could not read role "from.galaxy.only"`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			writeFiles(t, dir, test.Files)
			store := run.NewMemStore()
			d := New(store, okRunner(), zap.NewNop(), WithPolicies(excludeDryRuns(t)), WithNoJanitor())
			t.Cleanup(d.Close)

			got, err := d.Submit(ctx, filepath.Join(dir, "site.yml"), "hosts.ini",
				run.WithTool(run.ToolAnsible), run.WithDryRun(true))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held = %v, want %v. Recorded: %q", held, test.WantHeld,
					got.DryRunFindings())
			}
			stored, err := store.Get(ctx, got.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !stored.DryRun {
				t.Error("the stored run lost its dry-run flag: the gate must change the judgment, " +
					"never what executes")
			}
			if len(stored.DryRunScans) != 1 || stored.DryRunScans[0].Scanner != run.CheckModeScanner {
				t.Fatalf("scans = %+v, want the gate's one scan of the playbook recorded",
					stored.DryRunScans)
			}
			if !test.WantHeld {
				if len(stored.DryRunFindings()) != 0 || stored.HoldNote != "" {
					t.Errorf("a clean dry run recorded forcing work: %q %q", stored.DryRunFindings(),
						stored.HoldNote)
				}
				return
			}
			if stored.HeldByPolicy != "prod ansible" {
				t.Errorf("held by %q, want the rule that excludes dry runs", stored.HeldByPolicy)
			}
			if !strings.Contains(strings.Join(stored.DryRunFindings(), "\n"), test.WantForced) {
				t.Errorf("recorded %q, want an entry naming %q", stored.DryRunFindings(),
					test.WantForced)
			}
			// The hold names the two clean fixes, and the approver's notification says it.
			for _, want := range []string{test.WantForced, "Two clean fixes",
				`drop exclude_dry_run from "prod ansible"`} {
				if !strings.Contains(stored.HoldNote, want) {
					t.Errorf("hold note %q does not say %q", stored.HoldNote, want)
				}
			}
			if detail := heldDetail(stored); !strings.Contains(detail, stored.HoldNote) {
				t.Errorf("the held notification does not carry the hold note: %q", detail)
			}
		})
	}
}

// TestAnUnreadablePlaybookFailsClosed covers the dry run whose playbook the gate cannot read at
// all. Nothing proves it forces nothing, so it is held rather than exempted.
func TestAnUnreadablePlaybookFailsClosed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithPolicies(excludeDryRuns(t)),
		WithNoJanitor())
	t.Cleanup(d.Close)
	got, err := d.Submit(ctx, filepath.Join(t.TempDir(), "absent.yml"), "hosts.ini",
		run.WithTool(run.ToolAnsible), run.WithDryRun(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want pending_approval: a playbook the gate could not read was "+
			"exempted as a preview", got.Status)
	}
	if len(got.DryRunScans) != 1 || got.DryRunScans[0].Classification != run.DryRunIncomplete {
		t.Fatalf("scans = %+v, want one incomplete scan", got.DryRunScans)
	}
	if findings := got.DryRunFindings(); len(findings) != 1 ||
		!strings.HasPrefix(findings[0], "could not read playbook ") ||
		!strings.Contains(findings[0], "(not on this server)") {
		t.Errorf("findings = %q, want the unread playbook named with why", findings)
	}
}

// TestAnApprovedForcingDryRunStillRunsInCheckMode covers the other half of the fix. The gate treats
// the run as a change for matching, and the run that executes is the dry run that was asked for,
// still passing --check.
func TestAnApprovedForcingDryRunStillRunsInCheckMode(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"site.yml": forcedTaskPlaybook})
	store := run.NewMemStore()
	runner := &specRecorder{}
	d := New(store, runner, zap.NewNop(), WithPolicies(excludeDryRuns(t)), WithNoJanitor())
	t.Cleanup(d.Close)
	held, err := d.Submit(ctx, filepath.Join(dir, "site.yml"), "hosts.ini",
		run.WithTool(run.ToolAnsible), run.WithDryRun(true))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if held.Status != run.StatusPendingApproval {
		t.Fatalf("status = %q, want pending_approval", held.Status)
	}
	if _, err := d.Approve(ctx, held.ID, decider("approver", "session")); err != nil {
		t.Fatalf("Approve() error = %v", err)
	}
	final := waitTerminal(t, store, held.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("status = %q, want succeeded", final.Status)
	}
	specs := runner.executed()
	if len(specs) != 1 || !specs[0].DryRun {
		t.Errorf("executed %+v, want one run in check mode", specs)
	}
}

// TestAPipelineStepThatForcesRealTasksHoldsThePipeline covers the workflow door. A pipeline whose
// dry-run step forces real work is held as a whole, and the record names the step.
func TestAPipelineStepThatForcesRealTasksHoldsThePipeline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Step       string
		WantHeld   bool
		WantForced []string
	}{{ // Test 0: A clean dry-run step leaves the pipeline exempt.
		Step: cleanPlaybook, WantHeld: false,
	}, { // Test 1: A forcing dry-run step holds it and is named.
		Step: forcedTaskPlaybook, WantHeld: true,
		WantForced: []string{`step "check": site.yml: task "Restart web" sets check_mode to false`},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"site.yml": test.Step})
			d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithPolicies(excludeDryRuns(t)),
				WithNoJanitor())
			t.Cleanup(d.Close)
			got, err := d.SubmitPipeline(ctx, "nightly", "hosts.ini", []run.PipelineStep{{
				Name: "check", Playbook: filepath.Join(dir, "site.yml"), DryRun: true,
			}}, run.WithDryRun(true))
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held = %v, want %v. Recorded: %q", held, test.WantHeld,
					got.DryRunFindings())
			}
			if diff := cmp.Diff(test.WantForced, got.DryRunFindings(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("DryRunFindings() mismatch (-want +got):\n%s", diff)
			}
			if len(got.DryRunScans) != 1 || got.DryRunScans[0].Step != "check" {
				t.Errorf("scans = %+v, want the step's scan named by its step", got.DryRunScans)
			}
			if test.WantHeld && !strings.Contains(got.HoldNote, `step "check"`) {
				t.Errorf("hold note %q does not name the step that held the pipeline", got.HoldNote)
			}
		})
	}
}

// TestAProjectDryRunIsReadAtTheCommitItRuns covers a dry run drawn from a project, which is how a
// template or an agent proposes one. The gate fetches the project and reads the playbook there, so
// a task forcing real work pushed a moment before the launch is seen.
func TestAProjectDryRunIsReadAtTheCommitItRuns(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	repo := newTreeRepo(t, map[string]string{"site.yml": cleanPlaybook})
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	p := &project.Project{ID: "proj_dry", Name: "infra", RepoURL: repo, Branch: "main"}
	if err := projects.Save(ctx, p); err != nil {
		t.Fatalf("Save(project) error = %v", err)
	}
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithProjects(projects, syncer),
		WithPolicies(excludeDryRuns(t)), WithNoJanitor())
	t.Cleanup(d.Close)
	submit := func() *run.Run {
		t.Helper()
		r, err := d.Submit(ctx, "site.yml", "hosts.ini", run.WithProject(p.ID), run.WithDryRun(true))
		if err != nil {
			t.Fatalf("Submit() error = %v", err)
		}
		return r
	}
	// The first sync, so the checkout exists the way it does on any install that has run anything.
	wt, err := syncer.Sync(p, "")
	if err != nil {
		t.Fatalf("Sync() error = %v", err)
	}
	wt.Cleanup()
	if clean := submit(); clean.Status == run.StatusPendingApproval {
		t.Fatalf("a clean project dry run was held: %q", clean.DryRunFindings())
	}
	commitTree(t, repo, map[string]string{"site.yml": forcedTaskPlaybook})
	forced := submit()
	if forced.Status != run.StatusPendingApproval {
		t.Fatalf("a project dry run forcing real work, pushed before the launch, was exempt")
	}
	want := `site.yml: task "Restart web" sets check_mode to false`
	if diff := cmp.Diff([]string{want}, forced.DryRunFindings()); diff != "" {
		t.Errorf("DryRunFindings() mismatch (-want +got):\n%s", diff)
	}
	if src := forced.DryRunScans[0].Source; !strings.Contains(src, "read at commit ") {
		t.Errorf("scan source = %q, want the commit the gate read", src)
	}
}
