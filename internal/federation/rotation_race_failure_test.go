package federation

import (
	"context"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
)

// fedRaceIssuer is the issuer URL every simulated process in these tests serves.
const fedRaceIssuer = "https://st.example.com"

// fedRaceWait bounds how long a paused process waits for the process racing it. Unfixed code has
// no cross-process serialization, so the racing process finishes well inside it. A fix that
// serializes rotations across processes blocks the racing process until the paused one is done,
// and the wait then times out and lets the paused process go on, so a fix never deadlocks a test.
const fedRaceWait = 10 * time.Second

// fedHookStore is one process's view of a key store other processes write too. It runs a hook
// once, after a List has read the shared store and before the caller sees what it read, so a test
// can let another process act at exactly that point. It can also fail chosen saves, standing for a
// crash or a dropped database connection between two writes.
type fedHookStore struct {
	// KeyStore is the shared store every simulated process reads and writes.
	KeyStore
	// mu guards afterList and failSave.
	mu sync.Mutex
	// afterList runs once, after the next List has read the shared store.
	afterList func()
	// failSave returns an error for a save that must fail and nil for one that goes through.
	failSave func(k *Key) error
}

// List reads the shared store, then runs the pending hook before returning what it read.
func (s *fedHookStore) List(ctx context.Context) ([]*Key, error) {
	keys, err := s.KeyStore.List(ctx)
	s.mu.Lock()
	hook := s.afterList
	s.afterList = nil
	s.mu.Unlock()
	if hook != nil {
		hook()
	}
	return keys, err
}

// Save fails when failSave says so and otherwise writes through to the shared store.
func (s *fedHookStore) Save(ctx context.Context, k *Key) error {
	s.mu.Lock()
	fail := s.failSave
	s.mu.Unlock()
	if fail != nil {
		if err := fail(k); err != nil {
			return err
		}
	}
	return s.KeyStore.Save(ctx, k)
}

// fedPauseAfterList arranges for other to run, as another process, while the next List on view
// is paused between reading the store and returning. The paused process waits up to fedRaceWait
// for other and then carries on with what it read. The returned channel closes once other has
// finished.
func fedPauseAfterList(view *fedHookStore, other func()) <-chan struct{} {
	done := make(chan struct{})
	view.mu.Lock()
	defer view.mu.Unlock()
	view.afterList = func() {
		go func() {
			defer close(done)
			other()
		}()
		select {
		case <-done:
		case <-time.After(fedRaceWait):
		}
	}
	return done
}

// fedAwait waits for done, failing the test rather than hanging when it never closes.
func fedAwait(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(6 * fedRaceWait):
		t.Fatal("the racing process never finished")
	}
}

// fedFinished reports whether done has closed.
func fedFinished(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// fedReplica returns an issuer standing for one server or worker process that reads and writes
// store and reads clock.
func fedReplica(t *testing.T, store KeyStore, clock *testClock, opts ...Option) *Issuer {
	t.Helper()
	iss, err := NewIssuer(fedRaceIssuer, store, testSealer, append([]Option{WithClock(clock.Now)},
		opts...)...)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	return iss
}

// fedLeak opens k's private half, standing for the compromise an emergency rotation answers: a key
// that leaked, from a memory dump, a log, or the database together with the encryption key.
func fedLeak(t *testing.T, k *Key) *rsa.PrivateKey {
	t.Helper()
	priv, err := openPrivate(testSealer, k)
	if err != nil {
		t.Fatalf("open the key that leaks: %v", err)
	}
	return priv
}

// fedForge signs a token for the sample run with priv under kid, as whoever holds a leaked key
// would, without going through any issuer.
func fedForge(t *testing.T, priv *rsa.PrivateKey, kid string, now time.Time) string {
	t.Helper()
	c := sampleClaims()
	c.Issuer, c.Subject, c.ID = fedRaceIssuer, sampleSubject, "forged-by-whoever-holds-the-key"
	c.IssuedAt, c.NotBefore = now.Unix(), now.Unix()
	c.ExpiresAt = now.Add(DefaultTokenTTL).Unix()
	payload, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("encode the forged claims: %v", err)
	}
	signer, err := jose.NewSigner(jose.SigningKey{
		Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: priv, KeyID: kid},
	}, (&jose.SignerOptions{}).WithType("JWT"))
	if err != nil {
		t.Fatalf("build the forging signer: %v", err)
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign the forged token: %v", err)
	}
	token, err := jws.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize the forged token: %v", err)
	}
	return token
}

