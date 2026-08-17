package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/user"
)

// TestAManageGrantCanEditWhatItCanDelete covers a delegation that half worked, in the direction that
// makes no sense.
//
// A manage grant is how an admin hands somebody the care of one object without making them a member of
// the organization that owns it. The route gate honors that: the PUT is admitted on the grant alone. The
// handler then asked whether the caller belongs to the object's organization, on every edit, including a
// rename that moves nothing. So a manage-delegated non-member was refused every change.
//
// Delete asked no such question and succeeded. The delegation therefore let somebody destroy an object
// they were not allowed to rename, which is the wrong way round twice over: the safe operation was
// forbidden and the unrecoverable one allowed.
//
// The membership question belongs to a change of organization, which is the act it exists to bound:
// entering an org gives every member of it access, and leaving takes that away. Both directions are
// still checked. An unchanged organization is not a placement.
func TestAManageGrantCanEditWhatItCanDelete(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	// The world: one organization owning three objects, and a caller who is not a member of it but
	// holds a manage grant on each.
	build := func(t *testing.T) (http.Handler, string, credential.Store, project.Store, inventory.Store) {
		t.Helper()
		users := user.NewMemStore()
		orgs := org.NewMemStore()
		grants := grant.NewMemStore()
		creds := credential.NewMemStore()
		projects := project.NewMemStore()
		inventories := inventory.NewMemStore()
		sealer := credential.NewSealer("pass", "salt")

		if err := orgs.Save(ctx, &org.Org{ID: "org_owner", Name: "owner"}); err != nil {
			t.Fatalf("Save() org error = %v", err)
		}
		sealed, err := sealer.Seal("secret")
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		if err := creds.Save(ctx, &credential.Credential{
			ID: "cred_c", Name: "prod key", Kind: credential.KindSSHKey, Secret: sealed,
			OrgID: "org_owner", CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() credential error = %v", err)
		}
		if err := projects.Save(ctx, &project.Project{
			ID: "proj_p", Name: "prod", RepoURL: "https://example.com/p.git", OrgID: "org_owner",
		}); err != nil {
			t.Fatalf("Save() project error = %v", err)
		}
		if err := inventories.Save(ctx, &inventory.Inventory{
			ID: "inv_i", Name: "prod fleet", Content: "[all]\nweb-1\n", OrgID: "org_owner",
		}); err != nil {
			t.Fatalf("Save() inventory error = %v", err)
		}

		u, err := user.New("caretaker", "pw", user.RoleOperator)
		if err != nil {
			t.Fatalf("user.New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("Save() user error = %v", err)
		}
		for _, object := range []string{"cred_c", "proj_p", "inv_i"} {
			if err := grants.Save(ctx, &grant.Grant{
				ID: "grant_" + object, Subject: u.ID, Object: object, Access: grant.AccessManage,
				CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("Save() grant error = %v", err)
			}
		}

		h := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
			WithGrants(grants, true), WithOrgs(orgs), WithUsers(users),
			WithCredentials(creds, sealer), WithProjects(projects),
			WithInventories(inventories)).Handler()
		return h, u.ID, creds, projects, inventories
	}

	put := func(t *testing.T, h http.Handler, userID, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPut, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req = req.WithContext(context.WithValue(req.Context(), actorKey{},
			Actor{UserID: userID, Role: user.RoleOperator, Name: "caretaker"}))
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}

	// A rename of each kind, which moves nothing between organizations.
	t.Run("a rename is allowed", func(t *testing.T) {
		t.Parallel()
		h, userID, creds, projects, inventories := build(t)

		if rec := put(t, h, userID, "/v1/credentials/cred_c",
			`{"name":"renamed key","kind":"ssh_key"}`); rec.Code != http.StatusOK {
			t.Errorf("renaming a managed credential = %d, want 200 (body %s)",
				rec.Code, rec.Body.String())
		} else if got, _ := creds.Get(ctx, "cred_c"); got.Name != "renamed key" {
			t.Errorf("credential name = %q, want the rename applied", got.Name)
		}

		if rec := put(t, h, userID, "/v1/projects/proj_p",
			`{"name":"renamed prod","repo_url":"https://example.com/p.git"}`); rec.Code != http.StatusOK {
			t.Errorf("renaming a managed project = %d, want 200 (body %s)", rec.Code, rec.Body.String())
		} else if got, _ := projects.Get(ctx, "proj_p"); got.Name != "renamed prod" {
			t.Errorf("project name = %q, want the rename applied", got.Name)
		}

		if rec := put(t, h, userID, "/v1/inventories/inv_i",
			`{"name":"renamed fleet","content":"[all]\nweb-1\n"}`); rec.Code != http.StatusOK {
			t.Errorf("renaming a managed inventory = %d, want 200 (body %s)",
				rec.Code, rec.Body.String())
		} else if got, _ := inventories.Get(ctx, "inv_i"); got.Name != "renamed fleet" {
			t.Errorf("inventory name = %q, want the rename applied", got.Name)
		}
	})

	// Moving one into an organization the caller does not belong to is still refused, since that is the
	// act the membership question exists to bound.
	t.Run("a move into a foreign org is refused", func(t *testing.T) {
		t.Parallel()
		h, userID, creds, _, _ := build(t)
		rec := put(t, h, userID, "/v1/credentials/cred_c",
			`{"name":"prod key","kind":"ssh_key","org_id":"org_elsewhere"}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("moving a managed credential into a foreign org = %d, want %d (body %s)",
				rec.Code, http.StatusForbidden, rec.Body.String())
		}
		if got, _ := creds.Get(ctx, "cred_c"); got.OrgID != "org_owner" {
			t.Errorf("the credential moved to %q anyway", got.OrgID)
		}
	})

	// And taking one out of an organization the caller does not belong to is refused too, because that
	// takes access away from every member of it.
	t.Run("a move out of a foreign org is refused", func(t *testing.T) {
		t.Parallel()
		h, userID, creds, _, _ := build(t)
		rec := put(t, h, userID, "/v1/credentials/cred_c",
			`{"name":"prod key","kind":"ssh_key","org_id":""}`)
		if rec.Code != http.StatusForbidden {
			t.Errorf("taking a managed credential out of a foreign org = %d, want %d (body %s)",
				rec.Code, http.StatusForbidden, rec.Body.String())
		}
		if got, _ := creds.Get(ctx, "cred_c"); got.OrgID != "org_owner" {
			t.Errorf("the credential left its organization anyway, now %q", got.OrgID)
		}
	})
}
