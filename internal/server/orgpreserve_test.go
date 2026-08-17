package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// TestAnEditKeepsTheOwningOrganization proves an edit that does not mention an organization keeps
// the one the record has.
//
// Every edit dialog in the product sends the fields it renders and no others, and none of them
// renders an organization. The handlers wrote the field unconditionally from the request, so a
// rename silently un-owned the record: under strict grants the organization's members lost it, and
// otherwise every operator in the install gained access to it. The schedule handler always
// preserved its owner; these four did not. An explicit empty string still moves a record out, which
// is a real operation and stays available.
func TestAnEditKeepsTheOwningOrganization(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	const owner = "org_platform"

	templates := template.NewMemStore()
	inventories := inventory.NewMemStore()
	projects := project.NewMemStore()
	if err := templates.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml", OrgID: owner,
	}); err != nil {
		t.Fatalf("Save template: %v", err)
	}
	if err := inventories.Save(ctx, &inventory.Inventory{
		ID: "inv_1", Name: "prod", Content: "web01\n", OrgID: owner,
	}); err != nil {
		t.Fatalf("Save inventory: %v", err)
	}
	if err := projects.Save(ctx, &project.Project{
		ID: "proj_1", Name: "infra", RepoURL: "https://git.example/infra.git", OrgID: owner,
	}); err != nil {
		t.Fatalf("Save project: %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithTemplates(templates), WithInventories(inventories), WithProjects(projects)).Handler()
	put := func(path, body string) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, path, strings.NewReader(body)))
		return rec.Code
	}

	// Exactly what each dialog sends: the rendered fields, no organization.
	if code := put("/v1/templates/tpl_1", `{"name":"deploy renamed","playbook":"site.yml"}`); code != http.StatusOK {
		t.Fatalf("template update = %d, want 200", code)
	}
	if code := put("/v1/inventories/inv_1", `{"name":"prod renamed","content":"web01\n"}`); code != http.StatusOK {
		t.Fatalf("inventory update = %d, want 200", code)
	}
	if code := put("/v1/projects/proj_1", `{"name":"infra renamed","repo_url":"https://git.example/infra.git"}`); code != http.StatusOK {
		t.Fatalf("project update = %d, want 200", code)
	}

	tpl, err := templates.Get(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("Get template: %v", err)
	}
	if tpl.OrgID != owner {
		t.Errorf("template owner after a rename = %q, want %q: the edit un-owned the record", tpl.OrgID, owner)
	}
	inv, err := inventories.Get(ctx, "inv_1")
	if err != nil {
		t.Fatalf("Get inventory: %v", err)
	}
	if inv.OrgID != owner {
		t.Errorf("inventory owner after a rename = %q, want %q", inv.OrgID, owner)
	}
	proj, err := projects.Get(ctx, "proj_1")
	if err != nil {
		t.Fatalf("Get project: %v", err)
	}
	if proj.OrgID != owner {
		t.Errorf("project owner after a rename = %q, want %q", proj.OrgID, owner)
	}

	// A present empty organization is the explicit move-out, and it still works.
	if code := put("/v1/templates/tpl_1", `{"name":"deploy renamed","playbook":"site.yml","org_id":""}`); code != http.StatusOK {
		t.Fatalf("explicit move-out = %d, want 200", code)
	}
	tpl, err = templates.Get(ctx, "tpl_1")
	if err != nil {
		t.Fatalf("Get template: %v", err)
	}
	if tpl.OrgID != "" {
		t.Errorf("template owner after an explicit move-out = %q, want it unowned", tpl.OrgID)
	}
}
