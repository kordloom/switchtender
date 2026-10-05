package dispatch

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/plantest"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestProposedApplyCarriesItsOriginAndItsCommit covers two things the plan gate left off the apply it
// proposes, both of which decide what the evidence for the most destructive run in the product says.
//
// The apply had no actor. It is created by the executor while the plan runs, long after the plan's own
// request returned, so nothing filled it in: an actor-scoped approval policy could not match the apply,
// and the run that actually destroys infrastructure was attributed to nobody. The plan's actor is the
// truthful answer, the same reasoning already applied to the receipt and the owning organization.
//
// And it was not pinned to a commit. A plan is read, approved on the strength of what it said it would
// destroy, and then the apply syncs the project again and takes whatever the branch head is by then. An
// approval of one plan could release an apply of different code, with nothing in the record showing the
// substitution.
func TestProposedApplyCarriesItsOriginAndItsCommit(t *testing.T) {
	t.Parallel()
	policies := []*policy.Policy{
		{ID: "pol_1", Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 1},
	}
	plan := &run.Run{
		ID: "run_plan", Tool: run.ToolTerraform, Command: "infra/prod", DryRun: true,
		Actor: "casey", ActorType: "session", ActorUserID: "usr_casey", CommitSHA: "abc123def456",
		OrgID: "org_1", ProjectID: "prj_1",
	}

	// The options the gate would submit the apply with, applied to a blank run so the values it
	// carries can be read directly. A destroy count over the threshold is the held case.
	proposal := &run.Run{}
	run.ApplyOptions(proposal, applyOptions(plan, policies, 3, true))

	if proposal.Actor != "casey" {
		t.Errorf("proposed apply actor = %q, want casey: the run that destroys infrastructure is "+
			"attributed to nobody, and an actor-scoped policy cannot match it", proposal.Actor)
	}
	if proposal.ActorType != "session" {
		t.Errorf("proposed apply actor type = %q, want session", proposal.ActorType)
	}
	// And the account behind the name. Separation of duties compares accounts and falls back to the
	// display name only when one side has none, so an apply carrying a name alone let the person who
	// submitted the plan release the apply that destroys the infrastructure: the fallback compared a
	// browser session's username against the plan's credential label, which never match. This is the
	// highest blast radius run the gate governs.
	if proposal.ActorUserID != "usr_casey" {
		t.Errorf("proposed apply ActorUserID = %q, want usr_casey: without it the distinct-approver "+
			"rule cannot compare accounts and the requester can release their own destroy",
			proposal.ActorUserID)
	}
	if proposal.PinnedCommit != plan.CommitSHA {
		t.Errorf("proposed apply PinnedCommit = %q, want the plan's commit %q: the approval of one "+
			"plan would release an apply of whatever the branch holds later",
			proposal.PinnedCommit, plan.CommitSHA)
	}
	// The rest of what the apply inherits, so this test fails if the propagation is rearranged.
	if proposal.DryRun {
		t.Error("the proposed apply is a dry run, so it would preview instead of applying")
	}
	if proposal.ProposedFrom != plan.ID || proposal.OrgID != plan.OrgID {
		t.Errorf("proposal = %+v, want it tied to the plan and its tenant", proposal)
	}
	if proposal.Status != run.StatusPendingApproval {
		t.Errorf("proposal status = %q, want pending_approval: a plan over the destroy limit must be "+
			"held", proposal.Status)
	}

	// A plan whose own request carried no actor, such as one fired by a schedule, does not invent one.
	anon := &run.Run{ID: "run_anon", Tool: run.ToolTerraform, Command: "infra/prod"}
	blank := &run.Run{}
	run.ApplyOptions(blank, applyOptions(anon, policies, 0, true))
	if blank.Actor != "" || blank.ActorType != "" {
		t.Errorf("apply from an unattributed plan = %+v, want no invented actor", blank)
	}
}

