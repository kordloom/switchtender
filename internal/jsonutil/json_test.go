package jsonutil

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestMarshal verifies compact and indented serialization behavior.
func TestMarshal(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In      any
		WantOut string
		Pretty  bool
	}{
		{ // Test 0: Compact basic map.
			In:      map[string]int{"a": 1},
			Pretty:  false,
			WantOut: `{"a":1}`,
		},
		{ // Test 1: Pretty basic map indented two spaces.
			In:      map[string]int{"a": 1},
			Pretty:  true,
			WantOut: "{\n  \"a\": 1\n}",
		},
		{ // Test 2: Compact nil renders null.
			In:      nil,
			Pretty:  false,
			WantOut: "null",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Marshal(test.In, test.Pretty)
			if err != nil {
				t.Fatalf("Marshal failed: %v", err)
			}
			if diff := cmp.Diff(test.WantOut, string(got)); diff != "" {
				t.Errorf("mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
