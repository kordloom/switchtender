package run

import (
	"errors"
	"fmt"
	"testing"
)

//nolint:funlen // Test function.
func TestCheckLimitFile(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want error
		In   string
	}{{ // Test 0: No limit.
		In: "", Want: nil,
	}, { // Test 1: One host.
		In: "web01", Want: nil,
	}, { // Test 2: A list of hosts.
		In: "web01,web02", Want: nil,
	}, { // Test 3: A group pattern with an exclusion and an intersection.
		In: "web:!canary:&east", Want: nil,
	}, { // Test 4: A wildcard and a range.
		In: "web*.example.com:db[0:3]", Want: nil,
	}, { // Test 5: A regular expression pattern.
		In: "~web[0-9]+", Want: nil,
	}, { // Test 6: The whole limit is a file.
		In: "@/etc/shells", Want: ErrLimitFile,
	}, { // Test 7: A file after a comma.
		In: "nosuchhost,@/etc/shells", Want: ErrLimitFile,
	}, { // Test 8: A file after a colon.
		In: "web:@/etc/shells", Want: ErrLimitFile,
	}, { // Test 9: A file after a comma, the path absent.
		In: "web01,@/nonexistent/x", Want: ErrLimitFile,
	}, { // Test 10: A file after a space, which Ansible cuts on when there is no comma.
		In: "web01 @/nonexistent/x", Want: ErrLimitFile,
	}, { // Test 11: A file after a closing bracket.
		In: "web01]@/nonexistent/x", Want: ErrLimitFile,
	}, { // Test 12: A file after an opening bracket.
		In: "web01[@/nonexistent/x", Want: ErrLimitFile,
	}, { // Test 13: A file behind U+001C, which Python strips as whitespace and Go does not.
		In: "\x1c@/nonexistent/x", Want: ErrLimitFile,
	}, { // Test 14: A file behind leading spaces.
		In: "  @/etc/shells", Want: ErrLimitFile,
	}, { // Test 15: A file behind an exclusion mark.
		In: "web:!@/etc/shells", Want: ErrLimitFile,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := CheckLimitFile(test.In); !errors.Is(err, test.Want) {
				t.Errorf("CheckLimitFile(%q) = %v, want %v", test.In, err, test.Want)
			}
		})
	}
}
