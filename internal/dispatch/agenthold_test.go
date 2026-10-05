package dispatch

import (
	"context"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dossier"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// smokeExemptNote is what the evidence records about an agent run the smoke exemption let through.
const smokeExemptNote = `requested by an agent bound to account "dev-lead", exempt from the ` +
	`default hold by policy "nightly smoke"`

// agentOpts returns the options an agent token's request stamps on what it submits: the token's
// label, its kind, the account it is bound to by id and by name, and the identity evidence naming
// that account.
func agentOpts() []run.SubmitOption {
	return []run.SubmitOption{run.WithActor("release-agent"),
		run.WithActorType(policy.ActorKindAgent), run.WithActorAccount("usr_dev_lead"),
		run.WithAccount("dev-lead"),
		run.WithInitiator(&run.Initiator{InitiatedBy: "release-agent", BoundTo: "dev-lead"})}
}

// personOpts returns the options a signed-in person's request stamps on what it submits.
func personOpts() []run.SubmitOption {
	return []run.SubmitOption{run.WithActor("dev-lead"), run.WithActorType("session")}
}

// smokeExemption returns a policy store holding one exemption: agent bash runs naming smoke.
func smokeExemption(t *testing.T) policy.Store {
	t.Helper()
	return rulesHolding(t, &policy.Policy{
		ID: "pol_smoke", Name: "nightly smoke", Tool: run.ToolBash, CommandContains: "smoke",
		Effect: policy.EffectExempt, MaxDestroy: policy.DisabledMaxDestroy,
	})
}

// newAgentHoldDispatcher returns a dispatcher over store, with rules as its policy store when it
// is not nil and with no policy store at all when it is.
func newAgentHoldDispatcher(t *testing.T, store run.Store, rules policy.Store) *Dispatcher {
	t.Helper()
	opts := []Option{WithNoJanitor()}
	if rules != nil {
		opts = append(opts, WithPolicies(rules))
	}
	d := New(store, okRunner(), zap.NewNop(), opts...)
	t.Cleanup(d.Close)
	return d
}

// TestAnAgentsRunIsHeldWithNoPolicyWritten drives the built-in hold through Submit, the path every
// direct submission takes. An agent's run waits for a person on an install that has written no
// policy, and on one with no policy store at all, while a person's identical run goes ahead and an
// exemption lets the agent runs it names go ahead with the exemption recorded.
//
//nolint:funlen // Test function.
func TestAnAgentsRunIsHeldWithNoPolicyWritten(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Rules builds the policy store, nil for an install with none.
		Rules func(t *testing.T) policy.Store
		// Opts say who submits.
		Opts []run.SubmitOption
		// Command is the bash command submitted.
		Command string
		// WantHeld is whether the run waits for a person.
		WantHeld bool
		// WantNotes are the notes the run carries.
		WantNotes []string
	}{{ // Test 0: An agent's run with no policy store at all is held by default.
		Opts: agentOpts(), Command: "systemctl restart web", WantHeld: true,
		WantNotes: []string{policy.AgentDefaultName},
	}, { // Test 1: A person's identical run goes ahead, the control for test 0.
		Opts: personOpts(), Command: "systemctl restart web", WantHeld: false,
	}, { // Test 2: An empty policy store holds the agent's run the same way.
		Rules: func(*testing.T) policy.Store { return policy.NewMemStore() },
		Opts:  agentOpts(), Command: "systemctl restart web", WantHeld: true,
		WantNotes: []string{policy.AgentDefaultName},
	}, { // Test 3: An exemption lets the agent's run it covers go ahead, named in the evidence.
		Rules: smokeExemption, Opts: agentOpts(), Command: "./run smoke tests", WantHeld: false,
		WantNotes: []string{smokeExemptNote},
	}, { // Test 4: The exemption covers nothing it does not name.
		Rules: smokeExemption, Opts: agentOpts(), Command: "rm -rf /var/backups", WantHeld: true,
		WantNotes: []string{policy.AgentDefaultName},
	}, { // Test 5: The exemption never touches a person's run.
		Rules: smokeExemption, Opts: personOpts(), Command: "./run smoke tests", WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			var rules policy.Store
			if test.Rules != nil {
				rules = test.Rules(t)
			}
			d := newAgentHoldDispatcher(t, store, rules)
			opts := append([]run.SubmitOption{run.WithTool(run.ToolBash),
				run.WithCommand(test.Command)}, test.Opts...)
			got, err := d.Submit(ctx, "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held = %v, want %v (held by %q)", held, test.WantHeld, got.HeldByPolicy)
			}
			stored, err := store.Get(ctx, got.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if diff := cmp.Diff(test.WantNotes, stored.PolicyNotes, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("policy notes mismatch (-want +got):\n%s", diff)
			}
			if test.WantHeld {
				if stored.HeldByPolicy != policy.AgentDefaultName {
					t.Errorf("held by %q, want %q", stored.HeldByPolicy, policy.AgentDefaultName)
				}
				return
			}
			if final := waitTerminal(t, store, got.ID); final.Status != run.StatusSucceeded {
				t.Errorf("status = %q, want the run to have gone ahead and succeeded", final.Status)
			}
		})
	}
}

