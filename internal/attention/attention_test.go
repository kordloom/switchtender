package attention

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
)

// now is the store clock every evaluation in these tests runs at.
var now = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

// timing is the dispatcher's real lease timing: a thirty second lease renewed every three seconds,
// swept every ten, with workers reporting every ten.
var timing = DefaultTiming(30*time.Second, 3*time.Second, 10*time.Second, 10*time.Second)

// ago returns the instant d before now.
func ago(d time.Duration) time.Time { return now.Add(-d) }

// at returns a pointer to the instant d before now.
func at(d time.Duration) *time.Time {
	t := ago(d)
	return &t
}

// ptr returns a pointer to v.
func ptr[T any](v T) *T { return &v }

// itemView is the part of an item a test compares.
type itemView struct {
	// Key identifies the item.
	Key string
	// Kind is run, workflow, split, or schedule.
	Kind string
	// Blocker is the main blocker.
	Blocker Blocker
	// Badges are the other conditions' blockers.
	Badges []Blocker
	// Since is when the item entered its main blocker.
	Since time.Time
	// Alerting reports the item is past its alert threshold.
	Alerting bool
	// MainRun is the run the main condition is on.
	MainRun string
	// Holder names the run a blocked item waits behind.
	Holder string
	// Scope is the approval scope, empty for anything else.
	Scope string
	// WorkflowState says whether a workflow waiting at a step is running or paused.
	WorkflowState string
	// Excluded names the account that may not approve.
	Excluded string
	// Agent names the agent acting for that account.
	Agent string
	// Eligible is how many connected workers serve the queue, minus one when not stated.
	Eligible int
	// Interacts reports the item says how its conditions interact.
	Interacts bool
}

// view reduces an item to what the tests compare.
func view(it Item) itemView {
	v := itemView{Key: it.Key, Kind: it.Kind, Blocker: it.Blocker, Since: it.Since,
		Alerting: it.Alerting, MainRun: it.Main.RunID, Holder: it.Main.HolderRunID, Eligible: -1,
		Interacts: it.Interaction != ""}
	for _, b := range it.Badges {
		v.Badges = append(v.Badges, b.Blocker)
	}
	if ap := it.Main.Approval; ap != nil {
		v.Scope, v.WorkflowState = ap.Scope, ap.WorkflowState
		v.Excluded, v.Agent = ap.Approvers.Excluded, ap.Approvers.Agent
	}
	if it.Main.EligibleWorkers != nil {
		v.Eligible = *it.Main.EligibleWorkers
	}
	return v
}

// pipeline is a running workflow with a step whose worker was lost and an approval step waiting.
func pipeline(status run.Status, claimed bool) []*run.Run {
	steps := []run.PipelineStep{
		{Name: "build", Tool: run.ToolBash, Command: "build"},
		{Name: "migrate", Tool: run.ToolBash, Command: "migrate"},
		{Name: "gate", Type: run.StepApproval, DependsOn: []string{"build"}},
		{Name: "deploy", Tool: run.ToolBash, Command: "deploy", DependsOn: []string{"gate"}},
	}
	parent := &run.Run{ID: "run_wf", Playbook: "release", Kind: run.KindPipeline, Status: status,
		CreatedAt: ago(time.Hour), Steps: steps}
	if claimed {
		parent.ClaimedBy, parent.ClaimedAt = "server-1", at(time.Second)
	}
	pid := parent.ID
	gate := &run.Run{ID: "run_gate", ParentID: &pid, Kind: run.KindApproval, StepName: "gate",
		StepIndex: ptr(2), Status: run.StatusPendingApproval, CreatedAt: ago(20 * time.Minute)}
	return []*run.Run{parent, gate}
}