// fedVerifies reports whether token verifies against the key set iss publishes, which is what a
// cloud checks once it has fetched that set.
func fedVerifies(t *testing.T, iss *Issuer, token string) bool {
	t.Helper()
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("ParseSigned() error = %v", err)
	}
	set, err := iss.JWKS(context.Background())
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	for _, k := range set.Key(jws.Signatures[0].Header.KeyID) {
		if _, err := jws.Verify(k.Key); err == nil {
			return true
		}
	}
	return false
}

// fedOrphans returns the ids of keys that sign by now, other than the one that signs new tokens,
// with no retirement scheduled: keys no schedule will ever retire, remove, or erase.
func fedOrphans(t *testing.T, store KeyStore, now time.Time) []string {
	t.Helper()
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	current := signer(keys, now)
	var out []string
	for _, k := range keys {
		if current != nil && k.ID == current.ID {
			continue
		}
		if signs(k, now) && k.RetiredAt == nil {
			out = append(out, k.ID)
		}
	}
	return out
}

// TestFederationRotationRacingAnEmergencyKeepsTheCompromisedKeyOut is two replicas on one
// database. An admin on one asks for a routine rotation while an admin on the other answers a
// suspected compromise with an emergency rotation. The routine rotation reads the keys, the
// emergency rotation runs to completion, and the routine rotation then writes back the copy of the
// compromised key it read earlier, sealed private half and all, with a removal two days out.
//
// The promise broken is the emergency rotation's: every other key leaves the published set at that
// moment with its private half erased, and nothing the removed key signs verifies again. After the
// race the compromised key is published again for two days, its private half is back in the store,
// and a token forged with the leaked key verifies against the key set the install serves.
func TestFederationRotationRacingAnEmergencyKeepsTheCompromisedKeyOut(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shared := NewMemKeyStore()
	clock := &testClock{now: lifecycleStart}
	routineView := &fedHookStore{KeyStore: shared}
	routine := fedReplica(t, routineView, clock)
	responder := fedReplica(t, shared, clock)
	if err := responder.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	compromisedID := mintKID(t, responder)
	leaked := fedLeak(t, storedKey(t, shared, compromisedID))
	clock.Advance(time.Hour)

	var emergency KeyInfo
	var emergencyErr error
	done := fedPauseAfterList(routineView, func() {
		emergency, emergencyErr = responder.EmergencyRotate(ctx)
	})
	_, routineErr := routine.Rotate(ctx)
	fedAwait(t, done)
	if emergencyErr != nil {
		t.Fatalf("EmergencyRotate() error = %v", emergencyErr)
	}
	t.Logf("the racing routine rotation returned %v", routineErr)

	after := storedKey(t, shared, compromisedID)
	if after.Sealed != "" {
		t.Errorf("the compromised key's private half is back in the store after the emergency " +
			"rotation erased it")
	}
	if slices.Contains(publishedIDs(t, responder), compromisedID) {
		t.Errorf("the compromised key %s is published again after the emergency rotation, now "+
			"until %v", compromisedID, after.RemovedAt)
	}
	clock.Advance(time.Minute)
	if fedVerifies(t, responder, fedForge(t, leaked, compromisedID, clock.Now())) {
		t.Errorf("a token forged with the compromised key verifies against the key set the install " +
			"publishes after the emergency rotation")
	}
	if got := mintKID(t, responder); got != emergency.ID {
		t.Errorf("the install signs with %s after the emergency rotation, want the emergency key %s",
			got, emergency.ID)
	}
}

