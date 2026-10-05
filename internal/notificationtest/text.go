package notificationtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/notification"
)

// testUnstorableFinishText verifies an attempt's error and note holding a NUL byte or bytes that
// are not UTF-8 are recorded with each replaced, and that the delivery then goes on as any other.
//
// The error is what a mail server or an endpoint answered. An SMTP reply holding a NUL failed the
// write on PostgreSQL, so the attempt was never recorded: the delivery stayed claimed until its
// claim lapsed, was claimed again, failed the same way, and retried for as long as the server ran.
func testUnstorableFinishText(t *testing.T, store notification.Store) {
	ctx := context.Background()
	mustRecord(t, store, event("run_text", "started", "", base), to("ntf_text"))
	claimed, err := store.Claim(ctx, "w1", base, lease, 10, 0)
	if err != nil || len(claimed) != 1 {
		t.Fatalf("Claim() = %v, %v, want the one delivery", keys(claimed), err)
	}
	retryAt := base.Add(time.Minute)
	if err := store.Finish(ctx, claimed[0], "w1", notification.Outcome{
		Status: notification.DeliveryPending, Error: "smtp answered 550 \x00 caf\xe9",
		Note: "earlier \x00 note", At: base, NextAttemptAt: retryAt}); err != nil {
		t.Fatalf("Finish() with an unstorable reply error = %v", err)
	}
	got, err := store.Deliveries(ctx, notification.DeliveryFilter{NotificationID: "ntf_text"})
	if err != nil || len(got) != 1 {
		t.Fatalf("Deliveries() = %v, %v, want the one delivery", got, err)
	}
	tests := []struct {
		Field string
		Got   string
		Want  string
	}{{ // Test 0: The reply's NUL and its byte that is not UTF-8 are replaced.
		Field: "last_error", Got: got[0].LastError, Want: "smtp answered 550 � caf�",
	}, { // Test 1: The note's NUL is replaced.
		Field: "note", Got: got[0].Note, Want: "earlier � note",
	}}
	for testNum, test := range tests {
		if diff := cmp.Diff(test.Want, test.Got); diff != "" {
			t.Errorf("test %d: recorded %s mismatch (-want +got):\n%s", testNum, test.Field, diff)
		}
	}
	again, err := store.Claim(ctx, "w1", retryAt, lease, 10, 0)
	if err != nil || len(again) != 1 || again[0].Attempts != 1 {
		t.Fatalf("Claim() when the retry is due = %+v, %v, want the delivery after 1 attempt",
			again, err)
	}
	if err := store.Finish(ctx, again[0], "w1", notification.Outcome{
		Status: notification.DeliveryDelivered, At: retryAt}); err != nil {
		t.Fatalf("Finish(delivered) error = %v", err)
	}
}