// TestPinnedApplyRefusesADifferentCommit covers the enforcement half. The pin is only a record until
// something checks it: the apply syncs the project, learns the commit it actually got, and must refuse
// when that is not the commit the approved plan was read from.
func TestPinnedApplyRefusesADifferentCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Pinned  string
		Synced  string
		WantErr bool
	}{ // Test 0: The same commit runs.
		{"same commit", "abc123", "abc123", false},
		// Test 1: A moved branch is refused rather than applied.
		{"branch moved", "abc123", "def456", true},
		// Test 2: An unpinned run, which is every ordinary run, is unaffected.
		{"no pin", "", "def456", false},
		// Test 3: A pin with nothing synced, such as a run with no project, is not a mismatch.
		{"nothing synced", "abc123", "", false},
	}
	for i, tc := range tests {
		// Through stampCommit, which is the call the sync actually makes: recording the commit and
		// honoring the pin are one step, so a change that skips the check also skips the record.
		r := &run.Run{PinnedCommit: tc.Pinned}
		err := stampCommit(r, tc.Synced)
		if r.CommitSHA != tc.Synced {
			t.Errorf("test %d (%s): the synced commit was not recorded on the run", i, tc.Name)
		}
		if (err != nil) != tc.WantErr {
			t.Errorf("test %d (%s): checkPinnedCommit = %v, wantErr %v", i, tc.Name, err, tc.WantErr)
		}
		if tc.WantErr && err != nil {
			// The message has to name both commits: the operator's next step is to look at what
			// changed between them.
			for _, want := range []string{tc.Pinned, tc.Synced} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("test %d: error %q does not name %q", i, err, want)
				}
			}
		}
	}
}

// proposingStore is a run store that also knows how to have an apply proposed for it, which is what a
// relay-backed store does. It records the call so a test can prove the dispatcher took that path
// instead of creating the run itself.
type proposingStore struct {
	run.Store
	// planID is the plan the dispatcher asked about.
	planID string
	// destroys and read are what it reported.
	destroys int
	read     bool
	// calls counts how many times it was asked.
	calls int
	// plan is the plan file it was handed.
	plan []byte
}

// ProposeApply records the request and stores a held apply, standing in for the control node.
func (p *proposingStore) ProposeApply(ctx context.Context, planID string, destroys int, read bool,
	plan []byte) (*run.Run, error) {
	p.calls++
	p.planID, p.destroys, p.read = planID, destroys, read
	p.plan = append([]byte(nil), plan...)
	proposal := &run.Run{
		ID: run.NewID(), Status: run.StatusPendingApproval, ProposedFrom: planID,
		Tool: run.ToolTerraform, Command: "infra/prod", CreatedAt: time.Now(),
	}
	if err := p.Save(ctx, proposal); err != nil {
		return nil, err
	}
	return proposal, nil
}

