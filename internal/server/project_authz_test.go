package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/user"
)

// TestProjectWriteAuthorizesTheCredentialItClonesWith checks that writing a project cannot borrow a
// credential the actor was never granted.
//
// A project names the credential its clone authenticates with. Nothing authorized that credential
// when the project was written, and the launch-time check does not cover a project's own credential
// either, so it was checked at neither point. A caller holding a manage grant on one project could
// repoint it at a repository of their choosing, clone it as the identity in somebody else's
// credential, and read the result back through the file browser they may legitimately use.
func TestProjectWriteAuthorizesTheCredentialItClonesWith(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	orgs := org.NewMemStore()
	if err := orgs.Save(ctx, &org.Org{ID: "org_mallory", Name: "mallory"}); err != nil {
		t.Fatalf("Save() org error = %v", err)
	}
	if err := orgs.AddMember(ctx, "org_mallory", "user_m", org.RoleMember); err != nil {
		t.Fatalf("AddMember() error = %v", err)
	}
	// Mallory manages one project. She holds nothing on the victim's credential.
	authz := &authorizer{
		strict: true,
		grants: &fakeGrants{byObject: map[string][]*grant.Grant{
			"prj_hers": {{Subject: "user_m", Access: grant.AccessManage}},
		}},
		orgs: orgs,
	}
	store := project.NewMemStore()
	if err := store.Save(ctx, &project.Project{
		ID: "prj_hers", Name: "hers", RepoURL: "https://example.com/hers.git",
	}); err != nil {
		t.Fatalf("Save() project error = %v", err)
	}
	handler := updateProjectHandler(store, nil, authz, zap.NewNop())

	body := `{"name":"hers","repo_url":"https://attacker.example.com/collect.git",` +
		`"credential_id":"cred_victim"}`
	req := httptest.NewRequest(http.MethodPut, "/v1/projects/prj_hers", strings.NewReader(body))
	req.SetPathValue("id", "prj_hers")
	req = req.WithContext(context.WithValue(req.Context(), actorKey{},
		Actor{UserID: "user_m", Role: user.RoleViewer}))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code < 400 {
		t.Errorf("the write answered %d, so a project now clones with a credential its author "+
			"was never granted", rec.Code)
	}
	got, err := store.Get(ctx, "prj_hers")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.CredentialID == "cred_victim" {
		t.Errorf("stored project uses %q, a credential the actor holds nothing on", got.CredentialID)
	}
	if strings.Contains(got.RepoURL, "attacker") {
		t.Errorf("stored repo is %q, pointed at a host the actor chose", got.RepoURL)
	}
}

