package cmd

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/handoff"
)

// TestWorkerKeyNewWritesAKeyAndPrintsItsPublicHalf pins the registration workflow an operator runs:
// key new writes the private key, mode 0600, and prints the line the pool file takes, key public
// prints the same line again from the file, and a second key new over the same file is refused
// rather than replacing a key a pool depends on.
func TestWorkerKeyNewWritesAKeyAndPrintsItsPublicHalf(t *testing.T) {
	path := filepath.Join(t.TempDir(), "delivery.key")
	out, errOut, code := runCLI(t, "worker", "key", "new", "--out", path)
	if code != 0 {
		t.Fatalf("worker key new exited %d: %s", code, errOut)
	}
	var made deliveryKeyInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &made); err != nil {
		t.Fatalf("worker key new printed %q, not one JSON line: %v", out, err)
	}
	key, err := handoff.LoadPrivateKeyFile(path)
	if err != nil {
		t.Fatalf("the written key does not load: %v", err)
	}
	want := deliveryKeyInfo{KeyID: key.ID(), DeliveryKey: key.Public().String(), File: path}
	if diff := cmp.Diff(want, made); diff != "" {
		t.Errorf("key new output mismatch (-want +got):\n%s", diff)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatalf("Stat() error = %v", err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Errorf("the key file is mode %#o, want 0600", info.Mode().Perm())
		}
	}

	out, errOut, code = runCLI(t, "worker", "key", "public", path)
	if code != 0 {
		t.Fatalf("worker key public exited %d: %s", code, errOut)
	}
	var shown deliveryKeyInfo
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &shown); err != nil {
		t.Fatalf("worker key public printed %q: %v", out, err)
	}
	if diff := cmp.Diff(deliveryKeyInfo{KeyID: key.ID(), DeliveryKey: key.Public().String()},
		shown); diff != "" {
		t.Errorf("key public output mismatch (-want +got):\n%s", diff)
	}

	if _, _, code := runCLI(t, "worker", "key", "new", "--out", path); code == 0 {
		t.Errorf("a second key new over an existing key succeeded")
	}
	again, err := handoff.LoadPrivateKeyFile(path)
	if err != nil || again.ID() != key.ID() {
		t.Errorf("the existing key was replaced or damaged: %v", err)
	}
}

// TestWorkerKeyCommandsRefuseWhatTheyCannotTrust pins the refusals at the command line: key new
// with nowhere to write, key public on a file another account can read, and a worker given a
// delivery key with no control node to receive deliveries from.
func TestWorkerKeyCommandsRefuseWhatTheyCannotTrust(t *testing.T) {
	dir := t.TempDir()
	loose := filepath.Join(dir, "loose.key")
	k, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	if err := handoff.WriteKeyFile(loose, k); err != nil {
		t.Fatalf("WriteKeyFile() error = %v", err)
	}
	if err := os.Chmod(loose, 0o644); err != nil {
		t.Fatalf("Chmod() error = %v", err)
	}
	tests := []struct {
		Name       string
		Args       []string
		WantStderr string
		SkipOn     string
	}{{ // Test 0: key new needs a file to write.
		Name: "no out", Args: []string{"worker", "key", "new"}, WantStderr: "--out",
	}, { // Test 1: A key file another account can read is refused.
		Name: "loose file", Args: []string{"worker", "key", "public", loose},
		WantStderr: "lets another account read it", SkipOn: "windows",
	}, { // Test 2: A delivery key on a worker that leases from a database has nothing to open.
		Name: "no server", Args: []string{"worker", "--delivery-key", loose},
		WantStderr: "--delivery-key opens secrets a control node seals to a relay worker",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			if runtime.GOOS == test.SkipOn {
				t.Skip("this platform keeps no POSIX mode bits")
			}
			_, errOut, code := runCLI(t, test.Args...)
			if code == 0 {
				t.Fatalf("%v succeeded, want a refusal", test.Args)
			}
			if !strings.Contains(errOut, test.WantStderr) {
				t.Errorf("stderr = %q, want it to name %q", errOut, test.WantStderr)
			}
		})
	}
}

// TestLoadDeliveryKeysRefusesABadSet pins how a relay worker reads its --delivery-key files: every
// file must be a private key only its owner can read, and the same key twice is a mistake rather
// than a rotation.
func TestLoadDeliveryKeysRefusesABadSet(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	write := func(name string) string {
		k, err := handoff.GenerateKey()
		if err != nil {
			t.Fatalf("GenerateKey() error = %v", err)
		}
		path := filepath.Join(dir, name)
		if err := handoff.WriteKeyFile(path, k); err != nil {
			t.Fatalf("WriteKeyFile() error = %v", err)
		}
		return path
	}
	oldKey, newKey := write("old.key"), write("new.key")
	notAKey := filepath.Join(dir, "not.key")
	if err := os.WriteFile(notAKey, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	tests := []struct {
		Paths   []string
		WantLen int
		Want    error
	}{{ // Test 0: No key is an empty ring, which a relay worker without delivery runs with.
		Paths: nil, WantLen: 0, Want: nil,
	}, { // Test 1: An old and a new key, as during a rotation.
		Paths: []string{oldKey, newKey}, WantLen: 2, Want: nil,
	}, { // Test 2: The same key named twice.
		Paths: []string{oldKey, oldKey}, Want: handoff.ErrKey,
	}, { // Test 3: A file that is not a key.
		Paths: []string{notAKey}, Want: handoff.ErrKeyFile,
	}, { // Test 4: A file that does not exist.
		Paths: []string{filepath.Join(dir, "missing.key")}, Want: handoff.ErrKeyFile,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ring, err := loadDeliveryKeys(test.Paths)
			if !errors.Is(err, test.Want) {
				t.Fatalf("loadDeliveryKeys() error = %v, want %v", err, test.Want)
			}
			if err == nil && ring.Len() != test.WantLen {
				t.Errorf("ring holds %d keys, want %d", ring.Len(), test.WantLen)
			}
		})
	}
}
