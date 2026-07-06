package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/dcadolph/yardmaster/internal/dispatch"
	"github.com/dcadolph/yardmaster/internal/live"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/schedule"
)

// fakeStreamer returns a fixed channel for any run.
type fakeStreamer struct {
	// ch is handed to every subscriber.
	ch chan live.Message
}

// Subscribe returns the fixed channel and a no-op cancel.
func (f *fakeStreamer) Subscribe(string) (<-chan live.Message, func()) {
	return f.ch, func() {}
}

// fakeCanceler records the canceled id and returns a fixed result.
type fakeCanceler struct {
	// ok is the value returned by Cancel.
	ok bool
	// gotID is the id from the most recent Cancel call.
	gotID string
}

// Cancel records the id and returns the fixed result.
func (f *fakeCanceler) Cancel(id string) bool {
	f.gotID = id
	return f.ok
}

// fakeRetrier records the retried id and returns canned results.
type fakeRetrier struct {
	// run is returned on success.
	run *run.Run
	// err is returned instead of run when non-nil.
	err error
	// gotID is the id from the most recent retry call.
	gotID string
}

// RetryFailedShards records the id and returns the configured run or error.
func (f *fakeRetrier) RetryFailedShards(_ context.Context, parentID string) (*run.Run, error) {
	f.gotID = parentID
	if f.err != nil {
		return nil, f.err
	}
	return f.run, nil
}

// fakeSubmitter records the last submission and returns canned results.
type fakeSubmitter struct {
	// run is returned on success.
	run *run.Run
	// err is returned instead of run when non-nil.
	err error
	// gotPlaybook is the playbook from the most recent submit call.
	gotPlaybook string
	// gotInventory is the inventory from the most recent submit call.
	gotInventory string
	// gotShards is the shard count from the most recent SubmitSplit call.
	gotShards int
	// gotSteps is the step count from the most recent SubmitPipeline call.
	gotSteps int
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

// SubmitSplit records the arguments including shard count and returns the configured run or error.
func (f *fakeSubmitter) SubmitSplit(_ context.Context, playbook, inventory string, shards int) (*run.Run, error) {
	f.gotPlaybook = playbook
	f.gotInventory = inventory
	f.gotShards = shards
	if f.err != nil {
		return nil, f.err
	}
	return f.run, nil
}

// SubmitPipeline records the step count and returns the configured run or error.
func (f *fakeSubmitter) SubmitPipeline(_ context.Context, name, inventory string, steps []run.PipelineStep) (*run.Run, error) {
	f.gotPlaybook = name
	f.gotInventory = inventory
	f.gotSteps = len(steps)
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
			Submitter:  &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}},
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
			Submitter:  &fakeSubmitter{err: errors.New("boom")},
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

