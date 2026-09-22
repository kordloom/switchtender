package outcome_test

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTwoDifferentSpecsNeverShareADigest pins the approved-spec gate against a collision.
//
// A run's spec is redacted before it is disclosed, and its digest was then taken by redacting the
// already-redacted bytes a second time. Redaction is not idempotent: the first pass turns a quoted
// secret into a marker, and the second pass, seeing no quote after the key, matches from the key to
// the end of the line and erases everything after it. So two commands that differ only in that tail
// reduced to identical bytes and shared one digest.
//
// That is not cosmetic. The digest is what the approver's decision commits to and what execution
// rechecks against the stored row. A command approved as one thing could be edited into another
// that collides, and the gate at execution, and the receipt at verify, both said it matched. The
// repro is the fleet-widening tamper the gate exists to stop: approve a run limited to one canary
// host, rewrite it to the whole fleet with a destroy tag, same digest.
func TestTwoDifferentSpecsNeverShareADigest(t *testing.T) {
	t.Parallel()
	approved := &run.Run{
		ID: "r", Tool: run.ToolBash,
		Command: `ansible-playbook site.yml -e 'ansible_password: "s3cr3t"' --limit canary --tags deploy`,
	}
	tampered := &run.Run{
		ID: "r", Tool: run.ToolBash,
		Command: `ansible-playbook site.yml -e 'ansible_password: "s3cr3t"' --limit ALL --tags deploy,destroy`,
	}

	da, err := outcome.SpecDigest(approved)
	if err != nil {
		t.Fatalf("SpecDigest(approved) error = %v", err)
	}
	db, err := outcome.SpecDigest(tampered)
	if err != nil {
		t.Fatalf("SpecDigest(tampered) error = %v", err)
	}
	if da == db {
		t.Fatalf("two commands that differ after a redacted value share the digest %s: a run "+
			"approved limited to one host can be rewritten to the whole fleet and still match "+
			"the digest the approver released", da)
	}
}

// TestTheSpecDigestCoversTheDisclosedBytes pins the other half: the digest is over exactly the
// bytes a receipt hands an auditor as spec_body, so a third party recomputing it agrees.
//
// The digest was over doubly-redacted bytes while the receipt disclosed singly-redacted bytes, so
// an auditor taking a plain SHA-256 of spec_body got a value the chain never committed, and the
// promise in the code that the disclosed bytes are "what the digest already covers" was false.
func TestTheSpecDigestCoversTheDisclosedBytes(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "r", Tool: run.ToolBash,
		Command:   `deploy.sh --token "abc123" --region us-east-1`,
		ExtraVars: map[string]any{"db_password": "hunter2"},
	}
	spec, err := outcome.Spec(r)
	if err != nil {
		t.Fatalf("Spec() error = %v", err)
	}
	digest, err := outcome.SpecDigest(r)
	if err != nil {
		t.Fatalf("SpecDigest() error = %v", err)
	}
	sum := sha256.Sum256(spec) // the exact bytes disclosed as spec_body
	want := "sha256:" + hex.EncodeToString(sum[:])
	if digest != want {
		t.Errorf("SpecDigest = %s, but a plain SHA-256 of the disclosed spec_body is %s: an "+
			"auditor who recomputes the digest from what they were shown disagrees with the chain",
			digest, want)
	}
}
