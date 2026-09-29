package server

import (
	"context"
	"errors"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/user"
)

// TestOIDCWillNotTakeOverAnAccountOnAnUnverifiedClaim covers a privilege escalation through a claim
// the identity provider never vouched for. The username came from the email claim, and an existing
// local account with that username was adopted wholesale along with its role. At any provider that
// lets a person set their own email address without proving it, and that is most of them, claiming the
// address of an admin account was enough to sign in as that admin.
//
// A verified claim is the provider asserting the address, which is the trust the deployment is built
// on, so that still signs in to the matching account. An unverified one may create a new account of
// its own, under an identity that cannot collide with a person's username, but may never step into an
// account that already exists.
func TestOIDCWillNotTakeOverAnAccountOnAnUnverifiedClaim(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	admin, err := user.New("owner@example.com", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New: %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("Save user: %v", err)
	}
	o := &OIDCAuth{users: users, defaultRole: user.RoleViewer, log: zap.NewNop()}

	// Test 0: An unverified claim naming the admin's address is refused rather than adopted.
	u, err := o.provision(ctx, "owner@example.com", false)
	if !errors.Is(err, errOIDCUnverified) {
		t.Fatalf("unverified sign-in to an existing account = (%v, %v), want a refusal: an "+
			"unverified email claim inherited an admin account", u, err)
	}

	// Test 1: A verified claim signs in to the same account, which is the deployment's whole point.
	u, err = o.provision(ctx, "owner@example.com", true)
	if err != nil {
		t.Fatalf("verified sign-in = %v, want the existing account", err)
	}
	if u.ID != admin.ID || u.Role != user.RoleAdmin {
		t.Errorf("verified sign-in resolved to %+v, want the stored admin", u)
	}

	// Test 2: An unverified claim for an address nobody holds may still provision, at the default
	// role. Refusing here would lock out every deployment whose provider omits the claim entirely.
	u, err = o.provision(ctx, "newcomer@example.com", false)
	if err != nil {
		t.Fatalf("unverified first sign-in = %v, want a provisioned account", err)
	}
	if u.Role != user.RoleViewer {
		t.Errorf("provisioned role = %s, want the default viewer", u.Role)
	}
}

// TestOIDCIdentityPrefersAVerifiedAddress covers which claim becomes the username, and whether the
// server treats it as vouched for. An address the provider did not mark verified is still usable as a
// name, since a provider that omits the claim is common, but it is not treated as proof.
func TestOIDCIdentityPrefersAVerifiedAddress(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Email        string
		Verified     bool
		Preferred    string
		Subject      string
		WantName     string
		WantVerified bool
	}{
		// Test 0: A verified address is the identity and counts as proof.
		{Email: "casey@example.com", Verified: true, Subject: "sub-1",
			WantName: "casey@example.com", WantVerified: true},
		// Test 1: The same address unverified names the same account but proves nothing, so it can
		// only ever create.
		{Email: "casey@example.com", Subject: "sub-1", WantName: "casey@example.com"},
		// Test 2: With no address, the preferred username stands in, unvouched for.
		{Preferred: "casey", Subject: "sub-1", WantName: "casey"},
		// Test 3: With neither, the subject is the identity. A provider's subject is stable and is
		// not something the person chooses, but it is not an address either.
		{Subject: "sub-1", WantName: "sub-1"},
	}
	for i, tc := range tests {
		name, verified := oidcIdentity(tc.Email, tc.Verified, tc.Preferred, tc.Subject)
		if name != tc.WantName || verified != tc.WantVerified {
			t.Errorf("test %d: oidcIdentity = (%q, %v), want (%q, %v)",
				i, name, verified, tc.WantName, tc.WantVerified)
		}
	}
}
