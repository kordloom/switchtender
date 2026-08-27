package util

import (
	"fmt"
	"strings"
	"testing"
)

// TestRedactAssignmentsNested pins that a secret is still masked when it is joined onto another
// assignment's value with no separating space, the case the value scan exists for, and that ordinary
// space and newline separated assignments are unchanged.
func TestRedactAssignmentsNested(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   string
		Want string
	}{{ // Test 0: A secret joined onto a non-secret value with no space is found.
		Name: "joined no space",
		In:   "cmd=psql;password=hunter2",
		Want: "cmd=psql;password=X",
	}, { // Test 1: The motivating case, space separated, is found.
		Name: "space separated",
		In:   "deploy_cmd=psql password=hunter2",
		Want: "deploy_cmd=psql password=X",
	}, { // Test 2: A non-secret assignment with no nested secret is untouched.
		Name: "plain non-secret",
		In:   "host=web port=8080",
		Want: "host=web port=8080",
	}, { // Test 3: A secret two levels deep with no spaces is still found.
		Name: "double nested",
		In:   "a=b=password=hunter2",
		Want: "a=b=password=X",
	}, { // Test 4: A yaml secret nested behind a colon, whose separator is not '='.
		Name: "yaml nested colon",
		In:   "note: harmless then ansible_ssh_pass: hunter2",
		Want: "note: harmless then ansible_ssh_pass: X",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, _ := RedactAssignments(test.In, "X")
			if got != test.Want {
				t.Errorf("RedactAssignments(%q) = %q, want %q", test.In, got, test.Want)
			}
			if strings.Contains(got, "hunter2") {
				t.Errorf("secret survived redaction in %q", got)
			}
		})
	}
}

// TestRedactAssignmentsStaysLinear guards the digest hot path against the quadratic scan that a
// crafted body could weaponize. A megabyte run of joined assignments once pinned a core for minutes;
// the redaction must now be linear. The test does not assert a wall-clock time, which would be flaky
// under load: it feeds an input large enough that a quadratic scan cannot finish inside the test
// timeout, so a regression to quadratic fails by timing out while the linear scan returns in
// milliseconds. The spread between the two is minutes versus milliseconds, so there is no middle
// ground for load to push it across.
func TestRedactAssignmentsStaysLinear(t *testing.T) {
	t.Parallel()
	// Both separators can chain, so both are guarded: the equals form the ini pattern reads and the
	// colon form the yaml pattern reads, each the shape that maximized the old rewind's rescanning.
	for _, in := range []string{strings.Repeat("a=", 500000), strings.Repeat("a:", 500000)} {
		got, _ := RedactAssignments(in, "X")
		// No secret keys, so nothing is masked and the text returns unchanged. The point is that it
		// returns at all, in linear time.
		if got != in {
			t.Errorf("a chain of non-secret assignments was altered: len(got)=%d, len(in)=%d",
				len(got), len(in))
		}
	}
}
