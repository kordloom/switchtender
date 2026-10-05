package attention_test

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/attention"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/user"
)

// timing is the dispatcher's real lease timing.
var timing = attention.DefaultTiming(30*time.Second, 3*time.Second, 10*time.Second,
	10*time.Second)

// fixture is a run store, a presence store, and an account store holding one account.
type fixture struct {
	// runs is the run store.
	runs run.Store
	// presence holds worker reports and raised alerts.
	presence attention.Store
	// source reads both.
	source *attention.Source
}

// newFixture builds stores with the dev-lead account and a source over them.
func newFixture(t *testing.T) fixture {
	t.Helper()
	ctx := context.Background()
	users := user.NewMemStore()
	if err := users.Save(ctx, &user.User{ID: "usr_lead", Username: "dev-lead",
		Role: user.RoleAdmin}); err != nil {
		t.Fatalf("Save(user) error = %v", err)
	}
	f := fixture{runs: run.NewMemStore(), presence: attention.NewMemStore()}
	f.source = &attention.Source{Runs: f.runs, Presence: f.presence,
		Schedules: schedule.NewMemStore(), Users: users, Timing: timing}
	return f
}

// TestSourceReadsTheStores pins that a snapshot reads every store an evaluation needs: an approved
// run's queue time, the workers' reports, and the account behind a run.
func TestSourceReadsTheStores(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	created := time.Now().Add(-3 * time.Hour)
	for _, r := range []*run.Run{
		{ID: "run_released", Playbook: "site.yml", Status: run.StatusPendingApproval,
			Queue: "prod", CreatedAt: created},
		{ID: "run_held", Playbook: "deploy.yml", Status: run.StatusPendingApproval,
			CreatedAt: created, RequireDistinctApprover: true, Actor: "deploy-bot",
			ActorType: "agent", ActorUserID: "usr_lead"},
		{ID: "run_default", Playbook: "ping.yml", Status: run.StatusPending, CreatedAt: created},
	} {
		if err := f.runs.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	// An approval releases the first run into the queue, which is a new wait.
	if moved, err := f.runs.TransitionStatus(ctx, "run_released", run.StatusPendingApproval,
		run.StatusPending); err != nil || !moved {
		t.Fatalf("TransitionStatus() = %v, %v", moved, err)
	}
	// A server serving the default queue is connected, and nothing serves prod.
	if err := f.presence.NoteWorker(ctx, "server-1", []string{""}, 4); err != nil {
		t.Fatalf("NoteWorker() error = %v", err)
	}
	snap, err := f.source.Snapshot(ctx)
	if err != nil {
		t.Fatalf("Snapshot() error = %v", err)
	}
	byKey := map[string]attention.Item{}
	for _, it := range snap.Items {
		byKey[it.Key] = it
	}
	tests := []struct {
		Key         string
		WantBlocker attention.Blocker
		WantRecent  bool
		WantExclude string
	}{{ // Test 0: The released run waits for a worker from its release, not its creation.
		Key: "run_released", WantBlocker: attention.NoWorker, WantRecent: true,
	}, { // Test 1: The held run names the account its agent acts for as the one that cannot approve.
		Key: "run_held", WantBlocker: attention.ApprovalNeeded, WantExclude: "dev-lead",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			it, ok := byKey[test.Key]
			if !ok {
				t.Fatalf("no item %s", test.Key)
			}
			if diff := cmp.Diff(test.WantBlocker, it.Blocker); diff != "" {
				t.Errorf("blocker mismatch (-want +got):\n%s", diff)
			}
			recent := time.Since(it.Since) < time.Minute
			if diff := cmp.Diff(test.WantRecent, recent); diff != "" {
				t.Errorf("recent wait mismatch (-want +got):\n%s since %v", diff, it.Since)
			}
			excluded := ""
			if it.Main.Approval != nil {
				excluded = it.Main.Approval.Approvers.Excluded
			}
			if diff := cmp.Diff(test.WantExclude, excluded); diff != "" {
				t.Errorf("excluded approver mismatch (-want +got):\n%s", diff)
			}
		})
	}
	if _, ok := byKey["run_default"]; ok {
		t.Errorf("the run on the default queue is listed, but a connected worker serves it")
	}
}

// alertSink records the alerts a monitor raises.
type alertSink struct {
	// mu guards got.
	mu sync.Mutex
	// got are the alerts raised, in order.
	got []*run.Run
}

// NotifyAttention records r.
func (s *alertSink) NotifyAttention(r *run.Run) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.got = append(s.got, r)
}

// TestMonitorAlertsOnceAcrossReplicas pins that two monitors sharing a store, two replicas on one
// database, raise one alert for a condition between them, and raise nothing for an item that is not
// past its threshold.
func TestMonitorAlertsOnceAcrossReplicas(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	f := newFixture(t)
	stale := time.Now().Add(-2 * time.Minute)
	fresh := time.Now()
	for _, r := range []*run.Run{
		{ID: "run_lost", Playbook: "deploy.yml", Status: run.StatusRunning, CreatedAt: stale,
			ClaimedBy: "worker-gone", ClaimedAt: &stale},
		{ID: "run_new", Playbook: "ping.yml", Status: run.StatusPending, Queue: "prod",
			CreatedAt: fresh},
	} {
		if err := f.runs.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	sink := &alertSink{}
	first := attention.NewMonitor(f.source, f.presence, sink, nil, time.Hour)
	second := attention.NewMonitor(f.source, f.presence, sink, nil, time.Hour)
	tests := []struct {
		Monitor    *attention.Monitor
		WantRaised int
	}{{ // Test 0: The first replica raises the lost worker's alert.
		Monitor: first, WantRaised: 1,
	}, { // Test 1: The second replica finds it already raised.
		Monitor: second, WantRaised: 0,
	}, { // Test 2: The first replica does not raise it again.
		Monitor: first, WantRaised: 0,
	}}
	for testNum, test := range tests {
		raised, err := test.Monitor.Sweep(ctx)
		if err != nil {
			t.Fatalf("test %d: Sweep() error = %v", testNum, err)
		}
		if diff := cmp.Diff(test.WantRaised, raised); diff != "" {
			t.Errorf("test %d: raised mismatch (-want +got):\n%s", testNum, diff)
		}
	}
	type note struct {
		// RunID is the run the alert is about.
		RunID string
		// Blocker is what stopped it.
		Blocker string
		// HasSummary reports the alert carries a one-line summary.
		HasSummary bool
	}
	var got []note
	for _, r := range sink.got {
		got = append(got, note{RunID: r.ID, Blocker: r.Attention.Blocker,
			HasSummary: r.Attention.Summary != ""})
	}
	want := []note{{RunID: "run_lost", Blocker: string(attention.WorkerLost), HasSummary: true}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("alerts mismatch (-want +got):\n%s", diff)
	}
}
