package identity

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/kordloom/loomseal/seal"
)

// writeKeyFile writes a stored identity under name in dir, standing in for a key file an operator,
// a deploy script, or a restore left behind.
func writeKeyFile(t *testing.T, dir, name, seed, installID string) {
	t.Helper()
	raw, err := json.Marshal(storedIdentity{InstallID: installID, Seed: seed})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), raw, 0o600); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", name, err)
	}
}

// seedHex returns a distinct 32 byte hex seed, so a test can name two keys that are certainly not
// the same key without depending on the generator.
func seedHex(fill byte) string {
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = fill
	}
	return hex.EncodeToString(seed)
}

// TestIdentityFromSeedRefusesAnythingThatIsNotAKey pins that a seed which is not exactly 32 bytes of
// hex is refused rather than stretched, truncated, or padded into one.
//
// This is the single funnel every load path runs through: the environment variable, the producer
// file, and the witness file all end here. Accepting a short seed would mean an install signs with a
// key an attacker can guess, and accepting a long one would mean two different configured seeds
// silently collapse to the same signing key, which makes one install's bundles verify as another's.
// The refusal has to name the size so an operator who pasted half a key can see what is wrong.
func TestIdentityFromSeedRefusesAnythingThatIsNotAKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Seed     string
		WantWord string
	}{{ // Test 0: An empty seed is not a key, which is also what a JSON file with no seed member gives.
		Name: "empty", Seed: "", WantWord: "seed must be",
	}, { // Test 1: One byte short is refused rather than padded out to a usable key.
		Name: "one byte short", Seed: strings.Repeat("ab", ed25519.SeedSize-1), WantWord: "seed must be",
	}, { // Test 2: One byte long is refused rather than truncated back down to the first 32.
		Name: "one byte long", Seed: strings.Repeat("ab", ed25519.SeedSize+1), WantWord: "seed must be",
	}, { // Test 3: An odd number of hex digits cannot decode at all.
		Name: "odd hex", Seed: strings.Repeat("a", 63), WantWord: "decode seed",
	}, { // Test 4: Non-hex characters are refused rather than skipped over.
		Name: "not hex", Seed: strings.Repeat("zz", ed25519.SeedSize), WantWord: "decode seed",
	}, { // Test 5: A seed of the right length wrapped in whitespace is not silently trimmed.
		Name: "whitespace", Seed: " " + seedHex(0x11) + " ", WantWord: "decode seed",
	}, { // Test 6: A base64 seed is the wrong encoding and is refused, not reinterpreted as hex.
		Name: "base64 not hex",
		Seed: base64.StdEncoding.EncodeToString(make([]byte, ed25519.SeedSize)), WantWord: "decode seed",
	}, { // Test 7: A very long value is refused on size rather than accepted or hung on.
		Name: "very long", Seed: strings.Repeat("ab", 4096), WantWord: "seed must be",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id, err := identityFromSeed(test.Seed, "in_test")
			if err == nil {
				t.Fatalf("identityFromSeed(%q) was accepted as a signing key", test.Name)
			}
			if !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("error = %q, want it to mention %q", err, test.WantWord)
			}
			// A refused seed hands back a zero identity, never a half-built one that would sign.
			if diff := cmp.Diff(Identity{}, id, cmp.AllowUnexported(Identity{})); diff != "" {
				t.Errorf("a refused seed returned a non-zero identity (-want +got):\n%s", diff)
			}
		})
	}
}

