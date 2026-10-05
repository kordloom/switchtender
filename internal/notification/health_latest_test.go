package notification

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestHealthReportsTheLatestFinishOfEach pins which delivery a target's status reports among
// several of a kind: the times and the reason are the latest finish's, never the latest recorded
// one's, and a failure counts toward the window by when it failed, so a delivery recorded before
// the window opened and given up on inside it is counted.
func TestHealthReportsTheLatestFinishOfEach(t *testing.T) {
	t.Parallel()
	base := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	at := func(d time.Duration) *time.Time {
		v := base.Add(d)
		return &v
	}
	n := &Notification{ID: "ntf_latest", Name: "latest", Kind: run.NotifyWebhook}
	tests := []struct {
		Name       string
		Recent     []*Delivery
		Since      time.Time
		WantHealth string
	}{{ // Test 0: Of two deliveries, the one recorded first finished last.
		Name: "delivered",
		Recent: []*Delivery{{
			RunID: "run_b", Status: DeliveryDelivered, CreatedAt: base.Add(time.Minute),
			FinishedAt: at(time.Minute),
		}, {
			RunID: "run_a", Status: DeliveryDelivered, CreatedAt: base,
			FinishedAt: at(5 * time.Minute),
		}},
		Since:      base.Add(-HealthWindow),
		WantHealth: "healthy delivered 09:05:00 failed none 0 \"\"",
	}, { // Test 1: Of two failures, the one recorded first was given up on last.
		Name: "failed",
		Recent: []*Delivery{{
			RunID: "run_b", Status: DeliveryFailed, LastError: "the target answered 404",
			CreatedAt: base.Add(time.Minute), FinishedAt: at(70 * time.Second),
		}, {
			RunID: "run_a", Status: DeliveryFailed, LastError: "the target answered 503",
			CreatedAt: base, FinishedAt: at(3 * time.Minute),
		}},
		Since: base.Add(-HealthWindow),
		WantHealth: "failing delivered none failed 09:03:00 2 " +
			"\"the target answered 503\"",
	}, { // Test 2: A failure recorded before the window and given up on inside it counts.
		Name: "window",
		Recent: []*Delivery{{
			RunID: "run_a", Status: DeliveryFailed, LastError: "the target answered 503",
			CreatedAt: base, FinishedAt: at(3 * time.Minute),
		}},
		Since: base.Add(time.Minute),
		WantHealth: "failing delivered none failed 09:03:00 1 " +
			"\"the target answered 503\"",
	}}
	clock := func(t *time.Time) string {
		if t == nil {
			return "none"
		}
		return t.Format("15:04:05")
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			h := HealthOf(n, test.Recent, test.Since)
			got := fmt.Sprintf("%s delivered %s failed %s %d %q", h.State,
				clock(h.LastDeliveredAt), clock(h.LastFailedAt), h.Failed, h.LastError)
			if diff := cmp.Diff(test.WantHealth, got); diff != "" {
				t.Errorf("health mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
