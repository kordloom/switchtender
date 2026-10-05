package migration

import (
	"strings"
	"testing"
	"time"
)

// TestImportedNotificationIsOrderedAndCleanedUpWithItsTemplate is the notification scenario. The
// fixture's rotation template carries the ops webhook's attachments for success and error. A run of
// it records the event that target hears, in the run's own sequence, on the run's record, with the
// target's secret address nowhere in it. Deleting the template then removes both attachments in the
// delete itself and states them inside the delete's own chain entry, while the target stays, and
// the exported chain with that entry in it verifies offline. It runs on both databases.
func TestImportedNotificationIsOrderedAndCleanedUpWithItsTemplate(t *testing.T) {
	t.Parallel()
	for _, store := range []storeKind{onSQLite, onPostgres} {
		t.Run(string(store), func(t *testing.T) {
			t.Parallel()
			in := newInstall(t, installOptions{Store: store})
			s := in.startServer("a")
			answer := "survey-answer-" + randomHex(t, 12)
			in.addSecret(answer)
			tplID := in.template(s, "rotate db password")
			ntfID := in.lookup(s, "notifications", "ops webhook")

			rec := in.launched(s, "operator", "rotate db password",
				map[string]any{"answers": map[string]any{"db_password": answer}})
			if done := in.waitDone(s, rec.ID); done.Status != "succeeded" {
				t.Fatalf("the rotation run = %s, want succeeded: %s", done.Status,
					describe(done.Raw))
			}
			// The run's terminal status is written before the end is announced, so the record of
			// what its targets were told can trail it by the time the outcome takes to commit.
			var told struct {
				// Deliveries are what the run's targets were told, in the run's order.
				Deliveries []struct {
					// NotificationID is the target.
					NotificationID string `json:"notification_id"`
					// Seq is the event's place in the run's sequence.
					Seq int64 `json:"seq"`
					// Event is the event.
					Event string `json:"event"`
					// Status is where the delivery stands.
					Status string `json:"status"`
				} `json:"deliveries"`
			}
			for deadline := time.Now().Add(waitLimit); ; time.Sleep(200 * time.Millisecond) {
				in.must(s, "admin", "GET", "/v1/runs/"+rec.ID+"/notifications", nil, 200).
					decode(t, &told)
				if len(told.Deliveries) > 0 || time.Now().After(deadline) {
					break
				}
			}
			if len(told.Deliveries) != 1 || told.Deliveries[0].NotificationID != ntfID ||
				told.Deliveries[0].Seq != 1 || told.Deliveries[0].Event != "success" {
				t.Fatalf("the run's notifications = %+v, want the ops webhook told of the "+
					"success as the run's first event", told.Deliveries)
			}

			in.must(s, "admin", "DELETE", "/v1/templates/"+tplID, nil, 200)
			attached := string(in.must(s, "admin", "GET",
				"/v1/notifications/"+ntfID+"/attachments", nil, 200).Body)
			if !strings.Contains(attached, `"count":0`) {
				t.Errorf("the template's attachments outlived it: %s", attached)
			}
			in.must(s, "admin", "GET", "/v1/notifications/"+ntfID, nil, 200)

			ev := in.checkEvidence(s, rec.ID)
			want := "/v1/templates/" + tplID + "?notification_attachments=2&notification_targets=" +
				ntfID
			deletes := 0
			for _, e := range ev.Audit {
				if e.Method != "DELETE" || !strings.HasPrefix(e.Path, "/v1/templates/"+tplID) {
					continue
				}
				deletes++
				if e.Path != want {
					t.Errorf("the delete's chain entry records %q, want %q", e.Path, want)
				}
			}
			if deletes != 1 {
				t.Errorf("the chain holds %d entries for the delete, want its one", deletes)
			}
		})
	}
}