// TestIdentityFromSeedAcceptsTheBoundaryAndDerivesTheKey pins the other side of that boundary: a
// seed of exactly the right length builds the key ed25519 derives from it, in upper or lower case
// hex, and the install id follows the key when the caller names none.
func TestIdentityFromSeedAcceptsTheBoundaryAndDerivesTheKey(t *testing.T) {
	t.Parallel()
	raw := make([]byte, ed25519.SeedSize)
	for i := range raw {
		raw[i] = byte(i)
	}
	want := ed25519.NewKeyFromSeed(raw)
	tests := []struct {
		Name      string
		Seed      string
		InstallID string
		WantID    string
	}{{ // Test 0: Lower case hex, with the caller naming the install.
		Name: "lower", Seed: hex.EncodeToString(raw), InstallID: "in_named", WantID: "in_named",
	}, { // Test 1: Upper case hex is the same key, since hex decoding is case insensitive.
		Name: "upper", Seed: strings.ToUpper(hex.EncodeToString(raw)), InstallID: "in_named",
		WantID: "in_named",
	}, { // Test 2: No install id given, so it is derived from the key that will sign.
		Name: "derived", Seed: hex.EncodeToString(raw), InstallID: "",
		WantID: installIDFromKey(want.Public().(ed25519.PublicKey)),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id, err := identityFromSeed(test.Seed, test.InstallID)
			if err != nil {
				t.Fatalf("identityFromSeed() error = %v", err)
			}
			if !id.Private().Equal(want) {
				t.Error("the parsed key is not the one ed25519 derives from this seed")
			}
			if diff := cmp.Diff(test.WantID, id.InstallID); diff != "" {
				t.Errorf("install id mismatch (-want +got):\n%s", diff)
			}
			// Every published form has to describe the same key, or a relying party pinning one
			// encoding rejects a bundle carrying the other.
			pub := want.Public().(ed25519.PublicKey)
			if diff := cmp.Diff(hex.EncodeToString(pub), id.PublicKeyHex()); diff != "" {
				t.Errorf("hex public key mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(base64.StdEncoding.EncodeToString(pub), id.PublicKeyBase64()); diff != "" {
				t.Errorf("base64 public key mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(seal.KeyID(pub), id.KeyID()); diff != "" {
				t.Errorf("key id mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestPublicEncodingsAreTheSameKey pins that the hex and base64 forms decode back to identical
// bytes.
//
// The two encodings exist because audit verify --pubkey takes hex and a bundle carries base64. If
// they ever described different keys, an operator would publish one on a trust page and relying
// parties checking the other would reject every genuine bundle, which reads exactly like a forgery.
func TestPublicEncodingsAreTheSameKey(t *testing.T) {
	t.Parallel()
	id, err := identityFromSeed(seedHex(0x5a), "in_test")
	if err != nil {
		t.Fatalf("identityFromSeed() error = %v", err)
	}
	fromHex, err := hex.DecodeString(id.PublicKeyHex())
	if err != nil {
		t.Fatalf("the published hex key does not decode: %v", err)
	}
	fromB64, err := base64.StdEncoding.DecodeString(id.PublicKeyBase64())
	if err != nil {
		t.Fatalf("the published base64 key does not decode: %v", err)
	}
	if diff := cmp.Diff(fromHex, fromB64); diff != "" {
		t.Errorf("the two published encodings are different keys (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff([]byte(id.Public()), fromHex); diff != "" {
		t.Errorf("the published key is not the identity's own (-want +got):\n%s", diff)
	}
	// The private half really is the signing key for that public half, or nothing this install
	// signs will verify against what it publishes.
	msg := []byte("bundle")
	if !ed25519.Verify(id.Public(), msg, ed25519.Sign(id.Private(), msg)) {
		t.Error("a signature from the identity's private key does not verify against its public key")
	}
	if len(id.Private()) != ed25519.PrivateKeySize {
		t.Errorf("private key is %d bytes, want %d", len(id.Private()), ed25519.PrivateKeySize)
	}
}

// TestLoadRefusesACorruptKeyFile pins that a key file which is not a usable identity fails the load
// rather than being replaced with a freshly generated one.
//
// This is the fail-closed rule that matters most in this package. Only fs.ErrNotExist may fall
// through to creating a key. If a parse failure or an unreadable file also fell through, an install
// whose key file was corrupted by a bad restore, a full disk, or a truncated write would quietly
// mint a brand new signing identity, and every relying party pinning the old fingerprint would start
// rejecting genuine bundles with nothing in the log saying the key changed. Refusing loudly at
// startup is recoverable; a silent new key is not.
func TestLoadRefusesACorruptKeyFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Body     string
		WantWord string
	}{{ // Test 0: Not JSON at all, which is what a truncated or overwritten file looks like.
		Name: "not json", Body: "this is not json", WantWord: "parse",
	}, { // Test 1: An empty file, the classic result of a crash between create and write.
		Name: "empty file", Body: "", WantWord: "parse",
	}, { // Test 2: JSON of the wrong shape, so unmarshal into the stored type fails.
		Name: "json array", Body: `["seed"]`, WantWord: "parse",
	}, { // Test 3: Valid JSON carrying no seed, which cannot sign anything.
		Name: "no seed", Body: `{"install_id":"in_abc"}`, WantWord: "seed must be",
	}, { // Test 4: JSON null unmarshals cleanly into a zero value, so the seed check has to catch it.
		Name: "json null", Body: `null`, WantWord: "seed must be",
	}, { // Test 5: A seed that is present but not a key is refused, not rounded into one.
		Name: "short seed", Body: `{"install_id":"in_abc","seed":"abcd"}`, WantWord: "seed must be",
	}, { // Test 6: A seed of the right length that is not hex is refused.
		Name: "not hex seed", Body: `{"seed":"` + strings.Repeat("g", 64) + `"}`, WantWord: "decode seed",
	}, { // Test 7: The seed as a JSON number rather than a string fails the unmarshal.
		Name: "seed not a string", Body: `{"seed":12345}`, WantWord: "parse",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			path := filepath.Join(dir, File)
			if err := os.WriteFile(path, []byte(test.Body), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			id, err := LoadFile(dir)
			if err == nil {
				t.Fatalf("LoadFile() accepted a corrupt key file and returned %s, so the install "+
					"silently signs with something other than its own key", id.InstallID)
			}
			if !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("error = %q, want it to mention %q", err, test.WantWord)
			}
			// The corrupt file is left exactly as it was. Overwriting it would destroy the evidence
			// an operator needs to recover the real key from a backup.
			got, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("ReadFile() error = %v", readErr)
			}
			if diff := cmp.Diff(test.Body, string(got)); diff != "" {
				t.Errorf("the corrupt key file was rewritten (-want +got):\n%s", diff)
			}
		})
	}
}

// TestLoadRefusesACorruptKeyFileThroughEveryEntryPoint pins that the fail-closed rule is a property
// of the package rather than of one function.
//
// Load, LoadFile, and LoadWitnessFile are three doors onto the same file. A refactor that added a
// fallback to one of them would leave the others still refusing, so a test aimed at only one door
// would keep passing while an install reached through another door minted a new key.
func TestLoadRefusesACorruptKeyFileThroughEveryEntryPoint(t *testing.T) {
	// Not parallel at any level: the subtests use t.Setenv, which owns process-global state.
	tests := []struct {
		Name string
		File string
		Load func(string) (Identity, error)
	}{{ // Test 0: The plain file loader.
		Name: "LoadFile", File: File, Load: LoadFile,
	}, { // Test 1: The env-aware loader, with no env key set, falls through to the same file.
		Name: "Load", File: File, Load: Load,
	}, { // Test 2: The witness loader reads its own file and must refuse the same way.
		Name: "LoadWitnessFile", File: WitnessFile, Load: LoadWitnessFile,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: Load consults the process environment, which t.Setenv owns.
			t.Setenv(KeyEnv, "")
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, test.File), []byte("{"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			if _, err := test.Load(dir); err == nil {
				t.Fatalf("%s accepted a corrupt key file instead of refusing", test.Name)
			}
		})
	}
}

// TestLoadRefusesAKeyFileItCannotRead pins that a permission failure is refused rather than treated
// as a missing file.
//
// The loader keys its create-on-first-use behavior off fs.ErrNotExist alone. A key file that exists
// but is owned by another account returns a permission error, and if that were folded in with "not
// there" the install would generate a second identity beside the one it already has and start
// signing with a key nobody has pinned.
func TestLoadRefusesAKeyFileItCannotRead(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can read a file regardless of its mode")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, File)
	writeKeyFile(t, dir, File, seedHex(0x33), "in_existing")
	if err := os.Chmod(path, 0o000); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(path, 0o600) })

	if _, err := LoadFile(dir); err == nil {
		t.Fatal("LoadFile() treated an unreadable key file as a missing one, so the install would " +
			"generate a second identity beside the one it already has")
	}
	// And the existing file is still there, untouched, rather than replaced by a new key.
	if _, err := os.Stat(path); err != nil {
		t.Errorf("the unreadable key file was disturbed: %v", err)
	}
}

// TestLoadRefusesADirectoryWhereTheKeyFileBelongs pins that a directory standing in for the key file
// is an error rather than a missing file. A create would then fail on link, and the install has to
// say so instead of looping or silently continuing unsigned.
func TestLoadRefusesADirectoryWhereTheKeyFileBelongs(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, File), 0o700); err != nil {
		t.Fatalf("Mkdir() error = %v", err)
	}
	if _, err := LoadFile(dir); err == nil {
		t.Fatal("LoadFile() accepted a directory in place of the key file")
	}
}

