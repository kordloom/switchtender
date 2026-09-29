package identity_test

import (
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
