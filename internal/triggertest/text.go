package triggertest

import (
	"context"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/trigger"
)

// testUnstorableRefusal verifies a refusal reason holding a NUL byte or bytes that are not UTF-8 is
// recorded with each replaced, on every backend.
//
// A reason quotes what a delivery carried, such as a branch name from a webhook payload. SQLite
// kept those bytes and PostgreSQL refused the write, so on PostgreSQL the trigger showed no refusal
// at all, and an operator saw a webhook that quietly stopped launching anything with no reason
// given.
func testUnstorableRefusal(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	created := time.Date(2026, 10, 4, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &trigger.Trigger{
		ID: "trg_text", Name: "deploy", TemplateID: "tpl_1", TokenHash: "text-hash", CreatedAt: created,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	tests := []struct {
		Reason     string
		WantReason string
	}{{ // Test 0: A NUL byte, which a JSON escape in a payload puts in a branch name.
		Reason: "branch main\x00x is not watched", WantReason: "branch main�x is not watched",
	}, { // Test 1: A byte that is not UTF-8.
		Reason: "ref caf\xe9 is not watched", WantReason: "ref caf� is not watched",
	}, { // Test 2: Ordinary text is kept as it is.
		Reason: "branch main is not watched", WantReason: "branch main is not watched",
	}}
	for testNum, test := range tests {
		at := created.Add(time.Duration(testNum+1) * time.Minute)
		if err := store.RecordRefusal(ctx, "trg_text", at, test.Reason); err != nil {
			t.Errorf("test %d: RecordRefusal() error = %v", testNum, err)
			continue
		}
		got, err := store.Get(ctx, "trg_text")
		if err != nil {
			t.Fatalf("test %d: Get() error = %v", testNum, err)
		}
		if diff := cmp.Diff(test.WantReason, got.LastError); diff != "" {
			t.Errorf("test %d: recorded reason mismatch (-want +got):\n%s", testNum, diff)
		}
	}
}
