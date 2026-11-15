package util_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestSpecDigestSeparatesCommandsThatDifferAfterASecret pins that two commands which differ only
// after a name: value secret disclose different spec bodies under different spec digests.
//
// The receipt digest is taken over the spec as shown, with secrets masked. When the mask for this
// form ran to the end of the line, echo password: x > proof.txt and echo password: an entirely
// different tail --limit all both disclosed echo password: «redacted», and switchtender verify
// printed spec OK for each under one digest. The receipt then proved only the text before the
// secret.
//
// It is an external test package because outcome imports this package through the audit redactor,
// which is the path under test.
func TestSpecDigestSeparatesCommandsThatDifferAfterASecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// First and Second are the two commands.
		First, Second string
		// Tails are text after the secret that each disclosed spec must carry, in order.
		Tails [2]string
	}{{ // Test 0: The two runs the seat approved and executed on the release binary.
		First:  "echo password: x > /tmp/sandbox/proof.txt",
		Second: "echo password: an entirely different tail --limit all",
		Tails:  [2]string{"proof.txt", "entirely different tail --limit all"},
	}, { // Test 1: A canary run widened to the fleet with a destroy tag.
		First:  "deploy.sh password: hunter2 --limit canary --tags deploy",
		Second: "deploy.sh password: hunter2 --limit all --tags destroy",
		Tails:  [2]string{"--limit canary --tags deploy", "--limit all --tags destroy"},
	}, { // Test 2: One namespace widened to all of them.
		First:  "kubectl delete ns staging --token: hunter2 -n staging",
		Second: "kubectl delete ns staging --token: hunter2 --all-namespaces --all",
		Tails:  [2]string{"-n staging", "--all-namespaces --all"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			var digests [2]string
			for i, command := range []string{test.First, test.Second} {
				r := &run.Run{ID: "r", Tool: run.ToolBash, Command: command}
				body, err := outcome.Spec(r)
				if err != nil {
					t.Fatalf("Spec(%q) error = %v", command, err)
				}
				if strings.Contains(string(body), "hunter2") {
					t.Errorf("the disclosed spec carries the secret: %s", body)
				}
				if !strings.Contains(string(body), test.Tails[i]) {
					t.Errorf("the disclosed spec lost %q, the command after the secret: %s",
						test.Tails[i], body)
				}
				if digests[i], err = outcome.SpecDigest(r); err != nil {
					t.Fatalf("SpecDigest(%q) error = %v", command, err)
				}
			}
			if digests[0] == digests[1] {
				t.Errorf("%q and %q share the spec digest %s, so one receipt stands for both",
					test.First, test.Second, digests[0])
			}
		})
	}
}
