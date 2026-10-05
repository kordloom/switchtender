package federationtest

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
)

// raceWait bounds how long a scenario waits for a simulated process to reach a point it must
// reach. It is generous because generating an RSA key under the race detector takes seconds.
const raceWait = 30 * time.Second

// heldWindow is how long a process holding the key lock keeps it once the racing process has asked
// for it. A racing process the lock does not hold back finishes well inside it, and it is short of
// the five seconds a SQLite writer waits for the lock before giving up.
const heldWindow = time.Second

// issuerURL is the issuer URL every simulated process serves.
const issuerURL = "https://st.example.com"

// issuerSealer seals the keys of every simulated process.
var issuerSealer = credential.NewSealer("federation-contract-passphrase",
	"federation-contract-salt")

// Replicas returns two handles on one fresh store, each standing for a process sharing it.
type Replicas func(t *testing.T) (federation.KeyStore, federation.KeyStore)

// IssuerContract runs the promises the federation issuer keeps across processes against two
// handles on one fresh store from replicas, for every store that keeps a clock of its own: a
// rotation is one decision every process shares, a removed and erased key never signs, is
// published, or comes back, and no single process's clock decides for the others.
func IssuerContract(t *testing.T, replicas Replicas) {
	t.Helper()
	t.Run("a rotation holding the lock makes a racing routine rotation wait and refuses it",
		func(t *testing.T) { testLockHolds(t, replicas, false) })
	t.Run("a rotation holding the lock makes a racing emergency rotation wait and remove its key",
		func(t *testing.T) { testLockHolds(t, replicas, true) })
	t.Run("a routine rotation that read before an emergency one keeps the compromised key out",
		func(t *testing.T) { testStaleRotation(t, replicas) })
	t.Run("a mint that read before an emergency rotation signs with the new key",
		func(t *testing.T) { testStaleMint(t, replicas) })
	t.Run("an interrupted rotation leaves the keys as they were",
		func(t *testing.T) { testInterruptedRotation(t, replicas) })
	t.Run("a fast clock on one process cannot cut a rotation short",
		func(t *testing.T) { testFastClock(t, replicas) })
	t.Run("a lagging replica publishes no removed key",
		func(t *testing.T) { testLaggingReplica(t, replicas) })
	t.Run("two processes starting on a new install at once publish one first key",
		func(t *testing.T) { testFirstKeyRace(t, replicas) })
}

// newIssuer returns an issuer over keys, the way serve and worker build one.
func newIssuer(t *testing.T, keys federation.KeyStore, opts ...federation.Option) *federation.Issuer {
	t.Helper()
	iss, err := federation.NewIssuer(issuerURL, keys, issuerSealer, opts...)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	return iss
}

// mint mints a token with iss and returns it with the claims it carries and the id of the key that
// signed it.
func mint(t *testing.T, iss *federation.Issuer) (string, federation.Claims, string) {
	t.Helper()
	token, _, err := iss.Mint(context.Background(), federation.Claims{
		Audience: "sts.amazonaws.com", RunID: "run_contract", OrgID: "org_acme",
		CredentialID: "cred_aws", Tool: "ansible", RunType: federation.RunTypeApply,
	}, federation.DefaultTokenTTL)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("ParseSigned() error = %v", err)
	}
	var claims federation.Claims
	if err := json.Unmarshal(jws.UnsafePayloadWithoutVerification(), &claims); err != nil {
		t.Fatalf("decode claims: %v", err)
	}
	return token, claims, jws.Signatures[0].Header.KeyID
}

// published returns the ids of the keys iss publishes, in order.
func published(t *testing.T, iss *federation.Issuer) []string {
	t.Helper()
	set, err := iss.JWKS(context.Background())
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	ids := []string{}
	for _, k := range set.Keys {
		ids = append(ids, k.KeyID)
	}
	return ids
}

