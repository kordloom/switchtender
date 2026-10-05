// Package decisiontest provides a shared behavior contract for decision.Store implementations so
// the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package decisiontest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/decision"
)

// Contract runs the decision.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() decision.Store) {
	t.Helper()
	t.Run("save get round trip", func(t *testing.T) { testRoundTrip(t, newStore()) })
	t.Run("a record is written once", func(t *testing.T) { testWrittenOnce(t, newStore()) })
	t.Run("a run's thread in order", func(t *testing.T) { testForRun(t, newStore()) })
	t.Run("redaction removes text and random together", func(t *testing.T) {
		testRedact(t, newStore())
	})
	t.Run("redaction refusals", func(t *testing.T) { testRedactRefusals(t, newStore()) })
	t.Run("delete withdraws a record", func(t *testing.T) { testDelete(t, newStore()) })
	t.Run("a pending redaction is finished once", func(t *testing.T) {
		testFinishRedaction(t, newStore())
	})
}

// base is the time every contract record is recorded relative to, at the precision the stores keep.
var base = time.Date(2026, 10, 1, 9, 30, 0, 123456000, time.UTC)

// reasoned returns a decision record carrying a reason and a separation-of-duties evaluation.
func reasoned(t *testing.T, id, runID string, at time.Time) *decision.Record {
	t.Helper()
	text := "approved, change window confirmed with the database team"
	random, commitment, err := decision.Commit(id, text)
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	return &decision.Record{
		ID: id, Kind: decision.KindDecision, DecisionID: id, RunID: runID, Verdict: "approved",
		At: at, Actor: "ops-admin", ActorType: "session", OnBehalfOf: "ops-admin",
		Reason: &decision.Reason{Text: text, Random: random, Commitment: commitment, Masked: true},
		SeparationOfDuties: &decision.SeparationOfDuties{Required: true, Requester: "dev-lead",
			Decider: "ops-admin", Independent: true, Result: decision.SoDSatisfied},
	}
}

// testRoundTrip verifies every field of a decision and a correction survives the store, and that a
// missing id is ErrNotFound.
func testRoundTrip(t *testing.T, store decision.Store) {
	ctx := context.Background()
	want := reasoned(t, "aud_round", "run_round", base)
	plain := &decision.Record{ID: "aud_plain", Kind: decision.KindDecision, DecisionID: "aud_plain",
		RunID: "run_round", StepRunID: "run_step", Verdict: "rejected", At: base.Add(time.Second),
		Actor: "laptop", ActorType: "token", OnBehalfOf: "ops-admin"}
	for _, r := range []*decision.Record{want, plain} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	for _, r := range []*decision.Record{want, plain} {
		got, err := store.Get(ctx, r.ID)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", r.ID, err)
		}
		if diff := cmp.Diff(r, got, cmpopts.EquateApproxTime(0)); diff != "" {
			t.Errorf("Get(%s) mismatch (-want +got):\n%s", r.ID, diff)
		}
	}
	if _, err := store.Get(ctx, "aud_missing"); !errors.Is(err, decision.ErrNotFound) {
		t.Errorf("Get(missing) error = %v, want ErrNotFound", err)
	}
}

// testWrittenOnce verifies a record cannot be replaced: a decision is evidence, and a second save
// under its id would rewrite the reason a commitment on the chain was computed over.
func testWrittenOnce(t *testing.T, store decision.Store) {
	ctx := context.Background()
	first := reasoned(t, "aud_once", "run_once", base)
	if err := store.Save(ctx, first); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	second := reasoned(t, "aud_once", "run_once", base)
	second.Reason.Text = "something else entirely"
	if err := store.Save(ctx, second); !errors.Is(err, decision.ErrExists) {
		t.Fatalf("second Save() error = %v, want ErrExists", err)
	}
	got, err := store.Get(ctx, "aud_once")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Reason.Text != first.Reason.Text {
		t.Errorf("reason after a refused save = %q, want the first %q", got.Reason.Text,
			first.Reason.Text)
	}
}

