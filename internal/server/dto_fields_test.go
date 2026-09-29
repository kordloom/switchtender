package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
)

// recordingRunner captures the spec each run executes with, so a test can assert what the tool was
// actually handed rather than only what the API stored.
type recordingRunner struct {
	// mu guards specs against the dispatcher's worker goroutines.
	mu sync.Mutex
	// specs holds every spec the runner was called with, in call order.
	specs []roundhouse.Spec
}

// Run records the spec and succeeds without doing any work.
func (r *recordingRunner) Run(_ context.Context, spec roundhouse.Spec, _ io.Writer) (roundhouse.Result, error) {
	r.mu.Lock()
	r.specs = append(r.specs, spec)
	r.mu.Unlock()
	return roundhouse.Result{ExitCode: 0}, nil
}

// lastSpec returns the most recently recorded spec and whether one was recorded.
func (r *recordingRunner) lastSpec() (roundhouse.Spec, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.specs) == 0 {
		return roundhouse.Spec{}, false
	}
	return r.specs[len(r.specs)-1], true
}

// waitStored polls the store until the run reaches a terminal status and returns it.
func waitStored(t *testing.T, store run.Store, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		rn, err := store.Get(context.Background(), id)
		if err != nil {
			t.Fatalf("read stored run: %v", err)
		}
		if rn.Status.Terminal() {
			return rn
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never finished, last status %q", id, rn.Status)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestCreateRunAppliesExtraVars proves extra_vars in a POST /v1/runs body reaches both the stored
// run and the tool that executes it.
//
// The request struct had no such field, so the decoder dropped it and the API answered 202. A
// plugin tool reads its input from the extra vars, which meant a caller configured the run, was
// told it was accepted, and watched an unconfigured run execute. The assertions are on the stored
// run and on the spec the runner was handed, because the status code is identical either way.
func TestCreateRunAppliesExtraVars(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Body is the JSON posted to the runs endpoint.
		Body string
		// WantVars is what the stored run and the executed spec must carry.
		WantVars map[string]any
	}{{ // Test 0: A flat extra var reaches the run.
		Body:     `{"playbook":"site.yml","inventory":"hosts","extra_vars":{"env":"prod"}}`,
		WantVars: map[string]any{"env": "prod"},
	}, { // Test 1: Several vars, including a nested object, survive intact.
		Body: `{"tool":"bash","command":"echo hi",` +
			`"extra_vars":{"release":"1.2.3","cfg":{"replicas":3}}}`,
		WantVars: map[string]any{
			"release": "1.2.3",
			"cfg":     map[string]any{"replicas": float64(3)},
		},
	}, { // Test 2: A body with no extra vars leaves the run without any.
		Body:     `{"playbook":"site.yml","inventory":"hosts"}`,
		WantVars: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			runner := &recordingRunner{}
			d := dispatch.New(store, runner, zap.NewNop())
			defer d.Close()
			handler := New(store, d, zap.NewNop()).Handler()

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec,
				httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(test.Body)))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
			}
			var created run.Run
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode reply: %v", err)
			}

			stored := waitStored(t, store, created.ID)
			if diff := cmp.Diff(test.WantVars, stored.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored run extra vars mismatch (-want +got):\n%s", diff)
			}
			spec, ok := runner.lastSpec()
			if !ok {
				t.Fatal("the run never reached the tool, so nothing proves the vars were passed on")
			}
			if diff := cmp.Diff(test.WantVars, spec.ExtraVars, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("executed spec extra vars mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTemplateLimitSurvivesTheAPI proves a template's host limit can be written through the API and
// that it narrows the runs the template launches.
//
// The template request struct declared no limit, so the field was dropped on create and, worse, on
// update: an imported template pinned to a canary host was written back with no limit the first
// time anybody renamed it, and the answer was 200. Every schedule, webhook, and launch behind it
// then reached the whole inventory. The assertions are on the stored template and on the run the
// launch submits.
func TestTemplateLimitSurvivesTheAPI(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// CreateBody is the JSON posted to create the template.
		CreateBody string
		// UpdateBody is the JSON put over it, empty to skip the update.
		UpdateBody string
		// WantLimit is the limit the stored template and the launched run must carry.
		WantLimit string
	}{{ // Test 0: A limit on create is stored and launched with.
		CreateBody: `{"name":"deploy","playbook":"site.yml","limit":"canary01"}`,
		WantLimit:  "canary01",
	}, { // Test 1: A rename that resends the limit keeps it.
		CreateBody: `{"name":"deploy","playbook":"site.yml","limit":"canary01"}`,
		UpdateBody: `{"name":"deploy renamed","playbook":"site.yml","limit":"canary01"}`,
		WantLimit:  "canary01",
	}, { // Test 2: A pattern naming several hosts survives the round trip.
		CreateBody: `{"name":"deploy","playbook":"site.yml","limit":"web01,web02:&staging"}`,
		UpdateBody: `{"name":"deploy","playbook":"site.yml","limit":"web01,web02:&staging"}`,
		WantLimit:  "web01,web02:&staging",
	}, { // Test 3: A template written with no limit launches against everything.
		CreateBody: `{"name":"deploy","playbook":"site.yml"}`,
		WantLimit:  "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			templates := template.NewMemStore()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}}
			handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(templates)).Handler()

			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/templates",
				strings.NewReader(test.CreateBody)))
			if rec.Code != http.StatusCreated {
				t.Fatalf("create status = %d, want 201: %s", rec.Code, rec.Body.String())
			}
			var created template.Template
			if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
				t.Fatalf("decode created template: %v", err)
			}

			if test.UpdateBody != "" {
				rec = httptest.NewRecorder()
				handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPut,
					"/v1/templates/"+created.ID, strings.NewReader(test.UpdateBody)))
				if rec.Code != http.StatusOK {
					t.Fatalf("update status = %d, want 200: %s", rec.Code, rec.Body.String())
				}
			}

			stored, err := templates.Get(context.Background(), created.ID)
			if err != nil {
				t.Fatalf("read stored template: %v", err)
			}
			if diff := cmp.Diff(test.WantLimit, stored.Limit); diff != "" {
				t.Errorf("stored template limit mismatch (-want +got):\n%s", diff)
			}

			rec = httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
				"/v1/templates/"+created.ID+"/launch", strings.NewReader(`{}`)))
			if rec.Code != http.StatusAccepted {
				t.Fatalf("launch status = %d, want 202: %s", rec.Code, rec.Body.String())
			}
			if sub.gotRun == nil {
				t.Fatal("the launch submitted no run")
			}
			if diff := cmp.Diff(test.WantLimit, sub.gotRun.Limit); diff != "" {
				t.Errorf("launched run limit mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
