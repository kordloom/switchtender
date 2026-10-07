package outcome

import (
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestSpecBindingCoversThePinnedCommit holds an approval to the commit its run was pinned to, so a
// run pinned to another commit after approval cannot pass the executor's check.
func TestSpecBindingCoversThePinnedCommit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name     string
		PinA     string
		PinB     string
		WantSame bool
	}{{ // Test 0: The same commit binds the same bytes.
		Name:     "same commit",
		PinA:     "a1b2c3",
		PinB:     "a1b2c3",
		WantSame: true,
	}, { // Test 1: A different commit moves the binding.
		Name:     "different commit",
		PinA:     "a1b2c3",
		PinB:     "d4e5f6",
		WantSame: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			a, err := SpecBinding(&run.Run{Tool: "ansible", Playbook: "site.yml", PinnedCommit: test.PinA})
			if err != nil {
				t.Fatal(err)
			}
			b, err := SpecBinding(&run.Run{Tool: "ansible", Playbook: "site.yml", PinnedCommit: test.PinB})
			if err != nil {
				t.Fatal(err)
			}
			if got := a == b; got != test.WantSame {
				t.Errorf("binding equal = %v, want %v", got, test.WantSame)
			}
		})
	}
}
