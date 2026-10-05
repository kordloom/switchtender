package handoff

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestKeysRoundTripThroughTheirTextForms pins that a key written by one side reads back as the same
// key on the other: the private key through its PKCS #8 file, and the public key through the line a
// pool file carries.
func TestKeysRoundTripThroughTheirTextForms(t *testing.T) {
	t.Parallel()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	pemBytes, err := k.MarshalPEM()
	if err != nil {
		t.Fatalf("MarshalPEM() error = %v", err)
	}
	back, err := ParsePrivateKey(pemBytes)
	if err != nil {
		t.Fatalf("ParsePrivateKey() error = %v", err)
	}
	pub, err := ParsePublicKey(k.Public().String())
	if err != nil {
		t.Fatalf("ParsePublicKey() error = %v", err)
	}
	got := []string{back.ID(), pub.ID(), back.Public().String()}
	want := []string{k.ID(), k.ID(), k.Public().String()}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("round trip mismatch (-want +got):\n%s", diff)
	}
	if len(k.ID()) != 32 {
		t.Errorf("key id %q is not 32 hex characters", k.ID())
	}
	if !strings.HasPrefix(k.Public().String(), PublicKeyPrefix) {
		t.Errorf("public key %q does not name its algorithm", k.Public().String())
	}
	// A seal to the parsed public key opens with the parsed private key, so the forms are not only
	// equal but usable.
	env, err := Seal(pub, "dmz", testBinding(), testPayload("run_alpha"))
	if err != nil {
		t.Fatalf("Seal() error = %v", err)
	}
	if _, err := Open(ring(t, back), testBinding(), env); err != nil {
		t.Errorf("Open() with the parsed key error = %v", err)
	}
}

// TestParsePublicKeyRefusesWhatIsNotAKey pins that a pool file holding anything but a delivery key
// fails to load, rather than registering a key nobody holds.
func TestParsePublicKeyRefusesWhatIsNotAKey(t *testing.T) {
	t.Parallel()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	raw := strings.TrimPrefix(k.Public().String(), PublicKeyPrefix)
	short := base64.StdEncoding.EncodeToString(make([]byte, 31))
	tests := []struct {
		In   string
		Want error
	}{{ // Test 0: The key with its prefix parses.
		In: k.Public().String(), Want: nil,
	}, { // Test 1: Surrounding whitespace from a YAML value is tolerated.
		In: "  " + k.Public().String() + "\n", Want: nil,
	}, { // Test 2: A bare base64 key without the algorithm prefix is refused.
		In: raw, Want: ErrKey,
	}, { // Test 3: Another algorithm's prefix is refused.
		In: "ed25519:" + raw, Want: ErrKey,
	}, { // Test 4: Text that is not base64 is refused.
		In: PublicKeyPrefix + "not base64!", Want: ErrKey,
	}, { // Test 5: A key of the wrong length is refused.
		In: PublicKeyPrefix + short, Want: ErrKey,
	}, { // Test 6: An empty value is refused.
		In: "", Want: ErrKey,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := ParsePublicKey(test.In); !errors.Is(err, test.Want) {
				t.Errorf("ParsePublicKey(%q) error = %v, want %v", test.In, err, test.Want)
			}
		})
	}
}

// TestParsePrivateKeyReadsOnlyOneX25519Key pins the private key format: one standard PKCS #8 block
// holding an X25519 key, the shape openssl genpkey writes. A signing key, a second block, or a
// different PEM type is refused rather than guessed at.
func TestParsePrivateKeyReadsOnlyOneX25519Key(t *testing.T) {
	t.Parallel()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	good, err := k.MarshalPEM()
	if err != nil {
		t.Fatalf("MarshalPEM() error = %v", err)
	}
	_, edKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("ed25519.GenerateKey() error = %v", err)
	}
	edDER, err := x509.MarshalPKCS8PrivateKey(edKey)
	if err != nil {
		t.Fatalf("MarshalPKCS8PrivateKey() error = %v", err)
	}
	ed := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: edDER})
	block, _ := pem.Decode(good)
	wrongType := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: block.Bytes})
	tests := []struct {
		In   []byte
		Want error
	}{{ // Test 0: The file MarshalPEM writes.
		In: good, Want: nil,
	}, { // Test 1: The bare block with no comment, as openssl writes it.
		In: pem.EncodeToMemory(block), Want: nil,
	}, { // Test 2: A signing key is not a delivery key.
		In: ed, Want: ErrKey,
	}, { // Test 3: Two keys in one file are refused rather than reading the first.
		In: append(append([]byte{}, good...), good...), Want: ErrKey,
	}, { // Test 4: Another PEM type is refused.
		In: wrongType, Want: ErrKey,
	}, { // Test 5: No PEM at all.
		In: []byte("x25519 key goes here"), Want: ErrKey,
	}, { // Test 6: A PEM block whose body is not PKCS #8.
		In: pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: []byte("junk")}), Want: ErrKey,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := ParsePrivateKey(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ParsePrivateKey() error = %v, want %v", err, test.Want)
			}
			if err == nil && got.ID() != k.ID() {
				t.Errorf("parsed key id %s, want %s", got.ID(), k.ID())
			}
		})
	}
}