// TestLoadRefusesABadEnvironmentKeyRatherThanFallingBackToTheFile pins that a malformed
// SWITCHTENDER_AUDIT_KEY stops the load.
//
// An operator who sets this variable is stating that the install signs with the key their secret
// manager holds. If a typo in that value quietly fell through to the file, the install would sign
// with a different key than the operator configured, and the bundles would carry a fingerprint
// nobody is pinning. The refusal names the variable so the operator knows which value to fix.
func TestLoadRefusesABadEnvironmentKeyRatherThanFallingBackToTheFile(t *testing.T) {
	// Not parallel at any level: the subtests use t.Setenv, which owns process-global state.
	tests := []struct {
		Name string
		Seed string
	}{{ // Test 0: A truncated paste, the most common way this value goes wrong.
		Name: "truncated", Seed: strings.Repeat("ab", 20),
	}, { // Test 1: Not hex at all.
		Name: "not hex", Seed: "this-is-not-a-key",
	}, { // Test 2: A trailing newline, which a shell heredoc or a file-backed secret adds.
		Name: "trailing newline", Seed: seedHex(0x44) + "\n",
	}, { // Test 3: Twice the length, which a doubled paste produces.
		Name: "doubled", Seed: seedHex(0x44) + seedHex(0x44),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: t.Setenv mutates process-global state.
			dir := t.TempDir()
			// A perfectly good key file is already here, so a fallback would look like success.
			writeKeyFile(t, dir, File, seedHex(0x77), "in_existing")
			t.Setenv(KeyEnv, test.Seed)

			id, err := Load(dir)
			if err == nil {
				t.Fatalf("Load() accepted a malformed %s and signed with %s instead", KeyEnv, id.KeyID())
			}
			if !strings.Contains(err.Error(), KeyEnv) {
				t.Errorf("error = %q, want it to name %s so the operator knows what to fix", err, KeyEnv)
			}
		})
	}
}

