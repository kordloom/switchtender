package util_test

import (
	"testing"

	"github.com/kordloom/switchtender/internal/util"
)

func TestFirstNonEmpty(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   []string
		Want string
	}{
		{In: nil, Want: ""},                    // Test 0: Nothing given.
		{In: []string{"", "", "c"}, Want: "c"}, // Test 1: Skips empties.
		{In: []string{"a", "b"}, Want: "a"},    // Test 2: First wins.
		{In: []string{"", ""}, Want: ""},       // Test 3: All empty.
	}
	for i, test := range tests {
		if got := util.FirstNonEmpty(test.In...); got != test.Want {
			t.Errorf("test %d: FirstNonEmpty(%v) = %q, want %q", i, test.In, got, test.Want)
		}
	}
}

func TestClip(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In    string
		Limit int
		Want  string
	}{
		{In: "short", Limit: 10, Want: "short"},      // Test 0: Under the limit passes through.
		{In: "abcdefgh", Limit: 5, Want: "abcde..."}, // Test 1: Byte cut plus ellipsis.
		{In: "héllo wörld", Limit: 2, Want: "h..."},  // Test 2: A cut landing inside a rune walks back.
	}
	for i, test := range tests {
		if got := util.Clip(test.In, test.Limit); got != test.Want {
			t.Errorf("test %d: Clip(%q, %d) = %q, want %q", i, test.In, test.Limit, got, test.Want)
		}
	}
}
