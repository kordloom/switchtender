package federation

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// lockWait bounds how long a test waits for a simulated process to reach a point it must reach. It
// is generous because generating an RSA key under the race detector takes seconds.
const lockWait = 30 * time.Second

// heldWindow is how long a process holding the store's lock keeps it once the racing process has
// asked for it. A racing process the lock does not hold back finishes well inside it.
const heldWindow = 300 * time.Millisecond

// insideHookStore is one process's view of a key store other processes share. It runs a hook once,
// from inside a change, right after the read the change decides on, so a test can let another
// process act while this one holds the store's lock.
type insideHookStore struct {
	// KeyStore is the shared store every simulated process reads and writes.
	KeyStore
	// mu guards hook.
	mu sync.Mutex
	// hook runs once, after the next List made inside a change.
	hook func()
}

// List reads the store and, inside a change, runs the pending hook before returning what it read.
func (s *insideHookStore) List(ctx context.Context) ([]*Key, error) {
	keys, err := s.KeyStore.List(ctx)
	if _, inside := ctx.Value(memChangeKey{}).(*memChange); !inside {
		return keys, err
	}
	s.mu.Lock()
	hook := s.hook
	s.hook = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return keys, err
}

// enteredStore is one process's view of a shared key store that reports when the process first
// asks for a change, which is when it starts waiting for the store's lock.
type enteredStore struct {
	// KeyStore is the shared store every simulated process reads and writes.
	KeyStore
	// once closes entered on the first change.
	once sync.Once
	// entered closes when the first change is asked for.
	entered chan struct{}
}

// Change reports that a change was asked for, then runs it.
func (s *enteredStore) Change(ctx context.Context, fn func(ctx context.Context) error) error {
	s.once.Do(func() { close(s.entered) })
	return s.KeyStore.Change(ctx, fn)
}

// TestARotationHoldingTheLockMakesARacingRotationWait is a rotation on one replica that has read
// the keys under the store's lock when a rotation on another replica asks for the lock. The second
// must wait until the first has written and released the lock, and then decide on what the first
// wrote: a second routine rotation is refused as one already under way, and an emergency rotation
// removes the key the first one published. A store whose changes did not serialize would let the
// second decide on keys the first was about to overwrite.
func TestARotationHoldingTheLockMakesARacingRotationWait(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Emergency     bool
		WantSecond    error
		WantPublished func(first, second KeyInfo, old string) []string
	}{{ // Test 0: A second routine rotation waits and is then refused with 409.
		WantSecond: ErrRotationPending,
		WantPublished: func(first, _ KeyInfo, old string) []string {
			return []string{old, first.ID}
		},
	}, { // Test 1: An emergency rotation waits and then removes the key the first one published.
		Emergency: true,
		WantPublished: func(_, second KeyInfo, _ string) []string {
			return []string{second.ID}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			shared := NewMemKeyStore()
			clock := &testClock{now: lifecycleStart}
			firstView := &insideHookStore{KeyStore: shared}
			secondView := &enteredStore{KeyStore: shared, entered: make(chan struct{})}
			first := fedReplica(t, firstView, clock)
			second := fedReplica(t, secondView, clock)
			if err := first.Ensure(ctx); err != nil {
				t.Fatalf("Ensure() error = %v", err)
			}
			old := mintKID(t, first)
			clock.Advance(time.Hour)

			var secondInfo KeyInfo
			var secondErr error
			done := make(chan struct{})
			finishedWhileHeld := false
			firstView.mu.Lock()
			firstView.hook = func() {
				go func() {
					defer close(done)
					if test.Emergency {
						secondInfo, secondErr = second.EmergencyRotate(ctx)
					} else {
						secondInfo, secondErr = second.Rotate(ctx)
					}
				}()
				select {
				case <-secondView.entered:
				case <-done:
				case <-time.After(lockWait):
					t.Error("the racing rotation never asked for the store's lock")
				}
				select {
				case <-done:
					finishedWhileHeld = true
				case <-time.After(heldWindow):
				}
			}
			firstView.mu.Unlock()
			firstInfo, err := first.Rotate(ctx)
			if err != nil {
				t.Fatalf("the first Rotate() error = %v", err)
			}
			fedAwait(t, done)
			if finishedWhileHeld {
				t.Error("the racing rotation finished while the first one held the store's lock")
			}
			if !errors.Is(secondErr, test.WantSecond) {
				t.Fatalf("the racing rotation error = %v, want %v", secondErr, test.WantSecond)
			}
			want := test.WantPublished(firstInfo, secondInfo, old)
			if diff := cmp.Diff(want, publishedIDs(t, first)); diff != "" {
				t.Errorf("published keys after both rotations (-want +got):\n%s", diff)
			}
			for _, k := range fedStored(t, shared) {
				if !slices.Contains(want, k.ID) && k.Sealed != "" {
					t.Errorf("key %s left the published set and kept its private half", k.ID)
				}
			}
		})
	}
}

