package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
)

// TestDemoPathsKeepIdentityBesideTheResolvedDatabase covers where the demo's producer identity comes
// from when --db is empty.
//
// The identity was loaded from the directory of the raw flag rather than the resolved database, and
// the directory of an empty path is the working directory. The default database path is a bare
// filename, so a default install keeps its production key in the directory it serves from: running
// the demo there published the production install identity, public key, and fingerprint on the
// demo's unauthenticated trust document, which is exposed to the internet by design. Run anywhere
// else it created a fresh private key in an unrelated directory.
func TestDemoPathsKeepIdentityBesideTheResolvedDatabase(t *testing.T) {
	original := demoDB
	t.Cleanup(func() { demoDB = original })

	// The working directory stands in for a production install's serve directory, holding the key
	// the demo must not reach for.
	work := t.TempDir()
	t.Chdir(work)
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	production, err := audit.LoadIdentity(".")
	if err != nil {
		t.Fatalf("LoadIdentity() for the production key error = %v", err)
	}

	// Test 0: An explicit --db keeps the identity beside that database.
	demoDB = filepath.Join(t.TempDir(), "demo.db")
	db, keyDir, err := demoPaths()
	if err != nil {
		t.Fatalf("demoPaths() error = %v", err)
	}
	if db != demoDB || keyDir != filepath.Dir(demoDB) {
		t.Errorf("demoPaths() = %q, %q, want %q beside %q", db, keyDir, demoDB, filepath.Dir(demoDB))
	}

	// Test 1: An empty --db resolves to a temporary database, and the identity follows it rather
	// than the working directory.
	demoDB = ""
	db, keyDir, err = demoPaths()
	if err != nil {
		t.Fatalf("demoPaths() with an empty --db error = %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(db) })
	if db == "" {
		t.Fatal("demoPaths() with an empty --db returned no database")
	}
	if keyDir != filepath.Dir(db) {
		t.Errorf("demoPaths() key directory = %q, want %q beside the resolved database %q",
			keyDir, filepath.Dir(db), db)
	}

	// Test 2: The identity the demo would publish is its own, not the one in the serve directory.
	demoID, err := audit.LoadIdentity(keyDir)
	if err != nil {
		t.Fatalf("LoadIdentity(%q) error = %v", keyDir, err)
	}
	if demoID.InstallID == production.InstallID || demoID.KeyID() == production.KeyID() {
		t.Errorf("the demo published install %q key %q, which is the production identity in the "+
			"working directory. That key and fingerprint reach the unauthenticated trust document",
			demoID.InstallID, demoID.KeyID())
	}
}