// TestEmptyEnvironmentKeyIsTreatedAsUnset pins that an exported but empty variable falls through to
// the file rather than being refused.
//
// An empty export is what a compose file, a systemd unit, or a Kubernetes manifest produces for a
// variable that was declared and not filled in. Refusing it would take down every install that has
// the variable in its template, and the file identity is the documented default.
func TestEmptyEnvironmentKeyIsTreatedAsUnset(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.
	dir := t.TempDir()
	want := seedHex(0x21)
	writeKeyFile(t, dir, File, want, "in_from_file")
	t.Setenv(KeyEnv, "")

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if diff := cmp.Diff(want, got.Seed); diff != "" {
		t.Errorf("an empty env key did not fall through to the file (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff("in_from_file", got.InstallID); diff != "" {
		t.Errorf("install id mismatch (-want +got):\n%s", diff)
	}
}

// TestEnvironmentKeyOnAnUnreadableInstallStillDerivesAnID pins the branch where the env key is good
// but the stored file cannot be read.
//
// Load reads the stored file only to keep the install id stable across a key rotation. When that
// read fails for any reason, the id is derived from the key rather than the load failing, because an
// operator holding the key in a secret manager must be able to start against a directory that has no
// usable file in it. Losing this would make the secret-manager path depend on a file it is meant to
// replace.
func TestEnvironmentKeyOnAnUnreadableInstallStillDerivesAnID(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.
	dir := t.TempDir()
	// A key file that exists and is unusable, so the id cannot be recovered from it.
	if err := os.WriteFile(filepath.Join(dir, File), []byte("{ broken"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	envSeed := seedHex(0x66)
	t.Setenv(KeyEnv, envSeed)

	got, err := Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v, want the env key to stand on its own", err)
	}
	if diff := cmp.Diff(envSeed, got.Seed); diff != "" {
		t.Errorf("seed mismatch (-want +got):\n%s", diff)
	}
	if want := installIDFromKey(got.Public()); got.InstallID != want {
		t.Errorf("install id = %s, want the key-derived %s", got.InstallID, want)
	}
}

// TestEnvironmentKeyDoesNotWriteItselfToDisk pins that the secret-manager path leaves no copy of the
// key behind.
//
// The whole point of holding the key in a secret manager is that it is not sitting in the state
// directory. If Load persisted the env seed, an operator who rotated the secret would still have the
// old key on disk, and the next start without the variable would silently fall back to it rather
// than failing the way a missing secret should.
func TestEnvironmentKeyDoesNotWriteItselfToDisk(t *testing.T) {
	// Not parallel: t.Setenv mutates process-global state.
	dir := t.TempDir()
	envSeed := seedHex(0x99)
	t.Setenv(KeyEnv, envSeed)

	if _, err := Load(dir); err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	for _, entry := range entries {
		raw, readErr := os.ReadFile(filepath.Join(dir, entry.Name()))
		if readErr != nil {
			continue
		}
		if strings.Contains(string(raw), envSeed) {
			t.Errorf("%s holds the environment seed, so the key a secret manager owns was copied "+
				"into the state directory", entry.Name())
		}
	}
}

// TestInstallIDIsStableAndKeyBound pins the two properties the id has to hold at once: the same key
// always produces the same id, and two different keys never produce the same one.
//
// Bundle verification refuses a bundle whose named install is not the install its key was born to,
// and it makes that call by deriving the id from the embedded key. An unstable derivation would
// reject genuine bundles, and a colliding one would let a key speak for an install it was not born
// to, which is the exact lift that check exists to close.
func TestInstallIDIsStableAndKeyBound(t *testing.T) {
	t.Parallel()
	seen := make(map[string]string)
	for i := range 64 {
		id, err := identityFromSeed(seedHex(byte(i)), "")
		if err != nil {
			t.Fatalf("identityFromSeed() error = %v", err)
		}
		// Stable: derived twice from the same key, it is the same id.
		if again := InstallIDFromKey(id.Public()); again != id.InstallID {
			t.Fatalf("install id is not stable: %s then %s", id.InstallID, again)
		}
		// The exported and unexported derivations are the same rule, so a verifier and a producer
		// agree. If they drifted, every bundle would be rejected as naming the wrong install.
		if got := installIDFromKey(id.Public()); got != InstallIDFromKey(id.Public()) {
			t.Fatalf("exported and internal derivations disagree: %s and %s", got, id.InstallID)
		}
		if !strings.HasPrefix(id.InstallID, "in_") {
			t.Errorf("install id %q does not carry the in_ prefix a reader identifies it by", id.InstallID)
		}
		if prev, ok := seen[id.InstallID]; ok {
			t.Fatalf("two different keys share install id %s: seeds %s and %s",
				id.InstallID, prev, id.Seed)
		}
		seen[id.InstallID] = id.Seed
	}
}

// TestInstallIDFromKeyPanicsOnAKeyTooShortToDerive records what the exported derivation does with a
// key that is not an ed25519 public key.
//
// It is exported for out-of-tree verifiers, and it indexes the first six bytes with no length check,
// so a caller that hands it whatever a bundle decoded to takes a panic rather than a refusal. The
// in-tree verifier checks the length first, so nothing ships broken today, but the exported
// signature invites the mistake. This test states the current contract out loud: callers must
// validate the length themselves.
func TestInstallIDFromKeyPanicsOnAKeyTooShortToDerive(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Key  ed25519.PublicKey
	}{{ // Test 0: A nil key, which is what a missing member decodes to.
		Name: "nil", Key: nil,
	}, { // Test 1: An empty key, what an empty base64 string decodes to.
		Name: "empty", Key: ed25519.PublicKey{},
	}, { // Test 2: One byte short of what the derivation reads.
		Name: "five bytes", Key: ed25519.PublicKey{1, 2, 3, 4, 5},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("InstallIDFromKey(%s) returned rather than panicking; if it now "+
						"refuses short keys that is an improvement and this test should say so",
						test.Name)
				}
			}()
			_ = InstallIDFromKey(test.Key)
		})
	}
	// Exactly six bytes is the smallest input that derives, so the boundary is at six and not seven.
	if got := InstallIDFromKey(ed25519.PublicKey{1, 2, 3, 4, 5, 6}); got != "in_010203040506" {
		t.Errorf("InstallIDFromKey(six bytes) = %q, want in_010203040506", got)
	}
}