// TestFederationEmergencyRotationRemovesAKeyARacingRotationPublished is the race the other way
// round. The emergency rotation reads the keys, and a routine rotation on the other replica runs to
// completion, publishing a new key and answering its admin, before the emergency rotation returns.
//
// The promise broken is that an emergency rotation treats every stored key as compromised and takes
// every other key out of the published set, a key a normal rotation published among them. The key
// the routine rotation published survives, stays published with its private half, and a day later
// becomes the key the install signs with, while the emergency key is never scheduled to retire.
func TestFederationEmergencyRotationRemovesAKeyARacingRotationPublished(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shared := NewMemKeyStore()
	clock := &testClock{now: lifecycleStart}
	responderView := &fedHookStore{KeyStore: shared}
	responder := fedReplica(t, responderView, clock)
	routine := fedReplica(t, shared, clock)
	if err := routine.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	clock.Advance(time.Hour)

	var raced KeyInfo
	var routineErr error
	done := fedPauseAfterList(responderView, func() { raced, routineErr = routine.Rotate(ctx) })
	emergency, err := responder.EmergencyRotate(ctx)
	if err != nil {
		t.Fatalf("EmergencyRotate() error = %v", err)
	}
	finishedFirst := fedFinished(done)
	fedAwait(t, done)
	if routineErr != nil {
		t.Logf("the racing routine rotation returned %v", routineErr)
	}

	if finishedFirst && routineErr == nil {
		if slices.Contains(publishedIDs(t, responder), raced.ID) {
			t.Errorf("key %s, published by a routine rotation that answered its admin before the "+
				"emergency rotation returned, is still published after the emergency rotation",
				raced.ID)
		}
		if storedKey(t, shared, raced.ID).Sealed != "" {
			t.Errorf("key %s keeps its private half after the emergency rotation", raced.ID)
		}
	}
	if orphans := fedOrphans(t, shared, clock.Now().Add(RotationDelay+time.Minute)); len(orphans) > 0 {
		t.Errorf("keys %v sign with no retirement scheduled once the pending key activates, so no "+
			"schedule ever removes them or erases their private halves", orphans)
	}
	clock.Advance(RotationDelay + time.Minute)
	if got := mintKID(t, responder); finishedFirst && routineErr == nil && got != emergency.ID {
		t.Errorf("a day after the emergency rotation the install signs with %s, the key the racing "+
			"rotation published, rather than the emergency key %s", got, emergency.ID)
	}
}

// TestFederationConcurrentRotationsOnTwoReplicasStartOneRotation is a routine rotation asked for on
// two replicas at once, a double submit a load balancer spread across both, or two admins. Each
// reads the keys before the other writes, so neither sees a rotation under way.
//
// The promise broken is that a normal rotation asked for while another is under way is refused with
// 409. Both succeed and publish a key each. Once they activate, one of the two signs and the other
// stays signing in the listing with no retirement, published and holding its private half until
// some later rotation happens to retire it.
func TestFederationConcurrentRotationsOnTwoReplicasStartOneRotation(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shared := NewMemKeyStore()
	clock := &testClock{now: lifecycleStart}
	firstView := &fedHookStore{KeyStore: shared}
	first := fedReplica(t, firstView, clock)
	second := fedReplica(t, shared, clock)
	if err := first.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	clock.Advance(time.Hour)

	var secondErr error
	done := fedPauseAfterList(firstView, func() { _, secondErr = second.Rotate(ctx) })
	_, firstErr := first.Rotate(ctx)
	fedAwait(t, done)
	refused := 0
	for _, err := range []error{firstErr, secondErr} {
		switch {
		case err == nil:
		case errors.Is(err, ErrRotationPending):
			refused++
		default:
			t.Errorf("Rotate() error = %v, want nil or ErrRotationPending, the documented 409", err)
		}
	}
	if refused != 1 {
		t.Errorf("%d of two concurrent rotations were refused, want exactly one: the second must "+
			"answer 409 rather than publish a second pending key", refused)
	}

	clock.Advance(RotationDelay + RetiredKeyGrace + time.Hour)
	current := mintKID(t, second)
	for _, k := range fedStored(t, shared) {
		if k.ID == current {
			continue
		}
		if published(k, clock.Now()) || k.Sealed != "" {
			t.Errorf("key %s is %s, published %v, with its private half kept, a day after its "+
				"rotation was due to remove it", k.ID, keyState(k, clock.Now()),
				published(k, clock.Now()))
		}
	}
}

