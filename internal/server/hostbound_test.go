package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
)

const (
	// overBound is inventory content naming one host past inventory.MaxHosts.
	overBound = "[web]\nh[0:100000]\n"
	// atBound is inventory content naming exactly inventory.MaxHosts hosts.
	atBound = "[web]\nh[1:100000]\n"
)

// errPastBound is the refusal the dispatcher gives a run against an inventory past the host bound.
var errPastBound = fmt.Errorf("inventory fleet: %w", inventory.ErrTooManyHosts)

// boundPreviewer answers every preview with err.
type boundPreviewer struct {
	// err is returned from every preview.
	err error
}

// PreviewInventory returns the configured error.
func (p boundPreviewer) PreviewInventory(context.Context, *inventory.Inventory) (*dispatch.Composition, error) {
	return nil, p.err
}

// TestInventoryWritePastTheHostBound pins the save-time refusal: creating or replacing an inventory
// whose content names more hosts than inventory.MaxHosts answers 400 with the bound named and stores
// nothing, while content at the bound, and content only Ansible can resolve, are stored.
func TestInventoryWritePastTheHostBound(t *testing.T) {
	t.Parallel()
	const original = "[web]\nweb01\n"
	tests := []struct {
		Method     string
		Path       string
		Content    string
		WantStatus int
	}{{ // Test 0: Creating one past the bound is refused.
		Method: http.MethodPost, Path: "/v1/inventories", Content: overBound,
		WantStatus: http.StatusBadRequest,
	}, { // Test 1: Creating one at the bound is stored.
		Method: http.MethodPost, Path: "/v1/inventories", Content: atBound,
		WantStatus: http.StatusCreated,
	}, { // Test 2: A plugin configuration names no hosts until Ansible resolves it.
		Method: http.MethodPost, Path: "/v1/inventories",
		Content: "plugin: amazon.aws.aws_ec2\nregions:\n  - us-east-1\n", WantStatus: http.StatusCreated,
	}, { // Test 3: Replacing one with content past the bound is refused.
		Method: http.MethodPut, Path: "/v1/inventories/inv_fleet", Content: overBound,
		WantStatus: http.StatusBadRequest,
	}, { // Test 4: Replacing one with content at the bound is stored.
		Method: http.MethodPut, Path: "/v1/inventories/inv_fleet", Content: atBound,
		WantStatus: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := inventory.NewMemStore()
			if err := store.Save(context.Background(), &inventory.Inventory{
				ID: "inv_fleet", Name: "fleet", Content: original, CreatedAt: time.Now(),
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithInventories(store)).Handler()
			body, err := json.Marshal(map[string]string{"name": "fleet", "content": test.Content})
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(test.Method, test.Path, strings.NewReader(string(body))))
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.WantStatus, rec.Body.String())
			}
			list, err := store.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if test.WantStatus != http.StatusBadRequest {
				return
			}
			if !strings.Contains(rec.Body.String(), "100000") {
				t.Errorf("refusal %s does not name the bound", rec.Body.String())
			}
			got, err := store.Get(context.Background(), "inv_fleet")
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if len(list) != 1 || got.Content != original {
				t.Errorf("a refused write changed the store: %d inventories, content %q", len(list), got.Content)
			}
		})
	}
}

// TestLaunchPastTheHostBoundIsTheCallersError pins the answer a launch gets when its inventory names
// more hosts than inventory.MaxHosts, as one stored before the save-time check does: the reason with
// the bound named, as the status each path gives an inventory it cannot read, never a 500.
func TestLaunchPastTheHostBoundIsTheCallersError(t *testing.T) {
	t.Parallel()
	ended := time.Date(2026, 10, 18, 2, 0, 0, 0, time.UTC)
	tests := []struct {
		Path       string
		Body       string
		WantStatus int
	}{{ // Test 0: A template launch.
		Path: "/v1/templates/tpl_patch/launch", WantStatus: http.StatusBadRequest,
	}, { // Test 1: A run submitted directly against the stored inventory.
		Path: "/v1/runs", Body: `{"playbook":"patch.yml","inventory_id":"inv_window"}`,
		WantStatus: http.StatusBadRequest,
	}, { // Test 2: A rerun, which takes a fresh snapshot of the inventory.
		Path: "/v1/runs/run_done/rerun", WantStatus: http.StatusConflict,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			runs := run.NewMemStore()
			if err := runs.Save(context.Background(), &run.Run{
				ID: "run_done", Playbook: "patch.yml", InventoryID: "inv_window",
				Status: run.StatusSucceeded, CreatedAt: ended, EndedAt: &ended,
			}); err != nil {
				t.Fatalf("Save(run) error = %v", err)
			}
			handler := New(runs, &fakeSubmitter{err: errPastBound}, zap.NewNop(),
				WithTemplates(noHostsTemplates(t))).Handler()
			rec := httptest.NewRecorder()
			req := httptest.NewRequest(http.MethodPost, test.Path, strings.NewReader(test.Body))
			if test.Body != "" {
				req.Header.Set("Content-Type", "application/json")
			}
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d: %s", rec.Code, test.WantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), "100000") {
				t.Errorf("refusal %s does not name the bound", rec.Body.String())
			}
		})
	}
}

// TestPreviewPastTheHostBoundIsUnprocessable pins the preview's answer when a composed inventory
// draws on one past the host bound: the reason, as the 422 a preview gives an input it cannot read.
func TestPreviewPastTheHostBoundIsUnprocessable(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithInventories(inventory.NewMemStore()),
		WithInventoryPreviewer(boundPreviewer{err: errPastBound})).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/inventories/preview",
		strings.NewReader(`{"name":"web","kind":"smart","host_filter":"name__icontains=web"}`)))
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "100000") {
		t.Errorf("refusal %s does not name the bound", rec.Body.String())
	}
}

// TestImportPastTheHostBound pins what an import of an export naming one host past the bound gets:
// a preview that warns, and an apply refused as the caller's error with nothing stored.
func TestImportPastTheHostBound(t *testing.T) {
	t.Parallel()
	var b strings.Builder
	b.WriteString("[")
	for i := 0; i <= inventory.MaxHosts; i++ {
		if i > 0 {
			b.WriteString(",")
		}
		fmt.Fprintf(&b, `{"name":"n%06d"}`, i)
	}
	b.WriteString("]")
	export := b.String()
	inventories := inventory.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithProjects(project.NewMemStore()),
		WithInventories(inventories),
		WithCredentials(credential.NewMemStore(), nil),
		WithTemplates(template.NewMemStore()),
		WithSchedules(schedule.NewMemStore()),
	).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/import/chef", strings.NewReader(export)))
	if rec.Code != http.StatusOK {
		t.Fatalf("preview status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var preview struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &preview); err != nil {
		t.Fatalf("decode preview: %v", err)
	}
	warned := false
	for _, w := range preview.Warnings {
		warned = warned || strings.Contains(w, "100000")
	}
	if !warned {
		t.Errorf("the preview does not warn about the bound: %v", preview.Warnings)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/import/chef?apply=true",
		strings.NewReader(export)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("apply status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "100000") {
		t.Errorf("refusal %s does not name the bound", rec.Body.String())
	}
	list, err := inventories.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a refused import stored %d inventories", len(list))
	}
}
