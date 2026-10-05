package storetest

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// startCase is one claim shape the fenced start is asked to move, and what it must answer.
type startCase struct {
	// Name says what the claim looks like when the start arrives.
	Name string
	// Shape claims the run and changes it the way the case needs, returning the owner and secret
	// the start presents.
	Shape func(t *testing.T, ctx context.Context, store run.Store, id string) (owner, secret string)
	// WantMoved is whether the start moves the run to running.
	WantMoved bool
}

// claimFor saves a pending run with id in its own queue and claims it for owner, returning the
// secret the claim minted.
func claimFor(t *testing.T, ctx context.Context, store run.Store, id, owner string) string {
	t.Helper()
	queue := "q-" + id
	if err := store.Save(ctx, &run.Run{ID: id, Playbook: "site.yml", Status: run.StatusPending,
		Queue: queue, CreatedAt: time.Now().Add(-time.Minute)}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	claimed, err := store.Claim(ctx, owner, []string{queue})
	if err != nil {
		t.Fatalf("Claim() error = %v", err)
	}
	if claimed.ID != id || claimed.ClaimSecret == "" {
		t.Fatalf("Claim() = %s with secret %t, want %s with a secret", claimed.ID,
			claimed.ClaimSecret != "", id)
	}
	return claimed.ClaimSecret
}

// testStartClaimed verifies the fenced start moves a pending run to running only for the claim that
// still holds it: the owner the claim recorded, presenting the capability the claim minted.
//
// The start used to compare the status alone. The janitor's requeue clears the claim and its
// capability and leaves the run pending, so a claimant that stalled past its lease started the run
// the janitor had taken back, under no capability at all, beside whichever executor claimed it
// next, and any holder of a pool token could start a run it never claimed. The cases share one
// store and one of them sweeps it, so they run in order rather than in parallel.
//
//nolint:funlen // Test function.
func testStartClaimed(t *testing.T, store run.Store) {
	ctx := context.Background()
	tests := []startCase{{ // Test 0: The claim that holds the run starts it.
		Name: "holder",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			return "w-a", claimFor(t, ctx, store, id, "w-a")
		},
		WantMoved: true,
	}, { // Test 1: The right owner presenting a capability the claim never minted.
		Name: "wrong secret",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			claimFor(t, ctx, store, id, "w-a")
			return "w-a", "not-the-claim-secret"
		},
	}, { // Test 2: The claim's capability presented under another owner's name.
		Name: "wrong owner",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			return "w-b", claimFor(t, ctx, store, id, "w-a")
		},
	}, { // Test 3: No capability at all from the owner that holds the run.
		Name: "no secret",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			claimFor(t, ctx, store, id, "w-a")
			return "w-a", ""
		},
	}, { // Test 4: A run nobody claimed, started with no owner and no capability.
		Name: "never claimed",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			if err := store.Save(ctx, &run.Run{ID: id, Playbook: "site.yml",
				Status: run.StatusPending, CreatedAt: time.Now()}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			return "", ""
		},
	}, { // Test 5: The janitor requeued the run, and the claimant that stalled wakes and starts it.
		Name: "requeued",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			secret := claimFor(t, ctx, store, id, "w-a")
			if _, err := store.ReclaimStale(ctx, -time.Minute); err != nil {
				t.Fatalf("ReclaimStale() error = %v", err)
			}
			return "w-a", secret
		},
	}, { // Test 6: Requeued and claimed again by a worker of the same name, then the stale start.
		Name: "claimed again",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			secret := claimFor(t, ctx, store, id, "w-a")
			if _, err := store.ReclaimStale(ctx, -time.Minute); err != nil {
				t.Fatalf("ReclaimStale() error = %v", err)
			}
			again, err := store.Claim(ctx, "w-a", []string{"q-" + id})
			if err != nil || again.ID != id {
				t.Fatalf("second Claim() = %v, %v, want %s claimed again", again, err, id)
			}
			return "w-a", secret
		},
	}, { // Test 7: A cancel requested after the claim stops the start.
		Name: "cancel requested",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			secret := claimFor(t, ctx, store, id, "w-a")
			if err := store.RequestCancel(ctx, id); err != nil {
				t.Fatalf("RequestCancel() error = %v", err)
			}
			return "w-a", secret
		},
	}, { // Test 8: A run already started is not started twice.
		Name: "already running",
		Shape: func(t *testing.T, ctx context.Context, store run.Store, id string) (string, string) {
			secret := claimFor(t, ctx, store, id, "w-a")
			if moved, err := store.StartClaimed(ctx, id, "w-a", secret, time.Now()); err != nil ||
				!moved {
				t.Fatalf("first StartClaimed() = %v, %v, want the run started", moved, err)
			}
			return "w-a", secret
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			id := fmt.Sprintf("run_start_%d", testNum)
			owner, secret := test.Shape(t, ctx, store, id)
			before, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			started := time.Now().Add(-time.Second).UTC().Truncate(time.Millisecond)
			moved, err := store.StartClaimed(ctx, id, owner, secret, started)
			if err != nil {
				t.Fatalf("StartClaimed() error = %v", err)
			}
			if moved != test.WantMoved {
				t.Fatalf("StartClaimed() = %v, want %v", moved, test.WantMoved)
			}
			after, err := store.Get(ctx, id)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !test.WantMoved {
				if after.Status != before.Status || after.ClaimedBy != before.ClaimedBy {
					t.Errorf("a refused start changed the run from %s held by %q to %s held by %q",
						before.Status, before.ClaimedBy, after.Status, after.ClaimedBy)
				}
				return
			}
			if after.Status != run.StatusRunning || after.ClaimedBy != owner || after.ClaimedAt == nil {
				t.Errorf("started run is %s held by %q, want running held by %q with a lease",
					after.Status, after.ClaimedBy, owner)
			}
			if after.StartedAt == nil || !after.StartedAt.Equal(started) {
				t.Errorf("started run records start %v, want %v", after.StartedAt, started)
			}
		})
	}
	if moved, err := store.StartClaimed(ctx, "run_start_missing", "w-a", "secret",
		time.Now()); err != nil || moved {
		t.Errorf("StartClaimed() on a missing run = %v, %v, want false, nil", moved, err)
	}
}
