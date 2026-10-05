package federation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// lifecycleStart is the instant every lifecycle test's clock starts at.
var lifecycleStart = time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)

// kidOf returns the key id in a token's header.
func kidOf(t *testing.T, token string) string {
	t.Helper()
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("ParseSigned() error = %v", err)
	}
	return jws.Signatures[0].Header.KeyID
}

// mintKID mints a token with iss and returns the id of the key that signed it.
func mintKID(t *testing.T, iss *Issuer) string {
	t.Helper()
	token, _, err := iss.Mint(context.Background(), sampleClaims(), DefaultTokenTTL)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	return kidOf(t, token)
}

// publishedIDs returns the key ids iss publishes, in order.
func publishedIDs(t *testing.T, iss *Issuer) []string {
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

// statesOf returns the state of each key iss lists, by id.
func statesOf(t *testing.T, iss *Issuer) map[string]string {
	t.Helper()
	keys, err := iss.Keys(context.Background())
	if err != nil {
		t.Fatalf("Keys() error = %v", err)
	}
	out := map[string]string{}
	for _, k := range keys {
		out[k.ID] = k.State
	}
	return out
}

// storedKey returns the stored key with id, failing the test when there is none.
func storedKey(t *testing.T, store KeyStore, id string) *Key {
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

// at returns a pointer to t, for comparing optional times.
func at(t time.Time) *time.Time { return &t }

// TestNormalRotationIsTwoPhase walks a normal rotation from end to end on a test clock: the new key
// is published at once and signs a day later, the old key signs until then and stays published a
// day after, and the old key's private half is erased once it is removed while its record stays.
func TestNormalRotationIsTwoPhase(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &testClock{now: lifecycleStart}
	iss, store := newTestIssuer(t, "https://st.example.com", clock)
	if err := iss.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	oldKID := mintKID(t, iss)

	clock.Advance(time.Hour)
	rotatedAt := clock.Now()
	info, err := iss.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	newKID := info.ID
	switchAt := rotatedAt.Add(RotationDelay)
	removeAt := switchAt.Add(RetiredKeyGrace)
	wantInfo := KeyInfo{
		ID: newKID, Algorithm: "RS256", State: StatePending, CreatedAt: rotatedAt,
		ActivatedAt: at(switchAt), Signing: false, Published: true,
	}
	if diff := cmp.Diff(wantInfo, info); diff != "" {
		t.Fatalf("Rotate() mismatch (-want +got):\n%s", diff)
	}
	old := storedKey(t, store, oldKID)
	if diff := cmp.Diff([]*time.Time{at(lifecycleStart), at(switchAt), at(removeAt)},
		[]*time.Time{old.ActivatedAt, old.RetiredAt, old.RemovedAt}); diff != "" {
		t.Errorf("the replaced key's schedule (-want +got):\n%s", diff)
	}

	tests := []struct {
		At            time.Time
		WantSigner    string
		WantPublished []string
		WantStates    map[string]string
	}{{ // Test 0: Just after the rotation the old key signs and both are published.
		At: rotatedAt, WantSigner: oldKID, WantPublished: []string{oldKID, newKID},
		WantStates: map[string]string{oldKID: StateSigning, newKID: StatePending},
	}, { // Test 1: A moment before the switch the old key still signs.
		At: switchAt.Add(-time.Second), WantSigner: oldKID, WantPublished: []string{oldKID, newKID},
		WantStates: map[string]string{oldKID: StateSigning, newKID: StatePending},
	}, { // Test 2: At the switch the new key signs and the old one stays published.
		At: switchAt, WantSigner: newKID, WantPublished: []string{oldKID, newKID},
		WantStates: map[string]string{oldKID: StateRetired, newKID: StateSigning},
	}, { // Test 3: A moment before removal the retired key is still published.
		At: removeAt.Add(-time.Second), WantSigner: newKID, WantPublished: []string{oldKID, newKID},
		WantStates: map[string]string{oldKID: StateRetired, newKID: StateSigning},
	}, { // Test 4: At removal the old key leaves the set and only the new key is published.
		At: removeAt, WantSigner: newKID, WantPublished: []string{newKID},
		WantStates: map[string]string{oldKID: StateRemoved, newKID: StateSigning},
	}}
	for testNum, test := range tests {
		// The checkpoints share one clock and one store, so they run in order.
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			clock.mu.Lock()
			clock.now = test.At
			clock.mu.Unlock()
			if got := mintKID(t, iss); got != test.WantSigner {
				t.Errorf("signed by %s, want %s", got, test.WantSigner)
			}
			if diff := cmp.Diff(test.WantPublished, publishedIDs(t, iss)); diff != "" {
				t.Errorf("published keys (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantStates, statesOf(t, iss)); diff != "" {
				t.Errorf("key states (-want +got):\n%s", diff)
			}
		})
	}

	removedKey := storedKey(t, store, oldKID)
	if removedKey.Sealed != "" {
		t.Error("a removed key still holds its private half")
	}
	if removedKey.PublicKey == "" || removedKey.RemovedAt == nil || removedKey.RetiredAt == nil ||
		removedKey.ActivatedAt == nil {
		t.Errorf("a removed key lost its record: %+v", removedKey)
	}
}

// TestATokenSignedBeforeTheSwitchVerifiesAfterIt pins the point of the grace period: a token the
// old key signed a moment before the new key took over still verifies against the published set
// afterwards, for as long as it is valid.
func TestATokenSignedBeforeTheSwitchVerifiesAfterIt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &testClock{now: lifecycleStart}
	iss, _ := newTestIssuer(t, "https://st.example.com", clock)
	if _, err := iss.Rotate(ctx); err != nil {
		t.Fatalf("first Rotate() error = %v", err)
	}
	if _, err := iss.Rotate(ctx); err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	clock.Advance(RotationDelay - time.Minute)
	token, _, err := iss.Mint(ctx, sampleClaims(), MaxTokenTTL)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	clock.Advance(MaxTokenTTL - time.Second)
	verifyToken(t, iss, token)
}

