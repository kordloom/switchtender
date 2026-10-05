// Package federationtest holds the federation.KeyStore contract every backend runs, so the
// in-memory, SQLite, and PostgreSQL stores cannot drift apart on how a signing key round-trips.
package federationtest

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/federation"
)

// KeyContract runs the federation.KeyStore contract against a fresh store from newStore.
func KeyContract(t *testing.T, newStore func() federation.KeyStore) {
	t.Helper()
	t.Run("round trip keeps every field", func(t *testing.T) { testRoundTrip(t, newStore()) })
	t.Run("retiring a key is a save", func(t *testing.T) { testRetire(t, newStore()) })
	t.Run("list is oldest first to the nanosecond", func(t *testing.T) { testOrder(t, newStore()) })
	t.Run("lifecycle times round trip and unset stays unset", func(t *testing.T) {
		testLifecycleTimes(t, newStore())
	})
	t.Run("removing a key erases its private half and keeps its record", func(t *testing.T) {
		testRemoveKeepsRecord(t, newStore())
	})
	t.Run("a removal and an erasure cannot be undone", func(t *testing.T) {
		testRemovalIsFinal(t, newStore())
	})
	t.Run("a change that fails keeps nothing it wrote", func(t *testing.T) {
		testFailedChangeKeepsNothing(t, newStore())
	})
	t.Run("a change sees its own writes and nobody else does until it ends", func(t *testing.T) {
		testChangeIsolation(t, newStore())
	})
	t.Run("changes run one at a time", func(t *testing.T) { testChangesSerialize(t, newStore()) })
	t.Run("the clock holds still inside a change", func(t *testing.T) {
		testChangeClock(t, newStore())
	})
}

// sampleKey returns a key signing from at, with every field set to a value nothing else produces.
func sampleKey(id string, at time.Time) *federation.Key {
	activated := at
	return &federation.Key{
		ID: id, Algorithm: "RS256", PublicKey: "-----BEGIN PUBLIC KEY-----\n" + id + "\n",
		Sealed: "sealed-" + id, CreatedAt: at, ActivatedAt: &activated,
	}
}

// testRoundTrip pins that every field, the sealed private key included, reads back as written. A
// store that dropped the sealed column would publish a key it can never sign with.
func testRoundTrip(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 9, 30, 0, 123456789, time.UTC)
	want := sampleKey("kid_round", at)
	if err := store.Save(ctx, want); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff([]*federation.Key{want}, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
}

// testRetire pins that saving a key again with a retirement time updates it in place, which is how
// a rotation retires the key it replaces.
func testRetire(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	k := sampleKey("kid_retire", at)
	if err := store.Save(ctx, k); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	retired := at.Add(time.Hour)
	k.RetiredAt = &retired
	if err := store.Save(ctx, k); err != nil {
		t.Fatalf("Save() retire error = %v", err)
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() returned %d keys after a retire, want the one key updated in place", len(got))
	}
	if got[0].RetiredAt == nil || !got[0].RetiredAt.Equal(retired) {
		t.Errorf("RetiredAt = %v, want %v", got[0].RetiredAt, retired)
	}
}

