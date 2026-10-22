package cmd

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/witness"
)

// keyDirNames returns the names directly inside dir.
func keyDirNames(t *testing.T, dir string) []string {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir() error = %v", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	return names
}

// TestWitnessRotateKeyNeedsAKeyDirectoryApartFromTheState pins the rotate-key command: it refuses
// without --key-dir and refuses a key directory that is or lies inside the state directory, writing
// nothing, and a key directory of its own rotates, printing both keys and leaving the state
// directory untouched.
func TestWitnessRotateKeyNeedsAKeyDirectoryApartFromTheState(t *testing.T) {
	t.Cleanup(func() {
		witnessRotateKeyDir, witnessRotateStateDir = "", "switchtender-witness"
	})
	base := t.TempDir()
	keyDir, stateDir := filepath.Join(base, "keys"), filepath.Join(base, "state")
	id, err := audit.LoadWitnessIdentity(keyDir)
	if err != nil {
		t.Fatalf("LoadWitnessIdentity() error = %v", err)
	}
	tests := []struct {
		// KeyDir is --key-dir.
		KeyDir string
		// StateDir is --state-dir.
		StateDir string
		// WantErr is a fragment the refusal must contain.
		WantErr string
	}{{ // Test 0: No key directory.
		StateDir: stateDir, WantErr: "--key-dir",
	}, { // Test 1: The key directory is the state directory, as serve defaults to.
		KeyDir: keyDir, StateDir: keyDir, WantErr: "state directory",
	}, { // Test 2: The key directory lies inside the state directory.
		KeyDir: keyDir, StateDir: base, WantErr: "state directory",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			witnessRotateKeyDir, witnessRotateStateDir = test.KeyDir, test.StateDir
			err := runWitnessRotate(testCommand(), nil)
			if err == nil || !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("runWitnessRotate() = %v, want a refusal naming %q", err, test.WantErr)
			}
			if diff := cmp.Diff([]string{"witness-key.json"}, keyDirNames(t, keyDir)); diff != "" {
				t.Errorf("a refused rotation wrote to the key directory (-want +got):\n%s", diff)
			}
		})
	}
	witnessRotateKeyDir, witnessRotateStateDir = keyDir, stateDir
	var out, errOut bytes.Buffer
	c := testCommand()
	c.SetOut(&out)
	c.SetErr(&errOut)
	if err := runWitnessRotate(c, nil); err != nil {
		t.Fatalf("runWitnessRotate() error = %v", err)
	}
	var got witness.Rotation
	if err := json.Unmarshal(out.Bytes(), &got); err != nil {
		t.Fatalf("decode %s: %v", out.Bytes(), err)
	}
	if got.Retired.KeyID != id.KeyID() || got.Current.KeyID == id.KeyID() ||
		got.Retired.Retired == "" || got.Retired.Retired != got.Current.ValidFrom {
		t.Errorf("rotation printed %+v, want %s retired at the instant the new key began", got,
			id.KeyID())
	}
	if !strings.Contains(errOut.String(), "start the witness again") {
		t.Errorf("stderr = %q, want it to say the witness must start again", errOut.String())
	}
	if _, err := os.Stat(stateDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the rotation touched the state directory: %v", err)
	}
}