// countingHookStore is one process's view of a shared key store that runs a hook before chosen
// reads, numbered from one, so a test can change the store between two reads of one operation.
type countingHookStore struct {
	// KeyStore is the shared store every simulated process reads and writes.
	KeyStore
	// mu guards calls.
	mu sync.Mutex
	// calls counts the reads so far.
	calls int
	// before runs before read number n and says nothing about whether the read goes ahead.
	before func(n int)
}

// List runs the hook for this read, then reads the store.
func (s *countingHookStore) List(ctx context.Context) ([]*Key, error) {
	s.mu.Lock()
	s.calls++
	n := s.calls
	s.mu.Unlock()
	s.before(n)
	return s.KeyStore.List(ctx)
}

// TestMintGivesUpWhenItsKeyKeepsBeingRemoved is a mint whose signing key an emergency rotation on
// another replica removes between every signature and the read after it. Each token is discarded
// unrecorded, and after mintAttempts tries the mint fails rather than signing forever or handing
// out a token whose key is gone.
func TestMintGivesUpWhenItsKeyKeepsBeingRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shared := NewMemKeyStore()
	clock := &testClock{now: lifecycleStart}
	rec := &recordedIssuances{}
	responder := fedReplica(t, shared, clock)
	if err := responder.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	removals := 0
	view := &countingHookStore{KeyStore: shared, before: func(n int) {
		// A mint reads the keys, signs, and reads them again: every second read follows a signature.
		if n%2 == 0 {
			if _, err := responder.EmergencyRotate(ctx); err != nil {
				t.Errorf("EmergencyRotate() error = %v", err)
			}
			removals++
		}
	}}
	worker := fedReplica(t, view, clock, WithIssuanceRecorder(rec))
	token, _, err := worker.Mint(ctx, sampleClaims(), DefaultTokenTTL)
	if err == nil {
		t.Fatalf("Mint() returned a token after its key was removed %d times", removals)
	}
	if token != "" {
		t.Error("Mint() failed and still returned a token")
	}
	if removals != mintAttempts {
		t.Errorf("the key was removed %d times before Mint gave up, want %d", removals, mintAttempts)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	if len(rec.got) != 0 {
		t.Errorf("Mint recorded %d issuances for tokens it discarded", len(rec.got))
	}
}

// legacyKey returns a sealed key created at created that activates at activation and retires at
// retirement, which may be nil, standing for a key an earlier build stored.
func legacyKey(t *testing.T, created, activation time.Time, retirement *time.Time) *Key {
	t.Helper()
	k, err := newKey(testSealer)
	if err != nil {
		t.Fatalf("newKey() error = %v", err)
	}
	stamp(k, created, activation)
	if retirement != nil {
		k.RetiredAt = cloneTime(retirement)
		removal := retirement.Add(RetiredKeyGrace)
		k.RemovedAt = &removal
	}
	return k
}

