package pgstore

import (
	"context"
	"crypto/rsa"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
)

// fedRaceIssuerURL is the issuer URL both replicas in the race serve.
const fedRaceIssuerURL = "https://st.example.com"

// fedRaceWait bounds how long the paused replica waits for the one racing it. A fix that
// serializes rotations across replicas blocks the racing one until the paused one is done, and the
// wait then times out rather than deadlocking.
const fedRaceWait = 30 * time.Second

// fedRaceClock is a settable clock both replicas read, so the race is about the store and not about
// skew.
type fedRaceClock struct {
	// mu guards now.
	mu sync.Mutex
	// now is the current reading.
	now time.Time
}

// Now returns the clock's reading.
func (c *fedRaceClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *fedRaceClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// fedRaceView is one replica's handle on the shared key table, which pauses once after a List has
// read the table so the other replica can act at exactly that point.
type fedRaceView struct {
	// KeyStore is the replica's own handle on the shared database.
	federation.KeyStore
	// mu guards afterList.
	mu sync.Mutex
	// afterList runs once, after the next List has read the table.
	afterList func()
}

// List reads the table, then runs the pending hook before returning what it read.
func (v *fedRaceView) List(ctx context.Context) ([]*federation.Key, error) {
	keys, err := v.KeyStore.List(ctx)
	v.mu.Lock()
	hook := v.afterList
	v.afterList = nil
	v.mu.Unlock()
	if hook != nil {
		hook()
	}
	return keys, err
}

// fedRacePause runs other, as the other replica, while the next List on view is paused, waiting up
// to fedRaceWait for it. The returned channel closes once other has finished.
func fedRacePause(view *fedRaceView, other func()) <-chan struct{} {
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

// fedRaceKey returns the stored key with id as the given replica reads it.
func fedRaceKey(t *testing.T, store federation.KeyStore, id string) *federation.Key {
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

// fedRaceLeak opens k's private half with sealer, standing for the key that leaked.
func fedRaceLeak(t *testing.T, sealer *credential.Sealer, k *federation.Key) *rsa.PrivateKey {
	t.Helper()
	plain, err := sealer.Open(k.Sealed)
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

// fedRaceForgeVerifies signs a token with priv under kid, as whoever holds a leaked key would, and
// reports whether it verifies against the key set iss publishes.
func fedRaceForgeVerifies(t *testing.T, iss *federation.Issuer, priv *rsa.PrivateKey, kid string,
	now time.Time) bool {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"iss": fedRaceIssuerURL, "aud": "sts.amazonaws.com", "jti": "forged",
		"sub": "org:org_acme:project:proj_web:template:tpl_deploy:env:prod:run_type:apply:" +
			"approved:true",
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
	set, err := iss.JWKS(context.Background())
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	for _, k := range set.Key(kid) {
		if _, err := jws.Verify(k.Key); err == nil {
			return true
		}
	}
	return false
}

// TestFederationEmergencyRotationHoldsAgainstARacingReplicaOnPostgres is two serve replicas on one
// PostgreSQL database, each with its own connection pool, the documented high availability shape.
// An admin on one asks for a routine rotation while an admin on the other answers a suspected
// compromise with an emergency rotation. The routine rotation reads the key table, the emergency
// rotation commits, and the routine rotation then upserts the row it read earlier for the key it is
// retiring, sealed private half and all.
//
// The promise broken is the emergency rotation's: every other key leaves the published set at that
// moment with its private half erased. The store's blind upsert puts the compromised key's private
// half back in the table, the key is published again for two days, and a token forged with the
// leaked key verifies against the key set the install serves.
func TestFederationEmergencyRotationHoldsAgainstARacingReplicaOnPostgres(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	ctx := context.Background()
	one, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() replica one error = %v", err)
	}
	t.Cleanup(func() { _ = one.Close() })
	two, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() replica two error = %v", err)
	}
	t.Cleanup(func() { _ = two.Close() })

	sealer := credential.NewSealer("fed-race-passphrase", "fed-race-salt")
	clock := &fedRaceClock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	routineView := &fedRaceView{KeyStore: one.FederationKeys()}
	routine, err := federation.NewIssuer(fedRaceIssuerURL, routineView, sealer,
		federation.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewIssuer() replica one error = %v", err)
	}
	responder, err := federation.NewIssuer(fedRaceIssuerURL, two.FederationKeys(), sealer,
		federation.WithClock(clock.Now))
	if err != nil {
		t.Fatalf("NewIssuer() replica two error = %v", err)
	}
	if err := responder.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	keys, err := two.FederationKeys().List(ctx)
	if err != nil || len(keys) != 1 {
		t.Fatalf("List() after Ensure() = %d keys, %v, want the one first key", len(keys), err)
	}
	compromised := keys[0]
	leaked := fedRaceLeak(t, sealer, compromised)
	clock.Advance(time.Hour)

	var emergencyErr error
	done := fedRacePause(routineView, func() { _, emergencyErr = responder.EmergencyRotate(ctx) })
	_, routineErr := routine.Rotate(ctx)
	select {
	case <-done:
	case <-time.After(6 * fedRaceWait):
		t.Fatal("the emergency rotation never finished")
	}
	if emergencyErr != nil {
		t.Fatalf("EmergencyRotate() error = %v", emergencyErr)
	}
	t.Logf("the racing routine rotation returned %v", routineErr)

	after := fedRaceKey(t, two.FederationKeys(), compromised.ID)
	if after.Sealed != "" {
		t.Errorf("the compromised key's sealed private half is back in federation_keys after the " +
			"emergency rotation erased it")
	}
	set, err := responder.JWKS(ctx)
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	if len(set.Key(compromised.ID)) > 0 {
		t.Errorf("the compromised key %s is published again after the emergency rotation, now "+
			"until %v", compromised.ID, after.RemovedAt)
	}
	clock.Advance(time.Minute)
	if fedRaceForgeVerifies(t, responder, leaked, compromised.ID, clock.Now()) {
		t.Errorf("a token forged with the compromised key verifies against the key set the " +
			"install publishes after the emergency rotation")
	}
}
