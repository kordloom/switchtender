package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
)

// chainEntry returns the newest entry recorded at path in db's chain.
func chainEntry(t *testing.T, db, path string) *audit.Entry {
	t.Helper()
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	chain, err := bundle.Audits().Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		if chain[i].Path == path {
			return chain[i]
		}
	}
	t.Fatalf("no entry at %s in the chain", path)
	return nil
}

// mintToken runs token new against db as the operator would and returns the minted token's id.
func mintToken(t *testing.T, db, name string) string {
	t.Helper()
	setString(t, &tokenDB, db)
	setString(t, &tokenName, name)
	setString(t, &tokenUser, "")
	setBool(t, &tokenAgent, false)
	setDuration(t, &tokenTTL, 0)
	if err := runTokenNew(testCommand(), nil); err != nil {
		t.Fatalf("runTokenNew() error = %v", err)
	}
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	tokens, err := bundle.Tokens().List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	for _, tok := range tokens {
		if tok.Name == name {
			return tok.ID
		}
	}
	t.Fatalf("no token named %q was stored", name)
	return ""
}

// TestCommandLineChangesAreBoundToTheInstall pins that an account or token change made from a shell
// commits to the install that made it and to what it changed. Those entries were written with no
// install id and no content digest, so the changes most worth auditing could be lifted onto another
// install, and the chain said that a token was minted without saying which.
func TestCommandLineChangesAreBoundToTheInstall(t *testing.T) {
	db := tempDB(t)
	minted := mintToken(t, db, "ci")
	id, err := installIdentity(db)
	if err != nil {
		t.Fatalf("installIdentity() error = %v", err)
	}

	entry := chainEntry(t, db, "/cli/token/new")
	if entry.InstallID == "" || entry.InstallID != id.InstallID {
		t.Errorf("token new entry install_id = %q, want this install's %q", entry.InstallID, id.InstallID)
	}
	want, err := json.Marshal(tokenChange{ID: minted, Name: "ci"})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if entry.ContentDigest == "" || !audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce, want) {
		t.Errorf("token new entry does not commit to the token it minted: digest %q", entry.ContentDigest)
	}

	setString(t, &tokenDB, db)
	if err := runTokenRevoke(testCommand(), []string{minted}); err != nil {
		t.Fatalf("runTokenRevoke() error = %v", err)
	}
	revoked := chainEntry(t, db, "/cli/token/revoke")
	body, err := json.Marshal(tokenChange{ID: minted})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if revoked.InstallID != id.InstallID || !audit.VerifyContentDigest(revoked.ContentDigest, revoked.Nonce, body) {
		t.Errorf("token revoke entry is not bound to the install and the token: install %q digest %q",
			revoked.InstallID, revoked.ContentDigest)
	}
}

// TestALostSigningKeyIsNotReplacedSilently pins what happens when an install's signing key goes
// missing. A command that needed it minted a new one, so the chain's earlier entries named one install
// and every bundle signed afterward named another, and none of it could verify. The bundle command
// printed the new key's fingerprint to publish. It now refuses, names the key it needs, and leaves no
// new key behind. A change still records, unbound, because refusing it would make nothing verifiable.
func TestALostSigningKeyIsNotReplacedSilently(t *testing.T) {
	db := tempDB(t)
	mintToken(t, db, "first")
	keyFile := filepath.Join(filepath.Dir(db), audit.IdentityFile)
	if _, err := os.Stat(keyFile); err != nil {
		t.Fatalf("the first change did not leave the install a key: %v", err)
	}
	if err := os.Remove(keyFile); err != nil {
		t.Fatalf("remove the key: %v", err)
	}

	setString(t, &bundleDB, db)
	setString(t, &bundleOut, filepath.Join(t.TempDir(), "bundle.json"))
	setInt(t, &bundleLimit, 0)
	setString(t, &bundleKeyDir, "")
	if err := runAuditBundle(testCommand(), nil); !errors.Is(err, errLostIdentity) {
		t.Errorf("audit bundle without the install's key: error = %v, want errLostIdentity", err)
	}
	if _, err := os.Stat(keyFile); err == nil {
		t.Error("audit bundle minted a new key where the install's key was missing")
	}

	mintToken(t, db, "second")
	if entry := chainEntry(t, db, "/cli/token/new"); entry.InstallID != "" {
		t.Errorf("a change made without the key was bound to install %q", entry.InstallID)
	}
	if _, err := os.Stat(keyFile); err == nil {
		t.Error("token new minted a new key where the install's key was missing")
	}
}

// TestAKeyFromAnotherInstallIsRefused pins that a signing key naming a different install than the
// chain is not used to bind or sign it, which would splice two installs' histories into one chain.
func TestAKeyFromAnotherInstallIsRefused(t *testing.T) {
	db := tempDB(t)
	mintToken(t, db, "first")
	other := t.TempDir()
	if _, err := audit.LoadIdentity(other); err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	foreign, err := os.ReadFile(filepath.Join(other, audit.IdentityFile))
	if err != nil {
		t.Fatalf("read the other install's key: %v", err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(db), audit.IdentityFile), foreign, 0o600); err != nil {
		t.Fatalf("plant the other install's key: %v", err)
	}
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	if _, err := loadProducerIdentity(context.Background(), bundle.Audits(), db); !errors.Is(err, errForeignIdentity) {
		t.Errorf("loadProducerIdentity() with another install's key: error = %v, want errForeignIdentity", err)
	}
}
