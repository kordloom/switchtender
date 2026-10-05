package scheduletest

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/schedule"
)

// testUnstorableFireText verifies a fire's failure and a skip's reason holding a NUL byte or bytes
// that are not UTF-8 are recorded with each replaced, on every backend.
//
// A failure carries the text of whatever refused the fire, which is not always this install's own.
// SQLite kept those bytes and PostgreSQL refused the write, so on PostgreSQL the schedule kept the
// previous fire's record and its failure was lost.
func testUnstorableFireText(t *testing.T, store schedule.Store) {
	ctx := context.Background()
	created := time.Date(2026, 10, 4, 1, 0, 0, 0, time.UTC)
	next := created.Add(time.Hour)
	if err := store.Save(ctx, &schedule.Schedule{
		ID: "sch_text", Name: "nightly", Cron: "0 * * * *", TemplateID: "tpl_x", Enabled: true,
		CreatedAt: created, NextRunAt: &next,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	tests := []struct {
		Skip        bool
		Text        string
		WantFailure string
		WantSkip    string
	}{{ // Test 0: A failure holding a NUL byte.
		Text: "inventory script said \x00", WantFailure: "inventory script said �",
	}, { // Test 1: A failure holding a byte that is not UTF-8.
		Text: "credential caf\xe9 has no secret", WantFailure: "credential caf� has no secret",
	}, { // Test 2: A skip reason holding both.
		Skip: true, Text: "host \x00 caf\xe9 matched nothing", WantSkip: "host � caf� matched nothing",
	}, { // Test 3: Ordinary text is kept as it is.
		Text: "no hosts matched", WantFailure: "no hosts matched",
	}}
	for testNum, test := range tests {
		at := created.Add(time.Duration(testNum+1) * time.Minute)
		record := store.RecordFire(ctx, "sch_text", at, "", test.Text)
		if test.Skip {
			record = store.RecordSkip(ctx, "sch_text", at, test.Text)
		}
		if record != nil {
			t.Errorf("test %d: recording the fire error = %v", testNum, record)
			continue
		}
		got, err := store.Get(ctx, "sch_text")
		if err != nil {
			t.Fatalf("test %d: Get() error = %v", testNum, err)
		}
		if diff := cmp.Diff(test.WantFailure, got.LastError); diff != "" {
			t.Errorf("test %d: recorded failure mismatch (-want +got):\n%s", testNum, diff)
		}
		if diff := cmp.Diff(test.WantSkip, got.LastSkip); diff != "" {
			t.Errorf("test %d: recorded skip mismatch (-want +got):\n%s", testNum, diff)
		}
	}
}