// TestTheEvidenceSaysWhyAnAgentsRunWaitedOrWent holds the receipt's source, the outcome record the
// chain commits, and the dossier to saying why an agent's run waited for a person, or which
// exemption let it go ahead. A person's run records neither, which is the control.
func TestTheEvidenceSaysWhyAnAgentsRunWaitedOrWent(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Opts say who submits.
		Opts []run.SubmitOption
		// Command is the bash command submitted.
		Command string
		// WantNotes are the notes the committed outcome record carries.
		WantNotes []string
	}{{ // Test 0: A held and then approved agent run says it was held by default.
		Opts: agentOpts(), Command: "systemctl restart web",
		WantNotes: []string{policy.AgentDefaultName},
	}, { // Test 1: An exempt agent run names its exemption.
		Opts: agentOpts(), Command: "./run smoke tests", WantNotes: []string{smokeExemptNote},
	}, { // Test 2: A person's run records nothing about the agent hold.
		Opts: personOpts(), Command: "systemctl restart web",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := newAgentHoldDispatcher(t, store, smokeExemption(t))
			opts := append([]run.SubmitOption{run.WithTool(run.ToolBash),
				run.WithCommand(test.Command)}, test.Opts...)
			got, err := d.Submit(ctx, "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if got.Status == run.StatusPendingApproval {
				page, err := dossier.Render(&dossier.Input{Run: got})
				if err != nil {
					t.Fatalf("dossier.Render() error = %v", err)
				}
				if !strings.Contains(string(page), policy.AgentDefaultName) {
					t.Errorf("the dossier of a held agent run does not say %q",
						policy.AgentDefaultName)
				}
				if _, err := d.Approve(ctx, got.ID, decider("approver", "session")); err != nil {
					t.Fatalf("Approve() error = %v", err)
				}
			}
			final := waitTerminal(t, store, got.ID)
			if final.Status != run.StatusSucceeded {
				t.Fatalf("status = %q, want succeeded", final.Status)
			}
			body, err := outcome.Body(ctx, store, final)
			if err != nil {
				t.Fatalf("outcome.Body() error = %v", err)
			}
			rec, err := outcome.Parse(body)
			if err != nil {
				t.Fatalf("outcome.Parse() error = %v", err)
			}
			if diff := cmp.Diff(test.WantNotes, rec.PolicyNotes, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("outcome policy notes mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestAnAgentsDryRunIsHeldUnlessShownChangeFree holds the built-in hold to the reading of a dry run
// exclude_dry_run uses. A dry run the gate's scan shows changes nothing goes ahead, and one whose
// playbook forces real work under check mode, or whose configuration runs a program while it plans,
// is the change it may be and waits, with a note naming what was found and the two fixes.
//
//nolint:funlen // Test function.
func TestAnAgentsDryRunIsHeldUnlessShownChangeFree(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Tool is the tool of the dry run.
		Tool string
		// Files are the playbook or configuration the dry run reads.
		Files map[string]string
		// Rules builds the policy store, nil for an install with none.
		Rules func(t *testing.T) policy.Store
		// Opts say who submits.
		Opts []run.SubmitOption
		// WantHeld is whether the dry run waits for a person.
		WantHeld bool
		// WantNoteParts are what the hold note must say.
		WantNoteParts []string
	}{{ // Test 0: A clean playbook's dry run goes ahead.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": cleanPlaybook},
		Opts: agentOpts(), WantHeld: false,
	}, { // Test 1: A playbook that forces a task under check mode is held.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": forcedTaskPlaybook},
		Opts: agentOpts(), WantHeld: true,
		WantNoteParts: []string{`site.yml: task "Restart web" sets check_mode to false`,
			"the default hold on an agent's run applies",
			"write a policy with effect exempt that covers this run"},
	}, { // Test 2: The same dry run from a person is not, the control for test 1.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": forcedTaskPlaybook},
		Opts: personOpts(), WantHeld: false,
	}, { // Test 3: An exemption covering the agent's forcing dry run lets it go ahead.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": forcedTaskPlaybook},
		Rules: func(t *testing.T) policy.Store {
			return rulesHolding(t, &policy.Policy{ID: "pol_ans", Name: "ansible checks",
				Tool: run.ToolAnsible, Effect: policy.EffectExempt,
				MaxDestroy: policy.DisabledMaxDestroy})
		},
		Opts: agentOpts(), WantHeld: false,
	}, { // Test 4: A clean configuration's plan goes ahead.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": cleanConfig},
		Opts: agentOpts(), WantHeld: false,
	}, { // Test 5: A configuration with an external data source runs a program, so it is held.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": externalConfig},
		Opts: agentOpts(), WantHeld: true,
		WantNoteParts: []string{
			"data.external.lookup runs a program during plan (main.tf line 1)",
			"write a policy with effect exempt that covers this run"},
	}, { // Test 6: The same plan from a person is not, the control for test 5.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": externalConfig},
		Opts: personOpts(), WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			writeFiles(t, dir, test.Files)
			store := run.NewMemStore()
			var rules policy.Store
			if test.Rules != nil {
				rules = test.Rules(t)
			}
			d := newAgentHoldDispatcher(t, store, rules)
			opts := append([]run.SubmitOption{run.WithTool(test.Tool), run.WithDryRun(true)},
				test.Opts...)
			playbook := ""
			if test.Tool == run.ToolAnsible {
				playbook = filepath.Join(dir, "site.yml")
			} else {
				opts = append(opts, run.WithCommand(dir))
			}
			got, err := d.Submit(ctx, playbook, "hosts.ini", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held = %v, want %v. Recorded: %q", held, test.WantHeld,
					got.DryRunFindings())
			}
			if !test.WantHeld {
				return
			}
			if got.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("held by %q, want %q", got.HeldByPolicy, policy.AgentDefaultName)
			}
			for _, want := range test.WantNoteParts {
				if !strings.Contains(got.HoldNote, want) {
					t.Errorf("hold note %q does not say %q", got.HoldNote, want)
				}
			}
		})
	}
}