// TestThePlanGateAsksAStoreThatCannotCreateRuns proves the wiring the relay depends on. A worker's store
// cannot create a run, so the gate has to ask the control node to build the proposal rather than
// submitting one. Without this the gated apply failed with a 404 on every worker, and the plan failed
// with it.
func TestThePlanGateAsksAStoreThatCannotCreateRuns(t *testing.T) {
	t.Parallel()
	store := &proposingStore{Store: run.NewMemStore()}
	policies := policy.NewMemStore()
	if err := policies.Save(context.Background(), &policy.Policy{
		ID: policy.NewID(), Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 1,
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	d := New(store, planRunner("Plan: 0 to add, 0 to change, 3 to destroy"), nil, WithPolicies(policies))
	defer d.Close()

	created, err := d.Submit(context.Background(), "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra/prod"))
	if err != nil {
		t.Fatalf("Submit: %v", err)
	}
	plan := waitTerminal(t, store, created.ID)
	if plan.Status != run.StatusSucceeded {
		t.Fatalf("plan run status = %q, want succeeded: %s", plan.Status, plan.Error)
	}
	if store.calls != 1 {
		t.Fatalf("the store was asked to propose %d times, want exactly 1: the gate submitted the "+
			"apply itself, which a worker's store cannot do", store.calls)
	}
	if store.planID != created.ID {
		t.Errorf("proposed for plan %q, want %q", store.planID, created.ID)
	}
	if store.destroys != 3 || !store.read {
		t.Errorf("reported destroys=%d read=%v, want 3 and true", store.destroys, store.read)
	}
	if string(store.plan) != string(plantest.File) {
		t.Errorf("the control node was handed plan file %q, want the one the plan saved", store.plan)
	}
}

// TestProposedApplyCarriesThePlansSourceAndLabels covers the rest of what the apply owes the plan
// that proposed it. The apply is the second half of one request, and it arrived with no origin, so
// the runs list showed a blank where it says what started a run, the template that launched the
// plan never listed the apply in its history, and a ticket label on the plan was nowhere on the run
// that destroys what the ticket is about.
func TestProposedApplyCarriesThePlansSourceAndLabels(t *testing.T) {
	t.Parallel()
	policies := []*policy.Policy{
		{ID: "pol_1", Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 0},
	}
	tests := []struct {
		Name         string
		Plan         *run.Run
		WantSource   string
		WantSourceID string
		WantLabels   map[string]string
	}{{ // Test 0: A template launch keeps its template and its labels.
		Name: "template launch",
		Plan: &run.Run{ID: "run_plan", Tool: run.ToolTerraform, Command: "infra/legacy",
			Source: "template", SourceID: "tpl_decom",
			Labels: map[string]string{"env": "prod", "ticket": "OPS-511"}},
		WantSource: "template", WantSourceID: "tpl_decom",
		WantLabels: map[string]string{"env": "prod", "ticket": "OPS-511"},
	}, { // Test 1: A plan submitted over the API keeps that origin with no source id.
		Name: "api submission",
		Plan: &run.Run{ID: "run_api", Tool: run.ToolTerraform, Command: "infra/legacy",
			Source: "api"},
		WantSource: "api",
	}, { // Test 2: A plan with no recorded origin does not have one invented for its apply.
		Name: "no origin",
		Plan: &run.Run{ID: "run_bare", Tool: run.ToolTerraform, Command: "infra/legacy"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			proposal := &run.Run{}
			run.ApplyOptions(proposal, applyOptions(test.Plan, policies, 3, true))
			if diff := cmp.Diff(test.WantSource, proposal.Source); diff != "" {
				t.Errorf("Source mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantSourceID, proposal.SourceID); diff != "" {
				t.Errorf("SourceID mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantLabels, proposal.Labels, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Labels mismatch (-want +got):\n%s", diff)
			}
			// The labels are the apply's own copy, so a later edit to the plan's cannot rewrite the
			// record of the run that destroyed something.
			if len(test.Plan.Labels) > 0 {
				test.Plan.Labels["ticket"] = "edited"
				if proposal.Labels["ticket"] == "edited" {
					t.Error("the apply shares its labels map with the plan, so editing one edits both")
				}
			}
		})
	}
}

// TestProposedApplyCarriesThePlansTimeoutTargetsAndIntent covers what the apply owes the request
// beyond its origin. A template that launches a terraform run with a timeout and notification
// targets meant them for the change, and the apply is the run that makes the change: it ran with no
// timeout of its own and told none of the template's channels that it held, finished, or failed. An
// apply proposed from a plain-language request also lost the request, which is what its approver
// checks the plan against.
func TestProposedApplyCarriesThePlansTimeoutTargetsAndIntent(t *testing.T) {
	t.Parallel()
	policies := []*policy.Policy{
		{ID: "pol_1", Name: "tf-destroy-guard", Tool: run.ToolTerraform, MaxDestroy: 0},
	}
	targets := []run.NotifyTarget{
		{Kind: "slack", URL: "https://hooks.slack.test/T0/B0/x"},
		{Kind: "email", To: "oncall@example.com", OnFailure: true},
	}
	tests := []struct {
		Name        string
		Plan        *run.Run
		WantTimeout int
		WantTargets []run.NotifyTarget
		WantIntent  string
	}{{ // Test 0: A template launch keeps its timeout and its targets.
		Name: "template launch",
		Plan: &run.Run{ID: "run_plan", Tool: run.ToolTerraform, Command: "infra/legacy",
			Timeout: 900, Notifications: targets},
		WantTimeout: 900, WantTargets: targets,
	}, { // Test 1: A plan proposed from a plain-language request keeps the request.
		Name: "plain-language proposal",
		Plan: &run.Run{ID: "run_intent", Tool: run.ToolTerraform, Command: "infra/legacy",
			Intent: "retire the legacy network"},
		WantIntent: "retire the legacy network",
	}, { // Test 2: A plan with none of them does not have any invented for its apply.
		Name: "none",
		Plan: &run.Run{ID: "run_bare", Tool: run.ToolTerraform, Command: "infra/legacy"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			proposal := &run.Run{}
			run.ApplyOptions(proposal, applyOptions(test.Plan, policies, 3, true))
			if proposal.Timeout != test.WantTimeout {
				t.Errorf("Timeout = %d, want %d", proposal.Timeout, test.WantTimeout)
			}
			diff := cmp.Diff(test.WantTargets, proposal.Notifications, cmpopts.EquateEmpty())
			if diff != "" {
				t.Errorf("Notifications mismatch (-want +got):\n%s", diff)
			}
			if proposal.Intent != test.WantIntent {
				t.Errorf("Intent = %q, want %q", proposal.Intent, test.WantIntent)
			}
			// The targets are the apply's own copy, so a later edit to the plan's cannot redirect
			// where the apply reports.
			if len(test.Plan.Notifications) > 0 && len(proposal.Notifications) > 0 {
				test.Plan.Notifications[0].URL = "https://edited.test"
				if proposal.Notifications[0].URL == "https://edited.test" {
					t.Error("the apply shares its targets with the plan, so editing one edits both")
				}
			}
		})
	}
}

// TestProposedApplyRunsThePlansWholeSpec pins that the apply executes exactly what its plan
// planned, field for field. The apply's options were a list kept beside the plan gate, and a kept
// list falls behind the run model: it lost the plan's timeout. Every field that decides how a run
// executes is set on the plan here, so a field the apply drops fails by name.
func TestProposedApplyRunsThePlansWholeSpec(t *testing.T) {
	t.Parallel()
	plan := &run.Run{
		ID: "run_plan", Tool: run.ToolTerraform, Command: "infra/prod",
		ExtraVars: map[string]any{"region": "us-east-1"}, CredentialIDs: []string{"cred_aws"},
		Tags: []string{"network"}, SkipTags: []string{"slow"}, Verbosity: 2, Forks: 10,
		DiffMode: true, ProjectID: "proj_1", InventoryID: "inv_1", Queue: "dmz", Timeout: 900,
		Image: "registry.example/ee:1", PullCredentialID: "cred_pull", CommitSHA: "abc123",
	}
	proposal := &run.Run{}
	run.ApplyOptions(proposal, applyOptions(plan, nil, 0, true))

	want := &run.Run{
		Tool: run.ToolTerraform, Command: "infra/prod",
		ExtraVars: map[string]any{"region": "us-east-1"}, CredentialIDs: []string{"cred_aws"},
		Tags: []string{"network"}, SkipTags: []string{"slow"}, Verbosity: 2, Forks: 10,
		DiffMode: true, ProjectID: "proj_1", InventoryID: "inv_1", Queue: "dmz", Timeout: 900,
		Image: "registry.example/ee:1", PullCredentialID: "cred_pull", PinnedCommit: "abc123",
	}
	spec := func(r *run.Run) *run.Run {
		return &run.Run{
			Tool: r.Tool, Command: r.Command, DryRun: r.DryRun, ExtraVars: r.ExtraVars,
			CredentialIDs: r.CredentialIDs, Tags: r.Tags, SkipTags: r.SkipTags,
			Verbosity: r.Verbosity, Forks: r.Forks, DiffMode: r.DiffMode, ProjectID: r.ProjectID,
			InventoryID: r.InventoryID, Queue: r.Queue, Timeout: r.Timeout, Image: r.Image,
			PullCredentialID: r.PullCredentialID, PinnedCommit: r.PinnedCommit,
		}
	}
	if diff := cmp.Diff(spec(want), spec(proposal), cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the apply's spec differs from its plan's (-want +got):\n%s", diff)
	}
}
