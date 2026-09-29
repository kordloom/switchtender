package cmd

import (
	"errors"
	"fmt"
	"testing"
)

// TestTokenCountGuard covers the pre-bind decision that refuses to serve an unauthenticated public
// API. The count-error case is the fail-closed fix: an unreadable token store must refuse to start,
// where the former guard skipped the check and fell through to bind on the guess that no tokens
// existed.
func TestTokenCountGuard(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name         string
		Count        int
		CountErr     error
		ReadOnly     bool
		ExternalAuth bool
		Loopback     bool
		Addr         string
		WantWarn     bool
		WantErr      bool
	}{
		{ // Test 0: Tokens exist, so serving is allowed with no warning.
			Name: "tokens exist", Count: 3, Addr: "0.0.0.0:8080",
		},
		{ // Test 1: A count error fails closed and refuses to start, even on a public bind.
			Name: "count error public", Count: 0, CountErr: errors.New("db locked"),
			Addr: "0.0.0.0:8080", WantErr: true,
		},
		{ // Test 2: A count error fails closed even for a loopback bind.
			Name: "count error loopback", Count: 0, CountErr: errors.New("db locked"),
			Loopback: true, Addr: "127.0.0.1:8080", WantErr: true,
		},
		{ // Test 3: An empty readable store on a public bind with no other auth is refused.
			Name: "empty public no auth", Count: 0, Addr: "0.0.0.0:8080", WantErr: true,
		},
		{ // Test 4: An empty store on loopback is allowed with a warning.
			Name: "empty loopback", Count: 0, Loopback: true, Addr: "127.0.0.1:8080", WantWarn: true,
		},
		{ // Test 5: An empty store with external auth on a public bind is allowed with a warning.
			Name: "empty sso public", Count: 0, ExternalAuth: true, Addr: "0.0.0.0:8080", WantWarn: true,
		},
		{ // Test 6: An empty store in read-only mode on a public bind is allowed with a warning.
			Name: "empty read-only public", Count: 0, ReadOnly: true, Addr: "0.0.0.0:8080", WantWarn: true,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			warn, err := tokenCountGuard(test.Count, test.CountErr, test.ReadOnly,
				test.ExternalAuth, test.Loopback, test.Addr)
			if (err != nil) != test.WantErr {
				t.Errorf("%s: err = %v, want error: %v", test.Name, err, test.WantErr)
			}
			if warn != test.WantWarn {
				t.Errorf("%s: warn = %v, want %v", test.Name, warn, test.WantWarn)
			}
		})
	}
}