// TestAnAgentsSplitIsHeldWithEveryShard covers a split, whose shards execute under the parent's
// release. An agent's split waits with every shard it cut, so no shard runs on its own.
func TestAnAgentsSplitIsHeldWithEveryShard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Opts say who submits.
		Opts []run.SubmitOption
		// WantHeld is whether the split and its shards wait for a person.
		WantHeld bool
	}{{ // Test 0: An agent's split and its shards are held.
		Opts: agentOpts(), WantHeld: true,
	}, { // Test 1: A person's split runs, the control.
		Opts: personOpts(), WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &flakyRunnerLister{hosts: []string{"a", "b", "c", "d"}, failHost: "none"}
			d := New(store, runner, zap.NewNop(), WithNoJanitor())
			t.Cleanup(d.Close)
			parent, err := d.SubmitSplit(ctx, "play.yml", "inv", 2, test.Opts...)
			if err != nil {
				t.Fatalf("SubmitSplit() error = %v", err)
			}
			if held := parent.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("parent held = %v, want %v", held, test.WantHeld)
			}
			if !test.WantHeld {
				if final := waitTerminal(t, store, parent.ID); final.Status != run.StatusSucceeded {
					t.Errorf("parent status = %q, want succeeded", final.Status)
				}
				return
			}
			assertHeldShards(t, store, parent.ID)
		})
	}
}

// assertHeldShards fails unless parentID cut at least one shard and every shard waits for a person
// under the built-in hold.
func assertHeldShards(t *testing.T, store run.Store, parentID string) {
	t.Helper()
	shards, err := store.Shards(context.Background(), parentID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) == 0 {
		t.Fatal("the held split cut no shards")
	}
	for _, s := range shards {
		if s.Status != run.StatusPendingApproval || s.HeldByPolicy != policy.AgentDefaultName {
			t.Errorf("shard %s is %q held by %q, want it held by %q", s.ID, s.Status,
				s.HeldByPolicy, policy.AgentDefaultName)
		}
	}
}

