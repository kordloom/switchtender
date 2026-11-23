package inventory

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestMergeListWithASharedKeyNeedsAnsible pins that the native engine does not pick a winner where
// PyYAML releases disagree: a merge key listing maps that set the same key goes to the user's own
// Ansible, with the key named, while a list of disjoint maps still resolves natively.
func TestMergeListWithASharedKeyNeedsAnsible(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Doc     string
		Want    error
		WantKey string
	}{{ // Test 0: Both maps set size.
		Doc: "extra: &extra\n  size: large\nbase: &base\n  size: small\n  region: x\n" +
			"all:\n  hosts:\n    w1:\n      <<: [*extra, *base]\n",
		Want: ErrNeedsAnsible, WantKey: "size",
	}, { // Test 1: Disjoint maps resolve natively.
		Doc: "extra: &extra\n  backup: true\nbase: &base\n  size: small\n" +
			"all:\n  hosts:\n    w1:\n      <<: [*extra, *base]\n",
	}, { // Test 2: 1 in one map and 0x1 in the other are one key once constructed.
		Doc:  "a: &a\n  1: one\nb: &b\n  0x1: hex\nall:\n  hosts:\n    w1:\n      <<: [*a, *b]\n",
		Want: ErrNeedsAnsible, WantKey: "0x1",
	}, { // Test 3: A shared key inside a merged map defers too.
		Doc: "x: &x\n  k: 1\ny: &y\n  k: 2\ninner: &inner\n  <<: [*x, *y]\n" +
			"all:\n  hosts:\n    w1:\n      <<: *inner\n",
		Want: ErrNeedsAnsible, WantKey: "k",
	}, { // Test 4: A key the host sets itself over a shared merged key still defers.
		Doc: "extra: &extra\n  size: large\nbase: &base\n  size: small\n" +
			"all:\n  hosts:\n    w1:\n      <<: [*extra, *base]\n      size: medium\n",
		Want: ErrNeedsAnsible, WantKey: "size",
	}, { // Test 5: One merged map and the host's own key is the ordinary case.
		Doc: "base: &base\n  size: small\nall:\n  hosts:\n    w1:\n      <<: *base\n      size: medium\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, err := nativeData(test.Doc)
			if !errors.Is(err, test.Want) {
				t.Fatalf("nativeData() error = %v, want %v", err, test.Want)
			}
			if test.WantKey == "" {
				return
			}
			reason, _ := NeedsAnsibleReason(err)
			if !strings.Contains(reason, "both set "+test.WantKey+",") ||
				!strings.Contains(reason, "Write the key once, on the host or in one map") {
				t.Errorf("reason = %q, want it to name %s and the fix", reason, test.WantKey)
			}
		})
	}
}

// TestMergeListWithASharedKeyRefusesAConstructedInput pins that a constructed inventory's input of
// that shape is refused, since an input is never handed to Ansible unread, and the refusal says how
// to write it instead.
func TestMergeListWithASharedKeyRefusesAConstructedInput(t *testing.T) {
	t.Parallel()
	doc := "extra: &extra\n  size: large\nbase: &base\n  size: small\n" +
		"all:\n  hosts:\n    w1:\n      <<: [*extra, *base]\n"
	_, err := ConstructedInput(doc)
	if !errors.Is(err, ErrConstructedInput) || errors.Is(err, ErrNeedsAnsible) {
		t.Fatalf("ConstructedInput() error = %v, want ErrConstructedInput and not ErrNeedsAnsible", err)
	}
	if !strings.Contains(err.Error(), "write the key once, on the host or in one map") &&
		!strings.Contains(err.Error(), "Write the key once, on the host or in one map") {
		t.Errorf("refusal %q does not say how to write it", err)
	}
}
