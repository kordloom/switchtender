package dispatch

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// planRunFor returns a finished terraform plan run for the relay proposal path to build an apply
// from.
func planRunFor(id string) *run.Run {
	return &run.Run{
		ID: id, Playbook: "", Inventory: "", Tool: run.ToolTerraform, Command: "infra/prod",
		Status: run.StatusSucceeded, CreatedAt: time.Now(), DryRun: true,
		Actor: "casey", ActorType: "session", ActorUserID: "usr_casey", OrgID: "org_a",
		AuditReceipt: "receipt_1", CommitSHA: "abc123",
	}
}

// TestProposeApplyForFacesTheSameRulesAsASubmit pins the relay's proposal path against the rules a
// direct submission faces. A worker cannot create runs, so the control node builds the apply from the
// plan it holds; if that path skipped the policy pass the same install would refuse a gated apply
// when the control node ran the plan and queue it when a worker did, which is a gate an attacker
// chooses their way around by choosing a worker.
func TestProposeApplyForFacesTheSameRulesAsASubmit(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which rule is in force.
		Name string
		// Policy is the stored rule, or nil for an install with none.
		Policy *policy.Policy
		// Destroys is what the worker's plan reported.
		Destroys int
		// Read reports whether the worker could read the plan summary.
		Read bool
		// WantStatus is the status the proposal must be stored in.
		WantStatus run.Status
		// Want is the error the proposal must return.
		Want error
	}{{ // Test 0: A deny rule refuses the apply outright, so no run is created at all.
		Name: "a deny rule refuses it",
		Policy: &policy.Policy{ID: policy.NewID(), Name: "no-prod-tf", Tool: run.ToolTerraform,
			Effect: policy.EffectDeny, MaxDestroy: -1},
		Destroys: 0, Read: true, Want: ErrPolicyDenied,
	}, { // Test 1: A blanket approval rule holds it for a person.
		Name: "a blanket approval rule holds it",
		Policy: &policy.Policy{ID: policy.NewID(), Name: "tf-needs-signoff",
			Tool: run.ToolTerraform, MaxDestroy: -1},
		Destroys: 0, Read: true, WantStatus: run.StatusPendingApproval,
	}, { // Test 2: A destroy threshold the plan crossed holds it.
		Name: "a destroy threshold it crossed",
		Policy: &policy.Policy{ID: policy.NewID(), Name: "tf-destroy-guard",
			Tool: run.ToolTerraform, MaxDestroy: 3},
		Destroys: 9, Read: true, WantStatus: run.StatusPendingApproval,
	}, { // Test 3: A plan the worker could not read is held rather than queued.
		Name: "an unreadable plan",
		Policy: &policy.Policy{ID: policy.NewID(), Name: "tf-destroy-guard",
			Tool: run.ToolTerraform, MaxDestroy: 3},
		Destroys: 0, Read: false, WantStatus: run.StatusPendingApproval,
	}, { // Test 4: A readable plan under the threshold queues to run.
		Name: "under the threshold",
		Policy: &policy.Policy{ID: policy.NewID(), Name: "tf-destroy-guard",
			Tool: run.ToolTerraform, MaxDestroy: 3},
		Destroys: 1, Read: true, WantStatus: run.StatusPending,
	}, { // Test 5: With no rules at all the apply queues, which is the ungoverned install.
		Name: "no rules", Destroys: 4, Read: true, WantStatus: run.StatusPending,
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := run.NewMemStore()
			plan := planRunFor(fmt.Sprintf("run_plan_%d", testNum))
			if err := store.Save(ctx, plan); err != nil {
				t.Fatalf("Save(plan) error = %v", err)
			}
			var policies []*policy.Policy
			if test.Policy != nil {
				policies = append(policies, test.Policy)
			}

			got, err := ProposeApplyFor(ctx, store, policies, plan, test.Destroys, test.Read)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ProposeApplyFor() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				if got != nil {
					t.Errorf("a refused proposal returned run %s", got.ID)
				}
				runs, lerr := store.List(ctx)
				if lerr != nil {
					t.Fatalf("List() error = %v", lerr)
				}
				for _, r := range runs {
					if r.ProposedFrom == plan.ID {
						t.Errorf("a denied apply was stored anyway as %s", r.ID)
					}
				}
				return
			}

			if got.Status != test.WantStatus {
				t.Errorf("proposal status = %q, want %q", got.Status, test.WantStatus)
			}
			if got.DryRun {
				t.Error("the proposed apply is a dry run, so it would change nothing")
			}
			if got.ProposedFrom != plan.ID {
				t.Errorf("ProposedFrom = %q, want the plan it came from", got.ProposedFrom)
			}
			if got.PolicySet == nil {
				t.Error("the proposal records no rule set in force, so a run under no rules is " +
					"indistinguishable from one whose rules were never captured")
			}
			// The apply is the run that actually destroys things, so its attribution has to survive.
			if got.Actor != plan.Actor || got.ActorType != plan.ActorType {
				t.Errorf("actor = %q/%q, want the plan's %q/%q: an apply attributed to nobody "+
					"matches no actor-scoped rule", got.Actor, got.ActorType, plan.Actor,
					plan.ActorType)
			}
			if got.ActorUserID != plan.ActorUserID {
				t.Errorf("ActorUserID = %q, want %q: separation of duties compares accounts, so an "+
					"apply that lost the account lets the plan's submitter release it",
					got.ActorUserID, plan.ActorUserID)
			}
			if got.OrgID != plan.OrgID {
				t.Errorf("OrgID = %q, want the plan's tenant %q", got.OrgID, plan.OrgID)
			}
			if got.AuditReceipt != plan.AuditReceipt {
				t.Errorf("AuditReceipt = %q, want the plan's %q", got.AuditReceipt, plan.AuditReceipt)
			}
			if got.PinnedCommit != plan.CommitSHA {
				t.Errorf("PinnedCommit = %q, want the commit the plan was read from %q: without the "+
					"pin an approval of one plan releases an apply of different code",
					got.PinnedCommit, plan.CommitSHA)
			}
			if test.WantStatus == run.StatusPendingApproval && got.HeldByPolicy == "" {
				t.Error("a held apply names no rule, so its evidence says nothing held it")
			}
		})
	}
}

