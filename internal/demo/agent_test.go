package demo

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// teamLicense is a license that holds every rule the seed writes, so the agent's exemption is
// seeded the way a licensed demo seeds it. The package's other tests run as Community, the tier of
// an install with no license.
var teamLicense = &license.License{Claims: license.Claims{
	V: 1, ID: "lic_demo_test", Org: "test", Tier: license.TierTeam,
	Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
}}

// agentSeed is a demo seeded through the real dispatcher and the real gate, with a runner that
// executes nothing and reports success, so the agent's scenarios run without ansible or terraform.
type agentSeed struct {
	// stores holds what the seed wrote.
	stores *seedStores
	// tokens holds the agent's token.
	tokens auth.Store
}

// seedTheAgent seeds the configuration, the person's waiting release, and the agent's requests,
// and returns what they wrote.
func seedTheAgent(t *testing.T) *agentSeed {
	t.Helper()
	ctx := context.Background()
	stores := newSeedStores()
	tokens := auth.NewMemStore()
	clock := NewSeedClock()
	runner := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec,
		io.Writer) (roundhouse.Result, error) {
		return roundhouse.Result{ExitCode: 0}, nil
	})
	disp := dispatch.New(stores.Runs, runner, zap.NewNop(), dispatch.WithPolicies(stores.Policies),
		dispatch.WithAudits(stores.Audit), dispatch.WithNoJanitor(), dispatch.WithClock(clock.Now))
	t.Cleanup(disp.Close)
	deps := stores.deps()
	deps.Tokens, deps.Submitter, deps.Approver, deps.Clock = tokens, disp, disp, clock
	seedConfig(ctx, deps, zap.NewNop())
	dir := t.TempDir()
	for name, body := range map[string]string{"site.yml": "- hosts: all\n  tasks: []\n",
		"restart-app.yml": "- hosts: web\n  tasks: []\n", "inv.ini": "[web]\nweb01\n"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	playbook, inv := filepath.Join(dir, "site.yml"), filepath.Join(dir, "inv.ini")
	seedWaitingWorkflow(ctx, deps, playbook, inv, zap.NewNop())
	seedAgent(ctx, deps, playbook, inv, dir, zap.NewNop())
	return &agentSeed{stores: stores, tokens: tokens}
}

// agentRuns returns the top-level runs the agent asked for, by tool.
func (s *agentSeed) agentRuns(t *testing.T) map[string]*run.Run {
	t.Helper()
	runs, err := s.stores.Runs.List(context.Background())
	if err != nil {
		t.Fatalf("Runs.List() error = %v", err)
	}
	out := map[string]*run.Run{}
	for _, r := range runs {
		if r.Actor == agentLabel {
			out[run.NormalizeTool(r.Tool)] = r
		}
	}
	return out
}