func TestCreateRunSplitRoutesToSubmitSplit(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_p", Status: run.StatusPending}}
	handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
	req := httptest.NewRequest(http.MethodPost, "/runs",
		strings.NewReader(`{"playbook":"site.yml","inventory":"hosts","shards":3}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202", rec.Code)
	}
	if sub.gotShards != 3 {
		t.Errorf("SubmitSplit shards = %d, want 3", sub.gotShards)
	}
}

func TestRunShards(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	parentID := "run_parent"
	if err := store.Save(context.Background(),
		&run.Run{ID: parentID, Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	idx, count := 0, 1
	if err := store.Save(context.Background(), &run.Run{
		ID: "run_child", Status: run.StatusSucceeded, CreatedAt: time.Now(),
		ParentID: &parentID, ShardIndex: &idx, ShardCount: &count,
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+parentID+"/shards", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "run_child") || !strings.Contains(body, `"count":1`) {
		t.Errorf("body %q missing shard", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/missing/shards", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown parent status = %d, want 404", rec.Code)
	}
}

func TestCreatePipeline(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name       string
		Body       string
		WantStatus int
		WantSteps  int
	}{
		{ // Test 0: Valid pipeline is accepted.
			Name: "valid", WantStatus: http.StatusAccepted, WantSteps: 2,
			Body: `{"name":"deploy","steps":[{"name":"a","playbook":"a.yml"},` +
				`{"name":"b","playbook":"b.yml"}]}`,
		},
		{ // Test 1: No steps is rejected.
			Name: "no steps", Body: `{"name":"deploy","steps":[]}`, WantStatus: http.StatusBadRequest,
		},
		{ // Test 2: A step without a playbook is rejected.
			Name: "missing playbook", Body: `{"steps":[{"name":"a"}]}`, WantStatus: http.StatusBadRequest,
		},
		{ // Test 3: Malformed JSON is rejected.
			Name: "bad json", Body: `{`, WantStatus: http.StatusBadRequest,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_p", Status: run.StatusPending}}
			handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec,
				httptest.NewRequest(http.MethodPost, "/pipelines", strings.NewReader(test.Body)))
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d", rec.Code, test.WantStatus)
			}
			if test.WantSteps > 0 && sub.gotSteps != test.WantSteps {
				t.Errorf("gotSteps = %d, want %d", sub.gotSteps, test.WantSteps)
			}
		})
	}
}

func TestRunSteps(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	parentID := "run_pipe"
	if err := store.Save(context.Background(), &run.Run{
		ID: parentID, Kind: run.KindPipeline, Status: run.StatusRunning, CreatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	idx := 0
	if err := store.Save(context.Background(), &run.Run{
		ID: "run_s", Status: run.StatusSucceeded, CreatedAt: time.Now(),
		ParentID: &parentID, StepIndex: &idx, StepName: "first",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/"+parentID+"/steps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "first") || !strings.Contains(body, `"count":1`) {
		t.Errorf("body %q missing step", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/missing/steps", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", rec.Code)
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
		{Name: "missing", Path: "/runs/nope/logs", WantStatus: http.StatusNotFound},                  // Test 1.
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

func TestUIMountAndRedirect(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusFound {
		t.Errorf("GET / status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); loc != "/ui/" {
		t.Errorf("GET / Location = %q, want /ui/", loc)
	}

	req = httptest.NewRequest(http.MethodGet, "/ui/", nil)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Errorf("GET /ui/ status = %d, want 200", rec.Code)
	}
}

func TestRunStreamTerminalEndsImmediately(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	if err := store.Save(context.Background(),
		&run.Run{ID: "run_done", Status: run.StatusSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	streamer := &fakeStreamer{ch: make(chan live.Message, 1)}
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithStreamer(streamer)).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/run_done/stream", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "event: end") {
		t.Errorf("body %q does not end the stream", rec.Body.String())
	}
}

func TestRunStreamRelaysMessages(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	if err := store.Save(context.Background(),
		&run.Run{ID: "run_live", Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	ch := make(chan live.Message, 2)
	ch <- live.Message{Type: "event", Data: []byte(`{"type":"play_start"}`)}
	ch <- live.Message{Type: "end"}
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithStreamer(&fakeStreamer{ch: ch})).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/run_live/stream", nil))

	body := rec.Body.String()
	if !strings.Contains(body, "event: event") || !strings.Contains(body, "play_start") {
		t.Errorf("body %q missing the relayed event", body)
	}
	if !strings.Contains(body, "event: end") {
		t.Errorf("body %q missing the end", body)
	}
}

func TestRunStreamNotFoundAndDisabled(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	if err := store.Save(context.Background(),
		&run.Run{ID: "run_live", Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	withStream := New(store, &fakeSubmitter{}, zap.NewNop(),
		WithStreamer(&fakeStreamer{ch: make(chan live.Message)})).Handler()
	rec := httptest.NewRecorder()
	withStream.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/missing/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown run status = %d, want 404", rec.Code)
	}

	noStream := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec = httptest.NewRecorder()
	noStream.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/runs/run_live/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("disabled streamer status = %d, want 404", rec.Code)
	}
}

func TestFleet(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	ctx := context.Background()
	if err := store.SaveHostSummary(ctx, "r1", []run.HostSummary{
		{Host: "db01", Worst: "failed", RanAt: time.Now()},
		{Host: "web01", Worst: "ok", RanAt: time.Now()},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/fleet?window=5", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "db01") || !strings.Contains(body, `"window":5`) {
		t.Errorf("body %q missing fleet data", body)
	}
}

func TestCreatePipelineBadGraph(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{err: fmt.Errorf("%w: %q", dispatch.ErrUnknownDependency, "ghost")}
	handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/pipelines", strings.NewReader(
		`{"steps":[{"name":"a","playbook":"a.yml","depends_on":["ghost"]}]}`)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "unknown dependency") {
		t.Errorf("body %q missing validation detail", rec.Body.String())
	}
}

func TestHostHistory(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.SaveHostSummary(context.Background(), "run_1", []run.HostSummary{
		{Host: "db01", Worst: "failed", RanAt: base},
	}); err != nil {
		t.Fatalf("SaveHostSummary() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hosts/db01/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "run_1") || !strings.Contains(body, `"count":1`) {
		t.Errorf("body %q missing history entry", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/hosts/ghost/runs", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"count":0`) {
		t.Errorf("unknown host = %d %q, want 200 with empty history", rec.Code, rec.Body.String())
	}
}

func TestTaskTrends(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	base := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if err := store.SaveTaskSummary(context.Background(), "run_1", []run.TaskSummary{
		{Task: "install", Seconds: 12.5, RanAt: base},
	}); err != nil {
		t.Fatalf("SaveTaskSummary() error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tasks?window=5", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "install") || !strings.Contains(body, `"window":5`) {
		t.Errorf("body %q missing trend", body)
	}
}

func TestCancelRun(t *testing.T) {
	t.Parallel()
	seed := func(status run.Status) run.Store {
		s := run.NewMemStore()
		_ = s.Save(context.Background(), &run.Run{ID: "run_1", Status: status, CreatedAt: time.Now()})
		return s
	}
	tests := []struct {
		Name       string
		Store      run.Store
		Canceler   Canceler
		Path       string
		WantStatus int
	}{
		{ // Test 0: A running run is canceled.
			Name: "running", Store: seed(run.StatusRunning), Canceler: &fakeCanceler{ok: true},
			Path: "/runs/run_1/cancel", WantStatus: http.StatusAccepted,
		},
		{ // Test 1: A finished run conflicts.
			Name: "terminal", Store: seed(run.StatusSucceeded), Canceler: &fakeCanceler{ok: true},
			Path: "/runs/run_1/cancel", WantStatus: http.StatusConflict,
		},
		{ // Test 2: An unknown run is not found.
			Name: "unknown", Store: run.NewMemStore(), Canceler: &fakeCanceler{ok: true},
			Path: "/runs/none/cancel", WantStatus: http.StatusNotFound,
		},
		{ // Test 3: A run the dispatcher cannot cancel conflicts.
			Name: "not cancelable", Store: seed(run.StatusRunning), Canceler: &fakeCanceler{ok: false},
			Path: "/runs/run_1/cancel", WantStatus: http.StatusConflict,
		},
		{ // Test 4: Cancellation disabled is not found.
			Name: "disabled", Store: seed(run.StatusRunning), Canceler: nil,
			Path: "/runs/run_1/cancel", WantStatus: http.StatusNotFound,
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			opts := []Option{}
			if test.Canceler != nil {
				opts = append(opts, WithCanceler(test.Canceler))
			}
			handler := New(test.Store, &fakeSubmitter{}, zap.NewNop(), opts...).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, test.Path, nil))
			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d", rec.Code, test.WantStatus)
			}
		})
	}
}

func TestRetryRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name             string
		Retrier          Retrier
		WantStatus       int
		WantBodyContains string
	}{
		{ // Test 0: A failed split retries and returns the new parent.
			Name: "retried",
			Retrier: &fakeRetrier{run: &run.Run{
				ID: "run_retry", Kind: run.KindSplit, Status: run.StatusPending,
			}},
			WantStatus: http.StatusAccepted, WantBodyContains: "run_retry",
		},
		{ // Test 1: An unknown run is not found.
			Name: "unknown", Retrier: &fakeRetrier{err: run.ErrNotFound},
			WantStatus: http.StatusNotFound, WantBodyContains: "run not found",
		},
		{ // Test 2: A non split run conflicts.
			Name: "not split", Retrier: &fakeRetrier{err: dispatch.ErrNotSplit},
			WantStatus: http.StatusConflict, WantBodyContains: "only split runs",
		},
		{ // Test 3: An unfinished run conflicts.
			Name: "not finished", Retrier: &fakeRetrier{err: dispatch.ErrNotFinished},
			WantStatus: http.StatusConflict, WantBodyContains: "has not finished",
		},
		{ // Test 4: Nothing to retry conflicts.
			Name: "nothing failed", Retrier: &fakeRetrier{err: dispatch.ErrNoFailedShards},
			WantStatus: http.StatusConflict, WantBodyContains: "no failed shards",
		},
		{ // Test 5: Retrier failure maps to 500.
			Name: "retrier error", Retrier: &fakeRetrier{err: errors.New("boom")},
			WantStatus: http.StatusInternalServerError, WantBodyContains: "could not retry run",
		},
		{ // Test 6: Retry disabled is not found.
			Name: "disabled", Retrier: nil,
			WantStatus: http.StatusNotFound, WantBodyContains: "retry not enabled",
		},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			opts := []Option{}
			if test.Retrier != nil {
				opts = append(opts, WithRetrier(test.Retrier))
			}
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), opts...).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec,
				httptest.NewRequest(http.MethodPost, "/runs/run_1/retry", nil))
			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d", rec.Code, test.WantStatus)
			}
			if !strings.Contains(rec.Body.String(), test.WantBodyContains) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), test.WantBodyContains)
			}
		})
	}
}