// testForRun verifies a run's records come back as the thread reads, by time and then id, without
// another run's.
func testForRun(t *testing.T, store decision.Store) {
	ctx := context.Background()
	records := []*decision.Record{
		{ID: "aud_c", Kind: decision.KindCorrection, DecisionID: "aud_a", RunID: "run_thread",
			At: base.Add(2 * time.Minute), Actor: "ops-admin"},
		{ID: "aud_a", Kind: decision.KindDecision, DecisionID: "aud_a", RunID: "run_thread",
			Verdict: "approved", At: base, Actor: "ops-admin"},
		{ID: "aud_b", Kind: decision.KindDecision, DecisionID: "aud_b", RunID: "run_thread",
			StepRunID: "run_gate", Verdict: "rejected", At: base, Actor: "ops-admin"},
		{ID: "aud_other", Kind: decision.KindDecision, DecisionID: "aud_other", RunID: "run_other",
			Verdict: "approved", At: base, Actor: "ops-admin"},
	}
	for _, r := range records {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	got, err := store.ForRun(ctx, "run_thread")
	if err != nil {
		t.Fatalf("ForRun() error = %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, r := range got {
		ids = append(ids, r.ID)
	}
	if diff := cmp.Diff([]string{"aud_a", "aud_b", "aud_c"}, ids); diff != "" {
		t.Errorf("ForRun() order mismatch (-want +got):\n%s", diff)
	}
	none, err := store.ForRun(ctx, "run_nobody")
	if err != nil {
		t.Fatalf("ForRun(empty) error = %v", err)
	}
	if len(none) != 0 {
		t.Errorf("ForRun(empty) = %d records, want none", len(none))
	}
}

// testRedact verifies a redaction removes the text and the random value together, keeps the
// commitment, and records who, when, and why, so the commitment stays on the chain and can never be
// opened again.
func testRedact(t *testing.T, store decision.Store) {
	ctx := context.Background()
	rec := reasoned(t, "aud_redact", "run_redact", base)
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	red := decision.Redaction{At: base.Add(time.Hour), Actor: "privacy-admin", ActorType: "session",
		OnBehalfOf: "privacy-admin", Category: decision.CategoryPersonalData, EntryID: "aud_red_entry"}
	if err := store.Redact(ctx, "aud_redact", red); err != nil {
		t.Fatalf("Redact() error = %v", err)
	}
	got, err := store.Get(ctx, "aud_redact")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	want := &decision.Reason{Commitment: rec.Reason.Commitment, Masked: true, Redacted: &red}
	if diff := cmp.Diff(want, got.Reason, cmpopts.EquateApproxTime(0)); diff != "" {
		t.Errorf("redacted reason mismatch (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(rec.SeparationOfDuties, got.SeparationOfDuties); diff != "" {
		t.Errorf("a redaction touched the separation-of-duties record (-want +got):\n%s", diff)
	}
}

// testRedactRefusals verifies a redaction says why it changed nothing.
func testRedactRefusals(t *testing.T, store decision.Store) {
	ctx := context.Background()
	plain := &decision.Record{ID: "aud_noreason", Kind: decision.KindDecision,
		DecisionID: "aud_noreason", RunID: "run_refuse", Verdict: "approved", At: base, Actor: "x"}
	twice := reasoned(t, "aud_twice", "run_refuse", base)
	for _, r := range []*decision.Record{plain, twice} {
		if err := store.Save(ctx, r); err != nil {
			t.Fatalf("Save(%s) error = %v", r.ID, err)
		}
	}
	red := decision.Redaction{At: base, Actor: "privacy-admin", Category: decision.CategoryOther}
	if err := store.Redact(ctx, "aud_twice", red); err != nil {
		t.Fatalf("first Redact() error = %v", err)
	}
	tests := []struct {
		// ID is the record named.
		ID string
		// Want is the refusal.
		Want error
	}{{ // Test 0: An unknown record.
		ID: "aud_unknown", Want: decision.ErrNotFound,
	}, { // Test 1: A record given no reason has nothing to redact.
		ID: "aud_noreason", Want: decision.ErrNoReason,
	}, { // Test 2: A reason already redacted.
		ID: "aud_twice", Want: decision.ErrRedacted,
	}}
	for _, test := range tests {
		if err := store.Redact(ctx, test.ID, red); !errors.Is(err, test.Want) {
			t.Errorf("Redact(%s) error = %v, want %v", test.ID, err, test.Want)
		}
	}
	got, err := store.Get(ctx, "aud_noreason")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Reason != nil {
		t.Errorf("a refused redaction gave a reasonless record a reason: %+v", got.Reason)
	}
}

// testDelete verifies a record whose chain entry never landed can be withdrawn, once.
func testDelete(t *testing.T, store decision.Store) {
	ctx := context.Background()
	rec := reasoned(t, "aud_withdraw", "run_withdraw", base)
	if err := store.Save(ctx, rec); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Delete(ctx, "aud_withdraw"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if _, err := store.Get(ctx, "aud_withdraw"); !errors.Is(err, decision.ErrNotFound) {
		t.Errorf("Get(deleted) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "aud_withdraw"); !errors.Is(err, decision.ErrNotFound) {
		t.Errorf("Delete(again) error = %v, want ErrNotFound", err)
	}
}