// TestAnAgentsRetryOfFailedShardsIsHeld covers retrying a split's failed shards. The retry is
// authorized by the retry request, so an agent asking to retry a person's failed split gets a held
// retry, not a second run of the spec the person was released to run.
func TestAnAgentsRetryOfFailedShardsIsHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Opts say who asks for the retry.
		Opts []run.SubmitOption
		// WantHeld is whether the retry waits for a person.
		WantHeld bool
	}{{ // Test 0: An agent's retry of a person's failed split is held with its shards.
		Opts: agentOpts(), WantHeld: true,
	}, { // Test 1: The person's own retry runs, the control.
		Opts: personOpts(), WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			runner := &flakyRunnerLister{hosts: []string{"a", "b", "c", "d"}, failHost: "b"}
			d := New(store, runner, zap.NewNop(), WithNoJanitor())
			t.Cleanup(d.Close)
			parent, err := d.SubmitSplit(ctx, "play.yml", "inv", 2, personOpts()...)
			if err != nil {
				t.Fatalf("SubmitSplit() error = %v", err)
			}
			if got := waitTerminal(t, store, parent.ID); got.Status != run.StatusFailed {
				t.Fatalf("parent status = %q, want failed", got.Status)
			}
			runner.fixed.Store(true)
			retry, err := d.RetryFailedShards(ctx, parent.ID, test.Opts...)
			if err != nil {
				t.Fatalf("RetryFailedShards() error = %v", err)
			}
			if held := retry.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("retry held = %v, want %v", held, test.WantHeld)
			}
			if !test.WantHeld {
				if final := waitTerminal(t, store, retry.ID); final.Status != run.StatusSucceeded {
					t.Errorf("retry status = %q, want succeeded", final.Status)
				}
				return
			}
			if retry.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("retry held by %q, want %q", retry.HeldByPolicy, policy.AgentDefaultName)
			}
			assertHeldShards(t, store, retry.ID)
		})
	}
}

// TestAnAgentsRelaunchOfFailedHostsIsHeld covers relaunching the hosts a person's run left failed.
// The relaunch belongs to whoever asked for it, so an agent's relaunch waits for a person.
func TestAnAgentsRelaunchOfFailedHostsIsHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Opts say who asks for the relaunch.
		Opts []run.SubmitOption
		// WantHeld is whether the relaunch waits for a person.
		WantHeld bool
	}{{ // Test 0: An agent's relaunch is held.
		Opts: agentOpts(), WantHeld: true,
	}, { // Test 1: The person's own relaunch runs, the control.
		Opts: personOpts(), WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			d := newAgentHoldDispatcher(t, store, nil)
			src := &run.Run{ID: "run_src", Playbook: "site.yml", Inventory: "hosts.ini",
				Tool: run.ToolAnsible, Actor: "dev-lead", ActorType: "session",
				Status: run.StatusRunning, CreatedAt: time.Now()}
			if err := store.Save(ctx, src); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if err := store.SaveHostSummary(ctx, src.ID, []run.HostSummary{
				{Host: "web01", OK: 3}, {Host: "web02", Failures: 1, Worst: "failed"},
			}); err != nil {
				t.Fatalf("SaveHostSummary() error = %v", err)
			}
			src.Status = run.StatusFailed
			if err := store.Save(ctx, src); err != nil {
				t.Fatalf("Save(finished) error = %v", err)
			}
			relaunch, err := d.RelaunchFailedHosts(ctx, src.ID, test.Opts...)
			if err != nil {
				t.Fatalf("RelaunchFailedHosts() error = %v", err)
			}
			if held := relaunch.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("relaunch held = %v, want %v", held, test.WantHeld)
			}
			if test.WantHeld && relaunch.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("relaunch held by %q, want %q", relaunch.HeldByPolicy,
					policy.AgentDefaultName)
			}
		})
	}
}

