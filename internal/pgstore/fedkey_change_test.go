package pgstore

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/federationtest"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// fedChangeSealer seals the keys of the upgraded install's issuer.
var fedChangeSealer = credential.NewSealer("fed-change-passphrase", "fed-change-salt")

// fedChangeReplicas opens two handles, each with its own connection pool, on a database of the
// test's own, standing for two replicas of the documented high availability shape. The tests that
// use it run one at a time, because freshDatabase names each database by the clock and two created
// in the same instant collide.
func fedChangeReplicas(t *testing.T) (federation.KeyStore, federation.KeyStore) {
	t.Helper()
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	var out []federation.KeyStore
	for range 2 {
		db, err := Open(dsn)
		if err != nil {
			t.Fatalf("Open() error = %v", err)
		}
		t.Cleanup(func() { _ = db.Close() })
		out = append(out, db.FederationKeys())
	}
	return out[0], out[1]
}

// TestFederationIssuerContractOnPostgres runs the issuer's cross-process promises on two replicas,
// each with its own connection pool, sharing one PostgreSQL database.
func TestFederationIssuerContractOnPostgres(t *testing.T) {
	federationtest.IssuerContract(t, fedChangeReplicas)
}

// fedChangeIssuer returns an issuer over keys, the way serve and worker build one.
func fedChangeIssuer(t *testing.T, keys federation.KeyStore) *federation.Issuer {
	t.Helper()
	iss, err := federation.NewIssuer(fedRaceIssuerURL, keys, fedChangeSealer)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	return iss
}

// fedChangeKID mints a token with iss and returns the id of the key that signed it.
func fedChangeKID(t *testing.T, iss *federation.Issuer) string {
	t.Helper()
	token, _, err := iss.Mint(context.Background(), federation.Claims{
		Audience: "sts.amazonaws.com", RunID: "run_change", OrgID: "org_acme",
		CredentialID: "cred_aws", Tool: "ansible", RunType: federation.RunTypeApply,
	}, federation.DefaultTokenTTL)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("ParseSigned() error = %v", err)
	}
	return jws.Signatures[0].Header.KeyID
}

// TestFederationKeysKeepTheirMeaningAcrossTheLifecycleColumnHealOnPostgres is an install upgraded
// from a build whose federation_keys table had no activation or removal time, holding the key it
// signs with and a key it retired. The heal gives both the times that build meant: each key
// activated at its creation, and the retired key removed a grace period after it retired. The
// upgraded install keeps signing with the key every cloud already fetched, generates no key of its
// own, and erases the private half of the retired key, whose removal has long come.
func TestFederationKeysKeepTheirMeaningAcrossTheLifecycleColumnHealOnPostgres(t *testing.T) {
	shared := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
	if shared == "" {
		skipOrFail(t, "SWITCHTENDER_TEST_POSTGRES_DSN not set")
	}
	dsn := freshDatabase(t, shared)
	ctx := context.Background()
	retiredKey, signingKey := fedChangeEarlierKey(t), fedChangeEarlierKey(t)
	retiredCreated := time.Date(2026, 8, 1, 9, 0, 0, 0, time.UTC)
	retiredAt := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)
	signingCreated := retiredAt

	db, err := Open(dsn)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw := rawHandle(t, dsn)
	for _, stmt := range []string{
		"ALTER TABLE federation_keys DROP COLUMN activated_at",
		"ALTER TABLE federation_keys DROP COLUMN removed_at",
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("simulate the earlier table, %s: %v", stmt, err)
		}
	}
	for _, row := range []struct {
		// Key is the key the earlier build stored.
		Key *federation.Key
		// Created and Retired are its times, Retired empty for the key that signs.
		Created, Retired string
	}{
		{retiredKey, sqlutil.FormatTime(retiredCreated), sqlutil.FormatTime(retiredAt)},
		{signingKey, sqlutil.FormatTime(signingCreated), ""},
	} {
		if _, err := raw.Exec("INSERT INTO federation_keys (id, algorithm, public_key, sealed, "+
			"created_at, retired_at) VALUES ($1, $2, $3, $4, $5, $6)", row.Key.ID, row.Key.Algorithm,
			row.Key.PublicKey, row.Key.Sealed, row.Created, row.Retired); err != nil {
			t.Fatalf("store a key the earlier build wrote: %v", err)
		}
	}

	healed, err := Open(dsn)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = healed.Close() })
	keys, err := healed.FederationKeys().List(ctx)
	if err != nil {
		t.Fatalf("List() after the heal error = %v", err)
	}
	removal := retiredAt.Add(federation.RetiredKeyGrace)
	want := []*federation.Key{{
		ID: retiredKey.ID, Algorithm: "RS256", PublicKey: retiredKey.PublicKey,
		Sealed: retiredKey.Sealed, CreatedAt: retiredCreated, ActivatedAt: &retiredCreated,
		RetiredAt: &retiredAt, RemovedAt: &removal,
	}, {
		ID: signingKey.ID, Algorithm: "RS256", PublicKey: signingKey.PublicKey,
		Sealed: signingKey.Sealed, CreatedAt: signingCreated, ActivatedAt: &signingCreated,
	}}
	if diff := cmp.Diff(want, keys); diff != "" {
		t.Fatalf("the earlier keys after the heal (-want +got):\n%s", diff)
	}

	iss := fedChangeIssuer(t, healed.FederationKeys())
	if err := iss.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() after the upgrade error = %v", err)
	}
	if kid := fedChangeKID(t, iss); kid != signingKey.ID {
		t.Errorf("after the upgrade the install signs with %s, want %s, the key it signed with "+
			"before", kid, signingKey.ID)
	}
	infos, err := iss.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys() error = %v", err)
	}
	states := map[string]string{}
	for _, k := range infos {
		states[k.ID] = k.State
	}
	wantStates := map[string]string{retiredKey.ID: federation.StateRemoved,
		signingKey.ID: federation.StateSigning}
	if diff := cmp.Diff(wantStates, states); diff != "" {
		t.Errorf("key states after the upgrade (-want +got):\n%s", diff)
	}
	if fedRaceKey(t, healed.FederationKeys(), retiredKey.ID).Sealed != "" {
		t.Error("the retired key's private half outlived its removal after the upgrade")
	}
}

// fedChangeEarlierKey returns a key generated by an issuer of its own, with its sealed private
// half, standing for a key an earlier build stored.
func fedChangeEarlierKey(t *testing.T) *federation.Key {
	t.Helper()
	store := federation.NewMemKeyStore()
	iss := fedChangeIssuer(t, store)
	if err := iss.Ensure(context.Background()); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	keys, err := store.List(context.Background())
	if err != nil || len(keys) != 1 {
		t.Fatalf("List() = %d keys, %v, want one", len(keys), err)
	}
	return keys[0]
}
