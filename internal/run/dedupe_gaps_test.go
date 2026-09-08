package run

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// failingKeyStore answers every idempotency-key lookup with a fixed error, so a caller's handling
// of a store failure can be tested without a database. Every other method is inherited and unused.
type failingKeyStore struct {
	Store
	// err is what ByIdempotencyKey returns.
	err error
}

// ByIdempotencyKey returns the configured failure.
func (f failingKeyStore) ByIdempotencyKey(context.Context, string) (*Run, error) {
	return nil, f.err
}

// TestDedupeKeysAreDeterministicAndScoped pins that the derived key depends on the action, the run,
// and the time bucket, and on nothing else.
//
// The key has to be recomputable by another control node and by the same node after a restart,
// which is why it is wall-clock derived. Two different actions on the same run, or the same action
// on two runs, must never collide, or one would swallow the other and a change somebody asked for
// would never happen.
func TestDedupeKeysAreDeterministicAndScoped(t *testing.T) {
	t.Parallel()
	// A time sitting at the very start of a bucket, so the offsets below are exact.
	base := time.Unix(0, (time.Now().UnixNano()/int64(DedupeWindow))*int64(DedupeWindow)).UTC()

	same := DedupeKey("rerun", "run_a", base)
	if again := DedupeKey("rerun", "run_a", base.Add(DedupeWindow-time.Nanosecond)); again != same {
		t.Errorf("two moments in one bucket derived %q and %q, so a double click would not "+
			"collapse", same, again)
	}
	tests := []struct {
		Name   string
		Action string
		ID     string
		At     time.Time
	}{
		{Name: "another action", Action: "cancel", ID: "run_a", At: base}, // Test 0.
		{Name: "another run", Action: "rerun", ID: "run_b", At: base},     // Test 1.
		{Name: "the next bucket", Action: "rerun", ID: "run_a",
			At: base.Add(DedupeWindow)}, // Test 2.
		{Name: "the previous bucket", Action: "rerun", ID: "run_a",
			At: base.Add(-DedupeWindow)}, // Test 3.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := DedupeKey(test.Action, test.ID, test.At); got == same {
				t.Errorf("%s derived the same key %q, so one request would swallow the other",
					test.Name, got)
			}
		})
	}

	// Every derived key sits inside the reserved namespace, which is what keeps a caller from
	// planting a run under one.
	if !strings.HasPrefix(same, internalKeyPrefix) {
		t.Errorf("DedupeKey() = %q, want the reserved prefix %q", same, internalKeyPrefix)
	}
}

