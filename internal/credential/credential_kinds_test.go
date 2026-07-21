package credential

import (
	"fmt"
	"testing"
)

// TestValidKindConnectionKinds confirms the machine, become, and network connection kinds are valid,
// so a run can materialize them through the extra-vars-file mechanism.
func TestValidKindConnectionKinds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Kind Kind
		Want bool
	}{
		{KindSSHPassword, true}, // Test 0: Machine password auth.
		{KindBecome, true},      // Test 1: Privilege escalation.
		{KindNetwork, true},     // Test 2: Network device login.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Kind), func(t *testing.T) {
			t.Parallel()
			if got := ValidKind(test.Kind); got != test.Want {
				t.Errorf("ValidKind(%q) = %v, want %v", test.Kind, got, test.Want)
			}
		})
	}
}
