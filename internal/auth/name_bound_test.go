package auth

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestTokenNamesAreBounded pins the bound on a token's name, which is the actor every run and audit
// entry the token makes is indexed by. A name past the bound once minted a token whose every change
// was refused, because the audit trail could not index its actor, so minting it is refused instead.
func TestTokenNamesAreBounded(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Want error
	}{{ // Test 0: An ordinary label.
		Name: "deploy-bot",
	}, { // Test 1: A name at the bound is minted.
		Name: strings.Repeat("n", MaxNameBytes),
	}, { // Test 2: A name one byte past the bound is refused.
		Name: strings.Repeat("n", MaxNameBytes+1), Want: ErrNameTooLong,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plain, tok, err := New(test.Name)
			if !errors.Is(err, test.Want) {
				t.Fatalf("New() error = %v, want %v", err, test.Want)
			}
			if test.Want == nil && (plain == "" || tok == nil || tok.Name != test.Name) {
				t.Errorf("New() = %q, %v, want a token named as asked", plain, tok)
			}
			if test.Want != nil && (plain != "" || tok != nil) {
				t.Errorf("New() minted %v for a refused name", tok)
			}
		})
	}
}
