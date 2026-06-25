package server

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/run"
)

// fakeSubmitter records the last submission and returns canned results.
type fakeSubmitter struct {
	// run is returned on success.
	run *run.Run
	// err is returned instead of run when non-nil.
	err error
	// gotPlaybook is the playbook from the most recent Submit call.
	gotPlaybook string
	// gotInventory is the inventory from the most recent Submit call.
	gotInventory string
}

// Submit records the arguments and returns the configured run or error.
func (f *fakeSubmitter) Submit(_ context.Context, playbook, inventory string) (*run.Run, error) {
	f.gotPlaybook = playbook
	f.gotInventory = inventory
	if f.err != nil {
		return nil, f.err
	}
	return f.run, nil
}

func TestCreateRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name             string
		Body             string
		Submitter        *fakeSubmitter
		WantStatus       int
		WantBodyContains string
	}{
		{ // Test 0: Valid request is accepted.
			Name: "valid", Body: `{"playbook":"play.yml","inventory":"inv"}`,
			Submitter: &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}},
			WantStatus: http.StatusAccepted, WantBodyContains: "run_1",
		},
		{ // Test 1: Missing playbook is rejected.
			Name: "missing playbook", Body: `{"inventory":"inv"}`,
			Submitter: &fakeSubmitter{}, WantStatus: http.StatusBadRequest,
			WantBodyContains: "playbook is required",
		},
		{ // Test 2: Malformed JSON is rejected.
			Name: "bad json", Body: `{`, Submitter: &fakeSubmitter{},
			WantStatus: http.StatusBadRequest, WantBodyContains: "invalid request body",
		},
		{ // Test 3: Submitter failure maps to 500.
			Name: "submit error", Body: `{"playbook":"play.yml"}`,
			Submitter: &fakeSubmitter{err: errors.New("boom")},
			WantStatus: http.StatusInternalServerError, WantBodyContains: "could not submit run",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := New(run.NewMemStore(), test.Submitter, zap.NewNop()).Handler()
			req := httptest.NewRequest(http.MethodPost, "/runs", strings.NewReader(test.Body))
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d", rec.Code, test.WantStatus)
			}
			if !strings.Contains(rec.Body.String(), test.WantBodyContains) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), test.WantBodyContains)
			}
		})
	}
}

func TestCreateRunForwardsArgsAndLocation(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_42", Status: run.StatusPending}}
	handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
	req := httptest.NewRequest(http.MethodPost, "/runs",
		strings.NewReader(`{"playbook":"site.yml","inventory":"hosts"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if sub.gotPlaybook != "site.yml" || sub.gotInventory != "hosts" {
		t.Errorf("forwarded playbook=%q inventory=%q", sub.gotPlaybook, sub.gotInventory)
	}
	if loc := rec.Header().Get("Location"); loc != "/runs/run_42" {
		t.Errorf("Location = %q, want /runs/run_42", loc)
	}
}

func TestGetRun(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	seed := &run.Run{ID: "run_1", Playbook: "p", Status: run.StatusSucceeded, CreatedAt: time.Now()}
	if err := store.Save(context.Background(), seed); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	tests := []struct {
		Name       string
		Path       string
		WantStatus int
	}{
		{Name: "found", Path: "/runs/run_1", WantStatus: http.StatusOK},        // Test 0.
		{Name: "missing", Path: "/runs/nope", WantStatus: http.StatusNotFound}, // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, test.Path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d", rec.Code, test.WantStatus)
			}
		})
	}
}

func TestListRuns(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	if err := store.Save(context.Background(),
		&run.Run{ID: "run_1", Status: run.StatusSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	req := httptest.NewRequest(http.MethodGet, "/runs", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"count":1`) {
		t.Errorf("body %q does not report count", rec.Body.String())
	}
}

func TestRunLogs(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	if err := store.Save(context.Background(),
		&run.Run{ID: "run_1", Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := store.AppendLog(context.Background(), "run_1", []byte("PLAY RECAP")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	tests := []struct {
		Name       string
		Path       string
		WantStatus int
		WantBody   string
	}{
		{Name: "found", Path: "/runs/run_1/logs", WantStatus: http.StatusOK, WantBody: "PLAY RECAP"}, // Test 0.
		{Name: "missing", Path: "/runs/nope/logs", WantStatus: http.StatusNotFound},                 // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(http.MethodGet, test.Path, nil)
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, req)
			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d", rec.Code, test.WantStatus)
			}
			if test.WantBody != "" && !strings.Contains(rec.Body.String(), test.WantBody) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), test.WantBody)
			}
		})
	}
}

func TestHealth(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), `"status":"ok"`) {
		t.Errorf("body %q does not report ok", rec.Body.String())
	}
}

func TestNewPanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Store     run.Store
		Submitter Submitter
	}{
		{Name: "nil store", Store: nil, Submitter: &fakeSubmitter{}},        // Test 0.
		{Name: "nil submitter", Store: run.NewMemStore(), Submitter: nil},   // Test 1.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Error("New() did not panic on nil dependency")
				}
			}()
			New(test.Store, test.Submitter, zap.NewNop())
		})
	}
}
