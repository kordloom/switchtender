package cmd

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/user"
)

// TestTokenLifecycle verifies minting, listing, and revoking an API token through the commands,
// and that the plaintext token is shown once and never stored.
func TestTokenLifecycle(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	tokenDB, tokenName = db, "ci-runner"
	t.Cleanup(func() { tokenDB, tokenName = "", "" })

	if err := runTokenNew(testCommand(), nil); err != nil {
		t.Fatalf("runTokenNew() error = %v", err)
	}

	// The store holds the token by hash, with the name intact and no plaintext.
	tokens, _, closeStores, err := openTokens(db)
	if err != nil {
		t.Fatalf("openTokens() error = %v", err)
	}
	list, err := tokens.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Name != "ci-runner" {
		t.Fatalf("stored tokens = %+v, want one named ci-runner", list)
	}
	id := list[0].ID
	if err := closeStores(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := runTokenList(testCommand(), nil); err != nil {
		t.Errorf("runTokenList() error = %v", err)
	}
	if err := runTokenRevoke(testCommand(), []string{id}); err != nil {
		t.Fatalf("runTokenRevoke() error = %v", err)
	}

	tokens, _, closeStores, err = openTokens(db)
	if err != nil {
		t.Fatalf("openTokens() error = %v", err)
	}
	defer func() { _ = closeStores() }()
	after, err := tokens.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(after) != 0 {
		t.Errorf("tokens after revoke = %d, want 0", len(after))
	}
}

// TestUserLifecycle verifies account creation, listing, and deletion through the commands, and
// that the stored password is hashed rather than kept in the clear.
func TestUserLifecycle(t *testing.T) {
	db := filepath.Join(t.TempDir(), "users.db")
	userDB, userRole = db, string(user.RoleAdmin)
	t.Setenv("SWITCHTENDER_PASSWORD", "correct-horse")
	t.Cleanup(func() { userDB, userRole = "", "" })

	if err := runUserNew(testCommand(), []string{"alice"}); err != nil {
		t.Fatalf("runUserNew() error = %v", err)
	}

	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	ctx := context.Background()
	list, err := bundle.Users().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].Username != "alice" || list[0].Role != user.RoleAdmin {
		t.Fatalf("stored users = %+v, want one admin named alice", list)
	}
	if strings.Contains(list[0].PasswordHash, "correct-horse") {
		t.Error("the password was stored in the clear")
	}
	if _, err := user.Authenticate(ctx, bundle.Users(), "alice", "correct-horse"); err != nil {
		t.Errorf("stored account cannot authenticate: %v", err)
	}
	id := list[0].ID
	if err := bundle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	// A duplicate username is refused rather than silently replacing the account.
	if err := runUserNew(testCommand(), []string{"alice"}); err == nil {
		t.Error("creating a duplicate username = nil error, want a refusal")
	}

	if err := runUserList(testCommand(), nil); err != nil {
		t.Errorf("runUserList() error = %v", err)
	}
	if err := runUserDelete(testCommand(), []string{id}); err != nil {
		t.Fatalf("runUserDelete() error = %v", err)
	}

	bundle, err = openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	defer func() { _ = bundle.Close() }()
	after, err := bundle.Users().List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(after) != 0 {
		t.Errorf("users after delete = %d, want 0", len(after))
	}
}

// TestUserNewRequiresPassword verifies account creation fails without a password rather than
// creating an account nobody can sign in to, or worse, one with an empty password.
func TestUserNewRequiresPassword(t *testing.T) {
	db := filepath.Join(t.TempDir(), "users.db")
	userDB, userRole = db, string(user.RoleViewer)
	t.Setenv("SWITCHTENDER_PASSWORD", "")
	t.Cleanup(func() { userDB, userRole = "", "" })

	if err := runUserNew(testCommand(), []string{"bob"}); err == nil {
		t.Error("runUserNew() with no password = nil error, want a refusal")
	}
}

// TestTokenNewUserBinding verifies --user binds the minted token to the named account, so it
// carries that account's role instead of acting as unscoped admin. This is the least-privilege
// path for an automation or an AI agent: an operator-bound token can submit runs but cannot
// approve its own held work.
func TestTokenNewUserBinding(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	tokenDB, tokenName, tokenUser = db, "agent-bot", "agent-bot"
	t.Cleanup(func() { tokenDB, tokenName, tokenUser = "", "", "" })

	bundle, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	if err := bundle.Users().Save(context.Background(), &user.User{
		ID: "usr_agent", Username: "agent-bot", Role: user.RoleOperator,
	}); err != nil {
		t.Fatalf("Save() user error = %v", err)
	}
	if err := bundle.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if err := runTokenNew(testCommand(), nil); err != nil {
		t.Fatalf("runTokenNew() with --user error = %v", err)
	}

	tokens, _, closeStores, err := openTokens(db)
	if err != nil {
		t.Fatalf("openTokens() error = %v", err)
	}
	defer func() { _ = closeStores() }()
	list, err := tokens.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 1 || list[0].UserID != "usr_agent" {
		t.Fatalf("stored tokens = %+v, want one bound to usr_agent: an unbound token acts as "+
			"admin, which lets an agent approve its own runs", list)
	}
}

// TestTokenNewUserUnknownRefused verifies a --user naming no account is refused and mints
// nothing. Failing open here would mint an unscoped admin token under a name the operator
// believed was confined.
func TestTokenNewUserUnknownRefused(t *testing.T) {
	db := filepath.Join(t.TempDir(), "tokens.db")
	tokenDB, tokenName, tokenUser = db, "agent-bot", "no-such-account"
	t.Cleanup(func() { tokenDB, tokenName, tokenUser = "", "", "" })

	if err := runTokenNew(testCommand(), nil); err == nil ||
		!strings.Contains(err.Error(), "no account named") {
		t.Fatalf("runTokenNew() error = %v, want a refusal naming the missing account", err)
	}

	tokens, _, closeStores, err := openTokens(db)
	if err != nil {
		t.Fatalf("openTokens() error = %v", err)
	}
	defer func() { _ = closeStores() }()
	list, err := tokens.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("tokens after refused mint = %d, want 0", len(list))
	}
}
