package server

import (
	"context"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/user"
)

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
			if _, err := ProvisionFromDirectory(ctx, store, log, "dana", test.Seed, true, "test"); err != nil {
				t.Fatalf("seed provision error = %v", err)
			}
			got, err := ProvisionFromDirectory(ctx, store, log, "dana", test.Reported,
				test.DriveRole, "test")
			if err != nil {
				t.Fatalf("ProvisionFromDirectory() error = %v", err)
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