// TestEvaluate pins what each blocker looks like and when it begins: what is stopping each item,
// its main blocker and badges, the time it has been in its current blocker, and whether it alerts.
func TestEvaluate(t *testing.T) {
	t.Parallel()
	fresh := []Worker{{Owner: "server-1", Queues: []string{""}, Slots: 4,
		FirstSeen: ago(time.Hour), LastSeen: ago(5 * time.Second)}}
	agentHeld := &run.Run{ID: "run_held", Playbook: "deploy.yml", Status: run.StatusPendingApproval,
		CreatedAt: ago(10 * time.Minute), HeldByPolicy: "prod changes", RequireDistinctApprover: true,
		Actor: "deploy-bot", ActorType: "agent", ActorUserID: "usr_lead"}
	tests := []struct {
		Runs      []*run.Run
		Queued    map[string]time.Time
		Workers   []Worker
		Schedules []*schedule.Schedule
		Config    string
		WantItems []itemView
	}{{ // Test 0: A run whose lease was just renewed needs nothing.
		Runs: []*run.Run{{ID: "run_a", Status: run.StatusRunning, CreatedAt: ago(time.Minute),
			ClaimedBy: "server-1", ClaimedAt: at(2 * time.Second)}},
		Workers: fresh,
	}, { // Test 1: A lease three renewals overdue is a lost worker, not yet alerting.
		Runs: []*run.Run{{ID: "run_a", Status: run.StatusRunning, CreatedAt: ago(time.Minute),
			ClaimedBy: "worker-gone", ClaimedAt: at(12 * time.Second)}},
		Workers: fresh,
		WantItems: []itemView{{Key: "run_a", Kind: "run", Blocker: WorkerLost,
			Since: ago(12 * time.Second), MainRun: "run_a", Eligible: -1}},
	}, { // Test 2: A lost worker's run still held past two lease periods alerts.
		Runs: []*run.Run{{ID: "run_a", Status: run.StatusRunning, CreatedAt: ago(time.Hour),
			ClaimedBy: "worker-gone", ClaimedAt: at(61 * time.Second)}},
		Workers: fresh,
		WantItems: []itemView{{Key: "run_a", Kind: "run", Blocker: WorkerLost,
			Since: ago(61 * time.Second), Alerting: true, MainRun: "run_a", Eligible: -1}},
	}, { // Test 3: A run on a queue no worker serves has no worker, from when it was created.
		Runs: []*run.Run{{ID: "run_q", Status: run.StatusPending, Queue: "prod",
			CreatedAt: ago(16 * time.Minute)}},
		Workers: fresh,
		WantItems: []itemView{{Key: "run_q", Kind: "run", Blocker: NoWorker,
			Since: ago(16 * time.Minute), Alerting: true, MainRun: "run_q", Eligible: 0}},
	}, { // Test 4: A run a connected worker with a free slot serves is just queued.
		Runs:    []*run.Run{{ID: "run_q", Status: run.StatusPending, CreatedAt: ago(time.Hour)}},
		Workers: fresh,
	}, { // Test 5: An approved run has waited for a worker only since the approval released it.
		Runs: []*run.Run{{ID: "run_q", Status: run.StatusPending, Queue: "prod",
			CreatedAt: ago(5 * time.Hour)}},
		Queued:  map[string]time.Time{"run_q": ago(3 * time.Minute)},
		Workers: fresh,
		WantItems: []itemView{{Key: "run_q", Kind: "run", Blocker: NoWorker,
			Since: ago(3 * time.Minute), MainRun: "run_q", Eligible: 0}},
	}, { // Test 6: The wait for a worker starts when the last one serving the queue went silent.
		Runs: []*run.Run{{ID: "run_q", Status: run.StatusPending, Queue: "prod",
			CreatedAt: ago(20 * time.Minute)}},
		Workers: []Worker{{Owner: "worker-prod", Queues: []string{"prod"}, Slots: 2,
			FirstSeen: ago(time.Hour), LastSeen: ago(5 * time.Minute)}},
		WantItems: []itemView{{Key: "run_q", Kind: "run", Blocker: NoWorker,
			Since: ago(5 * time.Minute), MainRun: "run_q", Eligible: 0}},
	}, { // Test 7: Every worker serving the queue full past the threshold is blocked, held by the
		// longest running run.
		Runs: []*run.Run{
			{ID: "run_q", Status: run.StatusPending, CreatedAt: ago(20 * time.Minute)},
			{ID: "run_busy", Status: run.StatusRunning, CreatedAt: ago(time.Hour),
				StartedAt: at(50 * time.Minute), ClaimedBy: "server-1", ClaimedAt: at(time.Second)},
		},
		Workers: []Worker{{Owner: "server-1", Queues: []string{""}, Slots: 1,
			FirstSeen: ago(time.Hour), LastSeen: ago(5 * time.Second)}},
		WantItems: []itemView{{Key: "run_q", Kind: "run", Blocker: Blocked,
			Since: ago(20 * time.Minute), Alerting: true, MainRun: "run_q", Holder: "run_busy",
			Eligible: 1}},
	}, { // Test 8: The same full worker short of the threshold is just a queue.
		Runs: []*run.Run{
			{ID: "run_q", Status: run.StatusPending, CreatedAt: ago(5 * time.Minute)},
			{ID: "run_busy", Status: run.StatusRunning, CreatedAt: ago(time.Hour),
				ClaimedBy: "server-1", ClaimedAt: at(time.Second)},
		},
		Workers: []Worker{{Owner: "server-1", Queues: []string{""}, Slots: 1,
			FirstSeen: ago(time.Hour), LastSeen: ago(5 * time.Second)}},
	}, { // Test 9: A worker that never said how many runs it takes is not read as full.
		Runs: []*run.Run{
			{ID: "run_q", Status: run.StatusPending, CreatedAt: ago(time.Hour)},
			{ID: "run_busy", Status: run.StatusRunning, CreatedAt: ago(time.Hour),
				ClaimedBy: "relay-1", ClaimedAt: at(time.Second)},
		},
		Workers: []Worker{{Owner: "relay-1", Queues: []string{""}, FirstSeen: ago(time.Hour),
			LastSeen: ago(5 * time.Second)}},
	}, { // Test 10: A held run asks an admin other than the account an agent acts for, and never an
		// agent. Approvals do not alert by default.
		Runs:    []*run.Run{agentHeld},
		Workers: fresh,
		WantItems: []itemView{{Key: "run_held", Kind: "run", Blocker: ApprovalNeeded,
			Since: ago(10 * time.Minute), MainRun: "run_held", Scope: ScopeRun,
			Excluded: "dev-lead", Agent: "deploy-bot", Eligible: -1}},
	}, { // Test 11: An approval age alert a file turns on fires once the wait passes it.
		Runs:    []*run.Run{agentHeld},
		Workers: fresh,
		Config:  "defaults:\n  alert_approval: 5m\n",
		WantItems: []itemView{{Key: "run_held", Kind: "run", Blocker: ApprovalNeeded,
			Since: ago(10 * time.Minute), Alerting: true, MainRun: "run_held", Scope: ScopeRun,
			Excluded: "dev-lead", Agent: "deploy-bot", Eligible: -1}},
	}, { // Test 12: A workflow paused at an approval step waits on the step, not as a held run.
		Runs:    pipeline(run.StatusPendingApproval, false),
		Workers: fresh,
		WantItems: []itemView{{Key: "run_wf", Kind: "workflow", Blocker: ApprovalNeeded,
			Since: ago(20 * time.Minute), MainRun: "run_gate", Scope: ScopeWorkflowStep,
			WorkflowState: WorkflowPaused, Eligible: -1}},
	}, { // Test 13: A lost worker on one branch outranks an approval on another, which becomes a
		// badge, and the item says how the two bear on each other.
		Runs: append(pipeline(run.StatusRunning, true), &run.Run{ID: "run_migrate",
			ParentID: ptr("run_wf"), StepName: "migrate", StepIndex: ptr(1),
			Status: run.StatusRunning, CreatedAt: ago(30 * time.Minute), ClaimedBy: "worker-gone",
			ClaimedAt: at(20 * time.Second)}),
		Workers: fresh,
		WantItems: []itemView{{Key: "run_wf", Kind: "workflow", Blocker: WorkerLost,
			Badges: []Blocker{ApprovalNeeded}, Since: ago(20 * time.Second),
			MainRun: "run_migrate", Eligible: -1, Interacts: true}},
	}, { // Test 14: A held split is one item, and its held shards are not items of their own.
		Runs: []*run.Run{
			{ID: "run_split", Kind: run.KindSplit, Status: run.StatusPendingApproval,
				CreatedAt: ago(time.Minute), ShardCount: ptr(2)},
			{ID: "run_s0", ParentID: ptr("run_split"), ShardIndex: ptr(0),
				Status: run.StatusPendingApproval, CreatedAt: ago(time.Minute)},
			{ID: "run_s1", ParentID: ptr("run_split"), ShardIndex: ptr(1),
				Status: run.StatusPendingApproval, CreatedAt: ago(time.Minute)},
		},
		Workers: fresh,
		WantItems: []itemView{{Key: "run_split", Kind: "split", Blocker: ApprovalNeeded,
			Since: ago(time.Minute), MainRun: "run_split", Scope: ScopeRun, Eligible: -1}},
	}, { // Test 15: A schedule skipping its fires behind its own unfinished run past the threshold
		// is blocked, naming the run holding it.
		Runs: []*run.Run{{ID: "run_sched", Status: run.StatusRunning, Source: "schedule",
			SourceID: "sch_nightly", CreatedAt: ago(40 * time.Minute), ClaimedBy: "server-1",
			ClaimedAt: at(time.Second)}},
		Workers: fresh,
		Schedules: []*schedule.Schedule{{ID: "sch_nightly", Name: "nightly", Cron: "*/30 * * * *",
			Timezone: "UTC", Enabled: true, LastRunAt: at(40 * time.Minute),
			LastRunID: "run_sched"}},
		WantItems: []itemView{{Key: "schedule:sch_nightly", Kind: "schedule", Blocker: Blocked,
			Since: ago(30 * time.Minute), Alerting: true, MainRun: "run_sched",
			Holder: "run_sched", Eligible: -1}},
	}, { // Test 16: A skipped fire short of the threshold is not shown.
		Runs: []*run.Run{{ID: "run_sched", Status: run.StatusRunning, Source: "schedule",
			SourceID: "sch_nightly", CreatedAt: ago(15 * time.Minute), ClaimedBy: "server-1",
			ClaimedAt: at(time.Second)}},
		Workers: fresh,
		Schedules: []*schedule.Schedule{{ID: "sch_nightly", Name: "nightly", Cron: "*/10 * * * *",
			Timezone: "UTC", Enabled: true, LastRunAt: at(15 * time.Minute),
			LastRunID: "run_sched"}},
	}, { // Test 17: A per-queue threshold turns the no worker alert on sooner for that queue.
		Runs: []*run.Run{{ID: "run_q", Status: run.StatusPending, Queue: "prod",
			CreatedAt: ago(3 * time.Minute)}},
		Workers: fresh,
		Config:  "queues:\n  prod:\n    alert_no_worker: 2m\n",
		WantItems: []itemView{{Key: "run_q", Kind: "run", Blocker: NoWorker,
			Since: ago(3 * time.Minute), Alerting: true, MainRun: "run_q", Eligible: 0}},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var cfg *Config
			if test.Config != "" {
				var err error
				if cfg, err = ParseConfig([]byte(test.Config)); err != nil {
					t.Fatalf("ParseConfig() error = %v", err)
				}
			}
			snap := Evaluate(Input{
				Now: now, Runs: test.Runs, Queued: test.Queued, Workers: test.Workers,
				Schedules: test.Schedules, Config: cfg, Timing: timing,
				Accounts: func(id string) string {
					if id == "usr_lead" {
						return "dev-lead"
					}
					return ""
				},
				NextSteps: func(_, _ *run.Run) ([]string, []string, bool) {
					return []string{"deploy"}, nil, true
				},
			})
			var got []itemView
			for _, it := range snap.Items {
				got = append(got, view(it))
			}
			if diff := cmp.Diff(test.WantItems, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("Evaluate() items mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEvaluateCountsAndOrder pins that the four counts count items by their main blocker, so they
// add up to the list, and that the list leads with the most urgent blocker, oldest first within it.
func TestEvaluateCountsAndOrder(t *testing.T) {
	t.Parallel()
	runs := []*run.Run{
		{ID: "run_held_new", Status: run.StatusPendingApproval, CreatedAt: ago(time.Minute)},
		{ID: "run_held_old", Status: run.StatusPendingApproval, CreatedAt: ago(time.Hour)},
		{ID: "run_q", Status: run.StatusPending, Queue: "prod", CreatedAt: ago(time.Minute)},
		{ID: "run_lost", Status: run.StatusRunning, CreatedAt: ago(time.Minute),
			ClaimedBy: "worker-gone", ClaimedAt: at(20 * time.Second)},
	}
	snap := Evaluate(Input{Now: now, Runs: runs, Timing: timing})
	wantCounts := Counts{ApprovalNeeded: 2, NoWorker: 1, WorkerLost: 1}
	if diff := cmp.Diff(wantCounts, snap.Counts); diff != "" {
		t.Errorf("Counts mismatch (-want +got):\n%s", diff)
	}
	var keys []string
	for _, it := range snap.Items {
		keys = append(keys, it.Key)
	}
	want := []string{"run_lost", "run_q", "run_held_old", "run_held_new"}
	if diff := cmp.Diff(want, keys); diff != "" {
		t.Errorf("item order mismatch (-want +got):\n%s", diff)
	}
}

// TestEvaluateWording pins the sentences an operator reads: a retry reads as a reason, a step's
// approval says what each answer runs next, and a lost worker's run counts down to its reclaim.
func TestEvaluateWording(t *testing.T) {
	t.Parallel()
	parent := &run.Run{ID: "run_flaky", Kind: run.KindPipeline, Status: run.StatusRunning,
		CreatedAt: ago(time.Hour), ClaimedBy: "server-1", ClaimedAt: at(time.Second),
		Steps: []run.PipelineStep{{Name: "flaky", Tool: run.ToolBash, Command: "x", Retries: 2}}}
	retry := &run.Run{ID: "run_retry", ParentID: ptr("run_flaky"), StepName: "flaky",
		StepIndex: ptr(0), Attempt: 1, Status: run.StatusPending, Queue: "prod",
		CreatedAt: ago(time.Minute)}
	lost := &run.Run{ID: "run_lost", Status: run.StatusRunning, CreatedAt: ago(time.Minute),
		ClaimedBy: "worker-gone", ClaimedAt: at(12 * time.Second)}
	gate := pipeline(run.StatusPendingApproval, false)
	snap := Evaluate(Input{Now: now, Runs: append([]*run.Run{parent, retry, lost}, gate...),
		Timing: timing,
		NextSteps: func(_, _ *run.Run) ([]string, []string, bool) {
			return []string{"deploy"}, nil, true
		}})
	byKey := map[string]Item{}
	for _, it := range snap.Items {
		byKey[it.Key] = it
	}
	tests := []struct {
		Key      string
		Field    func(Item) string
		WantPart string
	}{{ // Test 0: A retried step says which retry it is.
		Key: "run_flaky", Field: func(it Item) string { return it.Main.Reason },
		WantPart: "This is retry 1 of 2.",
	}, { // Test 1: A step's approval names what an approval runs.
		Key:      "run_wf",
		Field:    func(it Item) string { return approvalOf(it, gate[1].ID).OnApprove },
		WantPart: "Runs deploy.",
	}, { // Test 2: A denial nothing handles fails the workflow, and the item says so.
		Key:      "run_wf",
		Field:    func(it Item) string { return approvalOf(it, gate[1].ID).OnDeny },
		WantPart: "No step handles a denial, so the workflow fails.",
	}, { // Test 3: A lost worker's run counts down to its reclaim.
		Key: "run_lost", Field: func(it Item) string { return it.Main.Next },
		WantPart: "Reclaimed automatically in 18s",
	}, { // Test 4: Nobody has to act before the reclaim is due.
		Key: "run_lost", Field: func(it Item) string { return it.Main.WhoCanAct },
		WantPart: "Nobody yet.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			it, ok := byKey[test.Key]
			if !ok {
				t.Fatalf("no item %s among %v", test.Key, snap.Items)
			}
			if got := test.Field(it); !strings.Contains(got, test.WantPart) {
				t.Errorf("text %q does not contain %q", got, test.WantPart)
			}
		})
	}
}

// TestApproverSentence pins who the sentence says can decide. Denying is admin work, so the
// sentence gives the denial to any admin and never to the excluded account, whose role may not
// allow it and which, for an agent's run, stands for an account the agent acts for.
func TestApproverSentence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantResult string
		In         Approvers
	}{{ // Test 0: No rule excludes anyone, so any admin decides.
		In:         Approvers{Role: "admin"},
		WantResult: "An admin can approve or deny it. No agent can approve it.",
	}, { // Test 1: The account that asked may not approve, and is not told it may deny.
		In: Approvers{Role: "admin", Excluded: "deploy-bot"},
		WantResult: "An admin other than deploy-bot can approve it, because the rule that " +
			"held it requires a different person from the one who asked. Any admin can deny " +
			"it. No agent can approve it.",
	}, { // Test 2: An agent's run excludes the account it acts for, and names the agent.
		In: Approvers{Role: "admin", Excluded: "dev-lead", Agent: "deploy-bot"},
		WantResult: "An admin other than dev-lead, the account agent deploy-bot acts for, can " +
			"approve it, because the rule that held it requires a different person from the one " +
			"who asked. Any admin can deny it. No agent can approve it.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantResult, approverSentence(test.In)); diff != "" {
				t.Errorf("approverSentence() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// approvalOf returns the approval detail of the condition on stepID, the main one or a badge.
func approvalOf(it Item, stepID string) *Approval {
	for _, c := range append([]Condition{it.Main}, it.Badges...) {
		if c.RunID == stepID && c.Approval != nil {
			return c.Approval
		}
	}
	return &Approval{}
}

// TestAlertKeyFollowsTheCondition pins that an alert's key stays the same for as long as one
// condition lasts and changes when a new one begins, which is what lets the store raise each once.
func TestAlertKeyFollowsTheCondition(t *testing.T) {
	t.Parallel()
	lost := func(renewed time.Duration) []*run.Run {
		return []*run.Run{{ID: "run_a", Status: run.StatusRunning, CreatedAt: ago(time.Hour),
			ClaimedBy: "worker-gone", ClaimedAt: at(renewed)}}
	}
	key := func(runs []*run.Run, clock time.Time) string {
		snap := Evaluate(Input{Now: clock, Runs: runs, Timing: timing})
		if len(snap.Items) != 1 {
			t.Fatalf("Evaluate() returned %d items, want 1", len(snap.Items))
		}
		return snap.Items[0].AlertKey
	}
	tests := []struct {
		First, Second string
		WantSame      bool
	}{{ // Test 0: The same lost lease evaluated a minute later is the same alert.
		First: key(lost(70*time.Second), now), Second: key(lost(70*time.Second),
			now.Add(time.Minute)), WantSame: true,
	}, { // Test 1: A lease renewed and lost again is a new alert.
		First: key(lost(70*time.Second), now), Second: key(lost(15*time.Second), now),
		WantSame: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantSame, test.First == test.Second); diff != "" {
				t.Errorf("same key mismatch (-want +got):\n%s\nfirst %s\nsecond %s", diff,
					test.First, test.Second)
			}
		})
	}
}
