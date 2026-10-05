package dispatch_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestOwedStartsAndHoldsReachNamedTargetsOnSQLite pins, against a real database, that a run's start
// and its hold reach the named targets attached for them when no announcing path followed the write
// that moved the run, as when its process stopped right after the write. The database marks each
// one owed in the same statement, and the sweep records it once: a run still where the event left
// it is announced, a workflow parked at an approval step is announced as the step it waits at, one
// already recorded by the process that moved the run is not recorded again, and a run that ended
// before the sweep found its start or hold has it recorded unsent, with why, rather than told late.
func TestOwedStartsAndHoldsReachNamedTargetsOnSQLite(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	hook := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(hook.Close)
	targets := db.Notifications()
	n := &notification.Notification{ID: "ntf_owed", Name: "owed", CreatedAt: time.Now()}
	if err := n.SetTarget(run.NotifyTarget{Kind: run.NotifyWebhook, URL: hook.URL + "/hook"},
		endsSealer{}); err != nil {
		t.Fatalf("SetTarget() error = %v", err)
	}
	if err := targets.Save(ctx, n); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, ev := range notification.Events {
		if err := targets.Attach(ctx, &notification.Attachment{ID: notification.NewAttachmentID(),
			NotificationID: n.ID, ObjectKind: notification.KindTemplate, ObjectID: "tpl_owed",
			Event: ev, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
	}
	router := notification.NewRouter(targets, endsSealer{},
		notification.SourceLineage(nil, nil, nil), nil)
	outbox := notification.NewOutbox(targets, router, endsSealer{}, nil,
		notification.WithPoll(10*time.Millisecond))
	runs := db.Runs()
	idle := roundhouse.RunnerFunc(func(context.Context, roundhouse.Spec, io.Writer) (
		roundhouse.Result, error) {
		return roundhouse.Result{}, nil
	})

	now := time.Now().UTC()
	step := 0
	parked := "run_owed_parked"
	tests := []struct {
		Name  string
		Saved []*run.Run
		Move  func() error
		Want  []string
	}{{ // Test 0: A run created held whose process stopped before announcing the hold.
		Name:  "held",
		Saved: []*run.Run{{ID: "run_owed_held", Status: run.StatusPendingApproval}},
		Want:  []string{"approval  delivered"},
	}, { // Test 1: A claimed run started, and its process stopped before announcing the start.
		Name: "started",
		Saved: []*run.Run{{ID: "run_owed_started", Status: run.StatusPending, Queue: "unserved",
			ClaimedBy: "gone", ClaimedAt: &now, ClaimSecret: "secret_1"}},
		Move: func() error {
			_, err := runs.StartClaimed(ctx, "run_owed_started", "gone", "secret_1", now)
			return err
		},
		Want: []string{"started  delivered"},
	}, { // Test 2: A hold the process recorded before it stopped short of settling it, from its
		// own copy of the run, which says more than the stored row the sweep reads.
		Name:  "recorded",
		Saved: []*run.Run{{ID: "run_owed_recorded", Status: run.StatusPendingApproval}},
		Move: func() error {
			r, err := runs.Get(ctx, "run_owed_recorded")
			if err != nil {
				return err
			}
			r.Warning = "held by the process that stopped"
			return outbox.Record(ctx, r, notification.Branch{})
		},
		Want: []string{"approval  delivered"},
	}, { // Test 3: A held run rejected before the sweep found its hold.
		Name:  "rejected",
		Saved: []*run.Run{{ID: "run_owed_rejected", Status: run.StatusPendingApproval}},
		Move: func() error {
			_, err := runs.TransitionStatus(ctx, "run_owed_rejected", run.StatusPendingApproval,
				run.StatusRejected)
			return err
		},
		Want: []string{"approval  skipped not sent: the run had already ended, rejected, before " +
			"its hold could be announced", "failure  delivered"},
	}, { // Test 4: A started run finished before the sweep found its start.
		Name: "finished",
		Saved: []*run.Run{{ID: "run_owed_finished", Status: run.StatusPending, Queue: "unserved",
			ClaimedBy: "gone", ClaimedAt: &now, ClaimSecret: "secret_2"}},
		Move: func() error {
			if _, err := runs.StartClaimed(ctx, "run_owed_finished", "gone", "secret_2",
				now); err != nil {
				return err
			}
			_, err := runs.FinalizeRunning(ctx, "run_owed_finished", run.Finalization{
				Status: run.StatusSucceeded, EndedAt: now, Owner: "gone"})
			return err
		},
		Want: []string{"started  skipped not sent: the run had already ended, succeeded, before " +
			"its start could be announced", "success  delivered"},
	}, { // Test 5: A workflow parked at an approval step, announced as the step it waits at.
		Name: "parked",
		Saved: []*run.Run{{ID: parked, Status: run.StatusRunning, Kind: run.KindPipeline,
			ClaimedBy: "gone", ClaimedAt: &now, StartedAt: &now, Queue: "unserved",
			Steps: []run.PipelineStep{{Name: "gate", Type: run.StepApproval}}}, {
			ID: "run_owed_gate", Status: run.StatusPendingApproval, Kind: run.KindApproval,
			ParentID: &parked, StepName: "gate", StepIndex: &step,
		}},
		Move: func() error {
			_, err := runs.ParkForApproval(ctx, parked, "gone")
			return err
		},
		Want: []string{"approval gate delivered"},
	}}
	for testNum, test := range tests {
		for _, r := range test.Saved {
			r.Playbook, r.CreatedAt = "site.yml", now
			if r.ParentID == nil {
				r.Source, r.SourceID = "template", "tpl_owed"
			}
			if err := runs.Save(ctx, r); err != nil {
				t.Fatalf("test %d %s: Save(%s) error = %v", testNum, test.Name, r.ID, err)
			}
		}
		if test.Move != nil {
			if err := test.Move(); err != nil {
				t.Fatalf("test %d %s: moving the run error = %v", testNum, test.Name, err)
			}
		}
	}
	// The dispatcher starts after every move, standing in for the server that comes back after the
	// one that made them stopped, so none of them was announced by a live process.
	d := dispatch.New(runs, idle, nil, dispatch.WithNotificationOutbox(outbox),
		dispatch.WithNotifyClient(http.DefaultClient))
	t.Cleanup(d.Close)

	deadline := time.Now().Add(20 * time.Second)
	for waiting := true; waiting; {
		waiting = false
		for testNum, test := range tests {
			got := settledDeliveries(t, targets, test.Saved[0].ID)
			if cmp.Equal(test.Want, got) {
				continue
			}
			if time.Now().After(deadline) {
				t.Fatalf("test %d %s: deliveries of %s mismatch (-want +got):\n%s", testNum,
					test.Name, test.Saved[0].ID, cmp.Diff(test.Want, got))
			}
			waiting = true
		}
		time.Sleep(20 * time.Millisecond)
	}
	ledger, ok := runs.(run.EventLedger)
	if !ok {
		t.Fatal("the SQLite run store keeps no ledger of owed starts and holds")
	}
	deadline = time.Now().Add(5 * time.Second)
	for {
		owed, err := ledger.OwedEvents(ctx, 0, 0)
		if err == nil && len(owed) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("starts and holds still owed after they were handled: %v, %v", owed, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
	// Nothing was recorded twice once the sweep had been through every run more than once.
	time.Sleep(3 * time.Second)
	for testNum, test := range tests {
		if got := settledDeliveries(t, targets, test.Saved[0].ID); !cmp.Equal(test.Want, got) {
			t.Errorf("test %d %s: deliveries of %s changed after they settled (-want +got):\n%s",
				testNum, test.Name, test.Saved[0].ID, cmp.Diff(test.Want, got))
		}
	}
}

// settledDeliveries renders the deliveries of one run that have finished as event, branch, status,
// and the reason a skipped one gives, sorted, leaving out any still pending.
func settledDeliveries(t *testing.T, store notification.Store, runID string) []string {
	t.Helper()
	list, err := store.Deliveries(context.Background(), notification.DeliveryFilter{RunID: runID})
	if err != nil {
		t.Fatalf("Deliveries(%s) error = %v", runID, err)
	}
	out := []string{}
	for _, dl := range list {
		if dl.Status == notification.DeliveryPending {
			out = append(out, dl.Event+" "+dl.Branch+" pending")
			continue
		}
		line := dl.Event + " " + dl.Branch + " " + dl.Status
		if dl.Status == notification.DeliverySkipped {
			line += " " + dl.LastError
		}
		out = append(out, strings.TrimSpace(line))
	}
	sort.Strings(out)
	return out
}
