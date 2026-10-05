package sqlitestore_test

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/federation"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/sqlutil"
)

// fedUpgradeIssuerURL is the issuer URL the upgraded install serves.
const fedUpgradeIssuerURL = "https://st.example.com"

// fedUpgradeClaims is the run identity the upgraded install mints a token for.
func fedUpgradeClaims() federation.Claims {
	return federation.Claims{
		Audience: "sts.amazonaws.com", RunID: "run_upgrade", OrgID: "org_acme",
		CredentialID: "cred_aws", Tool: "ansible", RunType: federation.RunTypeApply,
	}
}

// TestFederationKeyKeepsSigningAcrossTheLifecycleColumnHeal is an install upgraded from a build
// whose federation_keys table had no activation or removal time, holding the key it signs with,
// one every cloud trusting it has already fetched. The upgraded binary heals the table by adding
// both columns empty and starts.
//
// The promise broken is twofold. The heal adds the column but not its meaning: the empty
// activation reads as a key that never signs, so the first start generates a new key that signs at
// once, with none of the day's notice the rotation schedule promises a relying party, and every run
// fails at a cloud until it refetches the key set, or until an admin uploads it again where the set
// was given to the cloud directly. The old key is then listed as pending forever, never retired,
// never removed, and its private half is never erased.
func TestFederationKeyKeepsSigningAcrossTheLifecycleColumnHeal(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	sealer := credential.NewSealer("fed-upgrade-passphrase", "fed-upgrade-salt")
	created := time.Date(2026, 9, 1, 9, 0, 0, 0, time.UTC)

	earlier := federation.NewMemKeyStore()
	before, err := federation.NewIssuer(fedUpgradeIssuerURL, earlier, sealer,
		federation.WithClock(func() time.Time { return created }))
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	if err := before.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() before the upgrade error = %v", err)
	}
	minted, err := earlier.List(ctx)
	if err != nil || len(minted) != 1 {
		t.Fatalf("List() before the upgrade = %d keys, %v, want one", len(minted), err)
	}
	signing := minted[0]

	path := filepath.Join(t.TempDir(), "switchtender.db")
	db, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("Open() error = %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}
	raw, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open raw: %v", err)
	}
	for _, stmt := range []string{
		"ALTER TABLE federation_keys DROP COLUMN activated_at",
		"ALTER TABLE federation_keys DROP COLUMN removed_at",
	} {
		if _, err := raw.Exec(stmt); err != nil {
			t.Fatalf("simulate the earlier table, %s: %v", stmt, err)
		}
	}
	if _, err := raw.Exec("INSERT INTO federation_keys (id, algorithm, public_key, sealed, "+
		"created_at, retired_at) VALUES (?, ?, ?, ?, ?, '')", signing.ID, signing.Algorithm,
		signing.PublicKey, signing.Sealed, sqlutil.FormatTime(signing.CreatedAt)); err != nil {
		t.Fatalf("store the key the earlier build signed with: %v", err)
	}
	if err := raw.Close(); err != nil {
		t.Fatalf("close raw: %v", err)
	}

	healed, err := sqlitestore.Open(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	t.Cleanup(func() { _ = healed.Close() })
	now := created.Add(30 * 24 * time.Hour)
	after, err := federation.NewIssuer(fedUpgradeIssuerURL, healed.FederationKeys(), sealer,
		federation.WithClock(func() time.Time { return now }))
	if err != nil {
		t.Fatalf("NewIssuer() after the upgrade error = %v", err)
	}
	if err := after.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() after the upgrade error = %v", err)
	}
	token, _, err := after.Mint(ctx, fedUpgradeClaims(), federation.DefaultTokenTTL)
	if err != nil {
		t.Fatalf("Mint() after the upgrade error = %v", err)
	}
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("ParseSigned() error = %v", err)
	}
	if kid := jws.Signatures[0].Header.KeyID; kid != signing.ID {
		t.Errorf("after the upgrade the install signs with %s, a key generated at the first start "+
			"that no relying party has fetched, rather than %s, the key it signed with before",
			kid, signing.ID)
	}
	infos, err := after.Keys(ctx)
	if err != nil {
		t.Fatalf("Keys() error = %v", err)
	}
	if len(infos) != 1 {
		t.Errorf("the upgrade left %d keys, want the one key the install already had", len(infos))
	}
	for _, k := range infos {
		if k.ID == signing.ID && k.State != federation.StateSigning {
			t.Errorf("the key that signed before the upgrade is %q after it, want %q", k.State,
				federation.StateSigning)
		}
	}
}