// TestSeedAgentShowsTheGate drives every agent scenario the demo seeds through the real gate and
// checks what a visitor reads: an agent token bound to its account, the restart and the apply held
// by default and carrying the agent note the run page explains, the apply saying why it waits before
// anything plans, the smoke test run under its exemption, and the workflow and the self-approval
// refused with both refusals on the chain. A person's run carries none of the agent's identity,
// which is the control for the identity the agent's runs carry. It runs under a Team license, which
// holds the exemption, and not in parallel, since the license is process state.
//
//nolint:funlen // Test function.
func TestSeedAgentShowsTheGate(t *testing.T) {
	license.Set(teamLicense)
	t.Cleanup(func() { license.Set(nil) })
	ctx := context.Background()
	s := seedTheAgent(t)

	tokens, err := s.tokens.List(ctx)
	if err != nil || len(tokens) != 1 {
		t.Fatalf("tokens = %d (%v), want the agent's one", len(tokens), err)
	}
	tok := tokens[0]
	account, err := s.stores.Users.FindByUsername(ctx, agentAccount)
	if err != nil {
		t.Fatalf("FindByUsername(%s) error = %v", agentAccount, err)
	}
	if tok.Kind != auth.KindAgent || tok.UserID != account.ID || tok.Name != agentLabel ||
		tok.CreatedByType != "cli" || !strings.HasPrefix(tok.CreatedBy, "cli:") {
		t.Errorf("token = %+v, want an agent token labeled %s bound to %s, minted from the "+
			"command line", tok, agentLabel, agentAccount)
	}

	runs := s.agentRuns(t)
	tests := []struct {
		// Tool picks the agent's run.
		Tool string
		// WantStatus is where it stands.
		WantStatus run.Status
		// WantHeldBy names what holds it, empty when nothing does.
		WantHeldBy string
		// WantNote is a note its record must carry.
		WantNote string
		// WantReason is what its hold note must say, empty when the agent note says enough.
		WantReason string
	}{{ // Test 0: The restart on one web host, held by default and noted as the agent's.
		Tool: run.ToolAnsible, WantStatus: run.StatusPendingApproval,
		WantHeldBy: policy.AgentDefaultName, WantNote: policy.AgentDefaultName,
	}, { // Test 1: The apply, held before anything plans.
		Tool: run.ToolTerraform, WantStatus: run.StatusPendingApproval,
		WantHeldBy: policy.AgentDefaultName, WantNote: policy.AgentDefaultName,
		WantReason: "before anything plans",
	}, { // Test 2: The smoke test, run under the exemption and named by it.
		Tool: run.ToolBash, WantStatus: run.StatusSucceeded,
		WantNote: `requested by an agent bound to account "ops-lead", exempt from the default ` +
			`hold by policy "remediation-agent smoke tests"`,
	}}
	for testNum, test := range tests {
		r := runs[test.Tool]
		if r == nil {
			t.Errorf("test %d: the agent has no %s run", testNum, test.Tool)
			continue
		}
		if r.Status != test.WantStatus || r.HeldByPolicy != test.WantHeldBy {
			t.Errorf("test %d: %s run is %s held by %q, want %s held by %q", testNum, test.Tool,
				r.Status, r.HeldByPolicy, test.WantStatus, test.WantHeldBy)
		}
		if !slices.Contains(r.PolicyNotes, test.WantNote) {
			t.Errorf("test %d: notes = %q, want %q", testNum, r.PolicyNotes, test.WantNote)
		}
		if test.WantReason != "" && !strings.Contains(r.HoldNote, test.WantReason) {
			t.Errorf("test %d: hold note = %q, want it to say %q", testNum, r.HoldNote,
				test.WantReason)
		}
		// The identity a real agent token gives every run it asks for.
		if r.ActorType != policy.ActorKindAgent || r.Account != agentAccount ||
			r.ActorUserID != account.ID || r.Initiator == nil ||
			r.Initiator.InitiatedBy != agentLabel || r.Initiator.BoundTo != agentAccount ||
			r.Initiator.CredentialID != tok.ID || r.Initiator.ProvisionedByType != "cli" {
			t.Errorf("test %d: %s run identity = %s %q %+v, want the agent on %s", testNum,
				test.Tool, r.ActorType, r.Account, r.Initiator, agentAccount)
		}
	}

	chain, err := s.stores.Audit.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	restart := runs[run.ToolAnsible]
	var refusedWorkflow, refusedApproval bool
	for _, e := range chain {
		if strings.Contains(e.Path, "/decision/refused/policy/"+policy.AgentWorkflowApplyName) {
			refusedWorkflow = true
		}
		if restart != nil && e.Path == "/v1/runs/"+restart.ID+"/approve" &&
			e.Actor == agentLabel && e.ActorType == policy.ActorKindAgent {
			refusedApproval = true
		}
		// Every request the agent made is on the chain at the seed's time, not the seed instant.
		if e.Actor == agentLabel && e.At.After(time.Now().Add(-seedRunMargin)) {
			t.Errorf("the agent's %s %s is stamped %v, inside the margin before now", e.Method,
				e.Path, e.At)
		}
	}
	if !refusedWorkflow {
		t.Error("the agent's workflow with an apply step left no refusal on the chain")
	}
	if !refusedApproval {
		t.Error("the agent's attempt to approve its own run left no entry on the chain")
	}
	if restart != nil {
		if got, err := s.stores.Runs.Get(ctx, restart.ID); err != nil ||
			got.Status != run.StatusPendingApproval {
			t.Errorf("after the agent's own approval the restart is %v (%v), want still held",
				got, err)
		}
	}
}

// TestSeedWaitingWorkflowPausesAtItsStep checks the person's release the approvals queue shows: the
// build ran, the workflow waits at its approval step, and the deploy after it has not run. The
// person who asked carries no agent identity, which is the control for the agent's.
func TestSeedWaitingWorkflowPausesAtItsStep(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	s := seedTheAgent(t)
	runs, err := s.stores.Runs.List(ctx)
	if err != nil {
		t.Fatalf("Runs.List() error = %v", err)
	}
	var release *run.Run
	for _, r := range runs {
		if r.Kind == run.KindPipeline && r.Playbook == "Release 4.3" {
			release = r
		}
	}
	if release == nil {
		t.Fatal("no waiting release was seeded")
	}
	if release.ActorType == policy.ActorKindAgent || release.Initiator != nil {
		t.Errorf("the person's release carries an agent's identity: %s %+v", release.ActorType,
			release.Initiator)
	}
	pending, err := run.PendingApprovalSteps(ctx, s.stores.Runs, release.ID)
	if err != nil || len(pending) != 1 || pending[0].StepName != "approve-release" {
		t.Fatalf("pending steps = %v (%v), want the release waiting at approve-release", pending,
			err)
	}
	steps, err := s.stores.Runs.Steps(ctx, release.ID)
	if err != nil {
		t.Fatalf("Steps() error = %v", err)
	}
	ran := map[string]run.Status{}
	for _, st := range steps {
		ran[st.StepName] = st.Status
	}
	if ran["build"] != run.StatusSucceeded || ran["deploy"] != "" {
		t.Errorf("steps = %v, want the build succeeded and the deploy not started", ran)
	}
}

