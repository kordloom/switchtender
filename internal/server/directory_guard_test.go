package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/user"
)

// TestSAMLACSRefusesAnUnstartedSignIn drives the real assertion endpoint so the guard that ties a
// response to a sign-in this browser began is executed, not merely present.
//
// Without it the endpoint accepts an assertion nobody here asked for, which is the SAML form of the
// same attack the OIDC state check refuses: an assertion obtained elsewhere is posted to a victim's
// browser session. The check returns before the assertion is parsed, so this needs no identity
// provider.
func TestSAMLACSRefusesAnUnstartedSignIn(t *testing.T) {
	t.Parallel()
	s := &SAMLAuth{
		users:       user.NewMemStore(),
		defaultRole: user.RoleViewer,
		signKey:     []byte("test-saml-key"),
		log:         zap.NewNop(),
	}

	tests := []struct {
		Name   string
		Cookie *http.Cookie
	}{
		{Name: "no request-id cookie at all", Cookie: nil},
		{Name: "cookie is not signed", Cookie: &http.Cookie{Name: samlCookie, Value: "forged.sig"}},
		{
			Name:   "cookie carries no signature",
			Cookie: &http.Cookie{Name: samlCookie, Value: "notevenseparated"},
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodPost, "/auth/saml/acs",
				strings.NewReader("SAMLResponse=whatever"))
			req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
			if test.Cookie != nil {
				req.AddCookie(test.Cookie)
			}
			rec := httptest.NewRecorder()
			s.acs(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want a redirect back to sign-in", rec.Code)
			}
			// The specific message matters, not just that it redirected. Removing the guard still
			// redirects, because the unparseable assertion is refused a step later, so asserting only
			// "went back to sign-in" would pass with the guard deleted. This names the guard that
			// fired: the handshake check reports expiry, the assertion parser reports failure.
			loc := rec.Header().Get("Location")
			if !strings.Contains(loc, "/ui/login") {
				t.Errorf("redirect = %q, want the sign-in page", loc)
			}
			if !strings.Contains(loc, "expired") {
				t.Errorf("redirect = %q, want the request-id check to refuse it before the assertion "+
					"is parsed", loc)
			}
			// A refused assertion must not create an account or a session.
			accounts, err := s.users.List(req.Context())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(accounts) != 0 {
				t.Errorf("a refused assertion provisioned %d account(s)", len(accounts))
			}
		})
	}
}

// TestDirectoryProvisioningDrivesRole checks what happens to an account that already exists when a
// directory sign-in reports a different role.
//
// The demotion half is the one that matters and the one nothing executed. When the directory drives
// roles, a person removed from an admin group must lose admin here on their next sign-in; if that
// branch stops firing, revoking access in the directory looks like it worked while the account keeps
// every permission it had. When the directory does not drive roles, a local role an operator set by
// hand must survive, or a sign-in would quietly undo local administration.
func TestDirectoryProvisioningDrivesRole(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Seed      user.Role
		Reported  user.Role
		DriveRole bool
		Want      user.Role
	}{
		{
			Name: "directory demotes a former admin", Seed: user.RoleAdmin,
			Reported: user.RoleViewer, DriveRole: true, Want: user.RoleViewer,
		},
		{
			Name: "directory promotes an operator", Seed: user.RoleViewer,
			Reported: user.RoleAdmin, DriveRole: true, Want: user.RoleAdmin,
		},
		{
			Name: "no role mapping leaves the local role alone", Seed: user.RoleAdmin,
			Reported: user.RoleViewer, DriveRole: false, Want: user.RoleAdmin,
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			store := user.NewMemStore()
			log := zap.NewNop()

			// The account already exists at its seeded role, as it would after an earlier sign-in.
			if _, err := provisionFromDirectory(ctx, store, log, "dana", test.Seed, true, "test"); err != nil {
				t.Fatalf("seed provision error = %v", err)
			}
			got, err := provisionFromDirectory(ctx, store, log, "dana", test.Reported,
				test.DriveRole, "test")
			if err != nil {
				t.Fatalf("provisionFromDirectory() error = %v", err)
			}
			if got.Role != test.Want {
				t.Errorf("role = %q, want %q", got.Role, test.Want)
			}
			// The change must be persisted, not only returned, or the next request reads the old role.
			stored, err := store.FindByUsername(ctx, "dana")
			if err != nil {
				t.Fatalf("FindByUsername() error = %v", err)
			}
			if stored.Role != test.Want {
				t.Errorf("stored role = %q, want %q; the change did not reach the store",
					stored.Role, test.Want)
			}
		})
	}
}