// TestCreatedKeyIsUnpredictableAndOwnerOnly pins that a generated identity is a real random key held
// at owner-only permissions, and that two installs never share one.
//
// An install that was never configured by hand still signs, so the generator is the key most
// installs actually use. A predictable seed or a world-readable file would hand an attacker the
// ability to mint bundles that verify as this install, which is the one thing the whole audit chain
// rests on.
func TestCreatedKeyIsUnpredictableAndOwnerOnly(t *testing.T) {
	t.Parallel()
	seen := make(map[string]bool)
	for range 8 {
		dir := t.TempDir()
		id, err := LoadFile(dir)
		if err != nil {
			t.Fatalf("LoadFile() error = %v", err)
		}
		if seen[id.Seed] {
			t.Fatal("two freshly created installs share a signing key, so either can sign as the other")
		}
		seen[id.Seed] = true
		if len(id.Seed) != ed25519.SeedSize*2 {
			t.Errorf("generated seed is %d hex digits, want %d", len(id.Seed), ed25519.SeedSize*2)
		}
		// An all-zero seed is what a generator that silently failed would produce.
		if id.Seed == seedHex(0) {
			t.Error("the generated seed is all zeros, so the key generator produced nothing")
		}
		info, err := os.Stat(filepath.Join(dir, File))
		if err != nil {
			t.Fatalf("Stat() error = %v", err)
		}
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("key file mode = %o, want 600: the signing seed is readable by another account", perm)
		}
	}
}

// TestCreateLeavesNoTemporaryFileHoldingTheKey pins that the write-then-link dance cleans up after
// itself.
//
// The key is written to a temporary file first so a crash cannot leave a half-written key in place.
// That temporary file holds the full signing seed, so leaving it behind would put a second copy of
// the install's private key in the state directory under a name nothing manages, nothing rotates,
// and no operator knows to protect.
func TestCreateLeavesNoTemporaryFileHoldingTheKey(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	id, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
		if strings.HasSuffix(entry.Name(), ".tmp") {
			t.Errorf("%s was left behind holding a copy of the signing seed", entry.Name())
		}
	}
	if diff := cmp.Diff([]string{File}, names); diff != "" {
		t.Errorf("state directory contents (-want +got):\n%s", diff)
	}
	// And the one file that remains is the identity that was returned.
	raw, err := os.ReadFile(filepath.Join(dir, File))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	var stored storedIdentity
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatalf("the written file is not parseable: %v", err)
	}
	if diff := cmp.Diff(storedIdentity{InstallID: id.InstallID, Seed: id.Seed}, stored); diff != "" {
		t.Errorf("stored identity mismatch (-want +got):\n%s", diff)
	}
}

// TestConcurrentLoadsAgreeOnOneIdentity pins that many processes starting at once end up with the
// same install identity, which is the reason the writer links rather than renames.
//
// An install has one identity. If concurrent starts each believed their own generated key, the same
// install would emit bundles under several fingerprints and several ids, and a relying party pinning
// one of them would see the others as forgeries. Run this under -race, since these all share the
// directory.
func TestConcurrentLoadsAgreeOnOneIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const workers = 24

	var wg sync.WaitGroup
	results := make([]Identity, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = LoadFile(dir)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: LoadFile() error = %v", i, err)
		}
	}
	want := results[0]
	for i, got := range results {
		if got.Seed != want.Seed {
			t.Fatalf("worker %d signs with a different key than worker 0, so one install claims "+
				"two identities", i)
		}
		if got.InstallID != want.InstallID {
			t.Fatalf("worker %d claims install %s, worker 0 claims %s", i, got.InstallID, want.InstallID)
		}
	}
	// Exactly one key file, and it holds the key everybody agreed on.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	if len(entries) != 1 || entries[0].Name() != File {
		t.Fatalf("directory holds %d entries after concurrent starts, want just %s", len(entries), File)
	}
	stored, err := readIdentityFile(dir)
	if err != nil {
		t.Fatalf("readIdentityFile() error = %v", err)
	}
	if diff := cmp.Diff(want.Seed, stored.Seed); diff != "" {
		t.Errorf("the file on disk is not the key the callers were handed (-want +got):\n%s", diff)
	}
}

// TestConcurrentWitnessLoadsAgreeOnOneIdentity is the same rule for the witness file, which takes a
// different path through the loader and is what a witness pinned by a relying party depends on.
//
// It fails, and it is the realistic face of the createNamed defect below. The loser of the creation
// race adopts by reading producer-key.json rather than the file it was creating, so in a witness's
// own directory, where no producer file exists, that read fails and the witness does not start.
// Beside a producer key it is worse than a failure to start: the loser is handed the watched
// server's signing key.
func TestConcurrentWitnessLoadsAgreeOnOneIdentity(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	const workers = 16

	var wg sync.WaitGroup
	results := make([]Identity, workers)
	errs := make([]error, workers)
	start := make(chan struct{})
	for i := range workers {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = LoadWitnessFile(dir)
		}(i)
	}
	close(start)
	wg.Wait()

	for i, err := range errs {
		if err != nil {
			t.Fatalf("worker %d: LoadWitnessFile() error = %v; a witness that cannot start cannot "+
				"attest, and a witness that starts with a second key strands its checkpoints", i, err)
		}
	}
	for i, got := range results {
		if got.Seed != results[0].Seed {
			t.Fatalf("worker %d holds a different witness key than worker 0", i)
		}
	}
}