// TestClientKeyRefusalsAreExact pins the two shapes a caller may not send and the boundary of each.
//
// The reserved prefix is what stops a caller planting a run under the key a later rerun derives,
// which would make the rerun resolve to the planted run and never execute. The null byte is what
// separates an organization from its key, so a key containing one could spell another
// organization's stored key and reach their run.
func TestClientKeyRefusalsAreExact(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Supplied   string
		OrgID      string
		WantStored string
		Want       error
	}{{ // Test 0: An ordinary key on an install with no organizations is stored as sent.
		Name: "plain key, no org", Supplied: "nightly", WantStored: "nightly",
	}, { // Test 1: The same key under an organization is scoped to it.
		Name: "plain key, org", Supplied: "nightly", OrgID: "org_acme",
		WantStored: "org_acme\x00nightly",
	}, { // Test 2: The reserved prefix is refused whatever follows it.
		Name: "reserved prefix", Supplied: "st:anything", Want: ErrReservedKey,
	}, { // Test 3: The prefix alone is refused.
		Name: "reserved prefix alone", Supplied: "st:", Want: ErrReservedKey,
	}, { // Test 4: The refusal is on the prefix, so a key that merely starts with the letters is
		// allowed. Refusing more than the namespace would reject ordinary words.
		Name: "starts with the letters", Supplied: "static-key", WantStored: "static-key",
	}, { // Test 5: The prefix inside a key is not the prefix.
		Name: "prefix in the middle", Supplied: "nightly:st:x", WantStored: "nightly:st:x",
	}, { // Test 6: A null byte anywhere is refused, since it is the organization separator.
		Name: "null byte", Supplied: "night\x00ly", Want: ErrReservedKey,
	}, { // Test 7: A key shaped like another organization's stored key is refused, so it cannot be
		// forged from outside.
		Name: "forged org scope", Supplied: "org_globex\x00nightly", OrgID: "org_acme",
		Want: ErrReservedKey,
	}, { // Test 8: A leading null byte is refused too.
		Name: "leading null", Supplied: "\x00nightly", Want: ErrReservedKey,
	}, { // Test 9: A unicode key is ordinary text and is stored as sent.
		Name: "unicode key", Supplied: "夜間デプロイ", OrgID: "org_acme",
		WantStored: "org_acme\x00夜間デプロイ",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := ClientKey(test.Supplied, test.OrgID)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ClientKey(%q, %q) error = %v, want %v",
					test.Supplied, test.OrgID, err, test.Want)
			}
			if test.Want != nil {
				if got != "" {
					t.Errorf("a refused key still returned %q", got)
				}
				return
			}
			if got != test.WantStored {
				t.Errorf("ClientKey(%q, %q) = %q, want %q", test.Supplied, test.OrgID, got,
					test.WantStored)
			}
			// A stored key never enters the server's own namespace, whatever the caller sent.
			if strings.HasPrefix(got, internalKeyPrefix) {
				t.Errorf("the stored key %q sits in the reserved namespace", got)
			}
		})
	}
}

// TestClientKeyOfAnEmptyKeyUnderAnOrg records what an empty supplied key becomes.
//
// The submit handler only calls this when the header is present and non-blank, so the empty case is
// unreachable through the API today. It is recorded because the shape is worth seeing: under an
// organization an empty key does not stay empty, it becomes the separator-terminated org prefix,
// which is a real key that a second empty submission from the same organization would collide with.
// A caller reaching this function from anywhere else has to keep the handler's guard.
func TestClientKeyOfAnEmptyKeyUnderAnOrg(t *testing.T) {
	t.Parallel()
	plain, err := ClientKey("", "")
	if err != nil || plain != "" {
		t.Errorf("ClientKey(\"\", \"\") = (%q, %v), want an empty key, which never dedupes",
			plain, err)
	}
	scoped, err := ClientKey("", "org_acme")
	if err != nil {
		t.Fatalf("ClientKey(\"\", org) error = %v", err)
	}
	if scoped != "org_acme\x00" {
		t.Errorf("ClientKey(\"\", org) = %q, want the org prefix", scoped)
	}
	if scoped == "" {
		t.Error("an empty key stayed empty under an organization")
	}
}

// TestResolveDedupeReportsAStoreFailure pins that a lookup failure is returned rather than read as
// "there is no earlier run".
//
// Swallowing it would turn a database blip into a duplicate run: the caller would see no existing
// run, submit a fresh one, and the same change would happen twice. For a destructive playbook that
// is the difference between one deletion and two.
func TestResolveDedupeReportsAStoreFailure(t *testing.T) {
	t.Parallel()
	boom := errors.New("the database went away")
	existing, key, err := ResolveDedupe(context.Background(),
		failingKeyStore{err: boom}, "rerun", "run_a", time.Now())
	if !errors.Is(err, boom) {
		t.Errorf("ResolveDedupe() error = %v, want the store's failure surfaced", err)
	}
	if existing != nil || key != "" {
		t.Errorf("a failed lookup still answered (%v, %q), want nothing usable", existing, key)
	}
}

