package server

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/scrub"
	"github.com/kordloom/switchtender/internal/template"
)

// TestEditingAScrubbedObjectDoesNotStoreTheMask is the guard for the half of scrubbing that is easy
// to leave out and destroys data when it is.
//
// Hiding a secret on read is only safe if the update path recognizes the mask coming back. Every
// read of a template or a schedule is scrubbed for anyone below admin, and there is no unscrubbed
// read, so an operator who renames one necessarily submits the mask with the rename. Storing that
// verbatim replaces the credential with the literal text of a mask: no error, a response that echoes
// it back, and every later run failing on a password that is now "[redacted]". Losing the secret is
// worse than showing it.
//
// It is a round trip through the real handlers rather than a check of any one of them, so a field
// scrubbed later is covered without anybody remembering to extend this. An editable object that
// hides a secret belongs in this table.
func TestEditingAScrubbedObjectDoesNotStoreTheMask(t *testing.T) {
	t.Parallel()
	// A distinct secret per scrubbed field. A single shared value let one surviving field cover for
	// another being overwritten, which is exactly the bug this guards, so each is checked by name.
	const (
		cmdSecret  = "hunter2-command"
		varSecret  = "hunter2-vars"
		stepSecret = "hunter2-step"
	)

	tests := []struct {
		Name  string
		Read  string
		Write string
		// ID picks this object out of the list read, since a list may hold several.
		ID     string
		Seed   func(t *testing.T, tpl template.Store, sch schedule.Store)
		Edit   func(obj map[string]any)
		Stored func(t *testing.T, tpl template.Store, sch schedule.Store) string
		// Secrets are every value that must still be in the stored object afterwards, one per
		// scrubbed field.
		Secrets []string
	}{{
		Name: "template", Read: "/v1/templates", Write: "/v1/templates/tpl_1", ID: "tpl_1",
		Seed: func(t *testing.T, tpl template.Store, _ schedule.Store) {
			t.Helper()
			if err := tpl.Save(t.Context(), &template.Template{
				ID: "tpl_1", Name: "nightly", Tool: run.ToolBash, Inventory: "prod",
				Command:   "export PGPASSWORD=" + cmdSecret + " && ./nightly.sh",
				ExtraVars: map[string]any{"db_password": varSecret},
			}); err != nil {
				t.Fatalf("seed template: %v", err)
			}
		},
		Edit: func(o map[string]any) { o["name"] = "nightly renamed" },
		Stored: func(t *testing.T, tpl template.Store, _ schedule.Store) string {
			t.Helper()
			got, err := tpl.Get(t.Context(), "tpl_1")
			if err != nil {
				t.Fatalf("read template back: %v", err)
			}
			body, _ := json.Marshal(got)
			return string(body)
		},
		Secrets: []string{cmdSecret, varSecret},
	}, {
		Name: "workflow template", Read: "/v1/templates", Write: "/v1/templates/tpl_2", ID: "tpl_2",
		Seed: func(t *testing.T, tpl template.Store, _ schedule.Store) {
			t.Helper()
			if err := tpl.Save(t.Context(), &template.Template{
				ID: "tpl_2", Name: "flow", Inventory: "prod",
				Steps: []run.PipelineStep{
					{Name: "one", Tool: run.ToolBash, Command: "TOKEN=" + stepSecret + " ./s.sh"},
				},
			}); err != nil {
				t.Fatalf("seed workflow template: %v", err)
			}
		},
		Edit: func(o map[string]any) { o["name"] = "flow renamed" },
		Stored: func(t *testing.T, tpl template.Store, _ schedule.Store) string {
			t.Helper()
			got, err := tpl.Get(t.Context(), "tpl_2")
			if err != nil {
				t.Fatalf("read workflow template back: %v", err)
			}
			body, _ := json.Marshal(got)
			return string(body)
		},
		Secrets: []string{stepSecret},
	}, {
		Name: "schedule", Read: "/v1/schedules", Write: "/v1/schedules/sch_1", ID: "sch_1",
		Seed: func(t *testing.T, _ template.Store, sch schedule.Store) {
			t.Helper()
			if err := sch.Save(t.Context(), &schedule.Schedule{
				ID: "sch_1", Name: "nightly", Cron: "0 3 * * *", Inventory: "prod", Enabled: true,
				Steps: []run.PipelineStep{
					{Name: "one", Tool: run.ToolBash, Command: "TOKEN=" + stepSecret + " ./s.sh"},
				},
			}); err != nil {
				t.Fatalf("seed schedule: %v", err)
			}
		},
		Edit: func(o map[string]any) { o["name"] = "nightly renamed" },
		Stored: func(t *testing.T, _ template.Store, sch schedule.Store) string {
			t.Helper()
			got, err := sch.Get(t.Context(), "sch_1")
			if err != nil {
				t.Fatalf("read schedule back: %v", err)
			}
			body, _ := json.Marshal(got)
			return string(body)
		},
		Secrets: []string{stepSecret},
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			tpl := template.NewMemStore()
			sch := schedule.NewMemStore()
			test.Seed(t, tpl, sch)

			// A default install with no tokens configured, which is the documented open mode and
			// the shape this bug lives in: the caller may edit, and carries no actor, so the read
			// is scrubbed for them. An authenticated operator holding a manage grant reaches the
			// same place by a longer road.
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithTemplates(tpl), WithSchedules(sch)).Handler()

			call := func(method, path string, body []byte) *httptest.ResponseRecorder {
				rec := httptest.NewRecorder()
				var req *http.Request
				if body == nil {
					req = httptest.NewRequest(method, path, nil)
				} else {
					req = httptest.NewRequest(method, path, bytes.NewReader(body))
					req.Header.Set("Content-Type", "application/json")
				}
				handler.ServeHTTP(rec, req)
				return rec
			}

			read := call(http.MethodGet, test.Read, nil)
			if read.Code != http.StatusOK {
				t.Fatalf("test %d: GET %s = %d: %s", testNum, test.Read, read.Code,
					read.Body.String())
			}
			for _, want := range test.Secrets {
				if strings.Contains(read.Body.String(), want) {
					t.Fatalf("test %d: %s shows %q in the clear", testNum, test.Read, want)
				}
			}
			if !strings.Contains(read.Body.String(), scrub.Marker) {
				t.Fatalf("test %d: %s carries no mask, so this guard is asserting nothing",
					testNum, test.Read)
			}

			obj := listObjectByID(t, read.Body.Bytes(), test.ID)
			// A real client sends back what it may set. These are server-owned and the update
			// handler refuses unknown fields, so leaving them in would fail the request before it
			// ever reached the code this guard is about.
			for _, readOnly := range []string{"id", "created_at", "created_by", "next_run_at",
				"last_run_at", "last_run_id"} {
				delete(obj, readOnly)
			}
			test.Edit(obj)
			edited, err := json.Marshal(obj)
			if err != nil {
				t.Fatalf("test %d: marshal edit: %v", testNum, err)
			}

			write := call(http.MethodPut, test.Write, edited)
			stored := test.Stored(t, tpl, sch)

			// Either the edit is refused, or the real secret is still stored. Accepting the edit
			// and writing the mask over the credential is the one outcome that must not happen.
			// Only a conflict is a real answer here: it is the update refusing an echo it cannot
			// match. Anything else in the 4xx range means the request never reached that decision,
			// and a guard that passes because its own payload was malformed proves nothing.
			if write.Code >= http.StatusBadRequest && write.Code != http.StatusConflict {
				t.Fatalf("test %d: PUT %s = %d, so the update path was never exercised: %s",
					testNum, test.Write, write.Code, write.Body.String())
			}
			refused := write.Code >= http.StatusBadRequest
			for _, want := range test.Secrets {
				if strings.Contains(stored, want) {
					continue
				}
				if refused {
					t.Errorf("test %d: the edit was refused and %q is gone anyway: %s",
						testNum, want, stored)
					continue
				}
				t.Errorf("test %d: renaming a %s through its own scrubbed view destroyed %q. The "+
					"mask was stored over the real value, so every later run fails on a "+
					"credential that is now the text of a mask.\nstored: %s",
					testNum, test.Name, want, stored)
			}
		})
	}
}

// listObjectByID pulls one object out of a list response by id, whatever the envelope calls its
// array, so the test does not need to know each endpoint's wrapper.
func listObjectByID(t *testing.T, body []byte, id string) map[string]any {
	t.Helper()
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	for _, v := range envelope {
		list, ok := v.([]any)
		if !ok {
			continue
		}
		for _, item := range list {
			obj, ok := item.(map[string]any)
			if ok && obj["id"] == id {
				return obj
			}
		}
	}
	t.Fatalf("no object %q in list response: %s", id, body)
	return nil
}
