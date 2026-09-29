package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

func TestDeleteCredentialInUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	creds := credential.NewMemStore()
	for _, c := range []*credential.Credential{
		{ID: "cred_used", Name: "vault-prod", Kind: credential.KindToken},
		{ID: "cred_free", Name: "unused", Kind: credential.KindToken},
	} {
		if err := creds.Save(ctx, c); err != nil {
			t.Fatalf("Save credential: %v", err)
		}
	}
	tmpls := template.NewMemStore()
	if err := tmpls.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy-web", Playbook: "p.yml", CredentialIDs: []string{"cred_used"},
	}); err != nil {
		t.Fatalf("Save template: %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithCredentials(creds, nil), WithTemplates(tmpls)).Handler()

	del := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/credentials/"+id, nil))
		return rec
	}

	// Test 0: A referenced credential is refused with 409 that names the template.
	rec := del("cred_used")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in-use credential status = %d, want 409", rec.Code)
	}
	var body struct {
		UsedBy map[string][]string `json:"used_by"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body.UsedBy["templates"]; len(got) != 1 || got[0] != "deploy-web" {
		t.Errorf("used_by templates = %v, want [deploy-web]", got)
	}

	// Test 1: An unreferenced credential deletes normally.
	if rec := del("cred_free"); rec.Code != http.StatusOK {
		t.Errorf("delete unused credential status = %d, want 200", rec.Code)
	}
}

func TestDeleteProjectInUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	projs := project.NewMemStore()
	for _, p := range []*project.Project{
		{ID: "proj_used", Name: "infra", RepoURL: "https://example.test/infra.git"},
		{ID: "proj_free", Name: "scratch", RepoURL: "https://example.test/scratch.git"},
	} {
		if err := projs.Save(ctx, p); err != nil {
			t.Fatalf("Save project: %v", err)
		}
	}
	tmpls := template.NewMemStore()
	if err := tmpls.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "provision", Playbook: "p.yml", ProjectID: "proj_used",
	}); err != nil {
		t.Fatalf("Save template: %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithProjects(projs), WithTemplates(tmpls)).Handler()

	del := func(id string) int {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/projects/"+id, nil))
		return rec.Code
	}

	// Test 0: A project a template names is refused with 409.
	if got := del("proj_used"); got != http.StatusConflict {
		t.Errorf("delete in-use project status = %d, want 409", got)
	}
	// Test 1: An unreferenced project deletes normally.
	if got := del("proj_free"); got != http.StatusOK {
		t.Errorf("delete unused project status = %d, want 200", got)
	}
}

// TestCredentialListCarriesEveryUser pins that the list's used_by speaks with the delete guard's
// voice across every kind, not templates alone.
//
// The UI used to count template references only, so a credential holding a project's deploy key or
// an inventory source's token read as unused, and the reader learned otherwise only when the
// delete was refused. The list now carries the same reading the guard uses, so the column and the
// refusal cannot disagree.
func TestCredentialListCarriesEveryUser(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	creds := credential.NewMemStore()
	for _, c := range []*credential.Credential{
		{ID: "cred_multi", Name: "deploy-key", Kind: credential.KindSSHKey},
		{ID: "cred_free", Name: "unused", Kind: credential.KindToken},
	} {
		if err := creds.Save(ctx, c); err != nil {
			t.Fatalf("Save credential: %v", err)
		}
	}
	tmpls := template.NewMemStore()
	if err := tmpls.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy-web", Playbook: "p.yml", CredentialIDs: []string{"cred_multi"},
	}); err != nil {
		t.Fatalf("Save template: %v", err)
	}
	projects := project.NewMemStore()
	if err := projects.Save(ctx, &project.Project{
		ID: "prj_1", Name: "site", RepoURL: "git@example.com:site.git", CredentialID: "cred_multi",
	}); err != nil {
		t.Fatalf("Save project: %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithCredentials(creds, nil), WithTemplates(tmpls), WithProjects(projects)).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/credentials", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("list status = %d, want 200", rec.Code)
	}
	var body struct {
		Credentials []struct {
			ID     string              `json:"id"`
			UsedBy map[string][]string `json:"used_by"`
		} `json:"credentials"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	byID := map[string]map[string][]string{}
	for _, c := range body.Credentials {
		byID[c.ID] = c.UsedBy
	}
	used := byID["cred_multi"]
	if len(used["templates"]) != 1 || used["templates"][0] != "deploy-web" {
		t.Errorf("used_by templates = %v, want the template that references it", used["templates"])
	}
	if len(used["projects"]) != 1 || used["projects"][0] != "site" {
		t.Errorf("used_by projects = %v, want the project whose deploy key this is: counting "+
			"templates alone is how a load-bearing credential read as safe to delete", used["projects"])
	}
	if free := byID["cred_free"]; len(free) != 0 {
		t.Errorf("unused credential carries used_by %v, want it absent", free)
	}
}

func TestDeleteInventoryInUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	invs := inventory.NewMemStore()
	for _, i := range []*inventory.Inventory{
		{ID: "inv_used", Name: "prod-fleet", Content: "[web]\nh1\n"},
		{ID: "inv_free", Name: "scratch", Content: "[web]\nh2\n"},
	} {
		if err := invs.Save(ctx, i); err != nil {
			t.Fatalf("Save inventory: %v", err)
		}
	}
	tmpls := template.NewMemStore()
	if err := tmpls.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy-web", Playbook: "p.yml", InventoryID: "inv_used",
	}); err != nil {
		t.Fatalf("Save template: %v", err)
	}
	pols := policy.NewMemStore()
	if err := pols.Save(ctx, &policy.Policy{
		ID: "pol_1", Name: "prod holds", InventoryID: "inv_used",
	}); err != nil {
		t.Fatalf("Save policy: %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithInventories(invs), WithTemplates(tmpls), WithPolicies(pols)).Handler()

	del := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/inventories/"+id, nil))
		return rec
	}

	// Test 0: A referenced inventory is refused with 409 naming the template and the policy.
	rec := del("inv_used")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in-use inventory status = %d, want 409", rec.Code)
	}
	var body struct {
		UsedBy map[string][]string `json:"used_by"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body.UsedBy["templates"]; len(got) != 1 || got[0] != "deploy-web" {
		t.Errorf("used_by templates = %v, want [deploy-web]", got)
	}
	if got := body.UsedBy["policies"]; len(got) != 1 || got[0] != "prod holds" {
		t.Errorf("used_by policies = %v, want [prod holds]", got)
	}

	// Test 1: An unreferenced inventory deletes normally.
	if rec := del("inv_free"); rec.Code != http.StatusOK {
		t.Errorf("delete unused inventory status = %d, want 200", rec.Code)
	}
}

func TestDeleteTemplateInUse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tmpls := template.NewMemStore()
	for _, tm := range []*template.Template{
		{ID: "tpl_used", Name: "nightly-deploy", Playbook: "p.yml"},
		{ID: "tpl_free", Name: "one-off", Playbook: "q.yml"},
	} {
		if err := tmpls.Save(ctx, tm); err != nil {
			t.Fatalf("Save template: %v", err)
		}
	}
	scheds := schedule.NewMemStore()
	if err := scheds.Save(ctx, &schedule.Schedule{
		ID: "sch_1", Name: "3am deploy", Cron: "0 3 * * *", TemplateID: "tpl_used", Enabled: true,
	}); err != nil {
		t.Fatalf("Save schedule: %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTemplates(tmpls), WithSchedules(scheds)).Handler()

	del := func(id string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/templates/"+id, nil))
		return rec
	}

	// Test 0: A scheduled template is refused with 409 naming the schedule.
	rec := del("tpl_used")
	if rec.Code != http.StatusConflict {
		t.Fatalf("delete in-use template status = %d, want 409", rec.Code)
	}
	var body struct {
		UsedBy map[string][]string `json:"used_by"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got := body.UsedBy["schedules"]; len(got) != 1 || got[0] != "3am deploy" {
		t.Errorf("used_by schedules = %v, want [3am deploy]", got)
	}

	// Test 1: An unscheduled template deletes normally.
	if rec := del("tpl_free"); rec.Code != http.StatusOK {
		t.Errorf("delete unused template status = %d, want 200", rec.Code)
	}
}
