package outcome

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestSpecBindingAndDigestCoverThePinnedCommit holds an approval to the commit its run was pinned to,
// in both the binding the executor checks and the digest a receipt discloses. A run whose pin moved
// after approval, whether to another commit or to no commit at all, must not reduce to the bytes the
// approver released: the executor's check in dispatch.execute compares the binding recomputed at run
// time against the one stamped at approval, and a pin the binding ignores is one that could move
// between the two unnoticed.
func TestSpecBindingAndDigestCoverThePinnedCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the transition.
		Name string
		// PinA and PinB are the commit before and after. Empty means unpinned.
		PinA, PinB string
		// WantSame is whether the two must reduce to the same bytes.
		WantSame bool
	}{{ // Test 0: The same commit binds the same bytes, which is the ordinary approved case.
		Name: "same commit", PinA: "a1b2c3", PinB: "a1b2c3", WantSame: true,
	}, { // Test 1: A different commit moves the binding, the tamper this field exists to catch.
		Name: "different commit", PinA: "a1b2c3", PinB: "d4e5f6", WantSame: false,
	}, { // Test 2: A run approved unpinned that later claims a pin is not the run that was approved.
		Name: "unpinned then pinned", PinA: "", PinB: "a1b2c3", WantSame: false,
	}, { // Test 3: A run approved against a commit that later claims no pin at all is the same
		// tamper in the other direction: dropping the pin is as much a lie about what ran as
		// swapping it for another commit.
		Name: "pinned then unpinned", PinA: "a1b2c3", PinB: "", WantSame: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			a := &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml", PinnedCommit: test.PinA}
			b := &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml", PinnedCommit: test.PinB}

			bindingA, err := SpecBinding(a)
			if err != nil {
				t.Fatalf("SpecBinding(a) error = %v", err)
			}
			bindingB, err := SpecBinding(b)
			if err != nil {
				t.Fatalf("SpecBinding(b) error = %v", err)
			}
			if got := bindingA == bindingB; got != test.WantSame {
				t.Errorf("binding equal = %v, want %v", got, test.WantSame)
			}

			digestA, err := SpecDigest(a)
			if err != nil {
				t.Fatalf("SpecDigest(a) error = %v", err)
			}
			digestB, err := SpecDigest(b)
			if err != nil {
				t.Fatalf("SpecDigest(b) error = %v", err)
			}
			if got := digestA == digestB; got != test.WantSame {
				t.Errorf("disclosed digest equal = %v, want %v", got, test.WantSame)
			}
		})
	}
}

// TestThePinnedCommitIsDisclosedInTheSpec proves the pin is not only protected by the digest but
// actually readable in the spec a receipt publishes. The commit a run executed against is not a
// secret, and an approver or a later auditor needs to see which one it was, not merely trust that
// some unstated value matched.
func TestThePinnedCommitIsDisclosedInTheSpec(t *testing.T) {
	t.Parallel()
	r := &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml", PinnedCommit: "a1b2c3d4e5f6"}
	body, err := Spec(r)
	if err != nil {
		t.Fatalf("Spec() error = %v", err)
	}
	if !strings.Contains(string(body), "a1b2c3d4e5f6") {
		t.Errorf("disclosed spec = %s, want the pinned commit readable in it", body)
	}

	// An unpinned run discloses no pinned_commit key at all, rather than an empty one, so the
	// common case of no pin adds nothing to the published bytes.
	unpinned := &run.Run{Tool: run.ToolAnsible, Playbook: "site.yml"}
	body, err = Spec(unpinned)
	if err != nil {
		t.Fatalf("Spec() error = %v", err)
	}
	if strings.Contains(string(body), "pinned_commit") {
		t.Errorf("disclosed spec = %s, want no pinned_commit key for an unpinned run", body)
	}
}
