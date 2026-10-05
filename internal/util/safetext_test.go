package util

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestIsSafeTextMatchesWhatSafeTextKeeps pins IsSafeText to SafeText: it reports true exactly for
// the text SafeText returns unchanged, the text every store keeps as it is.
func TestIsSafeTextMatchesWhatSafeTextKeeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In       string
		WantSafe bool
	}{{ // Test 0: ASCII.
		In: "web01", WantSafe: true,
	}, { // Test 1: Valid UTF-8 beyond ASCII.
		In: "café", WantSafe: true,
	}, { // Test 2: A NUL byte.
		In: "web\x00one",
	}, { // Test 3: A byte that is not UTF-8.
		In: "caf\xe9",
	}, { // Test 4: Empty text.
		In: "", WantSafe: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.WantSafe, IsSafeText(test.In)); diff != "" {
				t.Errorf("IsSafeText() mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantSafe, SafeText(test.In) == test.In); diff != "" {
				t.Errorf("SafeText() kept the text unchanged mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
