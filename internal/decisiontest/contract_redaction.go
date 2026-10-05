package decisiontest

import (
	"context"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/decision"
)

// testFinishRedaction verifies the two halves of a redaction recorded after it claims its record:
// a pending redaction removes the text at once and is listed as pending, finishing it under its
// own entry clears the mark, finishing it again changes nothing, and finishing it under another
// entry, or a record redacted by no one, is refused.
func testFinishRedaction(t *testing.T, store decision.Store) {
	ctx := context.Background()
	for _, id := range []string{"aud_pending", "aud_plain"} {
		if err := store.Save(ctx, reasoned(t, id, "run_finish", base)); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	red := decision.Redaction{At: base, Actor: "privacy-admin",
		Category: decision.CategoryPersonalData, EntryID: "aud_redaction", Pending: true}
	if err := store.Redact(ctx, "aud_pending", red); err != nil {
		t.Fatalf("Redact() error = %v", err)
	}
	pending, err := store.PendingRedactions(ctx)
	if err != nil {
		t.Fatalf("PendingRedactions() error = %v", err)
	}
	var ids []string
	for _, r := range pending {
		ids = append(ids, r.ID)
		if r.Reason.Text != "" || r.Reason.Random != "" {
			t.Errorf("a pending redaction still holds its text: %+v", r.Reason)
		}
	}
	if diff := cmp.Diff([]string{"aud_pending"}, ids); diff != "" {
		t.Errorf("PendingRedactions() (-want +got):\n%s", diff)
	}
	tests := []struct {
		// ID is the record named.
		ID string
		// EntryID is the redaction entry named.
		EntryID string
		// Want is the error.
		Want error
	}{{ // Test 0: Another redaction's entry does not finish this one.
		ID: "aud_pending", EntryID: "aud_other", Want: decision.ErrRedacted,
	}, { // Test 1: The redaction's own entry finishes it.
		ID: "aud_pending", EntryID: "aud_redaction",
	}, { // Test 2: Finishing it again changes nothing and is not an error.
		ID: "aud_pending", EntryID: "aud_redaction",
	}, { // Test 3: A record nobody redacted has nothing to finish.
		ID: "aud_plain", EntryID: "aud_redaction", Want: decision.ErrRedacted,
	}, { // Test 4: An unknown record.
		ID: "aud_unknown", EntryID: "aud_redaction", Want: decision.ErrNotFound,
	}}
	for testNum, test := range tests {
		if err := store.FinishRedaction(ctx, test.ID, test.EntryID); !errors.Is(err, test.Want) {
			t.Errorf("test %d: FinishRedaction(%s, %s) error = %v, want %v", testNum, test.ID,
				test.EntryID, err, test.Want)
		}
	}
	got, err := store.Get(ctx, "aud_pending")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := red
	want.Pending = false
	if diff := cmp.Diff(&want, got.Reason.Redacted); diff != "" {
		t.Errorf("the finished redaction (-want +got):\n%s", diff)
	}
	if left, err := store.PendingRedactions(ctx); err != nil || len(left) != 0 {
		t.Errorf("PendingRedactions() after finishing = %d records, %v, want none", len(left), err)
	}
}
