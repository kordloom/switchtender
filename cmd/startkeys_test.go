package cmd

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// TestStartupRefusesPublishedAndMalformedKeys pins the two key checks serve and the worker make
// before anything is sealed or signed.
//
// The documentation printed change-me and change-me-too as the encryption pair, and a server started
// with them sealed every credential under values anyone can look up. A malformed audit key was only
// a warning, though the configuration reference says it stops startup, and the server then ran with
// no install binding on its chain. It sets environment variables, so it cannot run in parallel.
func TestStartupRefusesPublishedAndMalformedKeys(t *testing.T) {
	tests := []struct {
		Key, Salt, AuditKey string
		Want                error
		WantMentions        string
	}{{ // Test 0: The published example key.
		Key: "change-me", Salt: "a-real-salt-value", Want: errPublishedKey,
		WantMentions: "SWITCHTENDER_ENCRYPTION_KEY",
	}, { // Test 1: The published example salt.
		Key: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Salt: "change-me-too", Want: errPublishedKey,
		WantMentions: "SWITCHTENDER_ENCRYPTION_SALT",
	}, { // Test 2: An audit key too short to be a seed.
		AuditKey: "abcd", Want: errBadAuditKey, WantMentions: "32 bytes",
	}, { // Test 3: An audit key that is not hex.
		AuditKey: "not-hex", Want: errBadAuditKey, WantMentions: "openssl rand -hex 32",
	}, { // Test 4: Real values start.
		Key: "0f1e2d3c4b5a69788796a5b4c3d2e1f0", Salt: "a-real-salt-value",
		AuditKey: strings.Repeat("ab", 32),
	}, { // Test 5: Nothing set starts, as it always has.
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", test.Key)
			t.Setenv("SWITCHTENDER_ENCRYPTION_SALT", test.Salt)
			t.Setenv("SWITCHTENDER_AUDIT_KEY", test.AuditKey)
			err := refusePublishedKey()
			if err == nil {
				err = refuseBadAuditKey()
			}
			if !errors.Is(err, test.Want) {
				t.Fatalf("error = %v, want %v", err, test.Want)
			}
			if err != nil && !strings.Contains(err.Error(), test.WantMentions) {
				t.Errorf("error %q does not mention %q", err, test.WantMentions)
			}
		})
	}
}
