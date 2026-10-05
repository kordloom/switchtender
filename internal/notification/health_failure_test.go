package notification

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestNotificationHealthFollowsTheLatestFinish covers a target's delivery status while retries are
// in play. The documented states are healthy when the target's latest finished delivery arrived and
// failing when its latest finished delivery failed after its retries. The status reads the newest
// recorded delivery among the finished ones, and a delivery that is retried for three minutes
// finishes long after the ones recorded after it. So a target whose last outcome was a delivery
// given up on reads healthy, and the doctor adds "It has delivered since" about a delivery that
// came before the failure, and the reverse, a target whose last outcome was a success, reads
// failing.
func TestNotificationHealthFollowsTheLatestFinish(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		v := base.Add(d)
		return &v
	}
	n := &Notification{ID: "ntf_obx_flaky", Name: "flaky", Kind: run.NotifyWebhook}
	tests := []struct {
		Name      string
		Recent    []*Delivery
		WantState string
	}{{ // Test 0: The older delivery is given up on after the newer one arrived.
		Name: "failed last",
		Recent: []*Delivery{{
			NotificationID: n.ID, RunID: "run_b", Seq: 1, Status: DeliveryDelivered,
			Attempts: 1, CreatedAt: base.Add(10 * time.Second), FinishedAt: at(10 * time.Second),
		}, {
			NotificationID: n.ID, RunID: "run_a", Seq: 1, Status: DeliveryFailed,
			Attempts: MaxAttempts, LastError: "the target answered 503", CreatedAt: base,
			FinishedAt: at(3*time.Minute + 20*time.Second),
		}},
		WantState: StateFailing,
	}, { // Test 1: The older delivery arrives on a retry after the newer one was refused.
		Name: "delivered last",
		Recent: []*Delivery{{
			NotificationID: n.ID, RunID: "run_b", Seq: 1, Status: DeliveryFailed,
			Attempts: 1, LastError: "the target answered 404",
			CreatedAt: base.Add(10 * time.Second), FinishedAt: at(10 * time.Second),
		}, {
			NotificationID: n.ID, RunID: "run_a", Seq: 1, Status: DeliveryDelivered,
			Attempts: 3, CreatedAt: base, FinishedAt: at(80 * time.Second),
		}},
		WantState: StateHealthy,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := HealthOf(n, test.Recent, base.Add(-HealthWindow))
			if diff := cmp.Diff(test.WantState, got.State); diff != "" {
				t.Errorf("state of a target whose latest finished delivery is %s (-want +got):\n%s",
					test.Recent[1].Status, diff)
			}
		})
	}
}