// TestRotationRefusesWhileOneIsPending pins that a second normal rotation inside the day the first
// gave relying parties is refused with the time the pending key starts signing, and that a rotation
// after the switch goes ahead.
func TestRotationRefusesWhileOneIsPending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &testClock{now: lifecycleStart}
	iss, _ := newTestIssuer(t, "https://st.example.com", clock)
	if err := iss.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	first, err := iss.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	tests := []struct {
		Advance time.Duration
		Want    error
	}{{ // Test 0: Straight after, the first rotation is still under way.
		Advance: time.Minute, Want: ErrRotationPending,
	}, { // Test 1: A moment before the switch it still is.
		Advance: RotationDelay - 2*time.Minute, Want: ErrRotationPending,
	}, { // Test 2: Once the pending key signs, a new rotation is allowed.
		Advance: time.Minute, Want: nil,
	}}
	for testNum, test := range tests {
		// The cases advance one shared clock over one store, so they run in order.
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			clock.Advance(test.Advance)
			_, err := iss.Rotate(ctx)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Rotate() error = %v, want %v", err, test.Want)
			}
			if err != nil && !strings.Contains(err.Error(), first.ID) {
				t.Errorf("the refusal does not name the pending key: %v", err)
			}
		})
	}
}

// TestRotationWithNothingSigningSignsAtOnce pins that a rotation on a store with no key signs with
// its key at once, since there is no relying party to give a day's notice to.
func TestRotationWithNothingSigningSignsAtOnce(t *testing.T) {
	t.Parallel()
	clock := &testClock{now: lifecycleStart}
	iss, _ := newTestIssuer(t, "https://st.example.com", clock)
	info, err := iss.Rotate(context.Background())
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if info.State != StateSigning || !info.Signing || mintKID(t, iss) != info.ID {
		t.Errorf("Rotate() on an empty store = %+v, want a key that signs at once", info)
	}
}

// emergencySetup builds the key history an emergency rotation meets and returns the ids of the keys
// it should remove.
type emergencySetup func(t *testing.T, iss *Issuer, clock *testClock) []string