// TestProposeApplyForMakesOnlyOneApplyPerPlan pins the idempotency of the relay proposal. A worker
// whose response never arrived retries, which is legitimate, so the second call has to return the
// first proposal rather than mint a second real apply against the same infrastructure.
func TestProposeApplyForMakesOnlyOneApplyPerPlan(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	plan := planRunFor("run_plan_once")
	if err := store.Save(ctx, plan); err != nil {
		t.Fatalf("Save(plan) error = %v", err)
	}

	first, err := ProposeApplyFor(ctx, store, nil, plan, 2, true)
	if err != nil {
		t.Fatalf("ProposeApplyFor(first) error = %v", err)
	}
	second, err := ProposeApplyFor(ctx, store, nil, plan, 2, true)
	if err != nil {
		t.Fatalf("ProposeApplyFor(second) error = %v", err)
	}
	if second.ID != first.ID {
		t.Errorf("a retried report created apply %s, want the first one (%s): two applies means the "+
			"same destruction runs twice", second.ID, first.ID)
	}
	if want := applyKeyFor(plan.ID); first.IdempotencyKey != want {
		t.Errorf("IdempotencyKey = %q, want %q, which is what makes the retry resolve",
			first.IdempotencyKey, want)
	}
	if !strings.HasPrefix(first.IdempotencyKey, "st:") {
		t.Errorf("IdempotencyKey = %q, want the server's reserved prefix so no caller can supply it",
			first.IdempotencyKey)
	}
	// Distinct plans get distinct keys, or one plan's apply would answer another plan's report.
	if applyKeyFor("run_a") == applyKeyFor("run_b") {
		t.Error("two different plans share an apply key")
	}
}

// TestProposeApplyForRefusesWithoutAPlan pins the guard on the input. The apply's whole spec is taken
// from the plan run, so with no plan there is nothing to derive it from, and building one from
// defaults would apply an empty spec.
func TestProposeApplyForRefusesWithoutAPlan(t *testing.T) {
	t.Parallel()
	got, err := ProposeApplyFor(context.Background(), run.NewMemStore(), nil, nil, 0, true)
	if err == nil {
		t.Fatal("ProposeApplyFor reported success with no plan run")
	}
	if got != nil {
		t.Errorf("a refused proposal returned run %s", got.ID)
	}
}

// dupKeyStore reports a duplicate-key collision on the first save and serves the wrapped store after,
// standing in for two submissions racing on one idempotency key.
type dupKeyStore struct {
	run.Store
	// winner is the run already holding the key.
	winner *run.Run
	// collided reports that the collision was served.
	collided bool
}

// Save reports the collision once, then behaves normally.
func (s *dupKeyStore) Save(ctx context.Context, r *run.Run) error {
	if !s.collided && r.IdempotencyKey != "" {
		s.collided = true
		return run.ErrDuplicateKey
	}
	return s.Store.Save(ctx, r)
}

// ByIdempotencyKey returns the run that won the key.
func (s *dupKeyStore) ByIdempotencyKey(ctx context.Context, key string) (*run.Run, error) {
	if s.winner != nil && s.winner.IdempotencyKey == key {
		return s.winner, nil
	}
	return s.Store.ByIdempotencyKey(ctx, key)
}

