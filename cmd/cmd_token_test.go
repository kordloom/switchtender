package cmd

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

// TestTokenNewRefusesANegativeTTL proves a negative lifetime is rejected instead of quietly minting
// a token that never expires.
//
// Only a positive --ttl ever set an expiry, so a mistyped duration produced the opposite of the
// request: an operator who meant a short-lived credential got an immortal one, with nothing in the
// output to say so. The refusal happens before the store is touched, so no token is minted and no
// audit entry is written for a command that did nothing.
func TestTokenNewRefusesANegativeTTL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		TTL       time.Duration
		Want      error
		WantMints bool
	}{{ // Test 0: A negative lifetime is invalid usage.
		Name: "negative", TTL: -time.Hour, Want: ErrUsage, WantMints: false,
	}, { // Test 1: Zero still means the token never expires.
		Name: "zero", TTL: 0, Want: nil, WantMints: true,
	}, { // Test 2: A positive lifetime is minted with an expiry.
		Name: "positive", TTL: time.Hour, Want: nil, WantMints: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			// Not parallel: the token commands read package-level flag variables.
			dbPath := filepath.Join(t.TempDir(), "switchtender.db")
			tokenDB, tokenName, tokenUser, tokenTTL = dbPath, "ci", "", test.TTL
			t.Cleanup(func() { tokenDB, tokenName, tokenUser, tokenTTL = "", "", "", 0 })

			err := runTokenNew(testCommand(), nil)
			if !errors.Is(err, test.Want) {
				t.Fatalf("runTokenNew() error = %v, want %v", err, test.Want)
			}

			bundle, err := openBundle(dbPath)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			defer func() { _ = bundle.Close() }()
			list, err := bundle.Tokens().List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if test.WantMints != (len(list) > 0) {
				t.Fatalf("minted %d tokens, want minted = %v", len(list), test.WantMints)
			}
			if !test.WantMints {
				return
			}
			// A positive lifetime expires; zero never does.
			if (test.TTL > 0) != (list[0].ExpiresAt != nil) {
				t.Errorf("token ExpiresAt = %v for --ttl %s", list[0].ExpiresAt, test.TTL)
			}
		})
	}
}
