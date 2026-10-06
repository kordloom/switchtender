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

// TestAnAgentsDryRunIsHeld covers the preview an agent asks for. Check mode and a plan still run
// code with this server's credentials, Ansible's lookups and plugins and a plan's providers and
// data sources, so an agent's dry run waits for a person whatever the gate's scans found, the
// scans are still recorded, and the hold note says why. An exemption covering it lets it go ahead.
// A person's dry run is unchanged, under an exclude_dry_run rule as without one.
//
//nolint:funlen // Test function.
func TestAnAgentsDryRunIsHeld(t *testing.T) {
	t.Parallel()
	ansibleChecks := func(t *testing.T) policy.Store {
		return rulesHolding(t, &policy.Policy{ID: "pol_ans", Name: "ansible checks",
			Tool: run.ToolAnsible, Effect: policy.EffectExempt,
			MaxDestroy: policy.DisabledMaxDestroy})
	}
	previewsFree := func(t *testing.T) policy.Store {
		return rulesHolding(t, &policy.Policy{ID: "pol_prev", Name: "hold ansible",
			Tool: run.ToolAnsible, ExcludeDryRun: true, MaxDestroy: policy.DisabledMaxDestroy})
	}
	tests := []struct {
		// Tool is the tool of the dry run.
		Tool string
		// Files are the playbook or configuration the dry run reads.
		Files map[string]string
		// Rules builds the policy store, nil for an install with none.
		Rules func(t *testing.T) policy.Store
		// Opts say who submits.
		Opts []run.SubmitOption
		// WantHeldBy names what holds the dry run, empty when it goes ahead.
		WantHeldBy string
		// WantNoteParts are what the hold note must say.
		WantNoteParts []string
		// WantFinding is a finding the recorded scans must hold, empty for none.
		WantFinding string
	}{{ // Test 0: A clean playbook's check-mode run is held all the same, saying why.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": cleanPlaybook},
		Opts: agentOpts(), WantHeldBy: policy.AgentDefaultName,
		WantNoteParts: []string{"check mode still runs lookups", "this server's credentials",
			"waits for a person or for an exemption"},
	}, { // Test 1: A playbook that forces a task under check mode is held, with the scan recorded.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": forcedTaskPlaybook},
		Opts: agentOpts(), WantHeldBy: policy.AgentDefaultName,
		WantNoteParts: []string{"check mode still runs lookups"},
		WantFinding:   `site.yml: task "Restart web" sets check_mode to false`,
	}, { // Test 2: The same dry run from a person is not held, the control for test 1.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": forcedTaskPlaybook},
		Opts: personOpts(),
	}, { // Test 3: An exemption covering the agent's dry run lets it go ahead.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": forcedTaskPlaybook},
		Rules: ansibleChecks, Opts: agentOpts(),
	}, { // Test 4: A clean configuration's plan is held all the same, saying why.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": cleanConfig},
		Opts: agentOpts(), WantHeldBy: policy.AgentDefaultName,
		WantNoteParts: []string{"a plan still runs provider code and data sources",
			"this server's credentials"},
	}, { // Test 5: A configuration with an external data source is held, with the scan recorded.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": externalConfig},
		Opts: agentOpts(), WantHeldBy: policy.AgentDefaultName,
		WantFinding: "data.external.lookup runs a program during plan (main.tf line 1)",
	}, { // Test 6: The same plan from a person is not held, the control for test 5.
		Tool: run.ToolTerraform, Files: map[string]string{"main.tf": externalConfig},
		Opts: personOpts(),
	}, { // Test 7: A person's clean dry run under exclude_dry_run goes ahead, as it always did.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": cleanPlaybook},
		Rules: previewsFree, Opts: personOpts(),
	}, { // Test 8: A person's forcing dry run under it is held by the rule, saying what was found.
		Tool: run.ToolAnsible, Files: map[string]string{"site.yml": forcedTaskPlaybook},
		Rules: previewsFree, Opts: personOpts(), WantHeldBy: "hold ansible",
		WantNoteParts: []string{`"hold ansible" does not exempt it`,
			"drop exclude_dry_run from"},
		WantFinding: `site.yml: task "Restart web" sets check_mode to false`,
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
			if got.HeldByPolicy != test.WantHeldBy ||
				(got.Status == run.StatusPendingApproval) != (test.WantHeldBy != "") {
				t.Fatalf("status %s held by %q, want held by %q", got.Status, got.HeldByPolicy,
					test.WantHeldBy)
			}
			for _, want := range test.WantNoteParts {
				if !strings.Contains(got.HoldNote, want) {
					t.Errorf("hold note %q does not say %q", got.HoldNote, want)
				}
			}
			// The scans are evidence whether or not they decided anything.
			if len(got.DryRunScans) == 0 {
				t.Errorf("no scan was recorded on the dry run")
			}
			if test.WantFinding != "" && !slices.Contains(got.DryRunFindings(), test.WantFinding) {
				t.Errorf("recorded findings %q do not hold %q", got.DryRunFindings(),
					test.WantFinding)
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

// TestAnAgentsApplyTakesTwoApprovals covers the run with the largest blast radius the gate
// governs. Planning runs provider code and data sources with this server's credentials, so an
// agent's Terraform apply is held at submission, before anything plans, whatever other rules are
// in force. A person's release lets it plan, never apply: the apply its plan proposes waits for a
// second approval carrying the saved plan and the agent's identity, and only that approval applies
// it. A person's apply, and an exempt agent's, behave as they always did, which are the controls.
//
//nolint:funlen,gocognit // Test function.
func TestAnAgentsApplyTakesTwoApprovals(t *testing.T) {
	t.Parallel()
	limit := &policy.Policy{ID: "pol_limit", Name: "large teardown", Tool: run.ToolTerraform,
		MaxDestroy: 5}
	floor := &policy.Policy{ID: "pol_floor", Name: "irreversible needs a person",
		Reversibility: run.Irreversible, MaxDestroy: policy.DisabledMaxDestroy}
	exempt := &policy.Policy{ID: "pol_tf", Name: "tf agent", Tool: run.ToolTerraform,
		Effect: policy.EffectExempt, MaxDestroy: policy.DisabledMaxDestroy}
	regoGate := `plan_gate if input.run.tool == "terraform"

hold contains msg if {
	input.plan.destroys > 3
	msg := "plan destroys more than 3"
}`
	tests := []struct {
		// Rules builds the policy store, nil for an install with none.
		Rules func(t *testing.T) policy.Store
		// Config is the configuration the apply plans.
		Config string
		// Opts say who submits.
		Opts []run.SubmitOption
		// WantTwoSteps is whether the request is held, and its proposed apply held again.
		WantTwoSteps bool
		// WantProposal is, for a run that is not held, whether it plans first and proposes.
		WantProposal bool
		// WantProposalHeldBy names what holds the proposed apply, the built-in hold when empty.
		WantProposalHeldBy string
	}{{ // Test 0: With no rule written.
		Config: cleanConfig, Opts: agentOpts(), WantTwoSteps: true,
	}, { // Test 1: With a destroy limit in force.
		Rules:  func(t *testing.T) policy.Store { return rulesHolding(t, limit) },
		Config: cleanConfig, Opts: agentOpts(), WantTwoSteps: true,
	}, { // Test 2: With a reversibility floor in force, which names itself on the proposal.
		Rules:  func(t *testing.T) policy.Store { return rulesHolding(t, floor) },
		Config: cleanConfig, Opts: agentOpts(), WantTwoSteps: true,
		WantProposalHeldBy: "irreversible needs a person",
	}, { // Test 3: With a Rego plan gate in force.
		Rules:  func(t *testing.T) policy.Store { return regoStore(t, regoGate) },
		Config: cleanConfig, Opts: agentOpts(), WantTwoSteps: true,
	}, { // Test 4: With a configuration that runs a program while it plans.
		Config: externalConfig, Opts: agentOpts(), WantTwoSteps: true,
	}, { // Test 5: A person's apply applies as it always did, the control.
		Config: cleanConfig, Opts: personOpts(),
	}, { // Test 6: An exempt agent's apply applies as the exemption allows.
		Rules:  func(t *testing.T) policy.Store { return rulesHolding(t, exempt) },
		Config: cleanConfig, Opts: agentOpts(),
	}, { // Test 7: Under a destroy limit it plans first and its proposal goes ahead.
		Rules:  func(t *testing.T) policy.Store { return rulesHolding(t, exempt, limit) },
		Config: cleanConfig, Opts: agentOpts(), WantProposal: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			dir := t.TempDir()
			writeFiles(t, dir, map[string]string{"main.tf": test.Config})
			store := run.NewMemStore()
			opts := []Option{WithNoJanitor()}
			if test.Rules != nil {
				opts = append(opts, WithPolicies(test.Rules(t)))
			}
			d := New(store, planRunner("Plan: 0 to add, 0 to change, 1 to destroy"), zap.NewNop(),
				opts...)
			t.Cleanup(d.Close)
			created, err := d.Submit(ctx, "", "", append([]run.SubmitOption{
				run.WithTool(run.ToolTerraform), run.WithCommand(dir)}, test.Opts...)...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if held := created.Status == run.StatusPendingApproval; held != test.WantTwoSteps {
				t.Fatalf("held at submission = %v, want %v (held by %q)", held,
					test.WantTwoSteps, created.HeldByPolicy)
			}
			if !test.WantTwoSteps {
				if final := waitTerminal(t, store, created.ID); final.Status != run.StatusSucceeded {
					t.Fatalf("status = %q, want succeeded", final.Status)
				}
				if n := countProposals(t, store); (n > 0) != test.WantProposal {
					t.Fatalf("%d applies proposed, want proposal %v", n, test.WantProposal)
				}
				if test.WantProposal {
					proposal := waitProposal(t, store, created.ID)
					if done := waitTerminal(t, store, proposal.ID); done.Status != run.StatusSucceeded {
						t.Errorf("proposed apply status = %q, want succeeded", done.Status)
					}
				}
				return
			}
			// Held before anything planned, saying why.
			if !strings.Contains(created.HoldNote, "before anything plans") {
				t.Errorf("hold note %q does not say the apply waits before it plans",
					created.HoldNote)
			}
			if n := countProposals(t, store); n != 0 {
				t.Fatalf("%d applies proposed before anybody released the request", n)
			}
			// The first approval releases the request to plan, never to apply.
			if _, err := d.Approve(ctx, created.ID, decider("approver", "session")); err != nil {
				t.Fatalf("Approve(request) error = %v", err)
			}
			if final := waitTerminal(t, store, created.ID); final.Status != run.StatusSucceeded {
				t.Fatalf("released request status = %q, want succeeded", final.Status)
			}
			if log := readLog(t, store, created.ID); strings.Contains(log, "Apply complete") {
				t.Fatalf("the released request applied without a plan: %q", log)
			}
			proposal := waitProposal(t, store, created.ID)
			wantHeldBy := test.WantProposalHeldBy
			if wantHeldBy == "" {
				wantHeldBy = policy.AgentDefaultName
			}
			if proposal.Status != run.StatusPendingApproval || proposal.HeldByPolicy != wantHeldBy {
				t.Fatalf("proposed apply is %q held by %q, want it held by %q",
					proposal.Status, proposal.HeldByPolicy, wantHeldBy)
			}
			if proposal.PlanSHA256 == "" || proposal.Account != "dev-lead" ||
				proposal.Initiator == nil || proposal.Initiator.InitiatedBy != "release-agent" {
				t.Errorf("proposed apply carries plan %q on %q by %+v, want the saved plan, the "+
					"account, and the agent", proposal.PlanSHA256, proposal.Account,
					proposal.Initiator)
			}
			if !strings.Contains(proposal.HoldNote, "saved plan") {
				t.Errorf("proposal hold note %q does not say it carries the saved plan",
					proposal.HoldNote)
			}
			// The second approval applies exactly that plan.
			if _, err := d.Approve(ctx, proposal.ID, decider("approver", "session")); err != nil {
				t.Fatalf("Approve(proposal) error = %v", err)
			}
			if done := waitTerminal(t, store, proposal.ID); done.Status != run.StatusSucceeded {
				t.Errorf("approved apply status = %q, want succeeded", done.Status)
			}
		})
	}
}

// TestAnAgentsRerunOfAnApplyTakesTwoApprovals holds a rerun of an apply to the path a fresh
// submission takes. The rerun an agent asks for replays the spec of a finished apply, and is held
// before anything plans. The control is the same rerun asked for by a person, which applies.
func TestAnAgentsRerunOfAnApplyTakesTwoApprovals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Opts say who asks for the rerun.
		Opts []run.SubmitOption
		// WantHeld is whether the rerun waits for a person before it plans.
		WantHeld bool
	}{{ // Test 0: The agent's rerun is held before it plans.
		Opts: agentOpts(), WantHeld: true,
	}, { // Test 1: A person's rerun is not, the control.
		Opts: personOpts(), WantHeld: false,
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
			// A person's apply finished, so it can be rerun.
			src, err := d.Submit(ctx, "", "", append([]run.SubmitOption{
				run.WithTool(run.ToolTerraform), run.WithCommand(dir)}, personOpts()...)...)
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			done := waitTerminal(t, store, src.ID)
			rerun, err := d.Submit(ctx, done.Playbook, done.Inventory, append(append(
				done.ExecutionOptions(), run.WithSource("rerun", done.ID),
				run.WithRerunOf(done.ID)), test.Opts...)...)
			if err != nil {
				t.Fatalf("Submit(rerun) error = %v", err)
			}
			if held := rerun.Status == run.StatusPendingApproval; held != test.WantHeld {
				t.Fatalf("rerun held = %v, want %v (held by %q)", held, test.WantHeld,
					rerun.HeldByPolicy)
			}
			if test.WantHeld && (rerun.HeldByPolicy != policy.AgentDefaultName ||
				!strings.Contains(rerun.HoldNote, "before anything plans")) {
				t.Errorf("rerun held by %q with note %q, want the built-in hold before it plans",
					rerun.HeldByPolicy, rerun.HoldNote)
			}
		})
	}
}

// readLog returns the log stored for the run id.
func readLog(t *testing.T, store run.Store, id string) string {
	t.Helper()
	body, err := store.Log(context.Background(), id)
	if err != nil {
		t.Fatalf("Log(%s) error = %v", id, err)
	}
	return string(body)
}
