package cmd

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/identity"
	"github.com/kordloom/switchtender/internal/audit"
)

// serveUnder runs serve on db and returns how the start ended. A start the identity check failed to
// refuse binds a port nothing else holds and ends with the context.
func serveUnder(t *testing.T, db string) error {
	t.Helper()
	setString(t, &serveDB, db)
	setString(t, &serveAddr, "127.0.0.1:0")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	cmd := testCommand()
	cmd.SetContext(ctx)
	return runServe(cmd, nil)
}

// boundInstall returns the install db's chain is bound to.
func boundInstall(t *testing.T, db string) string {
	t.Helper()
	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	reader, ok := bundle.Audits().(audit.InstallReader)
	if !ok {
		t.Fatal("the store does not read its bound install")
	}
	bound, err := reader.BoundInstall(context.Background())
	if err != nil {
		t.Fatalf("BoundInstall() error = %v", err)
	}
	return bound
}

// TestServeRefusesAForeignIdentity pins that a server whose key names another install than its
// chain does not start. Running on would leave it unkeyed, checking no seal and passing no gate,
// while its operator believes the install is keyed.
func TestServeRefusesAForeignIdentity(t *testing.T) {
	// Not parallel: runServe reads package-level flag variables and the environment.
	quietServeFlags(t)
	db := tempDB(t)
	mintToken(t, db, "first")
	bound := boundInstall(t, db)
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
	before := (&adoptFixture{DB: db, Key: gateKey(t, 7)}).state(t).Entries
	err = serveUnder(t, db)
	if !errors.Is(err, errForeignIdentity) || !strings.Contains(err.Error(), "would run unkeyed") {
		t.Fatalf("runServe() with another install's key: error = %v, want the start refused", err)
	}
	if got := boundInstall(t, db); got != bound {
		t.Errorf("the chain is bound to %s after the refused start, want %s", got, bound)
	}
	if after := (&adoptFixture{DB: db, Key: gateKey(t, 7)}).state(t).Entries; after != before {
		t.Errorf("the refused start appended %d entries, want none", after-before)
	}
}

// TestAnAdoptionUnderOneKeyRefusesAServeUnderAnother is the mistake an unbound chain makes possible
// end to end: adopt run under one key binds the chain to that key's install, and a server started
// with the seed the install's servers were meant to use is refused rather than run unkeyed.
func TestAnAdoptionUnderOneKeyRefusesAServeUnderAnother(t *testing.T) {
	// Not parallel: it sets the environment and runServe reads package-level flag variables.
	quietServeFlags(t)
	db := tempDB(t)
	if got := boundInstall(t, db); got != "" {
		t.Fatalf("a fresh install's chain is bound to %s, want none", got)
	}
	t.Setenv(identity.KeyEnv, hex.EncodeToString(bytes.Repeat([]byte{1}, 32)))
	var out, prompt strings.Builder
	if err := adopt(context.Background(), adoptRequest{DB: db, Yes: true, In: strings.NewReader(""),
		Out: &out, Prompt: &prompt}); err != nil {
		t.Fatalf("adopt() under the first key error = %v\n%s", err, out.String())
	}
	if !strings.Contains(out.String(), "the key in "+identity.KeyEnv) {
		t.Errorf("adopt did not name the key it binds the chain to:\n%s", out.String())
	}
	bound := boundInstall(t, db)
	if bound == "" {
		t.Fatal("adoption left the chain bound to no install")
	}
	t.Setenv(identity.KeyEnv, hex.EncodeToString(bytes.Repeat([]byte{2}, 32)))
	err := serveUnder(t, db)
	if !errors.Is(err, errForeignIdentity) || !strings.Contains(err.Error(), bound) {
		t.Fatalf("runServe() under the second key: error = %v, want the start refused naming %s",
			err, bound)
	}
	if got := boundInstall(t, db); got != bound {
		t.Errorf("the chain is bound to %s after the refused start, want %s", got, bound)
	}
}

// TestAdoptNamesTheKeySourceOnAnUnboundChain pins where adopt says its key came from, since that key
// is the one an unbound chain is bound to. On a shared database whose key is this account's own
// file it says every server must sign with it.
func TestAdoptNamesTheKeySourceOnAnUnboundChain(t *testing.T) {
	// Not parallel: it sets the environment.
	quietServeFlags(t)
	sqlite := filepath.Join(t.TempDir(), "switchtender.db")
	dsn := "postgres://switchtender@db.internal/switchtender"
	tests := []struct {
		// Seed sets SWITCHTENDER_AUDIT_KEY.
		Seed bool
		// Dir sets SWITCHTENDER_IDENTITY_DIR.
		Dir string
		// DB is the install's database.
		DB string
		// WantShown is what the source must say.
		WantShown []string
		// WantWarned reports whether it warns that every server must sign with the key.
		WantWarned bool
	}{{ // Test 0: The seed names the environment.
		Seed: true, DB: dsn, WantShown: []string{"the key in " + identity.KeyEnv},
	}, { // Test 1: A SQLite install names the file beside its database.
		DB: sqlite, WantShown: []string{filepath.Join(filepath.Dir(sqlite), audit.IdentityFile)},
	}, { // Test 2: A shared database with neither set names this account's file and warns.
		DB: dsn, WantShown: []string{audit.IdentityFile, "first keyed start is refused",
			identity.KeyEnv}, WantWarned: true,
	}, { // Test 3: A shared database with a directory chosen names that directory and does not warn.
		DB: dsn, Dir: "/srv/switchtender/identity",
		WantShown: []string{filepath.Join("/srv/switchtender/identity", audit.IdentityFile)},
	}}
	for testNum, test := range tests {
		seed := ""
		if test.Seed {
			seed = hex.EncodeToString(bytes.Repeat([]byte{1}, 32))
		}
		t.Setenv(identity.KeyEnv, seed)
		t.Setenv(identityDirEnv, test.Dir)
		got := adoptKeySource(test.DB)
		for _, want := range test.WantShown {
			if !strings.Contains(got, want) {
				t.Errorf("test %d: source %q does not say %q", testNum, got, want)
			}
		}
		if warned := strings.Contains(got, "this account's own"); warned != test.WantWarned {
			t.Errorf("test %d: source %q warned %t, want %t", testNum, got, warned, test.WantWarned)
		}
	}
}
