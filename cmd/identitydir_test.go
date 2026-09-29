package cmd

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/identity"
)

// TestIdentityDirRefusesAVolatileHome pins that the producer signing identity is never placed in the
// system temp directory.
//
// A postgres DSN has no filesystem home, so the identity falls back to a per-user configuration
// directory. os.UserConfigDir fails when the account has no home, which is the ordinary shape of a
// container running a postgres-backed server, and the fallback from there used to be os.TempDir.
// That is the one place a signing key must not live: it is world-writable, and a restart empties it.
// A key that vanishes is not an outage, it is a second install identity, and because every audit
// entry is bound to the install that wrote it, the chain would attribute entries to a different
// install after every restart while still reporting itself sound.
func TestIdentityDirRefusesAVolatileHome(t *testing.T) {
	const dsn = "postgres://user:secret@db.internal:5432/switchtender?sslmode=require"

	// An account with no home, so os.UserConfigDir cannot answer.
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AppData", "")
	t.Setenv(identityDirEnv, "")

	dir, err := identityDir(dsn)
	if err == nil {
		t.Fatalf("identityDir returned %q with nowhere durable to put a key, want a refusal", dir)
	}
	if !errors.Is(err, errNoIdentityHome) {
		t.Errorf("error = %v, want it to wrap errNoIdentityHome so a caller can tell it apart", err)
	}
	if !strings.Contains(err.Error(), identityDirEnv) {
		t.Errorf("error = %q, want it to name %s, which is the way out of this state",
			err, identityDirEnv)
	}
	// The DSN carries a password. A message built from it would put that password in the logs.
	if strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), dsn) {
		t.Errorf("the refusal quotes the DSN, which carries a password: %q", err)
	}

	// The explicit placement is honored, which is what makes refusing reasonable.
	want := t.TempDir()
	t.Setenv(identityDirEnv, want)
	got, err := identityDir(dsn)
	if err != nil {
		t.Fatalf("identityDir with %s set: %v", identityDirEnv, err)
	}
	if got != want {
		t.Errorf("identityDir = %q, want the directory %s named, %q", got, identityDirEnv, want)
	}
}

// TestIdentityDirNeverAnswersWithTempDir pins the property directly, across the inputs that reach
// the fallback, so a later edit cannot reintroduce the temp directory by another route.
func TestIdentityDirNeverAnswersWithTempDir(t *testing.T) {
	tmp := os.TempDir()
	for _, db := range []string{
		"postgres://user:secret@host/db",
		"postgresql://user:secret@host/db",
		"switchtender.db",
		"/var/lib/switchtender/switchtender.db",
	} {
		t.Setenv("HOME", "")
		t.Setenv("XDG_CONFIG_HOME", "")
		t.Setenv("AppData", "")
		t.Setenv(identityDirEnv, "")
		got, err := identityDir(db)
		if err != nil {
			continue
		}
		if got == tmp || strings.HasPrefix(got, tmp+string(filepath.Separator)) {
			t.Errorf("identityDir(%q) = %q, which is inside the system temp directory; a signing "+
				"key there is world-writable and gone after a restart", db, got)
		}
	}
}

// TestIdentityDirKeepsASQLiteKeyBesideItsDatabase pins that the ordinary case is unchanged: a file
// database keeps its identity next to it, so a copied database carries its key.
func TestIdentityDirKeepsASQLiteKeyBesideItsDatabase(t *testing.T) {
	t.Setenv(identityDirEnv, "")
	got, err := identityDir("/var/lib/switchtender/switchtender.db")
	if err != nil {
		t.Fatalf("identityDir: %v", err)
	}
	if want := "/var/lib/switchtender"; got != want {
		t.Errorf("identityDir = %q, want %q", got, want)
	}
	bare, err := identityDir("switchtender.db")
	if err != nil {
		t.Fatalf("identityDir: %v", err)
	}
	if bare != "." {
		t.Errorf("identityDir = %q, want the working directory", bare)
	}
}

// TestIdentityDirAcceptsAnEnvKeyWithNoHome pins that a seed in SWITCHTENDER_AUDIT_KEY needs no
// durable directory, so a keyed no-home install is not refused.
//
// The env seed signs directly: that path in identity.Load never reads or writes the key file. The
// refusal for a homeless postgres install fired before that was reached, so the documented way to
// sign a shared chain, a keyed container, was turned away over a directory its key never touches,
// and serve fell back to an unattributed chain with unsigned bundles.
func TestIdentityDirAcceptsAnEnvKeyWithNoHome(t *testing.T) {
	const dsn = "postgres://user:secret@db.internal:5432/switchtender?sslmode=require"
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("AppData", "")
	t.Setenv(identityDirEnv, "")
	// A valid 32-byte hex seed, the shape identity.Load accepts.
	t.Setenv(identity.KeyEnv, strings.Repeat("ab", 32))

	dir, err := identityDir(dsn)
	if err != nil {
		t.Fatalf("identityDir with %s set errored on a no-home install: %v", identity.KeyEnv, err)
	}
	if dir != "." {
		t.Errorf("identityDir = %q, want the don't-care directory: the env seed needs none", dir)
	}
	// With no env key, the same no-home install still refuses rather than choosing a volatile dir.
	t.Setenv(identity.KeyEnv, "")
	if _, err := identityDir(dsn); err == nil {
		t.Error("identityDir accepted a no-home file-backed install, want the refusal")
	}
}
