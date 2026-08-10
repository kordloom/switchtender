package identity

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestIdentityNeverSerializesItsSeed pins that encoding an Identity cannot emit the signing seed.
//
// The seed carried a json tag, so any handler holding an identity was one careless respondJSON away
// from publishing the install's private signing key on an unauthenticated route. The key's entire
// value is that it never leaves the install, so it is written only through the on-disk type and is
// invisible to every other encoder.
func TestIdentityNeverSerializesItsSeed(t *testing.T) {
	t.Parallel()
	seed := strings.Repeat("ab", ed25519.SeedSize)
	id, err := identityFromSeed(seed, "in_test")
	if err != nil {
		t.Fatalf("identityFromSeed() error = %v", err)
	}
	for _, v := range []any{id, &id, []Identity{id}, map[string]Identity{"producer": id}} {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatalf("Marshal() error = %v", err)
		}
		if strings.Contains(string(raw), seed) {
			t.Errorf("encoding %T published the signing seed: %s", v, raw)
		}
		if strings.Contains(string(raw), `"seed"`) {
			t.Errorf("encoding %T carries a seed member: %s", v, raw)
		}
	}
	// The public half is what a relying party pins, and it must still be reachable.
	if id.PublicKeyHex() == "" || id.KeyID() == "" {
		t.Error("the identity stopped exposing its public key")
	}
}

// TestIdentityRoundTripsThroughItsFile pins that the seed still reaches disk and reads back, since
// hiding it from every encoder would be a bug if it also stopped persisting.
func TestIdentityRoundTripsThroughItsFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	created, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	reloaded, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() second call error = %v", err)
	}
	if created.Seed != reloaded.Seed || created.InstallID != reloaded.InstallID {
		t.Errorf("identity did not survive a reload: created %s, reloaded %s",
			created.InstallID, reloaded.InstallID)
	}
	if created.PublicKeyHex() != reloaded.PublicKeyHex() {
		t.Error("the reloaded identity signs with a different key")
	}
	// The stored file is the one place the seed is allowed to appear, at owner-only permissions.
	path := filepath.Join(dir, File)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if !strings.Contains(string(raw), created.Seed) {
		t.Error("the key file does not hold the seed, so the identity cannot be reloaded")
	}
	if _, err := hex.DecodeString(created.Seed); err != nil {
		t.Errorf("stored seed is not hex: %v", err)
	}
}
