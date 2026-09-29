package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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
	handler := updateProjectHandler(store, authz, zap.NewNop())

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