// settleCheckpoint is one moment a settling test mints at, with the key it must sign with there.
type settleCheckpoint struct {
	// At is the moment, as an offset from lifecycleStart.
	At time.Duration
	// WantSigner is the index of the key that signs then.
	WantSigner int
}

// TestSettlingRetiresAKeyNoScheduleWouldRetire stores the key histories an earlier build could
// leave behind, in which a key signs with nothing scheduled to retire, remove, or erase it, and
// pins that minting settles them: at every checkpoint no such key is left, the expected key signs,
// and once the last checkpoint is past every key but the signer is unpublished with its private
// half erased.
func TestSettlingRetiresAKeyNoScheduleWouldRetire(t *testing.T) {
	t.Parallel()
	day := RotationDelay
	tests := []struct {
		Keys        func(t *testing.T) []*Key
		Checkpoints []settleCheckpoint
	}{{ // Test 0: A rotation interrupted between its writes, leaving nothing to retire the old key.
		Keys: func(t *testing.T) []*Key {
			return []*Key{
				legacyKey(t, lifecycleStart.Add(-30*day), lifecycleStart.Add(-30*day), nil),
				legacyKey(t, lifecycleStart, lifecycleStart.Add(day), nil),
			}
		},
		Checkpoints: []settleCheckpoint{
			{At: time.Hour, WantSigner: 0}, {At: day, WantSigner: 1},
			{At: 2*day + time.Minute, WantSigner: 1},
		},
	}, { // Test 1: Two first keys generated at once, both signing.
		Keys: func(t *testing.T) []*Key {
			return []*Key{
				legacyKey(t, lifecycleStart.Add(-time.Hour), lifecycleStart.Add(-time.Hour), nil),
				legacyKey(t, lifecycleStart.Add(-time.Minute), lifecycleStart.Add(-time.Minute), nil),
			}
		},
		Checkpoints: []settleCheckpoint{
			{At: 0, WantSigner: 1}, {At: day + time.Minute, WantSigner: 1},
		},
	}, { // Test 2: Two rotations at once, two pending keys and the old key retiring at the later.
		Keys: func(t *testing.T) []*Key {
			later := lifecycleStart.Add(day + time.Minute)
			return []*Key{
				legacyKey(t, lifecycleStart.Add(-30*day), lifecycleStart.Add(-30*day), &later),
				legacyKey(t, lifecycleStart, lifecycleStart.Add(day), nil),
				legacyKey(t, lifecycleStart.Add(time.Minute), later, nil),
			}
		},
		Checkpoints: []settleCheckpoint{
			{At: time.Hour, WantSigner: 0}, {At: day, WantSigner: 1},
			{At: day + time.Minute, WantSigner: 2}, {At: 2*day + 2*time.Minute, WantSigner: 2},
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			clock := &testClock{now: lifecycleStart}
			iss, store := newTestIssuer(t, "https://st.example.com", clock)
			keys := test.Keys(t)
			for _, k := range keys {
				if err := store.Save(ctx, k); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
			}
			for _, cp := range test.Checkpoints {
				clock.mu.Lock()
				clock.now = lifecycleStart.Add(cp.At)
				clock.mu.Unlock()
				if got, want := mintKID(t, iss), keys[cp.WantSigner].ID; got != want {
					t.Errorf("at %v the install signs with %s, want %s", cp.At, got, want)
				}
				if orphans := fedOrphans(t, store, clock.Now()); len(orphans) > 0 {
					t.Errorf("at %v keys %v sign with nothing scheduled to retire them", cp.At,
						orphans)
				}
			}
			last := keys[test.Checkpoints[len(test.Checkpoints)-1].WantSigner].ID
			for _, k := range fedStored(t, store) {
				if k.ID == last {
					continue
				}
				if published(k, clock.Now()) || k.Sealed != "" {
					t.Errorf("key %s is %s and holds its private half %v once its schedule is "+
						"done", k.ID, keyState(k, clock.Now()), k.Sealed != "")
				}
			}
		})
	}
}