func TestSchedules(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithSchedules(schedule.NewMemStore())).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/schedules",
		strings.NewReader(`{"name":"nightly","cron":"0 2 * * *","playbook":"site.yml"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want 201", rec.Code)
	}
	var created schedule.Schedule
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode created: %v", err)
	}
	if created.ID == "" || created.NextRunAt == nil || !created.Enabled {
		t.Fatalf("schedule not fully populated: %+v", created)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/schedules", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nightly") {
		t.Errorf("list = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/schedules/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("get status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/schedules/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("delete status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/schedules/"+created.ID, nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("get after delete = %d, want 404", rec.Code)
	}
}

func TestScheduleValidation(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Body string
		Want int
	}{
		{Name: "bad cron", Body: `{"cron":"nope","playbook":"p.yml"}`, Want: http.StatusBadRequest},
		{Name: "no target", Body: `{"cron":"* * * * *"}`, Want: http.StatusBadRequest},
		{Name: "bad json", Body: `{`, Want: http.StatusBadRequest},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
				WithSchedules(schedule.NewMemStore())).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec,
				httptest.NewRequest(http.MethodPost, "/schedules", strings.NewReader(test.Body)))
			if rec.Code != test.Want {
				t.Errorf("status = %d, want %d", rec.Code, test.Want)
			}
		})
	}
}

func TestSchedulesDisabled(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/schedules", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 when scheduling is disabled", rec.Code)
	}
}

func TestNewPanicsOnNilDeps(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		Store     run.Store
		Submitter Submitter
	}{
		{Name: "nil store", Store: nil, Submitter: &fakeSubmitter{}},      // Test 0.
		{Name: "nil submitter", Store: run.NewMemStore(), Submitter: nil}, // Test 1.
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
