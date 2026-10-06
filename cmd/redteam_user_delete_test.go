package cmd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/user"
)

// TestRedTeamCLIUserDeleteRemovesForgeLinks proves the user delete command removes the account's
// forge links, as the API's delete does. The command deletes the account alone, so its forge
// account stays linked to an account that is gone: it can never be linked to anybody again, and
// the chain never records the link's end. It does not run in parallel because the command reads
// package-level flags.
func TestRedTeamCLIUserDeleteRemovesForgeLinks(t *testing.T) {
	tests := []struct {
		Store string
	}{{ // Test 0: SQLite.
		Store: "sqlite",
	}, { // Test 1: PostgreSQL.
		Store: "postgres",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			db := tempDB(t)
			if test.Store == "postgres" {
				dsn := os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN")
				if dsn == "" {
					if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
						t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and " +
							"SWITCHTENDER_TEST_POSTGRES_DSN is not")
					}
					t.Skip("SWITCHTENDER_TEST_POSTGRES_DSN not set")
				}
				// A new PostgreSQL install needs a Team license, which is process-global.
				license.Set(&license.License{Claims: license.Claims{
					V: 1, ID: "lic_redteam_cli", Org: "test", Tier: license.TierTeam,
					Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
				}})
				t.Cleanup(func() { license.Set(nil) })
				db = ownDatabase(t, dsn)
			}
			setString(t, &userDB, db)
			setString(t, &userRole, string(user.RoleAdmin))
			ctx := context.Background()
			bundle, err := openBundle(db)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			var ids []string
			for _, name := range []string{"admin-keep", "admin-gone"} {
				u, err := user.New(name, "a-long-enough-password", user.RoleAdmin)
				if err != nil {
					t.Fatalf("user.New() error = %v", err)
				}
				if err := bundle.Users().Save(ctx, u); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				ids = append(ids, u.ID)
			}
			if err := bundle.ForgeLinks().Create(ctx, &forgelink.Link{ID: "fl_cligone",
				UserID: ids[1], Provider: "github", APIURL: "https://api.github.com",
				ForgeUserID: 4001, CreatedAt: time.Now().UTC()}); err != nil {
				t.Fatalf("Create() error = %v", err)
			}
			if err := bundle.Close(); err != nil {
				t.Fatalf("close: %v", err)
			}
			if err := runUserDelete(testCommand(), []string{ids[1]}); err != nil {
				t.Fatalf("runUserDelete() error = %v", err)
			}
			after, err := openBundle(db)
			if err != nil {
				t.Fatalf("openBundle() error = %v", err)
			}
			defer func() { _ = after.Close() }()
			_, err = after.ForgeLinks().Lookup(ctx, "github", "https://api.github.com", 4001)
			if !errors.Is(err, forgelink.ErrNotFound) {
				t.Errorf("the deleted account's link still holds forge account 4001: %v", err)
			}
		})
	}
}
