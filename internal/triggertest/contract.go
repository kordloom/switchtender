// Package triggertest provides a shared behavior contract for trigger.Store implementations so
// the in-memory, SQLite, and PostgreSQL backends cannot drift apart.
package triggertest

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/trigger"
)

// Contract runs the trigger.Store contract against a fresh store from newStore.
func Contract(t *testing.T, newStore func() trigger.Store) {
	t.Helper()
	t.Run("lifecycle", func(t *testing.T) { testLifecycle(t, newStore()) })
	t.Run("find by token hash", func(t *testing.T) { testFindByHash(t, newStore()) })
	t.Run("list ordered", func(t *testing.T) { testList(t, newStore()) })
	t.Run("signing secret", func(t *testing.T) { testSigning(t, newStore()) })
	t.Run("review configuration", func(t *testing.T) { testReview(t, newStore()) })
	t.Run("refusal record", func(t *testing.T) { testRefusal(t, newStore()) })
	t.Run("unstorable refusal text", func(t *testing.T) { testUnstorableRefusal(t, newStore()) })
}

// testRefusal verifies the record of why a delivery started no run: RecordRefusal sets the reason and
// its time, a later fire clears both, a refusal for a deleted trigger never brings it back, and a
// whole-row save, which is how a backup is restored, carries both.
//
// The record is the only place an operator sees why a webhook stopped launching anything. A backend
// that dropped it would show a trigger whose fire time quietly stopped moving, and one whose fire did
// not clear it would show a refusal the trigger has long since got past.
func testRefusal(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, &trigger.Trigger{
		ID: "trg_ref", Name: "deploy", TemplateID: "tpl_1", TokenHash: "ref-hash", CreatedAt: created,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	refused := created.Add(time.Hour)
	const reason = `refused: survey question unanswered: "db_password" is required and has no default`
	if err := store.RecordRefusal(ctx, "trg_ref", refused, reason); err != nil {
		t.Fatalf("RecordRefusal() error = %v", err)
	}
	got, err := store.Get(ctx, "trg_ref")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.LastError != reason || got.LastErrorAt == nil || !got.LastErrorAt.Equal(refused) {
		t.Errorf("after a refusal: last_error=%q at %v, want %q at %v", got.LastError,
			got.LastErrorAt, reason, refused)
	}
	if got.LastFiredAt != nil {
		t.Errorf("a refusal stamped a fire time %v", got.LastFiredAt)
	}

	// A whole-row save of what was read, which a restore does, keeps the record.
	if err := store.Delete(ctx, "trg_ref"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Save(ctx, got); err != nil {
		t.Fatalf("Save(restored) error = %v", err)
	}
	restored, err := store.Get(ctx, "trg_ref")
	if err != nil {
		t.Fatalf("Get(restored) error = %v", err)
	}
	if restored.LastError != reason || restored.LastErrorAt == nil ||
		!restored.LastErrorAt.Equal(refused) {
		t.Errorf("a save lost the refusal record: last_error=%q at %v", restored.LastError,
			restored.LastErrorAt)
	}
	// A save over the row that exists, which a restore onto a live install does, replaces it whole.
	later := refused.Add(30 * time.Minute)
	restored.LastError, restored.LastErrorAt = "a later refusal", &later
	if err := store.Save(ctx, restored); err != nil {
		t.Fatalf("Save(over) error = %v", err)
	}
	replaced, err := store.Get(ctx, "trg_ref")
	if err != nil {
		t.Fatalf("Get(replaced) error = %v", err)
	}
	if replaced.LastError != "a later refusal" || replaced.LastErrorAt == nil ||
		!replaced.LastErrorAt.Equal(later) {
		t.Errorf("a save over the row kept the old record: last_error=%q at %v",
			replaced.LastError, replaced.LastErrorAt)
	}

	fired := refused.Add(time.Hour)
	if err := store.TouchFired(ctx, "trg_ref", fired); err != nil {
		t.Fatalf("TouchFired() error = %v", err)
	}
	cleared, err := store.Get(ctx, "trg_ref")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if cleared.LastError != "" || cleared.LastErrorAt != nil {
		t.Errorf("a fire left the refusal in place: last_error=%q at %v", cleared.LastError,
			cleared.LastErrorAt)
	}
	if cleared.LastFiredAt == nil || !cleared.LastFiredAt.Equal(fired) {
		t.Errorf("LastFiredAt = %v, want %v", cleared.LastFiredAt, fired)
	}

	if err := store.Delete(ctx, "trg_ref"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.RecordRefusal(ctx, "trg_ref", fired, reason); err != nil {
		t.Fatalf("RecordRefusal(deleted) error = %v", err)
	}
	if _, err := store.Get(ctx, "trg_ref"); !errors.Is(err, trigger.ErrNotFound) {
		t.Errorf("Get() after a refusal of a deleted trigger error = %v, want ErrNotFound", err)
	}
}

// testSigning verifies the sealed signing secret and enforcement flag round trip.
func testSigning(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	tg := &trigger.Trigger{
		ID: "trg_sig", Name: "signed", TemplateID: "tpl_1", TokenHash: "th",
		SigningSecret: "sealed-secret", RequireSignature: true, CreatedAt: time.Now(),
	}
	if err := store.Save(ctx, tg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "trg_sig")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.SigningSecret != "sealed-secret" || !got.RequireSignature {
		t.Errorf("Get() = %+v, want sealed secret and require signature set", got)
	}
}

// testLifecycle verifies a trigger round trips, updates, and deletes.
func testLifecycle(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	created := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	fired := created.Add(time.Hour)
	tg := &trigger.Trigger{
		ID: "trg_1", Name: "deploy", TemplateID: "tpl_9", TokenHash: "abc",
		LastFiredAt: &fired, CreatedAt: created,
	}
	if err := store.Save(ctx, tg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	got, err := store.Get(ctx, "trg_1")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.TemplateID != "tpl_9" || got.TokenHash != "abc" ||
		got.LastFiredAt == nil || !got.LastFiredAt.Equal(fired) {
		t.Errorf("Get() = %+v, want the saved trigger", got)
	}

	if _, err := store.Get(ctx, "ghost"); !errors.Is(err, trigger.ErrNotFound) {
		t.Errorf("Get(ghost) error = %v, want ErrNotFound", err)
	}
	if err := store.Delete(ctx, "trg_1"); err != nil {
		t.Fatalf("Delete() error = %v", err)
	}
	if err := store.Delete(ctx, "trg_1"); !errors.Is(err, trigger.ErrNotFound) {
		t.Errorf("Delete(gone) error = %v, want ErrNotFound", err)
	}
}

// testFindByHash verifies token lookup and the miss case.
func testFindByHash(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	if err := store.Save(ctx, &trigger.Trigger{
		ID: "trg_h", Name: "n", TemplateID: "tpl_1", TokenHash: "hash-xyz",
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.FindByTokenHash(ctx, "hash-xyz")
	if err != nil {
		t.Fatalf("FindByTokenHash() error = %v", err)
	}
	if got.ID != "trg_h" {
		t.Errorf("FindByTokenHash() = %s, want trg_h", got.ID)
	}
	if _, err := store.FindByTokenHash(ctx, "nope"); !errors.Is(err, trigger.ErrNotFound) {
		t.Errorf("FindByTokenHash(miss) error = %v, want ErrNotFound", err)
	}
}

// testList verifies triggers come back oldest first.
func testList(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	for i, id := range []string{"trg_b", "trg_a"} {
		if err := store.Save(ctx, &trigger.Trigger{
			ID: id, Name: id, TemplateID: "tpl", TokenHash: id + "h",
			CreatedAt: base.Add(time.Duration(1-i) * time.Hour),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	list, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 2 || list[0].ID != "trg_a" || list[1].ID != "trg_b" {
		t.Errorf("List() order = %+v, want trg_a then trg_b", list)
	}
}

// testReview verifies a review trigger's configuration round trips whole, that a push trigger reads
// back with no review at all, and that a stored review cannot be changed through a returned copy.
//
// A backend that dropped the review would turn a review trigger into a push trigger on the next
// read, and a pull request webhook would then fire the template for real instead of planning it.
func testReview(t *testing.T, store trigger.Store) {
	ctx := context.Background()
	want := &trigger.Review{
		Provider: trigger.ProviderGitLab, APIURL: "https://gitlab.example.com/api/v4",
		Repository: "infra/network", CredentialID: "cred_vcs", AllowForks: true,
	}
	if err := store.Save(ctx, &trigger.Trigger{
		ID: "trg_rev", Name: "review", TemplateID: "tpl_1", TokenHash: "rev-hash",
		Review: want, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.Save(ctx, &trigger.Trigger{
		ID: "trg_push", Name: "push", TemplateID: "tpl_1", TokenHash: "push-hash", CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.Get(ctx, "trg_rev")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if diff := cmp.Diff(want, got.Review); diff != "" {
		t.Errorf("Get() review mismatch (-want +got):\n%s", diff)
	}
	got.Review.Repository = "changed/elsewhere"
	again, err := store.FindByTokenHash(ctx, "rev-hash")
	if err != nil {
		t.Fatalf("FindByTokenHash() error = %v", err)
	}
	if diff := cmp.Diff(want, again.Review); diff != "" {
		t.Errorf("a returned copy changed the stored review (-want +got):\n%s", diff)
	}
	push, err := store.Get(ctx, "trg_push")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if push.Review != nil {
		t.Errorf("Get(push) review = %+v, want nil", push.Review)
	}
}