// TestProjectLocalRepoURLNeedsGlobalAdmin checks that only a global admin may point a project at a
// path on the server.
//
// A manage grant covers a PUT on one project with any URL the validator accepts, and manage implies
// use, so its holder could aim a file:// URL at another organization's checkout cache, sync it, and
// read it back through the file browser. Keeping a local URL already in place is not that, so a
// rename still saves.
func TestProjectLocalRepoURLNeedsGlobalAdmin(t *testing.T) {
	t.Parallel()
	const (
		remote = "https://example.com/hers.git"
		local  = "file:///srv/git/local.git"
	)
	tests := []struct {
		// Create sends POST /v1/projects instead of a PUT on the stored project.
		Create bool
		// Stored is the repo_url the project holds before a PUT. A create starts from an empty store.
		Stored string
		// Body is the request body.
		Body string
		// Role is the caller's global role. A non-admin holds a manage grant on the project.
		Role user.Role
		// WantCode is the status the handler answers.
		WantCode int
		// WantStored lists each stored project as "name repo_url" after the request.
		WantStored []string
	}{{ // Test 0: A manage grant cannot repoint a project at a file:// URL.
		Stored: remote, Body: `{"name":"hers","repo_url":"file:///srv/cache/prj_theirs"}`,
		Role: user.RoleOperator, WantCode: http.StatusForbidden, WantStored: []string{"hers " + remote},
	}, { // Test 1: Nor at a bare absolute path.
		Stored: remote, Body: `{"name":"hers","repo_url":"/srv/cache/prj_theirs"}`,
		Role: user.RoleOperator, WantCode: http.StatusForbidden, WantStored: []string{"hers " + remote},
	}, { // Test 2: Nor at a relative path.
		Stored: remote, Body: `{"name":"hers","repo_url":"./cache/prj_theirs"}`,
		Role: user.RoleOperator, WantCode: http.StatusForbidden, WantStored: []string{"hers " + remote},
	}, { // Test 3: Nor from one local path to another.
		Stored: local, Body: `{"name":"hers","repo_url":"file:///srv/cache/prj_theirs"}`,
		Role: user.RoleOperator, WantCode: http.StatusForbidden, WantStored: []string{"hers " + local},
	}, { // Test 4: A global admin may.
		Stored: remote, Body: `{"name":"hers","repo_url":"` + local + `"}`,
		Role: user.RoleAdmin, WantCode: http.StatusOK, WantStored: []string{"hers " + local},
	}, { // Test 5: A manage grant still changes one https remote for another.
		Stored: remote, Body: `{"name":"hers","repo_url":"https://example.com/next.git"}`,
		Role: user.RoleOperator, WantCode: http.StatusOK,
		WantStored: []string{"hers https://example.com/next.git"},
	}, { // Test 6: A manage grant still renames a local project whose URL it leaves alone.
		Stored: local, Body: `{"name":"renamed","repo_url":"` + local + `"}`,
		Role: user.RoleOperator, WantCode: http.StatusOK, WantStored: []string{"renamed " + local},
	}, { // Test 7: A create with a local URL by a non-admin is refused and stores nothing.
		Create: true, Body: `{"name":"new","repo_url":"` + local + `"}`,
		Role: user.RoleOperator, WantCode: http.StatusForbidden, WantStored: nil,
	}, { // Test 8: A create with a local URL by a global admin is stored.
		Create: true, Body: `{"name":"new","repo_url":"` + local + `"}`,
		Role: user.RoleAdmin, WantCode: http.StatusCreated, WantStored: []string{"new " + local},
	}, { // Test 9: A create with an https remote is not this check's concern.
		Create: true, Body: `{"name":"new","repo_url":"` + remote + `"}`,
		Role: user.RoleOperator, WantCode: http.StatusCreated, WantStored: []string{"new " + remote},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			authz := &authorizer{
				strict: true,
				grants: &fakeGrants{byObject: map[string][]*grant.Grant{
					"prj_hers": {{Subject: "user_m", Access: grant.AccessManage}},
				}},
			}
			store := project.NewMemStore()
			handler := createProjectHandler(store, nil, authz, zap.NewNop())
			req := httptest.NewRequest(http.MethodPost, "/v1/projects", strings.NewReader(test.Body))
			if !test.Create {
				if err := store.Save(ctx, &project.Project{
					ID: "prj_hers", Name: "hers", RepoURL: test.Stored,
				}); err != nil {
					t.Fatalf("Save() error = %v", err)
				}
				handler = updateProjectHandler(store, nil, authz, zap.NewNop())
				req = httptest.NewRequest(http.MethodPut, "/v1/projects/prj_hers",
					strings.NewReader(test.Body))
				req.SetPathValue("id", "prj_hers")
			}
			req = req.WithContext(context.WithValue(req.Context(), actorKey{},
				Actor{UserID: "user_m", Role: test.Role}))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if diff := cmp.Diff(test.WantCode, rec.Code); diff != "" {
				t.Errorf("status mismatch (-want +got):\n%s\nbody: %s", diff, rec.Body.String())
			}
			if rec.Code == http.StatusForbidden && !strings.Contains(rec.Body.String(), "global admin") {
				t.Errorf("refusal %q does not name the rule", rec.Body.String())
			}
			list, err := store.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var stored []string
			for _, p := range list {
				stored = append(stored, p.Name+" "+p.RepoURL)
			}
			if diff := cmp.Diff(test.WantStored, stored, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored projects mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