// verifies reports whether token verifies against the key set iss publishes, which is what a cloud
// checks once it has fetched that set.
func verifies(t *testing.T, iss *federation.Issuer, token string) bool {
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

// leak opens k's private half, standing for the compromise an emergency rotation answers.
func leak(t *testing.T, k *federation.Key) *rsa.PrivateKey {
	t.Helper()
	plain, err := issuerSealer.Open(k.Sealed)
	if err != nil {
		t.Fatalf("open the key that leaks: %v", err)
	}
	block, _ := pem.Decode([]byte(plain))
	if block == nil {
		t.Fatal("the leaked key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		t.Fatalf("parse the leaked key: %v", err)
	}
	priv, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		t.Fatal("the leaked key is not RSA")
	}
	return priv
}

// forge signs a token with priv under kid, as whoever holds a leaked key would.
func forge(t *testing.T, priv *rsa.PrivateKey, kid string) string {
	t.Helper()
	now := time.Now()
	payload, err := json.Marshal(map[string]any{
		"iss": issuerURL, "aud": "sts.amazonaws.com", "jti": "forged",
		"sub": "org:org_acme:project:none:template:none:env:none:run_type:apply:approved:true",
		"iat": now.Unix(), "nbf": now.Unix(), "exp": now.Add(15 * time.Minute).Unix(),
	})
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

// await waits for done, failing the test rather than hanging when it never closes.
func await(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(raceWait):
		t.Fatalf("%s never finished", what)
	}
}

// insideKey marks a context as inside a change asked for through an insideView.
type insideKey struct{}

// insideView is one process's handle on a shared store that runs a hook once, from inside a
// change, right after the read the change decides on, while the process holds the key lock.
type insideView struct {
	// KeyStore is the process's own handle on the shared store.
	federation.KeyStore
	// mu guards hook.
	mu sync.Mutex
	// hook runs once, after the next List made inside a change.
	hook func()
}

// Change runs the change with its context marked, so the reads inside it can be told apart.
func (v *insideView) Change(ctx context.Context, fn func(ctx context.Context) error) error {
	return v.KeyStore.Change(ctx, func(ctx context.Context) error {
		return fn(context.WithValue(ctx, insideKey{}, true))
	})
}

// List reads the store and, inside a change, runs the pending hook before returning what it read.
func (v *insideView) List(ctx context.Context) ([]*federation.Key, error) {
	keys, err := v.KeyStore.List(ctx)
	if ctx.Value(insideKey{}) == nil {
		return keys, err
	}
	v.mu.Lock()
	hook := v.hook
	v.hook = nil
	v.mu.Unlock()
	if hook != nil {
		hook()
	}
	return keys, err
}

// enteredView is one process's handle on a shared store that reports when the process first asks
// for a change, which is when it starts waiting for the key lock.
type enteredView struct {
	// KeyStore is the process's own handle on the shared store.
	federation.KeyStore
	// once closes entered on the first change.
	once sync.Once
	// entered closes when the first change is asked for.
	entered chan struct{}
}

// Change reports that a change was asked for, then runs it.
func (v *enteredView) Change(ctx context.Context, fn func(ctx context.Context) error) error {
	v.once.Do(func() { close(v.entered) })
	return v.KeyStore.Change(ctx, fn)
}

// pauseView is one process's handle on a shared store that runs another process once, after the
// next List has read the store and before it returns what it read. It waits up to raceWait for the
// other process, so a store whose lock held the other back would fail the scenario, not hang it.
type pauseView struct {
	// KeyStore is the process's own handle on the shared store.
	federation.KeyStore
	// mu guards other.
	mu sync.Mutex
	// other runs once, after the next List.
	other func()
	// done closes once other has finished.
	done chan struct{}
}

// pauseFor returns a view on keys that runs other after its next List.
func pauseFor(keys federation.KeyStore, other func()) *pauseView {
	return &pauseView{KeyStore: keys, other: other, done: make(chan struct{})}
}

// List reads the store, then runs the pending process before returning what it read.
func (v *pauseView) List(ctx context.Context) ([]*federation.Key, error) {
	keys, err := v.KeyStore.List(ctx)
	v.mu.Lock()
	other := v.other
	v.other = nil
	v.mu.Unlock()
	if other != nil {
		go func() {
			defer close(v.done)
			other()
		}()
		select {
		case <-v.done:
		case <-time.After(raceWait):
		}
	}
	return keys, err
}

// failingView is one process's handle on a shared store whose first save of one key fails,
// standing for a crash or a dropped connection between a rotation's writes.
type failingView struct {
	// KeyStore is the process's own handle on the shared store.
	federation.KeyStore
	// mu guards id and failed.
	mu sync.Mutex
	// id is the key whose first save fails.
	id string
	// failed reports that the save has failed.
	failed bool
}

// Save fails the first save of the chosen key and writes through otherwise.
func (v *failingView) Save(ctx context.Context, k *federation.Key) error {
	v.mu.Lock()
	fail := k.ID == v.id && !v.failed
	if fail {
		v.failed = true
	}
	v.mu.Unlock()
	if fail {
		return errors.New("the connection to the database was lost")
	}
	return v.KeyStore.Save(ctx, k)
}

// recorded keeps every issuance it is asked to record.
type recorded struct {
	// mu guards got.
	mu sync.Mutex
	// got holds every recorded issuance, in order.
	got []federation.Issuance
}

// RecordIssuance keeps is.
func (r *recorded) RecordIssuance(_ context.Context, is federation.Issuance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.got = append(r.got, is)
	return nil
}

// stored returns the key with id as keys holds it.
func stored(t *testing.T, keys federation.KeyStore, id string) *federation.Key {
	t.Helper()
	all, err := keys.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, k := range all {
		if k.ID == id {
			return k
		}
	}
	t.Fatalf("key %s is not stored", id)
	return nil
}

// testLockHolds is a rotation on one process that has read the keys under the key lock when a
// rotation on the other asks for the lock. The second waits until the first has written and let
// go, and then decides on what the first wrote: a second routine rotation is refused as one
// already under way, and an emergency rotation removes the key the first one published.
func testLockHolds(t *testing.T, replicas Replicas, emergency bool) {
	ctx := context.Background()
	one, two := replicas(t)
	firstView := &insideView{KeyStore: one}
	secondView := &enteredView{KeyStore: two, entered: make(chan struct{})}
	first, second := newIssuer(t, firstView), newIssuer(t, secondView)
	if err := first.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	_, _, old := mint(t, first)

	var secondInfo federation.KeyInfo
	var secondErr error
	done := make(chan struct{})
	finishedWhileHeld := false
	firstView.mu.Lock()
	firstView.hook = func() {
		go func() {
			defer close(done)
			if emergency {
				secondInfo, secondErr = second.EmergencyRotate(ctx)
			} else {
				secondInfo, secondErr = second.Rotate(ctx)
			}
		}()
		select {
		case <-secondView.entered:
		case <-done:
		case <-time.After(raceWait):
			t.Error("the racing rotation never asked for the key lock")
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
	await(t, done, "the racing rotation")
	if finishedWhileHeld {
		t.Error("the racing rotation finished while the first one held the key lock")
	}
	want := []string{old, firstInfo.ID}
	wantErr := federation.ErrRotationPending
	if emergency {
		want, wantErr = []string{secondInfo.ID}, nil
	}
	if !errors.Is(secondErr, wantErr) {
		t.Fatalf("the racing rotation error = %v, want %v", secondErr, wantErr)
	}
	if diff := cmp.Diff(want, published(t, first)); diff != "" {
		t.Errorf("published keys after both rotations (-want +got):\n%s", diff)
	}
	all, err := two.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, k := range all {
		if !slices.Contains(want, k.ID) && k.Sealed != "" {
			t.Errorf("key %s left the published set and kept its private half", k.ID)
		}
	}
}

// testStaleRotation is a routine rotation that reads the keys, then an emergency rotation on the
// other process answers a suspected compromise and finishes, and then the routine rotation goes
// on. It must decide on the keys as they stand under the lock, so the compromised key keeps its
// private half erased, stays out of the published set, and a token forged with it never verifies.
func testStaleRotation(t *testing.T, replicas Replicas) {
	ctx := context.Background()
	one, two := replicas(t)
	responder := newIssuer(t, two)
	if err := responder.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	_, _, compromised := mint(t, responder)
	leaked := leak(t, stored(t, two, compromised))

	var emergency federation.KeyInfo
	var emergencyErr error
	routineView := pauseFor(one, func() { emergency, emergencyErr = responder.EmergencyRotate(ctx) })
	_, routineErr := newIssuer(t, routineView).Rotate(ctx)
	await(t, routineView.done, "the emergency rotation")
	if emergencyErr != nil {
		t.Fatalf("EmergencyRotate() error = %v", emergencyErr)
	}
	t.Logf("the racing routine rotation returned %v", routineErr)
	if stored(t, two, compromised).Sealed != "" {
		t.Error("the compromised key's private half is back after the emergency rotation erased it")
	}
	if slices.Contains(published(t, responder), compromised) {
		t.Errorf("the compromised key %s is published again after the emergency rotation",
			compromised)
	}
	if verifies(t, responder, forge(t, leaked, compromised)) {
		t.Error("a token forged with the compromised key verifies after the emergency rotation")
	}
	if _, _, kid := mint(t, responder); kid != emergency.ID {
		t.Errorf("the install signs with %s after the emergency rotation, want %s", kid,
			emergency.ID)
	}
}

// testStaleMint is a mint that reads the keys, then an emergency rotation on the other process
// commits before it signs. Its token is signed by the emergency key and verifies against the key
// set the install publishes, and nothing is on record under the removed key.
func testStaleMint(t *testing.T, replicas Replicas) {
	ctx := context.Background()
	one, two := replicas(t)
	server := newIssuer(t, one)
	if err := server.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	_, _, compromised := mint(t, server)
	rec := &recorded{}
	var emergency federation.KeyInfo
	var emergencyErr error
	workerView := pauseFor(two, func() { emergency, emergencyErr = server.EmergencyRotate(ctx) })
	worker := newIssuer(t, workerView, federation.WithIssuanceRecorder(rec))
	token, _, kid := mint(t, worker)
	await(t, workerView.done, "the emergency rotation")
	if emergencyErr != nil {
		t.Fatalf("EmergencyRotate() error = %v", emergencyErr)
	}
	if kid != emergency.ID {
		t.Errorf("the worker signed with %s after the emergency rotation removed %s, want %s", kid,
			compromised, emergency.ID)
	}
	if !verifies(t, server, token) {
		t.Errorf("the worker's token, signed by %s, does not verify against the published set", kid)
	}
	rec.mu.Lock()
	defer rec.mu.Unlock()
	for _, is := range rec.got {
		if is.KeyID == compromised {
			t.Errorf("token %s is on record under the removed key %s", is.TokenID, compromised)
		}
	}
}

// testInterruptedRotation stops a routine rotation between its two writes. The new key and the old
// key's retirement land together or not at all, so the keys are as they were, the retry the error
// invites goes ahead rather than answering 409, and the old key retires when the new one signs.
func testInterruptedRotation(t *testing.T, replicas Replicas) {
	ctx := context.Background()
	one, _ := replicas(t)
	if err := newIssuer(t, one).Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	before, err := one.List(ctx)
	if err != nil || len(before) != 1 {
		t.Fatalf("List() after Ensure() = %d keys, %v, want one", len(before), err)
	}
	interrupted := newIssuer(t, &failingView{KeyStore: one, id: before[0].ID})
	if _, err := interrupted.Rotate(ctx); err == nil {
		t.Fatal("Rotate() succeeded through a save that failed")
	}
	after, err := one.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if diff := cmp.Diff(before, after); diff != "" {
		t.Errorf("the keys after the interrupted rotation (-want +got):\n%s", diff)
	}
	next, err := interrupted.Rotate(ctx)
	if err != nil {
		t.Fatalf("the retried Rotate() error = %v", err)
	}
	old := stored(t, one, before[0].ID)
	if old.RetiredAt == nil || next.ActivatedAt == nil || !old.RetiredAt.Equal(*next.ActivatedAt) {
		t.Errorf("the replaced key retires at %v, want %v, when the new key starts signing",
			old.RetiredAt, next.ActivatedAt)
	}
}

// testFastClock is a routine rotation under way when a worker whose clock runs two days fast
// mints. Every process judges the keys by the store's clock, so the worker neither erases the
// replaced key's private half, which would leave every accurately clocked process signing with the
// new key a day before its notice ran out, nor signs with the new key itself, and its token is
// stamped by the store's clock as well.
func testFastClock(t *testing.T, replicas Replicas) {
	ctx := context.Background()
	one, two := replicas(t)
	server := newIssuer(t, one)
	worker := newIssuer(t, two, federation.WithClock(func() time.Time {
		return time.Now().Add(2*federation.RotationDelay + 2*time.Hour)
	}))
	if err := server.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	_, _, old := mint(t, server)
	if _, err := server.Rotate(ctx); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	_, claims, kid := mint(t, worker)
	if kid != old {
		t.Errorf("the fast worker signs with %s, want %s, the key still signing", kid, old)
	}
	if skew := time.Since(time.Unix(claims.IssuedAt, 0)); skew > time.Hour || skew < -time.Hour {
		t.Errorf("the fast worker's token was issued %v away from the store's clock", skew)
	}
	if _, _, kid := mint(t, server); kid != old {
		t.Errorf("after the fast worker minted the server signs with %s, want %s", kid, old)
	}
	if stored(t, one, old).Sealed == "" {
		t.Errorf("the fast worker erased the private half of %s, the key still signing", old)
	}
}

// testLaggingReplica is an emergency rotation on one process and the key set served by another
// whose clock trails by an hour. Every process judges the keys by the store's clock, so the lagging
// one drops the compromised key at the moment of the rotation and signs with the emergency key.
func testLaggingReplica(t *testing.T, replicas Replicas) {
	ctx := context.Background()
	one, two := replicas(t)
	responder := newIssuer(t, one)
	laggard := newIssuer(t, two, federation.WithClock(func() time.Time {
		return time.Now().Add(-time.Hour)
	}))
	if err := responder.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	_, _, compromised := mint(t, responder)
	emergency, err := responder.EmergencyRotate(ctx)
	if err != nil {
		t.Fatalf("EmergencyRotate() error = %v", err)
	}
	if diff := cmp.Diff([]string{emergency.ID}, published(t, laggard)); diff != "" {
		t.Errorf("the lagging replica's key set after the emergency rotation (-want +got):\n%s",
			diff)
	}
	if _, _, kid := mint(t, laggard); kid != emergency.ID {
		t.Errorf("the lagging replica signs with %s after the emergency rotation removed %s, "+
			"want %s", kid, compromised, emergency.ID)
	}
}

// testFirstKeyRace is two processes starting on a new install at once, each finding no key. The
// first reads the empty store, the second generates and saves a first key, and the first then
// decides again under the lock, finds that key, and signs with it, so the install publishes one
// first key rather than two that a cloud must both fetch.
func testFirstKeyRace(t *testing.T, replicas Replicas) {
	ctx := context.Background()
	one, two := replicas(t)
	second := newIssuer(t, two)
	var secondErr error
	firstView := pauseFor(one, func() { secondErr = second.Ensure(ctx) })
	first := newIssuer(t, firstView)
	if err := first.Ensure(ctx); err != nil {
		t.Fatalf("the first Ensure() error = %v", err)
	}
	await(t, firstView.done, "the second process's start")
	if secondErr != nil {
		t.Fatalf("the second Ensure() error = %v", secondErr)
	}
	all, err := one.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("two processes starting at once stored %d keys, want one", len(all))
	}
	for _, iss := range []*federation.Issuer{first, second} {
		if _, _, kid := mint(t, iss); kid != all[0].ID {
			t.Errorf("a process signs with %s, want the one first key %s", kid, all[0].ID)
		}
	}
}
