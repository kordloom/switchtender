package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatchtest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// heldFrom wires a project over a local repository into a test dispatcher, for a test whose run is
// held and so must come from a project. The repository commits a placeholder playbook under each
// of names, or site.yml alone when none is named. It returns the option that wires the project and
// the option a submission names it with.
func heldFrom(t *testing.T, names ...string) (Option, run.SubmitOption) {
	t.Helper()
	var files map[string]string
	if len(names) > 0 {
		files = map[string]string{}
		for _, name := range names {
			files[name] = dispatchtest.HeldPlaybook
		}
	}
	p := dispatchtest.LocalProject(t, files)
	return WithProjects(p.Store, p.Syncer), run.WithProject(p.ID)
}

// holdAnsible returns a policy store holding every Ansible run.
func holdAnsible(t *testing.T) policy.Store {
	t.Helper()
	return dispatchtest.RulesHolding(t, &policy.Policy{
		ID: policy.NewID(), Name: "all-ansible", Tool: run.ToolAnsible,
		MaxDestroy: policy.DisabledMaxDestroy,
	})
}

// TestAHeldRunNamedByPathIsRefusedAndFromAProjectIsPinned walks every submit path that can hold a
// run. Named by a path on the server, the run is refused with the rule and the fix and nothing is
// stored, since an approval binds no file there. The same run drawn from a project is held with the
// project's commit pinned, which is what the approval then releases.
func TestAHeldRunNamedByPathIsRefusedAndFromAProjectIsPinned(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which submit path holds the run.
		Name string
		// Submit holds a run through one path, with from added to every submission it makes.
		Submit func(ctx context.Context, t *testing.T, d *Dispatcher, store run.Store,
			from ...run.SubmitOption) (*run.Run, error)
		// WantStored is how many top-level runs the store holds after a refusal: the run a retry
		// retries, and nothing the refused submission made.
		WantStored int
	}{{ // Test 0: An agent's single run, held by the built-in agent hold.
		Name: "an agent's run",
		Submit: func(ctx context.Context, _ *testing.T, d *Dispatcher, _ run.Store,
			from ...run.SubmitOption) (*run.Run, error) {
			return d.Submit(ctx, "site.yml", "", append(dispatchtest.AgentOpts(), from...)...)
		},
	}, { // Test 1: A split a rule holds.
		Name: "a held split",
		Submit: func(ctx context.Context, _ *testing.T, d *Dispatcher, _ run.Store,
			from ...run.SubmitOption) (*run.Run, error) {
			return d.SubmitSplit(ctx, "site.yml", "hosts.ini", 2,
				append([]run.SubmitOption{run.WithRequireApproval(true)}, from...)...)
		},
	}, { // Test 2: A pipeline a rule holds.
		Name: "a held pipeline",
		Submit: func(ctx context.Context, _ *testing.T, d *Dispatcher, _ run.Store,
			from ...run.SubmitOption) (*run.Run, error) {
			return d.SubmitPipeline(ctx, "release", "",
				[]run.PipelineStep{{Name: "deploy", Playbook: "site.yml"}},
				append([]run.SubmitOption{run.WithRequireApproval(true)}, from...)...)
		},
	}, { // Test 3: A retry of a failed split, which the agent asking for it holds.
		Name: "a retry",
		Submit: func(ctx context.Context, t *testing.T, d *Dispatcher, store run.Store,
			from ...run.SubmitOption) (*run.Run, error) {
			parent, err := d.SubmitSplit(ctx, "site.yml", "hosts.ini", 2, from...)
			if err != nil {
				t.Fatalf("SubmitSplit() error = %v", err)
			}
			dispatchtest.WaitForStatus(t, store, parent.ID, run.StatusFailed)
			return d.RetryFailedShards(ctx, parent.ID, dispatchtest.AgentOpts()...)
		},
		WantStored: 1,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s by path", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := New(store, &failingLister{hosts: []string{"web01", "web02"}}, zap.NewNop(),
				WithNoJanitor())
			defer d.Close()

			got, err := test.Submit(ctx, t, d, store)

			if !errors.Is(err, ErrHeldByPath) {
				t.Fatalf("submit error = %v, want %v", err, ErrHeldByPath)
			}
			if !strings.Contains(err.Error(), "put it in a project") {
				t.Errorf("error = %q, want it to say to put the playbook in a project", err)
			}
			if got != nil {
				t.Errorf("submit returned run %s, want none", got.ID)
			}
			stored, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(stored) != test.WantStored {
				t.Errorf("the store holds %d runs after the refusal, want %d", len(stored),
					test.WantStored)
			}
		})
		t.Run(fmt.Sprintf("test %d %s from a project", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			p := dispatchtest.LocalProject(t, map[string]string{
				"site.yml": dispatchtest.HeldPlaybook, "hosts.ini": "web01\nweb02\n"})
			store := run.NewMemStore()
			d := New(store, &failingLister{hosts: []string{"web01", "web02"}}, zap.NewNop(),
				WithNoJanitor(), WithProjects(p.Store, p.Syncer))
			defer d.Close()

			held, err := test.Submit(ctx, t, d, store, run.WithProject(p.ID))

			if err != nil {
				t.Fatalf("submit error = %v", err)
			}
			if held.Status != run.StatusPendingApproval {
				t.Fatalf("status = %q, want pending_approval", held.Status)
			}
			stored, err := store.Get(ctx, held.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if stored.PinnedCommit != p.Head {
				t.Errorf("stored PinnedCommit = %q, want the commit the project held, %q",
					stored.PinnedCommit, p.Head)
			}
		})
	}
}

