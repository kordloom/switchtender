package outcome_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheBindingSeparatesARewriteTheDisclosedDigestCannot records the property the disclosed digest
// cannot have, so the reason for a second value is written down rather than remembered.
//
// The digest is taken over the redacted spec because a receipt publishes those exact bytes. An
// unquoted secret assignment is masked to the end of its line, which is the right call for secrecy
// and means the digest cannot see anything written after the secret. That is not a defect to fix in
// the redaction: narrowing the mask would leave the tail of a passphrase in the clear, which is the
// bug the end-of-line rule was written for. It is a defect only if something security-bearing is
// built on it, which is why the execution gate is built on the binding instead.
//
// The binding is checked first and unconditionally. It used to be checked after a skip on the digest
// comparison, so the digest getting better would have stopped this from checking the gate's own
// property at all: the one assertion here that is security-bearing was the one a change could switch
// off. The digest is an observation below it, and the masking rule it depends on is asserted in
// TestTheApprovedSpecBindingSeparatesSpecsTheDigestCannot rather than left for somebody to re-check
// by hand.
func TestTheBindingSeparatesARewriteTheDisclosedDigestCannot(t *testing.T) {
	t.Parallel()
	approved := &run.Run{
		ID: "r", Tool: run.ToolBash,
		Command: `run.sh --ssh_pass: hunter2 --limit canary --tags deploy`,
	}
	tampered := &run.Run{
		ID: "r", Tool: run.ToolBash,
		Command: `run.sh --ssh_pass: hunter2 --limit ALL --tags deploy,destroy`,
	}
	// The binding is what the gate reads, so it is what must hold here.
	ba, err := outcome.SpecBinding(approved)
	if err != nil {
		t.Fatalf("SpecBinding(approved) error = %v", err)
	}
	bb, err := outcome.SpecBinding(tampered)
	if err != nil {
		t.Fatalf("SpecBinding(tampered) error = %v", err)
	}
	if ba == bb {
		t.Fatal("the binding does not separate an approved run from a rewritten one, so the " +
			"approved-spec gate cannot refuse the rewrite at all")
	}

	// The digest below is the reason that binding exists rather than a requirement of its own. A
	// digest that started separating these would be an improvement, so it is reported and not
	// failed on.
	da, err := outcome.SpecDigest(approved)
	if err != nil {
		t.Fatalf("SpecDigest(approved) error = %v", err)
	}
	db, err := outcome.SpecDigest(tampered)
	if err != nil {
		t.Fatalf("SpecDigest(tampered) error = %v", err)
	}
	if da != db {
		t.Logf("the redacted digest now separates these two specs (%s vs %s), so the masking rule "+
			"has changed shape since this was written", da, db)
	}
}