// fedStored returns every key in store.
func fedStored(t *testing.T, store KeyStore) []*Key {
	t.Helper()
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	return keys
}

// TestFederationRotationInterruptedBetweenItsWritesStillRetiresTheOldKey stops a routine rotation
// between its two writes, the new key saved and the old key's retirement not, as a crash, an OOM
// kill, a database failover, or the admin's request being canceled does. The admin asks again, as
// the error invites.
//
// The promise broken is the two-phase schedule: a day later the new key signs and the old key
// retires, and a day after that the old key leaves the published set with its private half erased.
// The retry is refused because the new key is pending, and nothing ever schedules the old key's
// retirement, so two days on it is still in the signing state, still published, and its private
// half is still stored.
func TestFederationRotationInterruptedBetweenItsWritesStillRetiresTheOldKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shared := NewMemKeyStore()
	clock := &testClock{now: lifecycleStart}
	view := &fedHookStore{KeyStore: shared}
	iss := fedReplica(t, view, clock)
	if err := iss.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	oldID := mintKID(t, iss)
	clock.Advance(time.Hour)

	lost := errors.New("the connection to the database was lost")
	failed := false
	view.mu.Lock()
	view.failSave = func(k *Key) error {
		if k.ID == oldID && !failed {
			failed = true
			return lost
		}
		return nil
	}
	view.mu.Unlock()
	if _, err := iss.Rotate(ctx); err == nil {
		t.Logf("Rotate() did not fail at the injected fault, so the schedule was written another way")
	}
	_, retryErr := iss.Rotate(ctx)
	t.Logf("the retried rotation returned %v", retryErr)

	clock.Advance(RotationDelay + RetiredKeyGrace + time.Hour)
	if got := mintKID(t, iss); got == oldID {
		t.Fatalf("two days after the rotation the old key %s still signs", oldID)
	}
	old := storedKey(t, shared, oldID)
	if published(old, clock.Now()) {
		t.Errorf("two days after the rotation the replaced key %s is %s and still published",
			oldID, keyState(old, clock.Now()))
	}
	if old.Sealed != "" {
		t.Errorf("two days after the rotation the replaced key %s still holds its private half",
			oldID)
	}
}

