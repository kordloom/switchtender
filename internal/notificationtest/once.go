package notificationtest

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/notification"
)

// testRecordOnceEach verifies a run's start, its own hold, and the hold of each approval step are
// each recorded once whatever copy of the run announces them, the way its end is, while a step's
// hold does not stand for the run's or another step's, and an attention alert, which can be raised
// again, is kept once per moment only.
func testRecordOnceEach(t *testing.T, store notification.Store) {
	ctx := context.Background()
	mustRecord(t, store, event("run_o", "started", "", base), to("ntf_a"))
	mustRecord(t, store, event("run_o", "approval", "", base.Add(time.Second)), to("ntf_a"))
	mustRecord(t, store, event("run_o", "approval", "deploy", base.Add(2*time.Second)),
		to("ntf_a"))
	mustRecord(t, store, event("run_o", "attention", "", base.Add(3*time.Second)), to("ntf_a"))
	tests := []struct {
		In           *notification.RunEvent
		WantRecorded bool
	}{{ // Test 0: The start again from a later copy of the run, as the sweep reads it.
		In: event("run_o", "started", "", base.Add(4*time.Second)),
	}, { // Test 1: The run's own hold again.
		In: event("run_o", "approval", "", base.Add(5*time.Second)),
	}, { // Test 2: The same step's hold again.
		In: event("run_o", "approval", "deploy", base.Add(6*time.Second)),
	}, { // Test 3: Another step's hold is its own.
		In: event("run_o", "approval", "verify", base.Add(7*time.Second)), WantRecorded: true,
	}, { // Test 4: Another attention alert is kept.
		In: event("run_o", "attention", "", base.Add(8*time.Second)), WantRecorded: true,
	}, { // Test 5: Another run's start.
		In: event("run_p", "started", "", base.Add(9*time.Second)), WantRecorded: true,
	}}
	for testNum, test := range tests {
		recorded, err := store.Record(ctx, test.In, to("ntf_a"))
		if err != nil {
			t.Fatalf("test %d: Record() error = %v", testNum, err)
		}
		if recorded != test.WantRecorded {
			t.Errorf("test %d: Record(%s %s on %q) recorded = %v, want %v", testNum,
				test.In.RunID, test.In.Event, test.In.Branch, recorded, test.WantRecorded)
		}
	}
	got, err := store.Deliveries(ctx, notification.DeliveryFilter{RunID: "run_o"})
	if err != nil {
		t.Fatalf("Deliveries() error = %v", err)
	}
	want := []string{"ntf_a/run_o/1", "ntf_a/run_o/2", "ntf_a/run_o/3", "ntf_a/run_o/4",
		"ntf_a/run_o/5", "ntf_a/run_o/6"}
	if diff := cmp.Diff(want, keys(got)); diff != "" {
		t.Errorf("deliveries of run_o mismatch (-want +got):\n%s", diff)
	}
}