// TestAPersonsPathNamedRunNoRuleHoldsStarts pins the other side of the rule: a run nothing holds
// is unchanged, so a person's first run by path still starts and finishes.
func TestAPersonsPathNamedRunNoRuleHoldsStarts(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, dispatchtest.OKRunner(), zap.NewNop(), WithNoJanitor(),
		WithPolicies(policy.NewMemStore()))
	defer d.Close()

	r, err := d.Submit(ctx, "/srv/playbooks/site.yml", "/srv/playbooks/hosts.ini",
		dispatchtest.PersonOpts()...)

	if err != nil {
		t.Fatalf("Submit() error = %v, want the run to start", err)
	}
	if got := dispatchtest.WaitTerminal(t, store, r.ID); got.Status != run.StatusSucceeded {
		t.Errorf("status = %q (%s), want succeeded", got.Status, got.Error)
	}
}

// TestRefuseHeldPathCoversWhatAnApprovalCannotBind walks the rule's reach: what is refused when an
// approval stands in front of it with no project, and what is not.
func TestRefuseHeldPathCoversWhatAnApprovalCannotBind(t *testing.T) {
	t.Parallel()
	held := run.StatusPendingApproval
	approvalStep := run.PipelineStep{Name: "gate", Type: run.StepApproval}

	tests := []struct {
		// Run is the run offered to the rule.
		Run *run.Run
		// Want is the error the rule returns, nil when the run passes.
		Want error
	}{{ // Test 0: A held playbook named by path.
		Run:  &run.Run{Status: held, Playbook: "site.yml"},
		Want: ErrHeldByPath,
	}, { // Test 1: A held playbook from a project.
		Run: &run.Run{Status: held, Playbook: "site.yml", ProjectID: "proj_1"},
	}, { // Test 2: A playbook by path nothing holds.
		Run: &run.Run{Status: run.StatusPending, Playbook: "site.yml"},
	}, { // Test 3: A held Terraform root named by path, which plans when it is released.
		Run:  &run.Run{Status: held, Tool: run.ToolTerraform, Command: "infra/prod"},
		Want: ErrHeldByPath,
	}, { // Test 4: A held OpenTofu root named by path.
		Run:  &run.Run{Status: held, Tool: run.ToolOpenTofu, Command: "infra/prod"},
		Want: ErrHeldByPath,
	}, { // Test 5: A held apply carrying a sealed plan, which holds its configuration.
		Run: &run.Run{Status: held, Tool: run.ToolTerraform, Command: "infra/prod",
			PlanSHA256: "abc123"},
	}, { // Test 6: A held bash command, whose program the spec seals.
		Run: &run.Run{Status: held, Tool: run.ToolBash, Command: "./deploy.sh"},
	}, { // Test 7: A held python script.
		Run: &run.Run{Status: held, Tool: run.ToolPython, Command: "print(1)"},
	}, { // Test 8: A workflow nothing holds, with an approval step ahead of a playbook by path.
		Run: &run.Run{Status: run.StatusPending, Kind: run.KindPipeline, Steps: []run.PipelineStep{
			approvalStep, {Name: "deploy", Playbook: "site.yml", DependsOn: []string{"gate"}}}},
		Want: ErrHeldByPath,
	}, { // Test 9: The same workflow from a project.
		Run: &run.Run{Status: run.StatusPending, Kind: run.KindPipeline, ProjectID: "proj_1",
			Steps: []run.PipelineStep{approvalStep,
				{Name: "deploy", Playbook: "site.yml", DependsOn: []string{"gate"}}}},
	}, { // Test 10: A workflow with an approval step and only script steps.
		Run: &run.Run{Status: run.StatusPending, Kind: run.KindPipeline, Steps: []run.PipelineStep{
			approvalStep, {Name: "notify", Tool: run.ToolBash, Command: "echo shipped",
				DependsOn: []string{"gate"}}}},
	}, { // Test 11: A workflow with no approval step that nothing holds.
		Run: &run.Run{Status: run.StatusPending, Kind: run.KindPipeline, Steps: []run.PipelineStep{
			{Name: "deploy", Playbook: "site.yml"}}},
	}, { // Test 12: A held workflow with a Terraform step named by path.
		Run: &run.Run{Status: held, Kind: run.KindPipeline, Steps: []run.PipelineStep{
			{Name: "plan", Tool: run.ToolTerraform, Command: "infra", DryRun: true}}},
		Want: ErrHeldByPath,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := refuseHeldPath(test.Run); !errors.Is(err, test.Want) ||
				(test.Want == nil && err != nil) {
				t.Errorf("refuseHeldPath() = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestAHeldTerraformRunWithNoProjectIsRefused covers Terraform through the dispatcher. An agent's
// apply is held before anything plans, and its release would plan whatever the folder then holds,
// so named by path it is refused. An apply carrying a sealed plan binds its configuration through
// the plan, so it is held with no project.
func TestAHeldTerraformRunWithNoProjectIsRefused(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, dispatchtest.OKRunner(), zap.NewNop(), WithNoJanitor(),
		WithPolicies(policy.NewMemStore()))
	defer d.Close()

	_, err := d.Submit(ctx, "", "", append(dispatchtest.AgentOpts(),
		run.WithTool(run.ToolTerraform), run.WithCommand(t.TempDir()))...)
	if !errors.Is(err, ErrHeldByPath) {
		t.Errorf("an agent's apply by path: error = %v, want %v", err, ErrHeldByPath)
	}

	sealed, err := d.Submit(ctx, "", "", run.WithTool(run.ToolTerraform),
		run.WithCommand(t.TempDir()), run.WithPlanFile("sealed-plan"),
		run.WithRequireApproval(true))
	if err != nil {
		t.Fatalf("an apply carrying a sealed plan: error = %v, want it held", err)
	}
	if sealed.Status != run.StatusPendingApproval {
		t.Errorf("an apply carrying a sealed plan is %q, want pending_approval", sealed.Status)
	}
}

// TestAWorkflowWithAnApprovalStepIsPinnedAndNotHeld pins what an approval step does at
// submission. The workflow is not held, so it starts with the status it would have had, and it is
// pinned to the project's commit, so the person deciding the step releases the code the workflow
// started on. The same workflow naming its playbooks by path is refused.
func TestAWorkflowWithAnApprovalStepIsPinnedAndNotHeld(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	steps := []run.PipelineStep{
		{Name: "build", Playbook: "site.yml"},
		{Name: "gate", Type: run.StepApproval, DependsOn: []string{"build"}},
		{Name: "deploy", Playbook: "site.yml", DependsOn: []string{"gate"}},
	}
	p := dispatchtest.LocalProject(t, nil)
	store := run.NewMemStore()
	d := New(store, dispatchtest.OKRunner(), zap.NewNop(), WithNoJanitor(),
		WithPolicies(policy.NewMemStore()), WithProjects(p.Store, p.Syncer))
	defer d.Close()

	if _, err := d.SubmitPipeline(ctx, "release", "", steps); !errors.Is(err, ErrHeldByPath) {
		t.Errorf("by path: SubmitPipeline() error = %v, want %v", err, ErrHeldByPath)
	}

	wf, err := d.SubmitPipeline(ctx, "release", "", steps, run.WithProject(p.ID))
	if err != nil {
		t.Fatalf("from a project: SubmitPipeline() error = %v", err)
	}
	if wf.Status != run.StatusPending {
		t.Errorf("status = %q, want pending: an approval step does not hold its workflow",
			wf.Status)
	}
	if wf.HeldByPolicy != "" {
		t.Errorf("HeldByPolicy = %q, want none", wf.HeldByPolicy)
	}
	if wf.PinnedCommit != p.Head {
		t.Errorf("PinnedCommit = %q, want the commit the project held, %q", wf.PinnedCommit,
			p.Head)
	}
	parked := waitParked(t, store, wf.ID)
	if parked.StepName != "gate" {
		t.Errorf("the workflow waits at %q, want gate", parked.StepName)
	}
}

// TestAScheduleFireRefusedByThePathRuleRecordsItsLastError fires an inline schedule a rule holds
// by path. A fire has no actor, so only an admin's own rule reaches it. The fire starts no run, and
// the schedule records the rule and the fix as its last error, where the schedules page shows it.
func TestAScheduleFireRefusedByThePathRuleRecordsItsLastError(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	d := New(store, dispatchtest.OKRunner(), zap.NewNop(), WithNoJanitor(),
		WithPolicies(holdAnsible(t)))
	defer d.Close()
	schedules := schedule.NewMemStore()
	due := time.Now().Add(-time.Minute)
	sc := &schedule.Schedule{
		ID: schedule.NewID(), Name: "nightly", Cron: "0 2 * * *", Timezone: "UTC",
		Playbook: "/srv/playbooks/site.yml", Inventory: "/srv/playbooks/hosts.ini",
		Enabled: true, CreatedAt: due, NextRunAt: &due,
	}
	if err := schedules.Save(ctx, sc); err != nil {
		t.Fatalf("schedules.Save() error = %v", err)
	}
	scheduler := schedule.NewScheduler(schedules, d, zap.NewNop(),
		schedule.WithInterval(5*time.Millisecond))
	scheduler.Start()
	defer scheduler.Close()

	var fired *schedule.Schedule
	for deadline := time.Now().Add(dispatchtest.WaitBudget); ; time.Sleep(5 * time.Millisecond) {
		got, err := schedules.Get(ctx, sc.ID)
		if err != nil {
			t.Fatalf("schedules.Get() error = %v", err)
		}
		if got.LastRunAt != nil {
			fired = got
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the schedule never fired")
		}
	}

	if fired.LastRunID != "" {
		t.Errorf("LastRunID = %q, want none: the fire must start no run", fired.LastRunID)
	}
	if !strings.Contains(fired.LastError, ErrHeldByPath.Error()) ||
		!strings.Contains(fired.LastError, "put it in a project") {
		t.Errorf("LastError = %q, want the rule and the fix", fired.LastError)
	}
	runs, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(runs) != 0 {
		t.Errorf("the store holds %d runs, want 0", len(runs))
	}
}
