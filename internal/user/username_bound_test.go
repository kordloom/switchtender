package user

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestUsernamesAreBounded pins the bound on a username, which is stored under a unique index and is
// the actor every run and audit entry of the account is indexed by. Every account is built by New,
// whether an administrator created it or a directory provisioned it from a claim, so the bound
// holds on every path that makes one.
func TestUsernamesAreBounded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Username string
		Want     error
	}{{ // Test 0: An email address is well inside the bound.
		Username: "ops-lead@example.com",
	}, { // Test 1: A name at the bound is accepted.
		Username: strings.Repeat("u", MaxUsernameBytes),
	}, { // Test 2: A name one byte past the bound is refused.
		Username: strings.Repeat("u", MaxUsernameBytes+1), Want: ErrUsernameTooLong,
	}, { // Test 3: A claim far past the bound is refused.
		Username: strings.Repeat("u", 4000), Want: ErrUsernameTooLong,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if err := CheckUsername(test.Username); !errors.Is(err, test.Want) {
				t.Errorf("CheckUsername() error = %v, want %v", err, test.Want)
			}
			u, err := New(test.Username, "a-long-enough-password", RoleViewer)
			if !errors.Is(err, test.Want) {
				t.Errorf("New() error = %v, want %v", err, test.Want)
			}
			if test.Want == nil && (u == nil || u.Username != test.Username) {
				t.Errorf("New() = %v, want an account named as asked", u)
			}
		})
	}
}