// TestEmergencyRotationSwitchesAndRemovesAtOnce pins the emergency rotation in every state it can
// meet: the new key signs at once, and every other key leaves the published set at that moment with
// its private half erased, including a key a normal rotation published but never started signing
// with and a retired key still in its grace period.
func TestEmergencyRotationSwitchesAndRemovesAtOnce(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Setup         emergencySetup
		WantNeverUsed bool
	}{{ // Test 0: One key signing.
		Setup: func(t *testing.T, iss *Issuer, _ *testClock) []string {
			return []string{mintKID(t, iss)}
		},
	}, { // Test 1: A normal rotation under way, the signing key and the pending key both go.
		Setup: func(t *testing.T, iss *Issuer, clock *testClock) []string {
			signing := mintKID(t, iss)
			clock.Advance(time.Hour)
			next, err := iss.Rotate(context.Background())
			if err != nil {
				t.Fatalf("Rotate() error = %v", err)
			}
			clock.Advance(time.Hour)
			return []string{signing, next.ID}
		},
		WantNeverUsed: true,
	}, { // Test 2: A retired key inside its grace period goes with the key that replaced it.
		Setup: func(t *testing.T, iss *Issuer, clock *testClock) []string {
			retiredKID := mintKID(t, iss)
			next, err := iss.Rotate(context.Background())
			if err != nil {
				t.Fatalf("Rotate() error = %v", err)
			}
			clock.Advance(RotationDelay + time.Hour)
			return []string{retiredKID, next.ID}
		},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			clock := &testClock{now: lifecycleStart}
			iss, store := newTestIssuer(t, "https://st.example.com", clock)
			gone := test.Setup(t, iss, clock)
			oldToken, _, err := iss.Mint(ctx, sampleClaims(), DefaultTokenTTL)
			if err != nil {
				t.Fatalf("Mint() error = %v", err)
			}
			clock.Advance(time.Minute)
			now := clock.Now()
			info, err := iss.EmergencyRotate(ctx)
			if err != nil {
				t.Fatalf("EmergencyRotate() error = %v", err)
			}
			if info.State != StateSigning || !info.Signing || info.ActivatedAt == nil ||
				!info.ActivatedAt.Equal(now) {
				t.Errorf("EmergencyRotate() = %+v, want a key signing from %s", info, now)
			}
			if got := mintKID(t, iss); got != info.ID {
				t.Errorf("a token minted after the emergency rotation is signed by %s, want %s", got,
					info.ID)
			}
			if diff := cmp.Diff([]string{info.ID}, publishedIDs(t, iss)); diff != "" {
				t.Errorf("published keys after the emergency rotation (-want +got):\n%s", diff)
			}
			set, err := iss.JWKS(ctx)
			if err != nil {
				t.Fatalf("JWKS() error = %v", err)
			}
			if len(set.Key(kidOf(t, oldToken))) != 0 {
				t.Error("a token the removed key signed still finds its key in the published set")
			}
			for _, id := range gone {
				k := storedKey(t, store, id)
				if k.Sealed != "" {
					t.Errorf("removed key %s still holds its private half", id)
				}
				if k.RemovedAt == nil || !k.RemovedAt.Equal(now) {
					t.Errorf("removed key %s RemovedAt = %v, want %v", id, k.RemovedAt, now)
				}
				if k.RetiredAt != nil && k.RetiredAt.After(now) {
					t.Errorf("removed key %s retires at %v, after its removal", id, k.RetiredAt)
				}
			}
			if test.WantNeverUsed {
				if k := storedKey(t, store, gone[1]); k.ActivatedAt != nil || k.RetiredAt != nil {
					t.Errorf("the pending key that never signed records activation %v and "+
						"retirement %v", k.ActivatedAt, k.RetiredAt)
				}
			}
			if _, err := iss.Rotate(ctx); err != nil {
				t.Errorf("a normal rotation after an emergency rotation error = %v", err)
			}
		})
	}
}