// TestCreateNamedAdoptsTheFileItWasAskedFor pins the loser-of-the-race path: when the link fails
// because the file already exists, the caller has to adopt the key in the file it was creating.
//
// It reads the producer file instead, whatever name it was asked for. A witness that loses this race
// is handed the producer key of the server it watches, which is precisely the countersigning the
// witness file separation exists to prevent, and in a witness-only directory the read fails outright
// so the witness will not start at all.
func TestCreateNamedAdoptsTheFileItWasAskedFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Name2        string
		WithProducer bool
	}{{ // Test 0: A witness losing the race beside a producer key must keep the witness key.
		Name: "witness beside a producer", Name2: WitnessFile, WithProducer: true,
	}, { // Test 1: A witness losing the race in a directory of its own must still succeed.
		Name: "witness alone", Name2: WitnessFile, WithProducer: false,
	}}
	const witnessSeed = "1111111111111111111111111111111111111111111111111111111111111111"
	const producerSeed = "2222222222222222222222222222222222222222222222222222222222222222"
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			// The state a losing racer finds: the winner's file is already in place.
			writeKeyFile(t, dir, test.Name2, witnessSeed, "in_witness")
			if test.WithProducer {
				writeKeyFile(t, dir, File, producerSeed, "in_producer")
			}
			got, err := createNamed(dir, test.Name2)
			if err != nil {
				t.Fatalf("createNamed(%s) error = %v, want the winner's key adopted", test.Name2, err)
			}
			if got.Seed == producerSeed {
				t.Fatal("the loser adopted the producer's key, so this witness countersigns the " +
					"server it watches")
			}
			if diff := cmp.Diff(witnessSeed, got.Seed); diff != "" {
				t.Errorf("adopted seed mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWitnessAndProducerKeysCoexistWithoutTouchingEachOther pins that creating one identity in a
// directory never disturbs the other, in either order.
//
// The two live side by side whenever a witness runs on the host it watches. If creating one
// overwrote or adopted the other, the witness would sign with the producer key and a relying party
// pinning the witness would be pinning the operator's own key, which makes the attestation
// forgeable by the very party it is meant to constrain.
func TestWitnessAndProducerKeysCoexistWithoutTouchingEachOther(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		ProducerLast bool
	}{{ // Test 0: The server starts first, then the witness joins it.
		Name: "producer first", ProducerLast: false,
	}, { // Test 1: The witness is there first and the server starts beside it.
		Name: "witness first", ProducerLast: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			var producer, witness Identity
			var err error
			if test.ProducerLast {
				if witness, err = LoadWitnessFile(dir); err != nil {
					t.Fatalf("LoadWitnessFile() error = %v", err)
				}
				if producer, err = LoadFile(dir); err != nil {
					t.Fatalf("LoadFile() error = %v", err)
				}
			} else {
				if producer, err = LoadFile(dir); err != nil {
					t.Fatalf("LoadFile() error = %v", err)
				}
				if witness, err = LoadWitnessFile(dir); err != nil {
					t.Fatalf("LoadWitnessFile() error = %v", err)
				}
			}
			if producer.Seed == witness.Seed {
				t.Fatal("the witness and the server it watches hold the same key")
			}
			if producer.KeyID() == witness.KeyID() {
				t.Fatal("the witness key fingerprint is the producer's, so pinning one pins the other")
			}
			// Both survive a reload, in either order, with neither having disturbed the other.
			reloadedProducer, err := LoadFile(dir)
			if err != nil {
				t.Fatalf("reload LoadFile() error = %v", err)
			}
			reloadedWitness, err := LoadWitnessFile(dir)
			if err != nil {
				t.Fatalf("reload LoadWitnessFile() error = %v", err)
			}
			if diff := cmp.Diff(producer.Seed, reloadedProducer.Seed); diff != "" {
				t.Errorf("producer key changed across a reload (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(witness.Seed, reloadedWitness.Seed); diff != "" {
				t.Errorf("witness key changed across a reload (-want +got):\n%s", diff)
			}
		})
	}
}

// TestWitnessRefusalIsNotFooledByEncodingOrCase pins the shape of the shared-key refusal.
//
// It compares the stored seed strings, so a copy that changed the JSON formatting still trips it and
// the refusal is not a byte comparison of whole files. The upper case case is the one worth knowing:
// the same key written in a different hex case is the same signing key and this refusal does not
// catch it, which is recorded here so the gap is a known one rather than an assumed absence.
func TestWitnessRefusalIsNotFooledByEncodingOrCase(t *testing.T) {
	t.Parallel()
	shared := seedHex(0x8c)
	tests := []struct {
		Name        string
		WitnessSeed string
		WantRefused bool
	}{{ // Test 0: The same seed under a reformatted file is still refused.
		Name: "same seed", WitnessSeed: shared, WantRefused: true,
	}, { // Test 1: A different key is the ordinary case and must be accepted.
		Name: "different seed", WitnessSeed: seedHex(0x3d), WantRefused: false,
	}, { // Test 2: The same key in upper case hex is not caught, which the refusal compares as text.
		Name: "same key upper case hex", WitnessSeed: strings.ToUpper(shared), WantRefused: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			writeKeyFile(t, dir, File, shared, "in_producer")
			// Written with a different install id and formatting, so only the seed can match.
			writeKeyFile(t, dir, WitnessFile, test.WitnessSeed, "in_witness")

			got, err := LoadWitnessFile(dir)
			if test.WantRefused {
				if err == nil {
					t.Fatalf("LoadWitnessFile() accepted key %s, which is the producer's", got.KeyID())
				}
				if !strings.Contains(err.Error(), WitnessFile) ||
					!strings.Contains(err.Error(), File) {
					t.Errorf("refusal = %q, want it to name both files so the operator can fix it", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("LoadWitnessFile() error = %v", err)
			}
			if test.Name == "same key upper case hex" && got.KeyID() != "" {
				// Recorded, not asserted as correct: the same key in another case gets through.
				producer, perr := identityFromSeed(shared, "")
				if perr != nil {
					t.Fatalf("identityFromSeed() error = %v", perr)
				}
				if got.KeyID() != producer.KeyID() {
					t.Error("the fixture no longer represents the same key in a different case")
				}
			}
		})
	}
}

// TestWitnessLoadSurvivesAnUnreadableProducerFile pins that the shared-key check does not turn an
// unrelated producer file problem into a witness that will not start.
//
// The check reads the producer file only to compare against. A witness pointed at a directory whose
// producer key is corrupt, or owned by the server's account and unreadable by the witness's, is the
// ordinary two-account deployment. Failing there would make the witness depend on being able to read
// the very secret it is designed not to hold.
func TestWitnessLoadSurvivesAnUnreadableProducerFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Body string
	}{{ // Test 0: A corrupt producer file.
		Name: "corrupt", Body: "not json",
	}, { // Test 1: An empty producer file.
		Name: "empty", Body: "",
	}, { // Test 2: A producer file carrying no seed to compare against.
		Name: "no seed", Body: `{"install_id":"in_abc"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, File), []byte(test.Body), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			got, err := LoadWitnessFile(dir)
			if err != nil {
				t.Fatalf("LoadWitnessFile() error = %v, want a witness to start beside an unreadable "+
					"producer file", err)
			}
			if got.Seed == "" {
				t.Error("the witness started without a key")
			}
		})
	}
}

// TestLoadCreatesTheStateDirectoryItWasGiven pins that a first boot against a directory that does
// not exist yet creates it at owner-only permissions rather than failing.
//
// The state directory is named by configuration and the server creates it on first use. If the key
// loader required it to exist, a fresh install would fail to start, and if it created it
// world-readable the key file's own 0600 would be the only thing protecting the seed from a listing.
func TestLoadCreatesTheStateDirectoryItWasGiven(t *testing.T) {
	t.Parallel()
	dir := filepath.Join(t.TempDir(), "nested", "state")
	id, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if id.Seed == "" {
		t.Fatal("no key was created")
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatalf("Stat() error = %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o700 {
		t.Errorf("state directory mode = %o, want 700", perm)
	}
}

// TestCreateReportsWhyItCouldNotWriteTheKey pins that a state directory the install cannot write to
// produces a named error at startup rather than an install that runs on without an identity.
//
// An install with no signing identity cannot sign a bundle, and the failure has to arrive at the
// moment the key would be created. The alternative is a server that starts, logs nothing unusual,
// and produces exports that no relying party can attribute to it, discovered only when an auditor
// asks for a bundle.
func TestCreateReportsWhyItCouldNotWriteTheKey(t *testing.T) {
	t.Parallel()
	if os.Geteuid() == 0 {
		t.Skip("running as root, which can write to a directory regardless of its mode")
	}
	tests := []struct {
		Name     string
		Dir      func(*testing.T) string
		WantWord string
	}{{ // Test 0: A file sits where a parent directory belongs, so nothing under it can be reached.
		// The read fails before the create is ever attempted, and the error is the operating
		// system's own rather than one this package labeled. It still names the key file, which is
		// what an operator needs to see, and it still refuses rather than carrying on unsigned.
		Name: "parent is a file",
		Dir: func(t *testing.T) string {
			t.Helper()
			file := filepath.Join(t.TempDir(), "not-a-dir")
			if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			return filepath.Join(file, "state")
		},
		WantWord: File,
	}, { // Test 1: The directory exists and is not writable, so the temporary file cannot be made.
		Name: "directory is read only",
		Dir: func(t *testing.T) string {
			t.Helper()
			dir := t.TempDir()
			if err := os.Chmod(dir, 0o500); err != nil {
				t.Fatalf("Chmod() error = %v", err)
			}
			t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
			return dir
		},
		WantWord: "write",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			id, err := LoadFile(test.Dir(t))
			if err == nil {
				t.Fatalf("LoadFile() reported identity %s while writing no key", id.InstallID)
			}
			if !strings.Contains(err.Error(), test.WantWord) {
				t.Errorf("error = %q, want it to mention %q", err, test.WantWord)
			}
			if id.Seed != "" || id.priv != nil {
				t.Error("a failed create handed back a partly built identity")
			}
		})
	}
}

// TestKeyFileSurvivesAnUnrelatedFileInTheDirectory pins that the loader reads its own file by name
// and is not confused by whatever else shares the state directory.
//
// The state directory holds the database, evidence packs, and whatever an operator left there. A
// loader that scanned rather than named could pick up a backup copy of an old key and silently sign
// with a fingerprint that was rotated away from.
func TestKeyFileSurvivesAnUnrelatedFileInTheDirectory(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	want := seedHex(0x4e)
	writeKeyFile(t, dir, File, want, "in_real")
	// A backup of an older key, exactly what a careful operator leaves behind before a rotation.
	writeKeyFile(t, dir, "producer-key.json.bak", seedHex(0xaa), "in_old")
	writeKeyFile(t, dir, "producer-key.json.2026-01-01", seedHex(0xbb), "in_older")

	got, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if diff := cmp.Diff(want, got.Seed); diff != "" {
		t.Errorf("the loader picked up a file other than %s (-want +got):\n%s", File, diff)
	}
	if diff := cmp.Diff("in_real", got.InstallID); diff != "" {
		t.Errorf("install id mismatch (-want +got):\n%s", diff)
	}
}

// TestStoredInstallIDWinsOverTheDerivedOne pins that a file naming an install id keeps it, even when
// the id does not match the key.
//
// The id is a stable identifier that survives key changes; derivation is only its birth rule. An
// install that rotated its key has a stored id that no longer derives from the key it holds, and
// re-deriving on load would rename that install and orphan every bundle already handed to an
// auditor under the old id.
func TestStoredInstallIDWinsOverTheDerivedOne(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	writeKeyFile(t, dir, File, seedHex(0x5f), "in_deliberately_unrelated")

	got, err := LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if diff := cmp.Diff("in_deliberately_unrelated", got.InstallID); diff != "" {
		t.Errorf("install id mismatch (-want +got):\n%s", diff)
	}
	if got.InstallID == installIDFromKey(got.Public()) {
		t.Fatal("the fixture is wrong: the stored id happens to be the derived one")
	}
	// A file with no stored id falls back to deriving one, so an install is never nameless.
	fresh := t.TempDir()
	writeKeyFile(t, fresh, File, seedHex(0x5f), "")
	derived, err := LoadFile(fresh)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}
	if want := installIDFromKey(derived.Public()); derived.InstallID != want {
		t.Errorf("install id = %q, want the derived %q", derived.InstallID, want)
	}
}

// TestZeroIdentityPanicsRatherThanSigningWithNothing records what a zero Identity does when asked
// for its key.
//
// Every constructor returns Identity{} alongside an error, so a caller that ignores the error holds
// one of these. Reading its public key panics on a nil private key rather than returning an empty or
// all-zero key. That is the safe direction: an all-zero public key would be a key an attacker knows
// the private half of, and a caller that ignored the error would then publish it as this install's
// identity. The panic is loud and immediate, so this pins it deliberately rather than by accident.
func TestZeroIdentityPanicsRatherThanSigningWithNothing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Call func(Identity)
	}{{ // Test 0: The public key itself.
		Name: "Public", Call: func(id Identity) { _ = id.Public() },
	}, { // Test 1: The fingerprint a trust page publishes.
		Name: "KeyID", Call: func(id Identity) { _ = id.KeyID() },
	}, { // Test 2: The hex form audit verify --pubkey takes.
		Name: "PublicKeyHex", Call: func(id Identity) { _ = id.PublicKeyHex() },
	}, { // Test 3: The base64 form a bundle carries.
		Name: "PublicKeyBase64", Call: func(id Identity) { _ = id.PublicKeyBase64() },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("%s on a zero identity returned a value; an all-zero or empty key "+
						"published as this install's identity is one an attacker holds", test.Name)
				}
			}()
			test.Call(Identity{})
		})
	}
	// Private is the one that answers, with nil, which cannot sign. Nothing here should read that
	// as a usable key.
	if priv := (Identity{}).Private(); priv != nil {
		t.Errorf("a zero identity handed out a %d byte private key", len(priv))
	}
}

// TestStoredIdentityIsTheOnlyEncoderThatEmitsTheSeed pins the other half of the seed-hiding rule:
// the on-disk type does emit it.
//
// The seed is hidden from every encoder so a careless respondJSON cannot publish the install's
// private signing key on an unauthenticated route. Hiding it everywhere would be a bug if the one
// place that must write it stopped, because the identity would then never persist and every restart
// would mint a new key.
func TestStoredIdentityIsTheOnlyEncoderThatEmitsTheSeed(t *testing.T) {
	t.Parallel()
	seed := seedHex(0x7b)
	raw, err := json.Marshal(storedIdentity{InstallID: "in_test", Seed: seed})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !strings.Contains(string(raw), seed) {
		t.Fatal("the on-disk type stopped writing the seed, so no identity can ever be reloaded")
	}
	// And the public type still refuses, including when embedded in something a handler responds
	// with, which is the shape the original mistake took.
	id, err := identityFromSeed(seed, "in_test")
	if err != nil {
		t.Fatalf("identityFromSeed() error = %v", err)
	}
	type trustPage struct {
		Producer Identity `json:"producer"`
		KeyID    string   `json:"key_id"`
	}
	page, err := json.Marshal(trustPage{Producer: id, KeyID: id.KeyID()})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if strings.Contains(string(page), seed) {
		t.Errorf("an identity nested in a response published the signing seed: %s", page)
	}
}
