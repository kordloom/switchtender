package cmd

import (
	"errors"
	"fmt"
	"io/fs"
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

// TestDemoTakesTheProxyFlagsServeTakes covers the one build that is actually exposed to the public
// internet behind a reverse proxy, and had no way to be told so.
//
// Every per-client budget resolves the caller through clientAddr, which believes a forwarding header
// only from a proxy the operator named. The demo never had the flag, so trustedProxies was always
// empty and every visitor resolved to the proxy's own address: the 32-stream-per-caller cap became a
// cap of 32 across all visitors at once. The run at the center of the approvals story is held, so it
// is non-terminal, and each visitor who opens it and leaves the tab open holds a slot until they
// close it.
func TestDemoTakesTheProxyFlagsServeTakes(t *testing.T) {
	for _, name := range []string{"trusted-proxy", "client-ip-header"} {
		if demoCmd.Flags().Lookup(name) == nil {
			t.Errorf("demo has no --%s, so behind a proxy every visitor shares one per-client "+
				"budget and the cap stops describing a caller", name)
		}
		if serveCmd.Flags().Lookup(name) == nil {
			t.Errorf("serve has no --%s, which this test assumed as the reference", name)
		}
	}
}

// TestATemporaryDemoLeavesNothingBehind covers what a demo run without --db leaves in the temp
// directory when it stops.
//
// Every run left its database, the write-ahead log and shared-memory files beside it, and a tree of
// playbooks and Terraform, so a machine that ran the demo often held hundreds of them. The producer
// identity lives in the same directory and has to survive, since it is what the next run signs with.
func TestATemporaryDemoLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	db := filepath.Join(dir, "switchtender-demo-1.db")
	assets := filepath.Join(dir, "switchtender-demo-assets-1")
	identity := filepath.Join(dir, "switchtender-audit.key")
	for _, name := range []string{db, db + "-wal", db + "-shm", db + "-journal", identity} {
		if err := os.WriteFile(name, []byte("x"), 0o600); err != nil {
			t.Fatalf("WriteFile(%q) error = %v", name, err)
		}
	}
	if err := os.MkdirAll(filepath.Join(assets, "ansible"), 0o750); err != nil {
		t.Fatalf("MkdirAll() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(assets, "ansible", "site.yml"), []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	removeDemoFiles(db, assets)

	for _, name := range []string{db, db + "-wal", db + "-shm", db + "-journal", assets} {
		if _, err := os.Stat(name); !errors.Is(err, fs.ErrNotExist) {
			t.Errorf("%s is still there after the demo stopped (Stat error = %v)", filepath.Base(name), err)
		}
	}
	if _, err := os.Stat(identity); err != nil {
		t.Errorf("the producer identity beside the database was removed with it: %v", err)
	}
}

// TestOnlyATemporaryDemoRemovesItsFiles covers which demo runs clean up after themselves. A --db run
// was pointed at a database someone wants, and a --seed-only run exists to leave one behind.
func TestOnlyATemporaryDemoRemovesItsFiles(t *testing.T) {
	t.Parallel()
	tests := []struct {
		DB       string
		SeedOnly bool
		WantOwns bool
	}{{ // Test 0: No --db is a temporary database the run made for itself.
		DB: "", SeedOnly: false, WantOwns: true,
	}, { // Test 1: --seed-only keeps the database it seeded.
		DB: "", SeedOnly: true, WantOwns: false,
	}, { // Test 2: --db keeps what it was pointed at.
		DB: "demo.db", SeedOnly: false, WantOwns: false,
	}, { // Test 3: --db with --seed-only keeps it too.
		DB: "demo.db", SeedOnly: true, WantOwns: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := ownsDemoFiles(test.DB, test.SeedOnly); got != test.WantOwns {
				t.Errorf("ownsDemoFiles(%q, %v) = %v, want %v", test.DB, test.SeedOnly, got, test.WantOwns)
			}
		})
	}
}