// TestASkewedProcessSignsWithTheEmergencyKey pins the clock skew case: a worker whose clock trails
// the server's by two seconds mints straight after the server's emergency rotation. The old key is
// already erased and the new key's activation is still, by the worker's clock, a moment away, and
// the worker signs with the new key rather than generating a key of its own no cloud has fetched.
func TestASkewedProcessSignsWithTheEmergencyKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemKeyStore()
	serverClock := &testClock{now: lifecycleStart}
	workerClock := &testClock{now: lifecycleStart.Add(-2 * time.Second)}
	server, err := NewIssuer("https://st.example.com", store, testSealer, WithClock(serverClock.Now))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	worker, err := NewIssuer("https://st.example.com", store, testSealer, WithClock(workerClock.Now))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	if err := server.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	serverClock.Advance(time.Hour)
	workerClock.Advance(time.Hour)
	info, err := server.EmergencyRotate(ctx)
	if err != nil {
		t.Fatalf("EmergencyRotate() error = %v", err)
	}
	if got := mintKID(t, worker); got != info.ID {
		t.Errorf("the skewed worker signed with %s, want the emergency key %s", got, info.ID)
	}
	keys, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(keys) != 2 {
		t.Errorf("stored keys = %d, want 2: the skewed worker generated a key of its own", len(keys))
	}
}

// TestASecondProcessFollowsTheScheduleFromTheStore pins that a worker on the same database follows
// a rotation the server scheduled, switching at the scheduled time with nothing telling it to.
func TestASecondProcessFollowsTheScheduleFromTheStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := NewMemKeyStore()
	clock := &testClock{now: lifecycleStart}
	server, err := NewIssuer("https://st.example.com", store, testSealer, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	worker, err := NewIssuer("https://st.example.com", store, testSealer, WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	oldKID := mintKID(t, server)
	next, err := server.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	if got := mintKID(t, worker); got != oldKID {
		t.Errorf("the worker signs with %s inside the notice period, want %s", got, oldKID)
	}
	clock.Advance(RotationDelay)
	if got := mintKID(t, worker); got != next.ID {
		t.Errorf("the worker signs with %s after the switch, want %s", got, next.ID)
	}
}

// recordedIssuances is an IssuanceRecorder that keeps what it is given and fails when told to.
type recordedIssuances struct {
	// mu guards got.
	mu sync.Mutex
	// got holds every issuance recorded, in order.
	got []Issuance
	// fail is returned from every record when set.
	fail error
}

// RecordIssuance keeps is, or returns fail.
func (r *recordedIssuances) RecordIssuance(_ context.Context, is Issuance) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.fail != nil {
		return r.fail
	}
	r.got = append(r.got, is)
	return nil
}

// TestMintRecordsTheSigningKey pins that every token the issuer mints is recorded with the id of
// the key that signed it, the token's own id, and its validity, and with nothing of the key or the
// token.
func TestMintRecordsTheSigningKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &testClock{now: lifecycleStart}
	rec := &recordedIssuances{}
	iss, err := NewIssuer("https://st.example.com", NewMemKeyStore(), testSealer,
		WithClock(clock.Now), WithIssuanceRecorder(rec))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	claims := sampleClaims()
	claims.ParentRunID = "run_parent"
	token, expires, err := iss.Mint(ctx, claims, 10*time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	got, kid := verifyToken(t, iss, token)
	want := []Issuance{{
		RunID: "run_abc", ParentRunID: "run_parent", CredentialID: "cred_aws", KeyID: kid,
		TokenID: got.ID, Actor: "operator-one", IssuedAt: lifecycleStart, ExpiresAt: expires,
	}}
	if diff := cmp.Diff(want, rec.got); diff != "" {
		t.Errorf("recorded issuance mismatch (-want +got):\n%s", diff)
	}
	if expires.Unix() != got.ExpiresAt {
		t.Errorf("recorded expiry %v, the token says %d", expires, got.ExpiresAt)
	}
	blob, err := json.Marshal(rec.got)
	if err != nil {
		t.Fatalf("marshal issuance: %v", err)
	}
	if strings.Contains(string(blob), token) || strings.Contains(string(blob), "PRIVATE") {
		t.Error("the recorded issuance carries the token or private key material")
	}
}

