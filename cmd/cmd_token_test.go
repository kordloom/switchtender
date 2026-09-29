package cmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/user"
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
			dbPath := tempDB(t)
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

// stdoutOf runs fn with standard output captured and returns what it printed.
func stdoutOf(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	return captured(t, &os.Stdout, fn)
}

// stderrOf runs fn with standard error captured and returns what it printed.
func stderrOf(t *testing.T, fn func() error) (string, error) {
	t.Helper()
	return captured(t, &os.Stderr, fn)
}

// captured runs fn with the file stream points at swapped for a pipe, and returns what fn wrote.
// The pipe is drained while fn runs, so output larger than the pipe buffer cannot block it.
func captured(t *testing.T, stream **os.File, fn func() error) (string, error) {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	done := make(chan []byte)
	go func() {
		out, _ := io.ReadAll(r)
		done <- out
	}()
	orig := *stream
	*stream = w
	runErr := fn()
	*stream = orig
	_ = w.Close()
	return string(<-done), runErr
}

// TestTokenNewPrintsTheRoleTheTokenActsWith pins the role token new reports. An agent token bound to
// an admin account runs as operator, but the output printed the account's admin role, telling the
// operator the agent could approve, mint tokens, and manage users, which it cannot.
func TestTokenNewPrintsTheRoleTheTokenActsWith(t *testing.T) {
	tests := []struct {
		Agent    bool
		Role     user.Role
		WantRole string
	}{{ // Test 0: An agent bound to an admin runs as operator.
		Agent: true, Role: user.RoleAdmin, WantRole: "operator",
	}, { // Test 1: A person's token bound to an admin is admin.
		Agent: false, Role: user.RoleAdmin, WantRole: "admin",
	}, { // Test 2: An agent bound to a viewer stays a viewer.
		Agent: true, Role: user.RoleViewer, WantRole: "viewer",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the token commands read package-level flag variables and this captures
			// standard output.
			dbPath := tempDB(t)
			bundle, err := openBundle(dbPath)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			account, err := user.New("owner", "correct-horse-battery", test.Role)
			if err != nil {
				t.Fatalf("user.New() error = %v", err)
			}
			if err := bundle.Users().Save(context.Background(), account); err != nil {
				t.Fatalf("save user: %v", err)
			}
			_ = bundle.Close()
			tokenDB, tokenName, tokenUser, tokenAgent = dbPath, "bot", "owner", test.Agent
			t.Cleanup(func() { tokenDB, tokenName, tokenUser, tokenAgent = "", "", "", false })

			out, err := stdoutOf(t, func() error { return runTokenNew(testCommand(), nil) })
			if err != nil {
				t.Fatalf("runTokenNew() error = %v", err)
			}
			var got map[string]string
			if err := json.Unmarshal([]byte(out), &got); err != nil {
				t.Fatalf("output %q is not JSON: %v", out, err)
			}
			if got["role"] != test.WantRole {
				t.Errorf("printed role = %q, want %q", got["role"], test.WantRole)
			}
		})
	}
}

// TestAnEmptyListPrintsAnEmptyArray pins the output of token list and user list on a database with
// no rows. Both printed null, which a script parsing the output as an array chokes on.
func TestAnEmptyListPrintsAnEmptyArray(t *testing.T) {
	tests := []struct {
		Name string
		Run  func() error
	}{{ // Test 0: token list.
		Name: "token list", Run: func() error { return runTokenList(testCommand(), nil) },
	}, { // Test 1: user list.
		Name: "user list", Run: func() error { return runUserList(testCommand(), nil) },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: the commands read package-level flag variables and this captures
			// standard output.
			dbPath := tempDB(t)
			tokenDB, userDB = dbPath, dbPath
			t.Cleanup(func() { tokenDB, userDB = "", "" })
			out, err := stdoutOf(t, test.Run)
			if err != nil {
				t.Fatalf("%s error = %v", test.Name, err)
			}
			if got := strings.TrimSpace(out); got != "[]" {
				t.Errorf("%s printed %q on an empty database, want []", test.Name, got)
			}
		})
	}
}
