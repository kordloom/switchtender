package project

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestRedactRepoURL covers stripping embedded credentials from repository URLs before they reach
// error text: passwords, token usernames, credential-free URLs, the scp-like shorthand, and an
// unparseable URL.
func TestRedactRepoURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In   string
		Want string
	}{{ // Test 0: Password userinfo is removed.
		In: "https://oauth2:tok3n@github.com/org/repo.git", Want: "https://github.com/org/repo.git",
	}, { // Test 1: Token-as-username userinfo is removed.
		In: "https://ghp_token@github.com/org/repo.git", Want: "https://github.com/org/repo.git",
	}, { // Test 2: A URL without credentials passes through unchanged.
		In: "https://github.com/org/repo.git", Want: "https://github.com/org/repo.git",
	}, { // Test 3: The scp-like shorthand has no password slot and passes through.
		In: "git@github.com:org/repo.git", Want: "git@github.com:org/repo.git",
	}, { // Test 4: An unparseable URL is replaced entirely.
		In: "https://bad\x7furl@host/repo.git", Want: "<invalid url>",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if diff := cmp.Diff(test.Want, redactRepoURL(test.In)); diff != "" {
				t.Errorf("redactRepoURL(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}