// TestTheAgentsExemptionFollowsTheLicense holds the exemption to the license check an admin's rule
// meets through the API. Under a license that holds it, the exemption is stored and the smoke test
// runs under it. Without one, the install is Community, which holds one rule and already holds the
// two the demo's governance shows, so the exemption is not stored and the smoke test waits like
// the agent's other runs. Not parallel: the license is process state.
func TestTheAgentsExemptionFollowsTheLicense(t *testing.T) {
	t.Cleanup(func() { license.Set(nil) })
	tests := []struct {
		// License is the license in force, nil for Community.
		License *license.License
		// WantExemption is whether the exemption is stored.
		WantExemption bool
		// WantSmoke is where the agent's smoke test stands.
		WantSmoke run.Status
	}{{ // Test 0: A license that holds the rule.
		License: teamLicense, WantExemption: true, WantSmoke: run.StatusSucceeded,
	}, { // Test 1: Community refuses it, the control.
		License: nil, WantExemption: false, WantSmoke: run.StatusPendingApproval,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			license.Set(test.License)
			s := seedTheAgent(t)
			rules, err := s.stores.Policies.List(context.Background())
			if err != nil {
				t.Fatalf("Policies.List() error = %v", err)
			}
			found := false
			for _, p := range rules {
				if p.Name == agentExemptionName && p.Exempts() && p.Account == agentAccount &&
					p.Actor == agentLabel && p.Tool == run.ToolBash {
					found = true
				}
			}
			if found != test.WantExemption {
				t.Errorf("exemption stored = %v, want %v", found, test.WantExemption)
			}
			if smoke := s.agentRuns(t)[run.ToolBash]; smoke == nil ||
				smoke.Status != test.WantSmoke {
				t.Errorf("smoke test = %+v, want %s", smoke, test.WantSmoke)
			}
		})
	}
}

// TestSeedNightlyHistoryFiresOncePerNight checks the volume the runs list and the two-week chart
// read: the nightly audit once a night at its schedule's time, across the nights before the run
// window, each carrying the schedule and template that fired it. Without a seed clock nothing is
// placed in the past, which is the control.
func TestSeedNightlyHistoryFiresOncePerNight(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Clock is whether the seed has a clock.
		Clock bool
		// WantNights is the fewest nights expected, zero for none.
		WantNights int
	}{{ // Test 0: Two weeks of nights, give or take the night the window opens on.
		Clock: true, WantNights: seedHistoryDays - 1,
	}, { // Test 1: No clock, no history, the control.
		Clock: false, WantNights: 0,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			stores := newSeedStores()
			deps := stores.deps()
			var clock *SeedClock
			if test.Clock {
				clock = NewSeedClock()
				deps.Clock = clock
			}
			deps.Submitter = &terminalSubmitter{runs: stores.Runs, audits: stores.Audit,
				clock: clock}
			ids := seedConfig(ctx, deps, zap.NewNop())
			if err := seedNightlyHistory(ctx, deps, "audit.yml", "inv.ini", ids,
				zap.NewNop()); err != nil {
				t.Fatalf("seedNightlyHistory() error = %v", err)
			}
			runs, err := stores.Runs.List(ctx)
			if err != nil {
				t.Fatalf("Runs.List() error = %v", err)
			}
			if test.WantNights == 0 {
				if len(runs) != 0 {
					t.Errorf("seeded %d nightly runs with no clock, want none", len(runs))
				}
				return
			}
			if len(runs) < test.WantNights || len(runs) > seedHistoryDays+1 {
				t.Fatalf("seeded %d nightly runs, want about %d", len(runs), seedHistoryDays)
			}
			days := map[string]bool{}
			for _, r := range runs {
				at := r.CreatedAt.UTC()
				days[at.Format("2006-01-02")] = true
				if at.Hour() != 2 || r.Source != "schedule" ||
					r.SourceID != ids.Schedules["Nightly audit"] ||
					r.TemplateID != ids.Templates["Nightly audit"] {
					t.Errorf("run %s at %v from %s %s template %s, want the 02:00 UTC fire of "+
						"the nightly schedule", r.ID, at, r.Source, r.SourceID, r.TemplateID)
				}
				if !r.CreatedAt.Before(clock.windowAt) {
					t.Errorf("run %s at %v is inside the run window", r.ID, r.CreatedAt)
				}
			}
			if len(days) != len(runs) {
				t.Errorf("%d runs fell on %d days, want one a night", len(runs), len(days))
			}
		})
	}
}