// TestSubmitResolvesToTheWinnerOfAKeyRace pins the unique-index backstop the pre-check cannot cover.
// Two submissions carrying one key can both pass the lookup and then race to save; the loser must
// resolve to the winner's run rather than fail the caller or create a second run, because a caller
// that retries on an error is exactly the caller that sent the key.
func TestSubmitResolvesToTheWinnerOfAKeyRace(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	winner := &run.Run{
		ID: "run_winner", Tool: run.ToolBash, Command: "deploy", Status: run.StatusPending,
		CreatedAt: time.Now(), IdempotencyKey: "idem_race",
	}
	if err := base.Save(ctx, winner); err != nil {
		t.Fatalf("Save(winner) error = %v", err)
	}
	store := &dupKeyStore{Store: base, winner: winner}
	d := New(store, okRunner(), zap.NewNop(), WithNoJanitor())
	defer d.Close()

	// The pre-check is bypassed by pointing the loser at a key the wrapped lookup does not answer
	// until the save collides.
	got, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"),
		run.WithIdempotencyKey("idem_race"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if got.ID != winner.ID {
		t.Errorf("the losing submission returned run %s, want the winner %s: a caller that retries "+
			"on an error is exactly the caller that sent the key", got.ID, winner.ID)
	}
}

// TestSubmitSplitLosingAKeyRaceCreatesNoShards is the same rule on the fan-out path. The loser must
// return the winner's parent and create no second set of shards, or the same host groups execute
// twice under two parents.
func TestSubmitSplitLosingAKeyRaceCreatesNoShards(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	base := run.NewMemStore()
	count := 2
	winner := &run.Run{
		ID: "run_split_winner", Playbook: "site.yml", Inventory: "inv", Kind: run.KindSplit,
		Status: run.StatusPending, CreatedAt: time.Now(), IdempotencyKey: "idem_split",
		ShardCount: &count,
	}
	if err := base.Save(ctx, winner); err != nil {
		t.Fatalf("Save(winner) error = %v", err)
	}
	store := &dupKeyStore{Store: base, winner: winner}
	runner := &countingRunnerLister{hosts: []string{"web01", "web02", "web03", "web04"}}
	d := New(store, runner, zap.NewNop(), WithNoJanitor())
	defer d.Close()

	got, err := d.SubmitSplit(ctx, "site.yml", "inv", 2, run.WithIdempotencyKey("idem_split"))
	if err != nil {
		t.Fatalf("SubmitSplit() error = %v", err)
	}
	if got.ID != winner.ID {
		t.Fatalf("the losing split returned parent %s, want the winner %s", got.ID, winner.ID)
	}
	shards, err := base.Shards(ctx, winner.ID)
	if err != nil {
		t.Fatalf("Shards() error = %v", err)
	}
	if len(shards) != 0 {
		t.Errorf("%d shards were created under the winner's parent by the losing submission, so the "+
			"same host groups execute twice", len(shards))
	}
}

// TestPinHeldRunCommitLeavesUnpinnableRunsAlone walks the guards on the approval-time commit pin. The
// pin exists so an approver's yes means the code that was current when they read it, and every guard
// here is a run for which there is nothing to pin: it is not held, it names no project, it is pinned
// already, or the install keeps no projects at all.
func TestPinHeldRunCommitLeavesUnpinnableRunsAlone(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says why the run cannot or need not be pinned.
		Name string
		// Run is the run offered to the pin.
		Run *run.Run
		// WantPin is the commit the run should carry afterward.
		WantPin string
	}{{ // Test 0: A run that is not held has no approval to bind.
		Name: "not held",
		Run:  &run.Run{Status: run.StatusPending, ProjectID: "proj_1"},
	}, { // Test 1: A run drawn from no project has no branch that can move.
		Name: "no project",
		Run:  &run.Run{Status: run.StatusPendingApproval},
	}, { // Test 2: A run already pinned keeps the pin it arrived with.
		Name: "already pinned",
		Run: &run.Run{Status: run.StatusPendingApproval, ProjectID: "proj_1",
			PinnedCommit: "deadbeef"},
		WantPin: "deadbeef",
	}, { // Test 3: A terminal run is past the point of being held.
		Name: "already finished",
		Run:  &run.Run{Status: run.StatusSucceeded, ProjectID: "proj_1"},
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cache := t.TempDir()
			syncer, err := project.NewSyncer(cache)
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithNoJanitor(),
				WithProjects(project.NewMemStore(), syncer))
			defer d.Close()

			d.pinHeldRunCommit(test.Run)

			if test.Run.PinnedCommit != test.WantPin {
				t.Errorf("PinnedCommit = %q, want %q", test.Run.PinnedCommit, test.WantPin)
			}
			if test.Run.Warning != "" {
				t.Errorf("Warning = %q, want none: nothing was attempted here", test.Run.Warning)
			}
		})
	}
}

