package idgen_test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/idgen"
)

func TestNew(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Prefix  string
		Bytes   int
		WantLen int
	}{
		{Prefix: "cred_", Bytes: 6, WantLen: 5 + 12}, // Test 0: Six bytes hex to twelve characters.
		{Prefix: "run_", Bytes: 8, WantLen: 4 + 16},  // Test 1: Eight bytes hex to sixteen characters.
	}
	for i, test := range tests {
		t.Run(fmt.Sprintf("test %d", i), func(t *testing.T) {
			t.Parallel()
			got := idgen.New(test.Prefix, test.Bytes)
			if len(got) != test.WantLen || !strings.HasPrefix(got, test.Prefix) {
				t.Errorf("New(%q, %d) = %q, want prefix %q and length %d",
					test.Prefix, test.Bytes, got, test.Prefix, test.WantLen)
			}
			if again := idgen.New(test.Prefix, test.Bytes); again == got {
				t.Errorf("New() returned the same id twice: %q", got)
			}
		})
	}
}