// TestLoadPrivateKeyFileRefusesWhatOthersCanRead pins the file check. The key opens every secret
// sealed to its pool, so a copy another account can read is refused before it is read, and a path
// that is not a regular file is refused too.
func TestLoadPrivateKeyFileRefusesWhatOthersCanRead(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("Windows keeps no POSIX mode bits; its access list is the operator's to set")
	}
	dir := t.TempDir()
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	private := filepath.Join(dir, "private.key")
	if err := WriteKeyFile(private, k); err != nil {
		t.Fatalf("WriteKeyFile() error = %v", err)
	}
	loose := filepath.Join(dir, "loose.key")
	if err := WriteKeyFile(loose, k); err != nil {
		t.Fatalf("WriteKeyFile() error = %v", err)
	}
	if err := os.Chmod(loose, 0o640); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	tests := []struct {
		Path string
		Want error
	}{{ // Test 0: A file only its owner can read loads.
		Path: private, Want: nil,
	}, { // Test 1: A file the owner's group can read is refused.
		Path: loose, Want: ErrKeyFile,
	}, { // Test 2: A missing file is refused.
		Path: filepath.Join(dir, "missing.key"), Want: ErrKeyFile,
	}, { // Test 3: A directory is refused.
		Path: dir, Want: ErrKeyFile,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := LoadPrivateKeyFile(test.Path)
			if !errors.Is(err, test.Want) {
				t.Fatalf("LoadPrivateKeyFile() error = %v, want %v", err, test.Want)
			}
			if err == nil && got.ID() != k.ID() {
				t.Errorf("loaded key id %s, want %s", got.ID(), k.ID())
			}
		})
	}
}

// TestWriteKeyFileNeverReplacesAKey pins that writing a key over an existing file is refused and
// leaves the existing key in place. A pool's key overwritten by mistake fails every delivery to
// that pool until a new one is registered.
func TestWriteKeyFileNeverReplacesAKey(t *testing.T) {
	t.Parallel()
	path := filepath.Join(t.TempDir(), "delivery.key")
	first, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	second, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	if err := WriteKeyFile(path, first); err != nil {
		t.Fatalf("WriteKeyFile() error = %v", err)
	}
	if err := WriteKeyFile(path, second); !errors.Is(err, ErrKeyFile) {
		t.Fatalf("a second WriteKeyFile() error = %v, want %v", err, ErrKeyFile)
	}
	kept, err := LoadPrivateKeyFile(path)
	if err != nil {
		t.Fatalf("LoadPrivateKeyFile() error = %v", err)
	}
	if kept.ID() != first.ID() {
		t.Errorf("the file holds key %s, want the first key %s", kept.ID(), first.ID())
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat() error = %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("key file mode = %#o, want 0600", info.Mode().Perm())
		}
	}
}

// TestKeyRingRefusesAKeyGivenTwice pins that the same key named twice is a configuration error
// rather than a silent rotation, and that a ring lists its keys in a stable order.
func TestKeyRingRefusesAKeyGivenTwice(t *testing.T) {
	t.Parallel()
	keyA, keyB := testKeys()
	tests := []struct {
		Keys    []*PrivateKey
		WantIDs []string
		Want    error
	}{{ // Test 0: Two different keys, as during a rotation.
		Keys: []*PrivateKey{keyA, keyB}, WantIDs: sortedIDs(keyA, keyB), Want: nil,
	}, { // Test 1: The same key twice.
		Keys: []*PrivateKey{keyA, keyA}, Want: ErrKey,
	}, { // Test 2: A nil key.
		Keys: []*PrivateKey{nil}, Want: ErrKey,
	}, { // Test 3: No keys at all is an empty ring, which opens nothing.
		Keys: nil, WantIDs: []string{}, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			r, err := NewKeyRing(test.Keys...)
			if !errors.Is(err, test.Want) {
				t.Fatalf("NewKeyRing() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.WantIDs, r.IDs(), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("IDs() mismatch (-want +got):\n%s", diff)
			}
			if r.Len() != len(test.WantIDs) {
				t.Errorf("Len() = %d, want %d", r.Len(), len(test.WantIDs))
			}
		})
	}
	var none *KeyRing
	if none.Len() != 0 || none.IDs() != nil {
		t.Errorf("a nil ring reports keys")
	}
}

// sortedIDs returns the ids of keys in the order a ring lists them.
func sortedIDs(keys ...*PrivateKey) []string {
	r, err := NewKeyRing(keys...)
	if err != nil {
		panic(err)
	}
	return r.IDs()
}
