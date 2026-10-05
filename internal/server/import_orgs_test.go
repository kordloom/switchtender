package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/org"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

// orgPlacementExport is an AWX export carrying the organization its smart inventory belongs to,
// with one inventory in that organization and one in another.
const orgPlacementExport = `{
  "organizations": [{"name": "Ops"}, {"name": "Dev"}],
  "inventory": [
    {"name": "web fleet", "organization": {"name": "Ops"}, "hosts": [{"name": "web1"}]},
    {"name": "dev fleet", "organization": {"name": "Dev"}, "hosts": [{"name": "web9"}]},
    {"name": "all web", "kind": "smart", "organization": {"name": "Ops"},
     "host_filter": "name__startswith=web"}
  ]
}`

// TestImportPlacesSmartInventoriesInTheirOrganization pins the API side of placing an imported
// smart inventory: the preview and the applied result both say where each placed inventory lands,
// and applying stores the inventories in the organization it created.
func TestImportPlacesSmartInventoriesInTheirOrganization(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Apply bool
	}{{ // Test 0: A preview says where each inventory would land.
		Apply: false,
	}, { // Test 1: An apply stores them there and says so.
		Apply: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			orgs, invs := org.NewMemStore(), inventory.NewMemStore()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithProjects(project.NewMemStore()), WithInventories(invs),
				WithCredentials(credential.NewMemStore(), nil), WithTemplates(template.NewMemStore()),
				WithSchedules(schedule.NewMemStore()), WithOrgs(orgs),
			).Handler()
			target := "/v1/import/awx"
			if test.Apply {
				target += "?apply=true"
			}
			req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(orgPlacementExport))
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, want 200 (body %s)", rec.Code, rec.Body.String())
			}
			var resp struct {
				// Organizations names the organizations the import places inventories in.
				Organizations []string `json:"organizations"`
				// InventoryOrganizations maps a placed inventory to its organization.
				InventoryOrganizations map[string]string `json:"inventory_organizations"`
			}
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if diff := cmp.Diff([]string{"Ops"}, resp.Organizations); diff != "" {
				t.Errorf("organizations mismatch (-want +got):\n%s", diff)
			}
			wantPlaced := map[string]string{"web fleet": "Ops", "all web": "Ops"}
			if diff := cmp.Diff(wantPlaced, resp.InventoryOrganizations); diff != "" {
				t.Errorf("inventory organizations mismatch (-want +got):\n%s", diff)
			}
			if !test.Apply {
				return
			}
			stored, err := orgs.List(ctx)
			if err != nil || len(stored) != 1 || stored[0].Name != "Ops" {
				t.Fatalf("stored organizations = %+v, %v, want one named Ops", stored, err)
			}
			list, err := invs.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			var placed []string
			for _, inv := range list {
				if inv.OrgID == stored[0].ID {
					placed = append(placed, inv.Name)
				}
			}
			if diff := cmp.Diff([]string{"all web", "web fleet"}, placed,
				cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Errorf("inventories stored in the organization (-want +got):\n%s", diff)
			}
		})
	}
}
