package storetest

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/run"
)

// secretLeaseContract runs the records of minted secrets every backend keeps: they round trip, list
// oldest first, update in place, and are taken exactly once however many processes take them.
func secretLeaseContract(t *testing.T, newStore func() run.Store) {
	t.Helper()
	t.Run("secret leases round trip and list oldest first", func(t *testing.T) {
		testSecretLeaseRecords(t, newStore())
	})
	t.Run("a secret lease is taken once", func(t *testing.T) {
		testSecretLeaseTakenOnce(t, newStore())
	})
}

// secretLeases returns store's records of minted secrets, failing when it keeps none.
func secretLeases(t *testing.T, store run.Store) run.SecretLeases {
	t.Helper()
	keeper, ok := store.(run.SecretLeases)
	if !ok {
		t.Fatalf("%T does not keep the records of minted secrets", store)
	}
	return keeper
}

// testSecretLeaseRecords saves records, reads them back in order, updates one, and takes one.
func testSecretLeaseRecords(t *testing.T, store run.Store) {
	ctx := context.Background()
	keeper := secretLeases(t, store)
	base := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	older := &run.SecretLease{ID: "slease_b", RunID: "run_1", ClaimHash: run.ClaimHash("claim-1"),
		CredentialID: "cred_a", Kind: "vault_dynamic", Handle: "sealed-handle-b",
		ExpiresAt: base.Add(time.Hour), CreatedAt: base}
	newer := &run.SecretLease{ID: "slease_a", RunID: "run_2", ClaimHash: run.ClaimHash("claim-2"),
		CredentialID: "cred_b", Kind: "vault_dynamic", Handle: "sealed-handle-a",
		ExpiresAt: base.Add(2 * time.Hour), CreatedAt: base.Add(time.Minute), Attempts: 2,
		RetryAt: base.Add(3 * time.Minute)}
	for _, l := range []*run.SecretLease{newer, older} {
		if err := keeper.SaveSecretLease(ctx, l); err != nil {
			t.Fatalf("SaveSecretLease(%s) error = %v", l.ID, err)
		}
	}
	equal := cmpopts.EquateApproxTime(time.Millisecond)
	got, err := keeper.ListSecretLeases(ctx, 0)
	if err != nil {
		t.Fatalf("ListSecretLeases() error = %v", err)
	}
	if diff := cmp.Diff([]*run.SecretLease{older, newer}, got, equal); diff != "" {
		t.Errorf("ListSecretLeases() mismatch, oldest first (-want +got):\n%s", diff)
	}
	if first, err := keeper.ListSecretLeases(ctx, 1); err != nil || len(first) != 1 ||
		first[0].ID != older.ID {
		t.Errorf("ListSecretLeases(1) = %v, %v, want only the oldest", first, err)
	}
	older.Attempts, older.RetryAt = 1, base.Add(30*time.Second)
	if err := keeper.SaveSecretLease(ctx, older); err != nil {
		t.Fatalf("SaveSecretLease(update) error = %v", err)
	}
	if taken, err := keeper.TakeSecretLease(ctx, newer.ID); err != nil || !taken {
		t.Fatalf("TakeSecretLease() = %v, %v, want the record taken", taken, err)
	}
	got, err = keeper.ListSecretLeases(ctx, 0)
	if err != nil {
		t.Fatalf("ListSecretLeases() error = %v", err)
	}
	if diff := cmp.Diff([]*run.SecretLease{older}, got, equal); diff != "" {
		t.Errorf("ListSecretLeases() after an update and a take mismatch (-want +got):\n%s", diff)
	}
}

// testSecretLeaseTakenOnce takes one record from eight goroutines at once: exactly one takes it, so
// of the replicas sweeping one secret, exactly one revokes it.
func testSecretLeaseTakenOnce(t *testing.T, store run.Store) {
	ctx := context.Background()
	keeper := secretLeases(t, store)
	now := time.Now()
	if err := keeper.SaveSecretLease(ctx, &run.SecretLease{ID: "slease_race", RunID: "run_race",
		Kind: "vault_dynamic", Handle: "sealed", ExpiresAt: now.Add(time.Hour),
		CreatedAt: now}); err != nil {
		t.Fatalf("SaveSecretLease() error = %v", err)
	}
	var mu sync.Mutex
	takers := 0
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			taken, err := keeper.TakeSecretLease(ctx, "slease_race")
			if err != nil {
				t.Errorf("TakeSecretLease() error = %v", err)
				return
			}
			if taken {
				mu.Lock()
				takers++
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if diff := cmp.Diff(1, takers); diff != "" {
		t.Errorf("takers of one record mismatch (-want +got):\n%s", diff)
	}
}
