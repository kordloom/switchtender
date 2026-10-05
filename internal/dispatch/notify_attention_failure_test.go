package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	named "github.com/kordloom/switchtender/internal/notification"
	"github.com/kordloom/switchtender/internal/run"
)

// obxPage is the part of a PagerDuty event a named pager target receives that these tests read.
type obxPage struct {
	// RoutingKey is the integration key paged.
	RoutingKey string `json:"routing_key"`
	// DedupKey is what PagerDuty collapses events into one incident by.
	DedupKey string `json:"dedup_key"`
}

// TestOutboxPagesANamedTargetAttachedForAttention covers the alert a crash leaves behind. A worker
// that dies and is never reclaimed, a queue no worker serves, or an approval waiting past its age
// alert raises an attention alert, and the documented promise is that a target attached for the
// attention event hears the alert on every kind, a PagerDuty target included, since attaching one
// for that event is the request to be paged, with the alert's id as the incident's dedup key so one
// condition opens one incident. A server delivers named targets through the outbox, and the outbox
// applies the pager rule for runs, which pages only for a run that failed or was interrupted, to
// the alert as well, so the alert is never recorded for the pager at all. The direct delivery path
// a server without an outbox would take does page, which is the path the existing test covers. And
// were the alert recorded, the outbox's sender keys every page on the run's id, so the alert and
// the run's later failure would fold into one incident.
func TestOutboxPagesANamedTargetAttachedForAttention(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Status  run.Status
		Blocker string
	}{{ // Test 0: The worker running it stopped reporting and no server reclaimed it.
		Name: "worker lost", Status: run.StatusRunning, Blocker: "worker_lost",
	}, { // Test 1: It is queued and no connected worker serves its queue.
		Name: "no worker", Status: run.StatusPending, Blocker: "no_worker",
	}, { // Test 2: It is held for approval past an age alert somebody turned on.
		Name: "approval needed", Status: run.StatusPendingApproval, Blocker: "approval_needed",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			pages := make(chan obxPage, 8)
			pager := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter,
				r *http.Request) {
				var p obxPage
				raw, _ := io.ReadAll(r.Body)
				if json.Unmarshal(raw, &p) == nil {
					pages <- p
				}
				w.WriteHeader(http.StatusAccepted)
			}))
			t.Cleanup(pager.Close)
			ctx := context.Background()
			targets := named.NewMemStore()
			n := &named.Notification{ID: "ntf_obx_pager", Name: "on call", CreatedAt: time.Now()}
			if err := n.SetTarget(run.NotifyTarget{Kind: run.NotifyPagerDuty, Key: "named-key"},
				plainSealer{}); err != nil {
				t.Fatalf("SetTarget() error = %v", err)
			}
			if err := targets.Save(ctx, n); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if err := targets.Attach(ctx, &named.Attachment{ID: named.NewAttachmentID(),
				NotificationID: n.ID, ObjectKind: named.KindTemplate, ObjectID: "tpl_named",
				Event: named.EventAttention, CreatedAt: time.Now()}); err != nil {
				t.Fatalf("Attach() error = %v", err)
			}
			router := named.NewRouter(targets, plainSealer{}, named.SourceLineage(nil, nil, nil),
				nil)
			outbox := named.NewOutbox(targets, router, plainSealer{}, nil,
				named.WithPoll(10*time.Millisecond))
			d := New(run.NewMemStore(), okRunner(), nil, WithNoJanitor(),
				WithNotificationOutbox(outbox), WithNotifyClient(http.DefaultClient))
			d.pagerDutyEndpoint = pager.URL
			t.Cleanup(d.Close)

			alertID := "att_obx_" + test.Blocker
			d.NotifyAttention(&run.Run{
				ID: "run_obx_alert", Status: test.Status, Tool: run.ToolBash, Command: "deploy",
				Source: "template", SourceID: "tpl_named",
				Attention: &run.AttentionNote{ID: alertID, Blocker: test.Blocker,
					Summary: "Run deploy needs attention."},
			})
			select {
			case p := <-pages:
				want := obxPage{RoutingKey: "named-key", DedupKey: alertID}
				if diff := cmp.Diff(want, p); diff != "" {
					t.Errorf("the alert's page mismatch (-want +got):\n%s", diff)
				}
			case <-time.After(5 * time.Second):
				list, _ := targets.Deliveries(ctx, named.DeliveryFilter{RunID: "run_obx_alert"})
				t.Fatalf("the PagerDuty target attached for the attention event was never paged "+
					"for the %s alert; deliveries recorded = %+v", test.Blocker, list)
			}
		})
	}
}
