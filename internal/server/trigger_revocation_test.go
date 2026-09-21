package server

import (
	"context"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/trigger"
)

// TestAFireInFlightCannotResurrectARevokedTrigger pins deletion as revocation.
//
// The fire path loads its trigger, launches, and only then stamps the fire time. Stamping used to
// write the whole loaded snapshot back through Save, an upsert, so a delivery that was in flight
// while an admin deleted the trigger re-inserted it, leaked token and all: the admin performed
// the documented revocation and an attacker's own in-flight request undid it. The stamp is an
// update by id now, and this holds it to that.
func TestAFireInFlightCannotResurrectARevokedTrigger(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := trigger.NewMemStore()
	_, tg, err := trigger.New("deploy hook", "tpl_1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if err := store.Save(ctx, tg); err != nil {
		t.Fatalf("Save: %v", err)
	}

	// The revocation lands while a fire holds its stale snapshot.
	if err := store.Delete(ctx, tg.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}

	// The fire finishes and stamps. It must not bring the trigger back.
	if err := store.TouchFired(ctx, tg.ID, time.Now()); err != nil {
		t.Fatalf("TouchFired after delete: %v", err)
	}
	if _, err := store.Get(ctx, tg.ID); err == nil {
		t.Fatal("the deleted trigger exists again: a stale fire resurrected a revoked webhook")
	}
	if _, err := store.FindByTokenHash(ctx, tg.TokenHash); err == nil {
		t.Fatal("the revoked token authenticates again after a stale fire")
	}
}
