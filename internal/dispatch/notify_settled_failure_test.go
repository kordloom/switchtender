package dispatch

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
)

// TestOutboxHearsARunTheJanitorSettled covers the runs no executor is left to finish. A named
// target attached for failure is promised every top-level run that fails or is interrupted, and the
// run's started event already reached it. When the worker running it dies, the janitor interrupts
// the run once its lease goes stale, and when a worker keeps its lease past the run's timeout, the
// janitor ends the run as failed. Both commit the outcome to the chain, and neither records the
// event for the run's named targets, so the target that heard the run start never hears that it
// stopped. The incident a failure target exists for, a change that died mid-flight, is the one it
// is never told about.
func TestOutboxHearsARunTheJanitorSettled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Build      func(now time.Time) *run.Run
		WantStatus run.Status
	}{{ // Test 0: The worker died, its lease went stale, and the sweep interrupted the run.
		Name: "worker lost",
		Build: func(now time.Time) *run.Run {
			stale := now.Add(-10 * time.Minute)
			return &run.Run{
				ID: "run_obx_lost", Status: run.StatusRunning, CreatedAt: stale,
				Tool: run.ToolBash, Command: "deploy", Source: "template", SourceID: "tpl_named",
				ClaimedBy: "worker-that-died", ClaimedAt: &stale, StartedAt: &stale,
			}
		},
		WantStatus: run.StatusInterrupted,
	}, { // Test 1: The worker kept its lease, the run outlived its timeout, and the node ended it.
		Name: "overrun",
		Build: func(now time.Time) *run.Run {
			started := now.Add(-10 * time.Minute)
			return &run.Run{
				ID: "run_obx_overrun", Status: run.StatusRunning, CreatedAt: started,
				Tool: run.ToolBash, Command: "deploy", Source: "template", SourceID: "tpl_named",
				ClaimedBy: "worker-still-beating", ClaimedAt: &now, StartedAt: &started,
				Timeout: 1,
			}
		},
		WantStatus: run.StatusFailed,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			recv := &scriptedReceiver{}
			srv := httptest.NewServer(http.HandlerFunc(recv.serve))
			t.Cleanup(srv.Close)
			targets := obxAttachedHook(t, srv.URL+"/hook")
			router := named.NewRouter(targets, plainSealer{}, named.SourceLineage(nil, nil, nil),
				nil)
			outbox := named.NewOutbox(targets, router, plainSealer{}, nil,
				named.WithPoll(10*time.Millisecond))

			runs := run.NewMemStore()
			orphan := test.Build(time.Now())
			if err := runs.Save(ctx, orphan); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			audits := audit.NewMemStore()
			// The janitor sweeps once as the dispatcher starts, which is the restart case too.
			d := New(runs, okRunner(), nil, WithAudits(audits), WithNotificationOutbox(outbox),
				WithNotifyClient(http.DefaultClient))
			t.Cleanup(d.Close)

			waitUntil(t, "the janitor to settle the run and commit its outcome", func() bool {
				got, err := runs.Get(ctx, orphan.ID)
				if err != nil || got.Status != test.WantStatus {
					return false
				}
				chain, err := audits.Chain(ctx)
				if err != nil {
					return false
				}
				for _, e := range chain {
					if strings.HasPrefix(e.Path, "/runs/"+orphan.ID+"/outcome/") {
						return true
					}
				}
				return false
			})

			// The outcome is on the chain, so the sweep that settled the run is done with it. Its
			// failure event is recorded on the same path or not at all.
			deadline := time.Now().Add(3 * time.Second)
			for time.Now().Before(deadline) {
				list, err := targets.Deliveries(ctx, named.DeliveryFilter{RunID: orphan.ID})
				if err != nil {
					t.Fatalf("Deliveries() error = %v", err)
				}
				for _, dl := range list {
					if dl.Event == named.EventFailure {
						return
					}
				}
				time.Sleep(20 * time.Millisecond)
			}
			list, _ := targets.Deliveries(ctx, named.DeliveryFilter{RunID: orphan.ID})
			hits, _ := recv.snapshot()
			t.Fatalf("the janitor settled %s as %s and committed its outcome, and the target "+
				"attached for failure was never told: deliveries = %+v, received = %+v",
				orphan.ID, test.WantStatus, list, hits)
		})
	}
}

// obxAttachedHook stores a webhook target at url, attached to template tpl_named for every event,
// and returns the store holding it.
func obxAttachedHook(t *testing.T, url string) named.Store {
	t.Helper()
	ctx := context.Background()
	store := named.NewMemStore()
	n := &named.Notification{ID: "ntf_obx_hook", Name: "hook", CreatedAt: time.Now()}
	if err := n.SetTarget(run.NotifyTarget{Kind: run.NotifyWebhook, URL: url},
		plainSealer{}); err != nil {
		t.Fatalf("SetTarget() error = %v", err)
	}
	if err := store.Save(ctx, n); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	for _, ev := range named.Events {
		if err := store.Attach(ctx, &named.Attachment{
			ID: named.NewAttachmentID(), NotificationID: n.ID, ObjectKind: named.KindTemplate,
			ObjectID: "tpl_named", Event: ev, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Attach() error = %v", err)
		}
	}
	return store
}