// TestPinHeldRunCommitSaysSoWhenItCannotPin pins the downgrade. Failing to pin is not fatal, because
// the run can still be approved and executed the way it always was, but silently dropping the
// guarantee would leave an approver believing their yes was bound to the commit they read. The run
// says the guarantee is absent instead.
func TestPinHeldRunCommitSaysSoWhenItCannotPin(t *testing.T) {
	t.Parallel()
	cache := t.TempDir()
	syncer, err := project.NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	// The project store is empty, so the lookup for proj_missing fails.
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithNoJanitor(),
		WithProjects(project.NewMemStore(), syncer))
	defer d.Close()

	r := &run.Run{
		ID: "run_unpinnable", Status: run.StatusPendingApproval, ProjectID: "proj_missing",
		CreatedAt: time.Now(),
	}
	d.pinHeldRunCommit(r)

	if r.PinnedCommit != "" {
		t.Errorf("PinnedCommit = %q, want none: nothing could be resolved", r.PinnedCommit)
	}
	if r.Warning == "" {
		t.Fatal("the run carries no warning, so an approver believes their decision is bound to a " +
			"commit when it is not")
	}
	if !strings.Contains(r.Warning, "pinned") {
		t.Errorf("Warning = %q, want it to say the commit could not be pinned", r.Warning)
	}
}

// TestPinHeldRunCommitWithNoProjectStoreIsSilent pins the install with git projects turned off. There
// is no guarantee to lose there, so warning about it on every held run would be noise on every
// approval page.
func TestPinHeldRunCommitWithNoProjectStoreIsSilent(t *testing.T) {
	t.Parallel()
	d := New(run.NewMemStore(), okRunner(), zap.NewNop(), WithNoJanitor())
	defer d.Close()

	r := &run.Run{Status: run.StatusPendingApproval, ProjectID: "proj_1"}
	d.pinHeldRunCommit(r)

	if r.PinnedCommit != "" || r.Warning != "" {
		t.Errorf("run = {pin:%q warning:%q}, want both empty on an install with no projects",
			r.PinnedCommit, r.Warning)
	}
}

// TestCheckPinnedCommitRefusesOnlyARealMismatch pins the enforcement the pin exists for. It must
// refuse a run whose project moved, and must not refuse an ordinary unpinned run, or every run on the
// install stops.
func TestCheckPinnedCommitRefusesOnlyARealMismatch(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name says which pairing is being checked.
		Name string
		// Pinned is the commit the approval bound.
		Pinned string
		// Synced is the commit the project sync produced.
		Synced string
		// Want is the error the check must return.
		Want error
	}{{ // Test 0: An ordinary unpinned run passes, which is nearly every run.
		Name: "unpinned", Pinned: "", Synced: "abc123",
	}, { // Test 1: A pinned run that synced nothing has no contradiction to report.
		Name: "pinned but nothing synced", Pinned: "abc123", Synced: "",
	}, { // Test 2: A pin matching the synced commit is the ordinary approved case.
		Name: "matching", Pinned: "abc123", Synced: "abc123",
	}, { // Test 3: A branch that moved between the approval and the claim is refused.
		Name: "the branch moved", Pinned: "abc123", Synced: "def456", Want: ErrCommitMoved,
	}, { // Test 4: Neither recorded is not a mismatch.
		Name: "neither recorded",
	}}

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r := &run.Run{PinnedCommit: test.Pinned}
			err := stampCommit(r, test.Synced)
			if !errors.Is(err, test.Want) {
				t.Fatalf("stampCommit() error = %v, want %v", err, test.Want)
			}
			if r.CommitSHA != test.Synced {
				t.Errorf("CommitSHA = %q, want the synced commit %q recorded either way",
					r.CommitSHA, test.Synced)
			}
			if test.Want == nil {
				return
			}
			if !strings.Contains(err.Error(), test.Pinned) ||
				!strings.Contains(err.Error(), test.Synced) {
				t.Errorf("error = %q, want it to name both commits so the operator can see what "+
					"changed", err)
			}
		})
	}
}

// TestNotifierRegisteredReportsTheRegistry pins the lookup a caller uses before wiring a channel. The
// registry is populated at startup and read while serving, so a lookup that answered wrongly would
// have a process either skip a configured channel or configure one twice.
func TestNotifierRegisteredReportsTheRegistry(t *testing.T) {
	t.Parallel()
	if NotifierRegistered("a-notifier-nobody-registered") {
		t.Error("NotifierRegistered reported a name that was never registered")
	}
	if NotifierRegistered("") {
		t.Error("NotifierRegistered reported the empty name, which cannot be registered")
	}
}
