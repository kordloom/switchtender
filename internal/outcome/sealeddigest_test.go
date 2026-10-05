package outcome_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestTheApprovalBindsWhichSealedAnswerWillBeOpened pins that an approval covers the sealed answer
// itself and not only the fact that one was given.
//
// The spec named a secret answer by its variable alone, so two runs that differed only in which
// sealed answer they carried had one spec, one binding, and one digest. A sealed answer copied in
// from another run under the same name, after an approver released the run, executed under an
// approval of a different answer and every record of the decision still matched. The digest of the
// ciphertext separates them, in the binding the executor checks and in the digest the receipt
// discloses. The variable is named like a secret on purpose: every redaction pass masks the value
// under such a key, and the digest has to survive the redacted form the receipt commits to.
func TestTheApprovalBindsWhichSealedAnswerWillBeOpened(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name is the secret variable.
		Name string
	}{
		{Name: "db_password"}, // Test 0: A name every redaction pass reads as secret.
		{Name: "api_token"},   // Test 1: Another stem the classifier knows.
		{Name: "release"},     // Test 2: A name nothing reads as secret.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			approved := &run.Run{ID: "run_1", Tool: run.ToolBash, Command: "rotate"}
			run.WithSealedVars(map[string]string{test.Name: "c2VhbGVkLWFwcHJvdmVk"})(approved)
			swapped := &run.Run{ID: "run_1", Tool: run.ToolBash, Command: "rotate"}
			run.WithSealedVars(map[string]string{test.Name: "c2VhbGVkLXN3YXBwZWQ="})(swapped)

			ba, err := outcome.SpecBinding(approved)
			if err != nil {
				t.Fatalf("SpecBinding(approved) error = %v", err)
			}
			bs, err := outcome.SpecBinding(swapped)
			if err != nil {
				t.Fatalf("SpecBinding(swapped) error = %v", err)
			}
			if ba == bs {
				t.Errorf("the approved and the swapped sealed answer share the binding %s, so an "+
					"approval of one releases the other", ba)
			}
			da, err := outcome.SpecDigest(approved)
			if err != nil {
				t.Fatalf("SpecDigest(approved) error = %v", err)
			}
			ds, err := outcome.SpecDigest(swapped)
			if err != nil {
				t.Fatalf("SpecDigest(swapped) error = %v", err)
			}
			if da == ds {
				t.Errorf("the redacted spec gives both sealed answers the digest %s, so the chain's "+
					"record of the decision cannot say which one was approved", da)
			}
			spec, err := outcome.Spec(approved)
			if err != nil {
				t.Fatalf("Spec() error = %v", err)
			}
			if !strings.Contains(string(spec), approved.SealedDigests[0].SHA256) {
				t.Errorf("the disclosed spec does not carry the sealed answer's digest: %s", spec)
			}
			if strings.Contains(string(spec), "c2VhbGVkLWFwcHJvdmVk") {
				t.Errorf("the disclosed spec carries the ciphertext: %s", spec)
			}
		})
	}
}

// TestASpecWithoutDigestsReadsAsItAlwaysDid pins that the binding changed nothing for a run it does
// not apply to: a run with no sealed answers, and a run stored before digests existed, whose sealed
// answers come back with no digests. Their specs are the bytes they always were, so every receipt
// and approval already issued over them still verifies.
func TestASpecWithoutDigestsReadsAsItAlwaysDid(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Run is the run whose spec is read.
		Run *run.Run
	}{{ // Test 0: No sealed answers at all.
		Run: &run.Run{ID: "run_plain", Tool: run.ToolBash, Command: "deploy"},
	}, { // Test 1: Sealed answers restored from a row written before digests existed.
		Run: func() *run.Run {
			r := &run.Run{ID: "run_legacy", Tool: run.ToolBash, Command: "rotate"}
			run.RestoreSealed(r, map[string]string{"db_password": "c2VhbGVkLWxlZ2FjeQ=="}, nil)
			return r
		}(),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			spec, err := outcome.Spec(test.Run)
			if err != nil {
				t.Fatalf("Spec() error = %v", err)
			}
			if strings.Contains(string(spec), "sealed_var_digests") {
				t.Errorf("the spec gained a digest list the run never had: %s", spec)
			}
		})
	}
}
