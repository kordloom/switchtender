package cmd

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"
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
		{ // Test 5: An empty store with external auth on a public bind serves with no warning,
			// because the API is not unauthenticated: an install configured for single sign-on
			// enforces from the first request. This case used to warn, which was the honest
			// reading of the old behavior, where the gate derived enforcement from the empty
			// token and account tables and served the whole API to anonymous callers as admin
			// until somebody happened to sign in.
			Name: "empty sso public", Count: 0, ExternalAuth: true, Addr: "0.0.0.0:8080",
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

// TestIdentityDir proves the producer signing identity directory is derived safely for both database
// kinds. A SQLite file keeps its identity beside it. A postgres DSN has no filesystem home, so it
// falls back to a stable per-user directory rather than filepath.Dir of the DSN, which would be a
// cwd-relative junk directory whose name embeds the DSN's user:password@host.
func TestIdentityDir(t *testing.T) {
	t.Parallel()
	// A SQLite file: identity lives in the directory beside it.
	if got := identityDir("/var/lib/switchtender/demo.db"); got != "/var/lib/switchtender" {
		t.Errorf("identityDir(sqlite) = %q, want /var/lib/switchtender", got)
	}
	// A postgres DSN must not become a path and must not carry the credentials into a directory name.
	const dsn = "postgres://svc:hunter2@db.internal:5432/prod?sslmode=disable"
	got := identityDir(dsn)
	if strings.Contains(got, "hunter2") || strings.Contains(got, "db.internal") ||
		strings.Contains(got, "postgres:") {
		t.Errorf("identityDir(dsn) = %q, leaks DSN content into the path", got)
	}
	if !strings.HasSuffix(got, filepath.Join("switchtender", "identity")) {
		t.Errorf("identityDir(dsn) = %q, want a stable switchtender/identity directory", got)
	}
	// postgresql:// is the same case.
	if g2 := identityDir("postgresql://u:p@h/db"); g2 != got {
		t.Errorf("identityDir(postgresql) = %q, want the same stable dir as postgres:// %q", g2, got)
	}
}