// TestAnAgentsWorkflowIsHeld covers a workflow, submitted through a different path than a single
// run. An agent's workflow waits for a person, a person's does not, and an exemption naming the
// agent covers the workflow and every step it runs.
func TestAnAgentsWorkflowIsHeld(t *testing.T) {
	t.Parallel()
	// Each case gets its own steps, since a submission sanitizes the steps it is handed in place.
	steps := func() []run.PipelineStep {
		return []run.PipelineStep{
			{Name: "build", Tool: run.ToolBash, Command: "make"},
			{Name: "ship", Tool: run.ToolBash, Command: "./deploy"},
		}
	}
	byName := func(t *testing.T) policy.Store {
		return rulesHolding(t, &policy.Policy{ID: "pol_agent", Name: "release agent",
			Actor: "release-agent", Account: "dev-lead", Effect: policy.EffectExempt,
			MaxDestroy: policy.DisabledMaxDestroy})
	}
	tests := []struct {
		// Rules builds the policy store, nil for an install with none.
		Rules func(t *testing.T) policy.Store
		// Opts say who submits.
		Opts []run.SubmitOption
		// WantHeld is whether the workflow waits for a person.
		WantHeld bool
	}{{ // Test 0: An agent's workflow is held.
		Opts: agentOpts(), WantHeld: true,
	}, { // Test 1: A person's workflow runs, the control.
		Opts: personOpts(), WantHeld: false,
	}, { // Test 2: An exemption naming the agent covers the workflow and its steps.
		Rules: byName, Opts: agentOpts(), WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			var rules policy.Store
			if test.Rules != nil {
				rules = test.Rules(t)
			}
			d := newAgentHoldDispatcher(t, store, rules)
			got, err := d.SubmitPipeline(ctx, "nightly", "", steps(), test.Opts...)
			if err != nil {
				t.Fatalf("SubmitPipeline() error = %v", err)
			}
			if held := got.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held = %v, want %v (held by %q)", held, test.WantHeld, got.HeldByPolicy)
			}
			if test.WantHeld && got.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("held by %q, want %q", got.HeldByPolicy, policy.AgentDefaultName)
			}
			if !test.WantHeld {
				if final := waitTerminal(t, store, got.ID); final.Status != run.StatusSucceeded {
					t.Errorf("status = %q, want succeeded", final.Status)
				}
			}
		})
	}
}

// TestAnAgentsApplyIsPlannedThenHeld covers the run with the largest blast radius the gate governs.
// An agent's Terraform apply is planned first with no policy written, and the apply its plan
// proposes waits for a person carrying the agent's identity, so the approval binds the saved plan
// and names who asked. A person's apply applies as it always did, which is the control.
func TestAnAgentsApplyIsPlannedThenHeld(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Opts say who submits.
		Opts []run.SubmitOption
		// WantProposal is whether the apply is planned and proposed rather than applied.
		WantProposal bool
	}{{ // Test 0: An agent's apply is planned, and its proposed apply is held.
		Opts: agentOpts(), WantProposal: true,
	}, { // Test 1: A person's apply applies directly, the control.
		Opts: personOpts(), WantProposal: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"main.tf": cleanConfig})
			store := run.NewMemStore()
			d := New(store, planRunner("Plan: 0 to add, 0 to change, 1 to destroy"), zap.NewNop(),
				WithNoJanitor())
			t.Cleanup(d.Close)
			opts := append([]run.SubmitOption{run.WithTool(run.ToolTerraform),
				run.WithCommand(dir)}, test.Opts...)
			created, err := d.Submit(ctx, "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if created.Status == run.StatusPendingApproval {
				t.Fatalf("the apply request was held before its plan, so an approver would " +
					"release a plan made only when it ran")
			}
			final := waitTerminal(t, store, created.ID)
			if final.Status != run.StatusSucceeded {
				t.Fatalf("status = %q, want succeeded", final.Status)
			}
			if !test.WantProposal {
				if n := countProposals(t, store); n != 0 {
					t.Errorf("%d applies were proposed for a person's apply, want none", n)
				}
				return
			}
			proposal := waitProposal(t, store, created.ID)
			if proposal.Status != run.StatusPendingApproval {
				t.Fatalf("proposed apply is %q, want it held for a person", proposal.Status)
			}
			if proposal.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("held by %q, want %q", proposal.HeldByPolicy, policy.AgentDefaultName)
			}
			if proposal.Initiator == nil || proposal.Initiator.InitiatedBy != "release-agent" ||
				proposal.Initiator.BoundTo != "dev-lead" {
				t.Errorf("proposed apply initiator = %+v, want the agent and its account",
					proposal.Initiator)
			}
			if !slices.Contains(proposal.PolicyNotes, policy.AgentDefaultName) {
				t.Errorf("proposed apply notes = %q, want the hold recorded", proposal.PolicyNotes)
			}
			if _, err := d.Approve(ctx, proposal.ID, decider("approver", "session")); err != nil {
				t.Fatalf("Approve() error = %v", err)
			}
			if applied := waitTerminal(t, store, proposal.ID); applied.Status != run.StatusSucceeded {
				t.Errorf("approved apply status = %q, want succeeded", applied.Status)
			}
		})
	}
}