// TestResolveDedupeOnAnEmptyStoreYieldsAKey pins that the first request of an action gets the key
// its run must carry, since that key is what makes the second click collapse onto this run.
func TestResolveDedupeOnAnEmptyStoreYieldsAKey(t *testing.T) {
	t.Parallel()
	now := time.Now()
	existing, key, err := ResolveDedupe(context.Background(), NewMemStore(), "rerun", "run_a", now)
	if err != nil {
		t.Fatalf("ResolveDedupe() error = %v", err)
	}
	if existing != nil {
		t.Errorf("an empty store resolved to %v, want nothing", existing)
	}
	if key != DedupeKey("rerun", "run_a", now) {
		t.Errorf("key = %q, want the current bucket's derived key", key)
	}
}

// TestResolveDedupeIgnoresAStaleRunInThePreviousBucket pins that a run holding the previous
// bucket's key but recorded outside the window does not collapse a fresh request onto itself, and
// that the request still comes away with a usable key.
//
// The lookup spans two buckets so two clicks either side of a boundary agree. Without bounding the
// match by the run's own creation time, that made the real window anything from one to two buckets
// depending on where in a bucket the first click fell, and a deliberate rerun a whole window later
// would silently return the earlier run instead of running.
func TestResolveDedupeIgnoresAStaleRunInThePreviousBucket(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemStore()
	base := time.Unix(0, (time.Now().UnixNano()/int64(DedupeWindow))*int64(DedupeWindow)).UTC()
	now := base.Add(2 * DedupeWindow)
	stale := now.Add(-DedupeWindow - time.Second)
	seedRun(t, store, "run_stale", DedupeKey("rerun", "run_a", stale), stale)

	existing, key, err := ResolveDedupe(ctx, store, "rerun", "run_a", now)
	if err != nil {
		t.Fatalf("ResolveDedupe() error = %v", err)
	}
	if existing != nil {
		t.Errorf("collapsed onto %s, recorded %s ago, so the requested run never happens",
			existing.ID, now.Sub(existing.CreatedAt))
	}
	if key != DedupeKey("rerun", "run_a", now) {
		t.Errorf("key = %q, want the current bucket's key: it is free, so the fresh run should "+
			"carry it and be protected in turn", key)
	}
}

// TestResolveDedupeExcludesTheWindowsFarEdge pins the exact boundary of the dedupe window: a run
// recorded exactly DedupeWindow ago is outside it, not inside it.
//
// The surrounding tests probe a second either side of the edge, so the comparison could be widened
// from strictly-less-than to less-than-or-equal without any of them noticing. The edge is the one
// place the two differ, and it decides whether a run the operator deliberately asked for executes
// or is silently answered with the previous one.
func TestResolveDedupeExcludesTheWindowsFarEdge(t *testing.T) {
	t.Parallel()
	base := time.Unix(0, (time.Now().UnixNano()/int64(DedupeWindow))*int64(DedupeWindow)).UTC()
	tests := []struct {
		Name     string
		Age      time.Duration
		WantSame bool
	}{{ // Test 0: One nanosecond inside the window is still the same request.
		Name: "last nanosecond inside", Age: DedupeWindow - time.Nanosecond, WantSame: true,
	}, { // Test 1: Exactly one window old is outside it, so the repeat is a fresh run.
		Name: "exactly one window", Age: DedupeWindow, WantSame: false,
	}, { // Test 2: One nanosecond past the edge is plainly outside.
		Name: "first nanosecond outside", Age: DedupeWindow + time.Nanosecond, WantSame: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			store := NewMemStore()
			now := base.Add(2 * DedupeWindow)
			created := now.Add(-test.Age)
			seedRun(t, store, "run_first", DedupeKey("rerun", "run_a", created), created)

			existing, _, err := ResolveDedupe(context.Background(), store, "rerun", "run_a", now)
			if err != nil {
				t.Fatalf("ResolveDedupe() error = %v", err)
			}
			if gotSame := existing != nil; gotSame != test.WantSame {
				t.Errorf("collapsed onto the existing run = %v, want %v (age %s, window %s)",
					gotSame, test.WantSame, test.Age, DedupeWindow)
			}
		})
	}
}
