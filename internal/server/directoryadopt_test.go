package server

import (
	"context"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/user"
)

// TestDirectoryWillNotTakeOverAnotherSourcesAccount covers an account takeover that needed only a
// misconfigured username claim.
//
// Provisioning matched on username alone, so an identity provider asserting the name of a local
// administrator was handed that administrator's account and role, and with group-driven roles on it
// could raise its own role as well. Safe on the defaults, where the username is a subject or a
// directory search result. Account takeover the moment an operator points --jwt-username-claim or
// --saml-username-attr at an email attribute against an issuer that lets a user assert their own
// address. OIDC already refuses the equivalent when the provider does not vouch for the address;
// JWT, LDAP, and SAML had no such check.
func TestDirectoryWillNotTakeOverAnotherSourcesAccount(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	t.Run("a local admin is not adoptable", func(t *testing.T) {
		t.Parallel()
		users := user.NewMemStore()
		admin, err := user.New("root", "a-real-password", user.RoleAdmin)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		admin.Source = "local"
		if err := users.Save(ctx, admin); err != nil {
			t.Fatalf("Save: %v", err)
		}

		_, err = provisionFromDirectory(ctx, users, zap.NewNop(), "root", user.RoleViewer, true, "jwt")
		if err == nil {
			t.Fatal("a directory identity signed in as the local administrator")
		}
		if !strings.Contains(err.Error(), "belongs to") {
			t.Errorf("error = %v, want it to name the owning source", err)
		}
		// And the refusal did not quietly change the account on the way out.
		got, err := users.FindByUsername(ctx, "root")
		if err != nil {
			t.Fatalf("FindByUsername: %v", err)
		}
		if got.Role != user.RoleAdmin || got.Source != "local" {
			t.Errorf("the account was modified: role=%s source=%s", got.Role, got.Source)
		}
	})

	t.Run("its own account is adoptable", func(t *testing.T) {
		t.Parallel()
		users := user.NewMemStore()
		// First sign-in provisions.
		first, err := provisionFromDirectory(ctx, users, zap.NewNop(), "jane", user.RoleOperator,
			true, "ldap")
		if err != nil {
			t.Fatalf("first sign-in: %v", err)
		}
		if first.Source != "ldap" {
			t.Errorf("source = %q, want the directory that made it", first.Source)
		}
		// Second sign-in reuses it, and group-driven roles still apply.
		second, err := provisionFromDirectory(ctx, users, zap.NewNop(), "jane", user.RoleAdmin,
			true, "ldap")
		if err != nil {
			t.Fatalf("second sign-in: %v", err)
		}
		if second.ID != first.ID {
			t.Errorf("a second sign-in made a new account: %s then %s", first.ID, second.ID)
		}
		if second.Role != user.RoleAdmin {
			t.Errorf("role = %s, want the directory's groups to drive it", second.Role)
		}
	})

	t.Run("an account predating the field is still adopted", func(t *testing.T) {
		t.Parallel()
		users := user.NewMemStore()
		legacy, err := user.New("olduser", "pw", user.RoleOperator)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		legacy.Source = "" // provisioned before the field existed
		if err := users.Save(ctx, legacy); err != nil {
			t.Fatalf("Save: %v", err)
		}
		// Refusing these would lock out every directory user on upgrade, so it is allowed and logged.
		if _, err := provisionFromDirectory(ctx, users, zap.NewNop(), "olduser",
			user.RoleOperator, false, "ldap"); err != nil {
			t.Errorf("an account with no recorded source was refused: %v", err)
		}
	})

	t.Run("a different directory is refused", func(t *testing.T) {
		t.Parallel()
		users := user.NewMemStore()
		if _, err := provisionFromDirectory(ctx, users, zap.NewNop(), "sam", user.RoleOperator,
			false, "ldap"); err != nil {
			t.Fatalf("provision: %v", err)
		}
		if _, err := provisionFromDirectory(ctx, users, zap.NewNop(), "sam", user.RoleAdmin,
			true, "saml"); err == nil {
			t.Error("a SAML identity took over an account LDAP provisioned")
		}
	})
}
