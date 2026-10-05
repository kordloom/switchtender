package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// testPolicyNotesRoundTrip verifies the warnings a policy noted on a run survive the store whole,
// through a single read and through a listing. A note is the evidence that a run went ahead past a
// warning, and each one is free text a module wrote, so a comma, a quote, or a parenthesis inside it
// must not split or mangle it, and a run with no notes must read back with none.
func testPolicyNotesRoundTrip(t *testing.T, store run.Store) {
	ctx := context.Background()
	noted := []string{
		`staging-advice (no change ticket, "env" label missing, rego sha256:3f9c0a1b2c4d)`,
		`step "deploy": release-advice (agent asked, rego sha256:0123456789ab)`,
	}
	for _, want := range [][]string{nil, noted} {
		id := fmt.Sprintf("run_noted_%d", len(want))
		if err := store.Save(ctx, &run.Run{
			ID: id, Playbook: "site.yml", Status: run.StatusPending, CreatedAt: time.Now(),
			PolicyNotes: want,
		}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
		got, err := store.Get(ctx, id)
		if err != nil {
			t.Fatalf("Get(%s) error = %v", id, err)
		}
		if diff := cmp.Diff(want, got.PolicyNotes, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("%s PolicyNotes mismatch (-want +got):\n%s", id, diff)
		}
	}
	listed, err := store.ListPage(ctx, run.ListFilter{}, 0, 0)
	if err != nil {
		t.Fatalf("ListPage() error = %v", err)
	}
	found := false
	for _, r := range listed {
		if r.ID != "run_noted_2" {
			continue
		}
		found = true
		if diff := cmp.Diff(noted, r.PolicyNotes); diff != "" {
			t.Errorf("listed PolicyNotes mismatch (-want +got):\n%s", diff)
		}
	}
	if !found {
		t.Error("the noted run is missing from the listing, so the listing read was not checked")
	}
}