// TestFederationMintNeverSignsWithAKeyAnEmergencyRotationRemoved is a worker that reads the keys,
// then an emergency rotation on the server runs to completion before the worker signs. The worker
// signs with the copy of the old key it read, records the issuance under that key's id, and hands
// the run a token signed by a key the emergency rotation had already removed and erased.
//
// The promise broken is that an emergency rotation's erasure stops the key signing: a signer that
// loaded the key keeps signing after the rotation returned. The run fails at the cloud, and the
// chain records a token issued under the compromised key after the emergency rotation, the very
// entry an incident review would read as misuse of the leaked key.
func TestFederationMintNeverSignsWithAKeyAnEmergencyRotationRemoved(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	shared := NewMemKeyStore()
	clock := &testClock{now: lifecycleStart}
	rec := &recordedIssuances{}
	workerView := &fedHookStore{KeyStore: shared}
	worker := fedReplica(t, workerView, clock, WithIssuanceRecorder(rec))
	server := fedReplica(t, shared, clock)
	if err := server.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	compromisedID := mintKID(t, server)
	clock.Advance(time.Hour)

	var emergency KeyInfo
	var emergencyErr error
	done := fedPauseAfterList(workerView, func() {
		emergency, emergencyErr = server.EmergencyRotate(ctx)
	})
	token, _, err := worker.Mint(ctx, sampleClaims(), DefaultTokenTTL)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	finishedFirst := fedFinished(done)
	fedAwait(t, done)
	if emergencyErr != nil {
		t.Fatalf("EmergencyRotate() error = %v", emergencyErr)
	}
	if !finishedFirst {
		return
	}
	kid := kidOf(t, token)
	if !fedVerifies(t, server, token) {
		t.Errorf("a token minted after the emergency rotation returned is signed by %s, which the "+
			"rotation had removed, rather than by the emergency key %s, so it never verifies",
			kid, emergency.ID)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, is := range rec.got {
		if is.KeyID == compromisedID {
			t.Errorf("the chain records token %s issued under the removed key %s after the "+
				"emergency rotation", is.TokenID, compromisedID)
		}
	}
}

// TestFederationEmergencyRotationRemovesTheKeyFromALaggingReplicasKeySet runs an emergency rotation
// on a server and reads the key set from a replica whose clock trails it by two seconds, the skew
// TestASkewedProcessSignsWithTheEmergencyKey already treats as ordinary.
//
// The promise broken is that every other key leaves the published set at the moment of the
// emergency rotation. The removal is stamped with the rotating process's clock and every replica
// judges it with its own, so the lagging replica keeps serving the compromised key until its
// clock catches up, for as long as the skew lasts, and a cloud that fetches the set from it in that
// window caches the key and accepts a token forged with it. The run store already learned this
// lesson and ages leases on the database clock.
func TestFederationEmergencyRotationRemovesTheKeyFromALaggingReplicasKeySet(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemKeyStore()
	responderClock := &testClock{now: lifecycleStart}
	laggardClock := &testClock{now: lifecycleStart.Add(-2 * time.Second)}
	responder := fedReplica(t, store, responderClock)
	laggard := fedReplica(t, store, laggardClock)
	if err := responder.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	compromisedID := mintKID(t, responder)
	leaked := fedLeak(t, storedKey(t, store, compromisedID))
	responderClock.Advance(time.Hour)
	laggardClock.Advance(time.Hour)
	if _, err := responder.EmergencyRotate(ctx); err != nil {
		t.Fatalf("EmergencyRotate() error = %v", err)
	}

	if slices.Contains(publishedIDs(t, laggard), compromisedID) {
		t.Errorf("the lagging replica still publishes the compromised key %s after the emergency "+
			"rotation", compromisedID)
	}
	if fedVerifies(t, laggard, fedForge(t, leaked, compromisedID, laggardClock.Now())) {
		t.Errorf("a token forged with the compromised key verifies against the key set the lagging " +
			"replica serves after the emergency rotation")
	}
}

// TestFederationRotationIsNotCutShortByOneFastClock is a routine rotation under way when a worker
// whose clock runs two days fast, a host that booted with a wrong clock before time sync, mints a
// token. By its clock the replaced key's removal has come, so it erases that key's private half for
// good. Back on accurately clocked processes the replaced key can no longer sign, and every process
// falls back to the pending key, which then signs nearly a day before its activation.
//
// The promise broken is the notice period: a relying party has a day to fetch the new key before
// any token needs it. One process's clock decides an irreversible erasure for the whole install,
// where the run store decides cross-process timing on the database clock.
func TestFederationRotationIsNotCutShortByOneFastClock(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	serverClock := &testClock{now: lifecycleStart}
	workerClock := &testClock{now: lifecycleStart}
	// The shared store stands for the database both processes use, so it keeps the database's
	// clock, which the accurately clocked server agrees with.
	store := NewMemKeyStoreWithClock(serverClock.Now)
	server := fedReplica(t, store, serverClock)
	worker := fedReplica(t, store, workerClock)
	if err := server.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	oldID := mintKID(t, server)
	serverClock.Advance(time.Hour)
	workerClock.Advance(time.Hour)
	next, err := server.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}

	workerClock.Advance(RotationDelay + RetiredKeyGrace + time.Hour)
	mintKID(t, worker)
	serverClock.Advance(time.Hour)
	if got := mintKID(t, server); got != oldID {
		t.Errorf("an hour into the day's notice the server signs with %s rather than the replaced "+
			"key %s, after a worker with a fast clock erased the replaced key's private half; the "+
			"pending key %s is not due to sign until %v", got, oldID, next.ID, next.ActivatedAt)
	}
}