// TestAnAgentsApplyThatRunsAProgramIsHeldBeforeItPlans covers the plan an apply runs first.
// Planning runs whatever the configuration runs while it plans, an external data source's program
// included, so an agent's apply whose plan is not shown to change nothing waits before anything
// executes, saying what was found. A clean configuration is still planned first, and a person's
// apply is planned or applied exactly as before.
func TestAnAgentsApplyThatRunsAProgramIsHeldBeforeItPlans(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Config is the configuration the apply plans.
		Config string
		// Opts say who submits.
		Opts []run.SubmitOption
		// WantHeld is whether the apply waits at submission, before it plans.
		WantHeld bool
		// WantProposal is whether the apply is planned and its proposed apply held.
		WantProposal bool
	}{{ // Test 0: An agent's apply whose configuration runs a program waits before it plans.
		Config: externalConfig, Opts: agentOpts(), WantHeld: true,
	}, { // Test 1: A clean configuration is planned first, and its proposed apply waits.
		Config: cleanConfig, Opts: agentOpts(), WantProposal: true,
	}, { // Test 2: A person's apply of the same configuration is not held, the control.
		Config: externalConfig, Opts: personOpts(),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"main.tf": test.Config})
			store := run.NewMemStore()
			d := New(store, planRunner("Plan: 0 to add, 0 to change, 1 to destroy"), zap.NewNop(),
				WithNoJanitor())
			t.Cleanup(d.Close)
			opts := append([]run.SubmitOption{run.WithTool(run.ToolTerraform),
				run.WithCommand(dir)}, test.Opts...)
			created, err := d.Submit(ctx, "", "", opts...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := created.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("held at submission = %v, want %v (note %q)", held, test.WantHeld,
					created.HoldNote)
			}
			if test.WantHeld {
				assertHeldBeforePlanning(t, created)
				return
			}
			if final := waitTerminal(t, store, created.ID); final.Status != run.StatusSucceeded {
				t.Fatalf("status = %q, want succeeded", final.Status)
			}
			if !test.WantProposal {
				if n := countProposals(t, store); n != 0 {
					t.Errorf("%d applies were proposed for a person's apply, want none", n)
				}
				return
			}
			proposal := waitProposal(t, store, created.ID)
			if proposal.Status != run.StatusPendingApproval ||
				proposal.HeldByPolicy != policy.AgentDefaultName {
				t.Errorf("proposed apply is %q held by %q, want it held by %q", proposal.Status,
					proposal.HeldByPolicy, policy.AgentDefaultName)
			}
		})
	}
}

// assertHeldBeforePlanning fails unless created is an agent's apply held at submission by the
// built-in hold, recording the read of its plan and a note that says what was found and how to fix
// it.
func assertHeldBeforePlanning(t *testing.T, created *run.Run) {
	t.Helper()
	if created.HeldByPolicy != policy.AgentDefaultName {
		t.Errorf("held by %q, want %q", created.HeldByPolicy, policy.AgentDefaultName)
	}
	if len(created.DryRunScans) == 0 || run.ScansChangeFree(created.DryRunScans) {
		t.Errorf("scans = %+v, want the read of the plan that found the program", created.DryRunScans)
	}
	for _, want := range []string{
		"data.external.lookup runs a program during plan (main.tf line 1)",
		"write a policy with effect exempt that covers this run",
	} {
		if !strings.Contains(created.HoldNote, want) {
			t.Errorf("hold note %q does not say %q", created.HoldNote, want)
		}
	}
	lead := "Planning this apply was not shown to change nothing, so the default hold on an " +
		"agent's run applies before it plans"
	if !strings.HasPrefix(created.HoldNote, lead) {
		t.Errorf("hold note %q does not lead with %q", created.HoldNote, lead)
	}
}
