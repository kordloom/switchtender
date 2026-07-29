package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

// newFileServer returns a handler serving one project whose checkout holds the given files, plus
// a secret outside the checkout that no request may ever reach.
func newFileServer(t *testing.T, files map[string]string) (http.Handler, string) {
	t.Helper()
	cache := t.TempDir()
	checkout := filepath.Join(cache, "proj_1")
	for rel, body := range files {
		full := filepath.Join(checkout, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	if err := os.WriteFile(filepath.Join(cache, "secret.txt"), []byte("do not leak"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	syncer, err := project.NewSyncer(cache)
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	projects := project.NewMemStore()
	if err := projects.Save(context.Background(), &project.Project{
		ID: "proj_1", Name: "web-platform", RepoURL: "https://example.com/repo.git",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithProjects(projects), WithProjectFiles(syncer)).Handler()
	return handler, cache
}

// TestProjectTreeHandler verifies a checkout lists its files and hides the git directory.
func TestProjectTreeHandler(t *testing.T) {
	t.Parallel()
	handler, _ := newFileServer(t, map[string]string{
		"site.yml":           "- hosts: all\n",
		"roles/web/main.yml": "- name: install\n",
		".git/config":        "[remote]\n\turl = https://token@example.com/repo.git\n",
	})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/proj_1/files", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("tree = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if !strings.Contains(body, "site.yml") || !strings.Contains(body, "roles/web/main.yml") {
		t.Errorf("tree missing tracked files: %s", body)
	}
	if strings.Contains(body, ".git") {
		t.Errorf("tree exposed the git directory, which can hold remote credentials: %s", body)
	}
}

// TestProjectFileHandlerRefusals verifies the file endpoint reads a tracked file and refuses every
// path that would escape the checkout, expose git, or name something absent. Refusals share one
// status so a caller cannot map the host filesystem by probing.
func TestProjectFileHandlerRefusals(t *testing.T) {
	t.Parallel()
	handler, cache := newFileServer(t, map[string]string{
		"site.yml":    "- hosts: all\n",
		".git/config": "[remote]\n",
	})
	// A symlink inside the checkout pointing at the secret beside it.
	link := filepath.Join(cache, "proj_1", "escape.txt")
	if err := os.Symlink(filepath.Join(cache, "secret.txt"), link); err != nil {
		t.Fatalf("Symlink() error = %v", err)
	}

	tests := []struct {
		Path       string
		WantStatus int
		WantBody   string
	}{
		{Path: "site.yml", WantStatus: http.StatusOK, WantBody: "hosts: all"},
		{Path: "./site.yml", WantStatus: http.StatusOK, WantBody: "hosts: all"},
		{Path: "roles/../site.yml", WantStatus: http.StatusOK, WantBody: "hosts: all"},
		{Path: "../secret.txt", WantStatus: http.StatusNotFound},
		{Path: "../../etc/passwd", WantStatus: http.StatusNotFound},
		{Path: "/etc/passwd", WantStatus: http.StatusNotFound},
		{Path: ".git/config", WantStatus: http.StatusNotFound},
		{Path: "escape.txt", WantStatus: http.StatusNotFound},
		{Path: "missing.yml", WantStatus: http.StatusNotFound},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet,
				"/v1/projects/proj_1/file?path="+test.Path, nil))
			if rec.Code != test.WantStatus {
				t.Fatalf("path %q = %d, want %d, body %s",
					test.Path, rec.Code, test.WantStatus, rec.Body.String())
			}
			if rec.Code != http.StatusOK {
				if strings.Contains(rec.Body.String(), "do not leak") {
					t.Errorf("path %q leaked content outside the checkout", test.Path)
				}
				return
			}
			var file project.FileContent
			if err := json.Unmarshal(rec.Body.Bytes(), &file); err != nil {
				t.Fatalf("decode: %v", err)
			}
			if !strings.Contains(file.Content, test.WantBody) {
				t.Errorf("path %q content = %q, want it to contain %q", test.Path, file.Content, test.WantBody)
			}
		})
	}
}

// TestProjectFileHandlerErrors verifies the surrounding failure paths: a missing path parameter, an
// unknown project, and a server with browsing switched off.
func TestProjectFileHandlerErrors(t *testing.T) {
	t.Parallel()
	handler, _ := newFileServer(t, map[string]string{"site.yml": "- hosts: all\n"})

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/proj_1/file", nil))
	if rec.Code != http.StatusBadRequest {
		t.Errorf("missing path = %d, want 400", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/nope/files", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown project = %d, want 404", rec.Code)
	}

	// A project that exists but was never synced has no checkout on disk.
	projects := project.NewMemStore()
	if err := projects.Save(context.Background(), &project.Project{ID: "proj_2", Name: "unsynced"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	unsynced := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithProjects(projects), WithProjectFiles(syncer)).Handler()
	rec = httptest.NewRecorder()
	unsynced.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/proj_2/files", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "synced") {
		t.Errorf("unsynced project = %d %s, want 404 explaining the missing checkout",
			rec.Code, rec.Body.String())
	}

	// Without a syncer the endpoints report that browsing is not enabled.
	off := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithProjects(projects)).Handler()
	rec = httptest.NewRecorder()
	off.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/projects/proj_2/files", nil))
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "not enabled") {
		t.Errorf("browsing disabled = %d %s, want 404 saying it is off", rec.Code, rec.Body.String())
	}
}