// testOrder pins oldest first, including two keys a fraction of a second apart, the case a text
// sort of the stored time gets backwards.
func testOrder(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	base := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	for _, k := range []*federation.Key{
		sampleKey("kid_a_newest", base.Add(2*time.Second)),
		sampleKey("kid_b_middle", base.Add(500*time.Millisecond)),
		sampleKey("kid_c_oldest", base),
	} {
		if err := store.Save(ctx, k); err != nil {
			t.Fatalf("Save(%s) error = %v", k.ID, err)
		}
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	ids := make([]string, 0, len(got))
	for _, k := range got {
		ids = append(ids, k.ID)
	}
	want := []string{"kid_c_oldest", "kid_b_middle", "kid_a_newest"}
	if diff := cmp.Diff(want, ids); diff != "" {
		t.Errorf("List() order mismatch (-want +got):\n%s", diff)
	}
}

// testLifecycleTimes pins that each of the three optional times reads back as written to the
// nanosecond, and that a time never set reads back unset rather than as the zero time, which the
// issuer would read as a key that activated, retired, or was removed at the start of the epoch.
func testLifecycleTimes(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	created := time.Date(2026, 10, 1, 9, 0, 0, 111, time.UTC)
	activated := created.Add(24*time.Hour + 222)
	retired := activated.Add(30*24*time.Hour + 333)
	removed := retired.Add(24*time.Hour + 444)
	scheduled := &federation.Key{
		ID: "kid_scheduled", Algorithm: "RS256", PublicKey: "pub-scheduled", Sealed: "sealed-s",
		CreatedAt: created, ActivatedAt: &activated, RetiredAt: &retired, RemovedAt: &removed,
	}
	neverSigned := &federation.Key{
		ID: "kid_never", Algorithm: "RS256", PublicKey: "pub-never", CreatedAt: created.Add(time.Second),
		RemovedAt: &removed,
	}
	for _, k := range []*federation.Key{scheduled, neverSigned} {
		if err := store.Save(ctx, k); err != nil {
			t.Fatalf("Save(%s) error = %v", k.ID, err)
		}
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []*federation.Key{scheduled, neverSigned}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("lifecycle times mismatch (-want +got):\n%s", diff)
	}
}

// testRemoveKeepsRecord pins that removing a key is a save that empties its private half and stamps
// its removal, and that the row stays: the key's public half and its times are the record of when
// it was trusted, which a token issuance in the audit chain names by id.
func testRemoveKeepsRecord(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	k := sampleKey("kid_removed", at)
	if err := store.Save(ctx, k); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	gone := at.Add(48 * time.Hour)
	retired := at.Add(24 * time.Hour)
	k.Sealed, k.RetiredAt, k.RemovedAt = "", &retired, &gone
	if err := store.Save(ctx, k); err != nil {
		t.Fatalf("Save() remove error = %v", err)
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("List() returned %d keys after a removal, want the one key kept as a record", len(got))
	}
	if got[0].Sealed != "" {
		t.Error("the removed key's private half survived the save that erased it")
	}
	if got[0].PublicKey == "" || got[0].RemovedAt == nil || !got[0].RemovedAt.Equal(gone) {
		t.Errorf("the removed key's record = %+v, want its public half and removal time", got[0])
	}
}

// testRemovalIsFinal pins the writes a store refuses whoever makes them: giving back a private half
// it holds erased, and clearing or postponing a removal it holds. A process that read a key before
// an emergency rotation removed it makes exactly those writes when it saves its stale copy, and the
// refusal leaves the stored key as it was. Moving a removal earlier and erasing stay allowed, since
// that is what an emergency rotation does to a key a normal one scheduled.
func testRemovalIsFinal(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	removal := at.Add(48 * time.Hour)
	tests := []struct {
		Erased bool
		Next   func(k *federation.Key)
		Want   error
	}{{ // Test 0: An erased private half is never written back.
		Erased: true, Next: func(k *federation.Key) { k.Sealed = "sealed-again" },
		Want: federation.ErrKeyRemoved,
	}, { // Test 1: A removal is never cleared.
		Next: func(k *federation.Key) { k.RemovedAt = nil }, Want: federation.ErrKeyRemoved,
	}, { // Test 2: A removal never moves later, even by a nanosecond.
		Next: func(k *federation.Key) {
			later := removal.Add(time.Nanosecond)
			k.RemovedAt = &later
		},
		Want: federation.ErrKeyRemoved,
	}, { // Test 3: A removal may move earlier, as an emergency rotation moves it.
		Next: func(k *federation.Key) {
			earlier := at.Add(time.Hour)
			k.RemovedAt = &earlier
		},
	}, { // Test 4: Erasing the private half is always allowed.
		Next: func(k *federation.Key) { k.Sealed = "" },
	}, { // Test 5: Saving the key unchanged is allowed.
		Erased: true, Next: func(*federation.Key) {},
	}}
	for testNum, test := range tests {
		id := fmt.Sprintf("kid_final_%d", testNum)
		stored := sampleKey(id, at)
		retired := at.Add(24 * time.Hour)
		stored.RetiredAt, stored.RemovedAt = &retired, cloneTime(removal)
		if test.Erased {
			stored.Sealed = ""
		}
		if err := store.Save(ctx, stored); err != nil {
			t.Fatalf("test %d: Save() error = %v", testNum, err)
		}
		next := sampleKey(id, at)
		next.Sealed, next.RetiredAt, next.RemovedAt = stored.Sealed, &retired, cloneTime(removal)
		test.Next(next)
		err := store.Save(ctx, next)
		if !errors.Is(err, test.Want) {
			t.Errorf("test %d: Save() error = %v, want %v", testNum, err, test.Want)
		}
		want := stored
		if test.Want == nil {
			want = next
		}
		if diff := cmp.Diff(want, keyByID(t, store, id), cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("test %d: the stored key after the save (-want +got):\n%s", testNum, diff)
		}
	}
}

// testFailedChangeKeepsNothing pins that a change whose function fails keeps none of its writes,
// which is what lets a rotation interrupted between its writes leave the keys as they were rather
// than a new key published with nothing scheduled to retire the old one.
func testFailedChangeKeepsNothing(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	if err := store.Save(ctx, sampleKey("kid_kept", at)); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	gaveUp := errors.New("the change gave up between its writes")
	err := store.Change(ctx, func(ctx context.Context) error {
		if err := store.Save(ctx, sampleKey("kid_new", at.Add(time.Hour))); err != nil {
			return err
		}
		kept := sampleKey("kid_kept", at)
		retired := at.Add(25 * time.Hour)
		kept.RetiredAt = &retired
		if err := store.Save(ctx, kept); err != nil {
			return err
		}
		return gaveUp
	})
	if !errors.Is(err, gaveUp) {
		t.Fatalf("Change() error = %v, want %v", err, gaveUp)
	}
	got, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff([]*federation.Key{sampleKey("kid_kept", at)}, got,
		cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the keys after a failed change (-want +got):\n%s", diff)
	}
}

// testChangeIsolation pins that a change reads its own writes, and that a read outside it does not
// see them until it ends, so no process acts on half a rotation.
func testChangeIsolation(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	err := store.Change(ctx, func(inner context.Context) error {
		if err := store.Save(inner, sampleKey("kid_inside", at)); err != nil {
			return err
		}
		if ids := keyIDs(t, store, inner); !slices.Contains(ids, "kid_inside") {
			t.Errorf("inside the change the keys are %v, want its own write among them", ids)
		}
		if ids := keyIDs(t, store, ctx); len(ids) != 0 {
			t.Errorf("outside the change the keys are %v before it ended, want none", ids)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Change() error = %v", err)
	}
	if diff := cmp.Diff([]string{"kid_inside"}, keyIDs(t, store, ctx)); diff != "" {
		t.Errorf("the keys after the change (-want +got):\n%s", diff)
	}
}

// testChangesSerialize pins that a second change waits for the first to end and then sees what it
// wrote, so two rotations, or a rotation and an emergency rotation, never decide on the same keys.
func testChangesSerialize(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	inside, release := make(chan struct{}), make(chan struct{})
	first := make(chan error, 1)
	go func() {
		first <- store.Change(ctx, func(ctx context.Context) error {
			if err := store.Save(ctx, sampleKey("kid_first", at)); err != nil {
				close(inside)
				return err
			}
			close(inside)
			<-release
			return nil
		})
	}()
	<-inside
	type result struct {
		// IDs are the keys the second change read.
		IDs []string
		// Err is the second change's error.
		Err error
	}
	second := make(chan result, 1)
	go func() {
		var ids []string
		err := store.Change(ctx, func(ctx context.Context) error {
			keys, err := store.List(ctx)
			for _, k := range keys {
				ids = append(ids, k.ID)
			}
			return err
		})
		second <- result{IDs: ids, Err: err}
	}()
	select {
	case got := <-second:
		t.Errorf("a second change ran while the first was still open and read %v", got.IDs)
		close(release)
		return
	case <-time.After(300 * time.Millisecond):
	}
	close(release)
	if err := <-first; err != nil {
		t.Fatalf("the first Change() error = %v", err)
	}
	got := <-second
	if got.Err != nil {
		t.Fatalf("the second Change() error = %v", got.Err)
	}
	if !slices.Contains(got.IDs, "kid_first") {
		t.Errorf("the second change read %v, want the key the first one wrote", got.IDs)
	}
}

// testChangeClock pins that the clock reads the same inside a change however long it runs, so every
// time a decision writes and judges by is one instant.
func testChangeClock(t *testing.T, store federation.KeyStore) {
	ctx := context.Background()
	err := store.Change(ctx, func(ctx context.Context) error {
		first, err := store.Now(ctx)
		if err != nil {
			return err
		}
		time.Sleep(5 * time.Millisecond)
		again, err := store.Now(ctx)
		if err != nil {
			return err
		}
		if !first.Equal(again) {
			t.Errorf("the clock inside one change read %v and then %v", first, again)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Change() error = %v", err)
	}
}

// keyByID returns the stored key with id, failing the test when there is none.
func keyByID(t *testing.T, store federation.KeyStore, id string) *federation.Key {
	t.Helper()
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, k := range keys {
		if k.ID == id {
			return k
		}
	}
	t.Fatalf("key %s is not stored", id)
	return nil
}

// keyIDs returns the ids of the keys store lists under ctx, oldest first.
func keyIDs(t *testing.T, store federation.KeyStore, ctx context.Context) []string {
	t.Helper()
	keys, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	ids := []string{}
	for _, k := range keys {
		ids = append(ids, k.ID)
	}
	return ids
}

// cloneTime returns a pointer to a copy of t.
func cloneTime(t time.Time) *time.Time { return &t }