// TestMintWithholdsATokenItCannotRecord pins that a token whose issuance cannot be recorded never
// leaves the issuer.
func TestMintWithholdsATokenItCannotRecord(t *testing.T) {
	t.Parallel()
	rec := &recordedIssuances{fail: errors.New("the audit chain is full")}
	iss, err := NewIssuer("https://st.example.com", NewMemKeyStore(), testSealer,
		WithIssuanceRecorder(rec))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	token, _, err := iss.Mint(context.Background(), sampleClaims(), DefaultTokenTTL)
	if !errors.Is(err, ErrEvidence) {
		t.Fatalf("Mint() error = %v, want %v", err, ErrEvidence)
	}
	if token != "" {
		t.Error("a token whose issuance was not recorded was returned anyway")
	}
}

// TestNothingTheIssuerReturnsCarriesPrivateMaterial pins that the key listing, both rotations, the
// key set, and the discovery document carry no private key material: not the sealed private half,
// not the opened PEM, and no private JSON web key parameter.
func TestNothingTheIssuerReturnsCarriesPrivateMaterial(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &testClock{now: lifecycleStart}
	iss, store := newTestIssuer(t, "https://st.example.com", clock)
	var answers []any
	if err := iss.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	rotated, err := iss.Rotate(ctx)
	if err != nil {
		t.Fatalf("Rotate() error = %v", err)
	}
	answers = append(answers, rotated)
	// The material is read while every key still holds it, since the emergency rotation erases it.
	private := privateMaterial(t, store)
	emergency, err := iss.EmergencyRotate(ctx)
	if err != nil {
		t.Fatalf("EmergencyRotate() error = %v", err)
	}
	private = append(private, privateMaterial(t, store)...)
	keys, err := iss.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys() error = %v", err)
	}
	set, err := iss.JWKS(ctx)
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	answers = append(answers, emergency, keys, set, iss.Discovery())
	if len(private) < 3 {
		t.Fatalf("collected %d private values, want at least the three keys' sealed halves",
			len(private))
	}
	for i, a := range answers {
		blob, err := json.Marshal(a)
		if err != nil {
			t.Fatalf("marshal answer %d: %v", i, err)
		}
		text := string(blob)
		for _, p := range private {
			if strings.Contains(text, p) {
				t.Errorf("answer %d carries private key material", i)
			}
		}
		for _, marker := range []string{"PRIVATE KEY", `"d":`, `"p":`, `"q":`, `"dp":`, `"dq":`,
			`"qi":`, "sealed"} {
			if strings.Contains(text, marker) {
				t.Errorf("answer %d carries %s: %s", i, marker, text)
			}
		}
	}
}

// privateMaterial returns, for every stored key that still holds its private half, the sealed value
// and the PEM it opens to.
func privateMaterial(t *testing.T, store KeyStore) []string {
	t.Helper()
	keys, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	var out []string
	for _, k := range keys {
		if k.Sealed == "" {
			continue
		}
		plain, err := testSealer.Open(k.Sealed)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		out = append(out, k.Sealed, plain)
	}
	return out
}

// TestKeyListingCarriesTheFourTimes pins the listing a person reads, oldest first, with each key's
// four times as stored and a removed key still listed.
func TestKeyListingCarriesTheFourTimes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	clock := &testClock{now: lifecycleStart}
	iss, _ := newTestIssuer(t, "https://st.example.com", clock)
	first := mintKID(t, iss)
	clock.Advance(time.Hour)
	second, err := iss.EmergencyRotate(ctx)
	if err != nil {
		t.Fatalf("EmergencyRotate() error = %v", err)
	}
	removedAt := lifecycleStart.Add(time.Hour)
	got, err := iss.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys() error = %v", err)
	}
	want := []KeyInfo{{
		ID: first, Algorithm: "RS256", State: StateRemoved, CreatedAt: lifecycleStart,
		ActivatedAt: at(lifecycleStart), RetiredAt: at(removedAt), RemovedAt: at(removedAt),
	}, {
		ID: second.ID, Algorithm: "RS256", State: StateSigning, CreatedAt: removedAt,
		ActivatedAt: at(removedAt), Signing: true, Published: true,
	}}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("Keys() mismatch (-want +got):\n%s", diff)
	}
}
