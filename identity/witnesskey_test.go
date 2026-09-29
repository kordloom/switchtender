package identity_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/identity"
)

// TestWitnessIdentityIgnoresTheProducerKeyEnvironment pins that a witness signs with its own key
// even where the producer's key is in scope.
//
// Load lets SWITCHTENDER_AUDIT_KEY win so an operator can hold the producer key in their own secret
// manager. A witness started in that environment picked it up and countersigned its own subject: a
// relying party pinning the witness key from the witness's own directory was pinning the watched
// server's key, and the watched operator could mint the attestation the witness exists to make
// unforgeable. Nothing about the resulting file or output looked wrong.
func TestWitnessIdentityIgnoresTheProducerKeyEnvironment(t *testing.T) {
	const producerSeed = "5b2f8c1d4e6a7b9c0d1e2f3a4b5c6d7e8f90a1b2c3d4e5f60718293a4b5c6d7e"
	dir := t.TempDir()

	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	witnessOwn, err := identity.LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	t.Setenv("SWITCHTENDER_AUDIT_KEY", producerSeed)
	producer, err := identity.Load(dir)
	if err != nil {
		t.Fatalf("Load() error = %v", err)
	}
	if producer.KeyID() == witnessOwn.KeyID() {
		t.Fatal("the fixture is wrong: the producer seed produced the witness's own key")
	}

	// The variable is in scope, exactly as it would be on a host that also runs the server.
	got, err := identity.LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() with the producer key in scope error = %v", err)
	}
	if got.KeyID() == producer.KeyID() {
		t.Error("the witness signed with the watched server's key, so it countersigns its own subject")
	}
	if got.KeyID() != witnessOwn.KeyID() {
		t.Errorf("witness key changed to %s, want its own %s; a changed key strands its checkpoints",
			got.KeyID(), witnessOwn.KeyID())
	}
}

// TestWitnessKeyIsItsOwnFile pins the other half of the same problem. Ignoring the environment stops a
// witness picking up a key an operator exported, and it does nothing about the file: the witness read
// producer-key.json, under that name, out of whatever directory it was pointed at, which defaults to the
// directory its checkpoint lives in. A witness run on the host it watches, or pointed at the server's
// state directory for convenience, signed its attestations with the watched server's producer key. A
// relying party pinning that key was pinning the operator's own, and the operator could mint the
// statement the witness exists to make unforgeable.
func TestWitnessKeyIsItsOwnFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// The server's identity, created where a server would create it.
	producer, err := identity.LoadFile(dir)
	if err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	// The witness, pointed at the very same directory.
	witnessKey, err := identity.LoadWitnessFile(dir)
	if err != nil {
		t.Fatalf("LoadWitnessFile() error = %v", err)
	}
	if witnessKey.KeyID() == producer.KeyID() {
		t.Error("the witness signed with the watched server's producer key, so its attestation is " +
			"one the watched operator can forge")
	}
	if witnessKey.InstallID == producer.InstallID {
		t.Error("the witness carries the watched install's id, so its attestation reads as the " +
			"server's own statement")
	}

	// It is stable across restarts, or every restart strands the checkpoints the previous key signed.
	again, err := identity.LoadWitnessFile(dir)
	if err != nil {
		t.Fatalf("second LoadWitnessFile() error = %v", err)
	}
	if again.KeyID() != witnessKey.KeyID() {
		t.Errorf("witness key changed to %s, want %s", again.KeyID(), witnessKey.KeyID())
	}

	// And a witness in a directory of its own, which is the ordinary case, still gets one.
	alone, err := identity.LoadWitnessFile(t.TempDir())
	if err != nil {
		t.Fatalf("LoadWitnessFile() in a fresh directory error = %v", err)
	}
	if alone.KeyID() == witnessKey.KeyID() {
		t.Error("two witnesses in different directories share a key")
	}
}

// TestWitnessRefusesTheProducerKeyCopiedIntoItsFile pins the last line of the separation, the one
// that fires when the two files hold the same key rather than merely sharing a directory.
//
// The distinct filename stops a witness from reading the producer file by accident. It does not stop
// somebody copying producer-key.json over witness-key.json, by a careless deploy script or a restore
// that fanned one key across a directory. A witness that then signed with it would countersign the
// server it watches, and its output would look correct: the file it wanted, holding a valid key. The
// loader refuses that key instead of using it, so the failure is loud and at startup rather than a
// silent forgery. Without this test the whole separation could be reverted to a plain read and only
// the accidental-read cases would notice.
func TestWitnessRefusesTheProducerKeyCopiedIntoItsFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	// A server identity, written where a server writes it.
	if _, err := identity.LoadFile(dir); err != nil {
		t.Fatalf("LoadFile() error = %v", err)
	}

	// Somebody copies the producer key over the witness key, so both files hold the same seed.
	producerBytes, err := os.ReadFile(filepath.Join(dir, identity.File))
	if err != nil {
		t.Fatalf("read %s: %v", identity.File, err)
	}
	if err := os.WriteFile(filepath.Join(dir, identity.WitnessFile), producerBytes, 0o600); err != nil {
		t.Fatalf("write %s: %v", identity.WitnessFile, err)
	}

	_, err = identity.LoadWitnessFile(dir)
	if err == nil {
		t.Fatal("a witness whose key file is a copy of the producer's was accepted, so it would " +
			"countersign the server it watches")
	}
	if !strings.Contains(err.Error(), "same key") {
		t.Errorf("refusal did not name the reason: %v", err)
	}
}
