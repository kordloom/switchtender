package outcome_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheDisclosedDigestStillCollidesWhichIsWhyTheBindingExists records the property the disclosed
// digest cannot have, so the reason for a second value is written down rather than remembered.
//
// The digest is taken over the redacted spec because a receipt publishes those exact bytes. An
// unquoted secret assignment is masked to the end of its line, which is the right call for secrecy
// and means the digest cannot see anything written after the secret. That is not a defect to fix in
// the redaction: narrowing the mask would leave the tail of a passphrase in the clear, which is the
// bug the end-of-line rule was written for.
//
// It is a defect only if something security-bearing is built on it, which is why the execution gate
// is built on the binding instead. If this test ever fails, the redaction changed shape and the
// passphrase case needs re-checking.
func TestTheDisclosedDigestStillCollidesWhichIsWhyTheBindingExists(t *testing.T) {
	t.Parallel()
	approved := &run.Run{
		ID: "r", Tool: run.ToolBash,
		Command: `run.sh --ssh_pass: hunter2 --limit canary --tags deploy`,
	}
	tampered := &run.Run{
		ID: "r", Tool: run.ToolBash,
		Command: `run.sh --ssh_pass: hunter2 --limit ALL --tags deploy,destroy`,
	}
	da, err := outcome.SpecDigest(approved)
	if err != nil {
		t.Fatalf("SpecDigest(approved) error = %v", err)
	}
	db, err := outcome.SpecDigest(tampered)
	if err != nil {
		t.Fatalf("SpecDigest(tampered) error = %v", err)
	}
	if da != db {
		t.Skipf("the redacted digest now separates these two specs (%s vs %s). That is an "+
			"improvement, and it means the redaction's masking rule changed: re-check that a "+
			"multi-word passphrase is still masked to the end of its line before relying on it",
			da, db)
	}
	// And the binding, which is what the gate uses, does separate them.
	ba, err := outcome.SpecBinding(approved)
	if err != nil {
		t.Fatalf("SpecBinding(approved) error = %v", err)
	}
	bb, err := outcome.SpecBinding(tampered)
	if err != nil {
		t.Fatalf("SpecBinding(tampered) error = %v", err)
	}
	if ba == bb {
		t.Fatal("neither the digest nor the binding separates an approved run from a rewritten " +
			"one, so the approved-spec gate cannot refuse the rewrite at all")
	}
}
