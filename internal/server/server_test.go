package server

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/event"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/user"
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

// RelaunchFailedHosts records the id and returns the configured run or error.
func (f *fakeRetrier) RelaunchFailedHosts(_ context.Context, runID string) (*run.Run, error) {
	f.gotID = runID
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
	// gotRun is a probe run with the most recent Submit call's options applied, so a test can
	// assert what the handler asked for.
	gotRun *run.Run
}

// Submit records the arguments, applies the options to a probe run for assertions, and returns
// the configured run or error.
func (f *fakeSubmitter) Submit(_ context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error) {
	f.gotPlaybook = playbook
	f.gotInventory = inventory
	probe := &run.Run{Playbook: playbook, Inventory: inventory}
	run.ApplyOptions(probe, opts)
	f.gotRun = probe
	if f.err != nil {
		return nil, f.err
	}
	return f.run, nil
}

// SubmitSplit records the arguments including shard count and returns the configured run or error.
func (f *fakeSubmitter) SubmitSplit(_ context.Context, playbook, inventory string, shards int, opts ...run.SubmitOption) (*run.Run, error) {
	f.gotPlaybook = playbook
	f.gotInventory = inventory
	f.gotShards = shards
	// The options are applied to a probe here for the same reason Submit does it. Discarding them
	// meant a test could not see whether a split was held, and a request asking for both approval
	// and shards silently got neither.
	probe := &run.Run{Playbook: playbook, Inventory: inventory}
	run.ApplyOptions(probe, opts)
	f.gotRun = probe
	if f.err != nil {
		return nil, f.err
	}
	return f.run, nil
}

// SubmitPipeline records the step count, applies the options to a probe run for assertions, and
// returns the configured run or error.
func (f *fakeSubmitter) SubmitPipeline(_ context.Context, name, inventory string, steps []run.PipelineStep, opts ...run.SubmitOption) (*run.Run, error) {
	f.gotPlaybook = name
	f.gotInventory = inventory
	f.gotSteps = len(steps)
	probe := &run.Run{Playbook: name, Inventory: inventory}
	run.ApplyOptions(probe, opts)
	f.gotRun = probe
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
			req := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(test.Body))
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
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"playbook":"site.yml","inventory":"hosts"}`))
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if sub.gotPlaybook != "site.yml" || sub.gotInventory != "hosts" {
		t.Errorf("forwarded playbook=%q inventory=%q", sub.gotPlaybook, sub.gotInventory)
	}
	if loc := rec.Header().Get("Location"); loc != "/v1/runs/run_42" {
		t.Errorf("Location = %q, want /runs/run_42", loc)
	}
}

func TestCreateRunSplitRoutesToSubmitSplit(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_p", Status: run.StatusPending}}
	handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
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

// TestCreateRunForwardsIdempotencyKey verifies the handler turns the Idempotency-Key header into a
// submit option, trimming whitespace and treating a blank header as no key.
func TestCreateRunForwardsIdempotencyKey(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Header  string
		SetIt   bool
		WantKey string
	}{
		{Name: "with key", Header: "idem-abc", SetIt: true, WantKey: "idem-abc"},
		{Name: "no header", SetIt: false, WantKey: ""},
		{Name: "whitespace only", Header: "   ", SetIt: true, WantKey: ""},
		{Name: "trimmed", Header: "  idem-xyz  ", SetIt: true, WantKey: "idem-xyz"},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sub := &fakeSubmitter{run: &run.Run{ID: "run_1", Status: run.StatusPending}}
			handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
			req := httptest.NewRequest(http.MethodPost, "/v1/runs",
				strings.NewReader(`{"playbook":"site.yml"}`))
			if test.SetIt {
				req.Header.Set("Idempotency-Key", test.Header)
			}
			rec := httptest.NewRecorder()

			handler.ServeHTTP(rec, req)

			if rec.Code != http.StatusAccepted {
				t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
			}
			if sub.gotRun == nil {
				t.Fatal("submitter received no run")
			}
			if sub.gotRun.IdempotencyKey != test.WantKey {
				t.Errorf("idempotency key = %q, want %q", sub.gotRun.IdempotencyKey, test.WantKey)
			}
		})
	}
}

// TestCreatePipelineForwardsIdempotencyKey verifies the pipeline handler forwards the header too.
func TestCreatePipelineForwardsIdempotencyKey(t *testing.T) {
	t.Parallel()
	sub := &fakeSubmitter{run: &run.Run{ID: "run_p", Status: run.StatusPending}}
	handler := New(run.NewMemStore(), sub, zap.NewNop()).Handler()
	req := httptest.NewRequest(http.MethodPost, "/v1/pipelines",
		strings.NewReader(`{"name":"deploy","steps":[{"name":"s1","playbook":"p.yml"}]}`))
	req.Header.Set("Idempotency-Key", "idem-pipe")
	rec := httptest.NewRecorder()

	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
	}
	if sub.gotRun == nil || sub.gotRun.IdempotencyKey != "idem-pipe" {
		t.Errorf("pipeline idempotency key = %+v, want idem-pipe", sub.gotRun)
	}
}

// TestCreateRunIdempotentReplay drives the whole path with a real dispatcher: a repeated POST
// carrying the same header returns the same run id, and a different key is a new run.
func TestCreateRunIdempotentReplay(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := dispatch.New(store, runner, zap.NewNop())
	defer d.Close()
	handler := New(store, d, zap.NewNop()).Handler()

	post := func(key string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/runs",
			strings.NewReader(`{"playbook":"site.yml","inventory":"inv"}`))
		req.Header.Set("Idempotency-Key", key)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusAccepted {
			t.Fatalf("status = %d, want 202: %s", rec.Code, rec.Body.String())
		}
		var got run.Run
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode response: %v", err)
		}
		return got.ID
	}

	first := post("idem-xyz")
	if second := post("idem-xyz"); second != first {
		t.Errorf("replayed POST id = %q, want %q", second, first)
	}
	if third := post("idem-other"); third == first {
		t.Error("a different key returned the original run")
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/"+parentID+"/shards", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "run_child") || !strings.Contains(body, `"count":1`) {
		t.Errorf("body %q missing shard", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/missing/shards", nil))
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
				httptest.NewRequest(http.MethodPost, "/v1/pipelines", strings.NewReader(test.Body)))
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/"+parentID+"/steps", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); !strings.Contains(body, "first") || !strings.Contains(body, `"count":1`) {
		t.Errorf("body %q missing step", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/missing/steps", nil))
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
		{Name: "found", Path: "/v1/runs/run_1", WantStatus: http.StatusOK},        // Test 0.
		{Name: "missing", Path: "/v1/runs/nope", WantStatus: http.StatusNotFound}, // Test 1.
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

	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
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
		{Name: "found", Path: "/v1/runs/run_1/logs", WantStatus: http.StatusOK, WantBody: "PLAY RECAP"}, // Test 0.
		{Name: "missing", Path: "/v1/runs/nope/logs", WantStatus: http.StatusNotFound},                  // Test 1.
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_done/stream", nil))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if !strings.Contains(rec.Body.String(), "event: end") {
		t.Errorf("body %q does not end the stream", rec.Body.String())
	}
}

// TestRunEventsPaging checks that the events endpoint honors after and limit and reports the
// cursor to page with.
func TestRunEventsPaging(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	// Events are appended while the run is running, as a real run does, then it finalizes, since the
	// store fences event writes to a terminal run.
	if err := store.Save(ctx, &run.Run{ID: "r", Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	at := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 5; i++ {
		if err := store.AppendEvents(ctx, "r",
			[]event.Event{{Type: event.TypeTaskStart, Time: at, Task: fmt.Sprintf("t%d", i)}}); err != nil {
			t.Fatalf("AppendEvents() error = %v", err)
		}
	}
	if err := store.Save(ctx, &run.Run{ID: "r", Status: run.StatusSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() finalize error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	get := func(query string) eventsResponse {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/r/events"+query, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", rec.Code)
		}
		var got eventsResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return got
	}

	if all := get(""); all.Count != 5 || all.NextAfter != 5 {
		t.Errorf("all = count %d next_after %d, want 5 and 5", all.Count, all.NextAfter)
	}
	first := get("?limit=2")
	if first.Count != 2 || first.NextAfter != 2 || first.Events[0].Task != "t0" {
		t.Errorf("first page = count %d next_after %d first %q, want 2, 2, t0",
			first.Count, first.NextAfter, first.Events[0].Task)
	}
	next := get("?after=2&limit=2")
	if next.Count != 2 || next.Events[0].Task != "t2" {
		t.Errorf("second page = count %d first %q, want 2 starting t2", next.Count, next.Events[0].Task)
	}
	if tail := get("?after=5"); tail.Count != 0 || tail.NextAfter != 5 {
		t.Errorf("tail = count %d next_after %d, want 0 and 5", tail.Count, tail.NextAfter)
	}
}

// TestRunStreamResumesAfterCursor checks that a stream started with ?after= sends only the
// events past the cursor, never replaying earlier history.
func TestRunStreamResumesAfterCursor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	// Events land while the run is running, then it finalizes, since the store fences event writes to
	// a terminal run.
	if err := store.Save(ctx, &run.Run{ID: "r", Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	at := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		if err := store.AppendEvents(ctx, "r",
			[]event.Event{{Type: event.TypeTaskStart, Time: at, Task: fmt.Sprintf("t%d", i)}}); err != nil {
			t.Fatalf("AppendEvents() error = %v", err)
		}
	}
	if err := store.Save(ctx, &run.Run{ID: "r", Status: run.StatusSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() finalize error = %v", err)
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/r/stream?after=2", nil))
	body := rec.Body.String()
	if strings.Contains(body, `"t0"`) || strings.Contains(body, `"t1"`) {
		t.Errorf("stream replayed history before the cursor:\n%s", body)
	}
	if !strings.Contains(body, `"t2"`) {
		t.Errorf("stream did not send the event after the cursor:\n%s", body)
	}
}

func TestRunStreamDrainsStore(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx,
		&run.Run{ID: "run_live", Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	wake := make(chan live.Message, 4)
	handler := New(store, &fakeSubmitter{}, zap.NewNop(),
		WithStreamer(&fakeStreamer{ch: wake})).Handler()

	srv := httptest.NewServer(handler)
	defer srv.Close()
	res, err := http.Get(srv.URL + "/v1/runs/run_live/stream")
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = res.Body.Close() }()

	// New store rows stream out, whether written by this process or any other; the hub message
	// only wakes the drain early.
	at := time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC)
	if err := store.AppendEvents(ctx, "run_live",
		[]event.Event{{Type: event.TypeTaskStart, Time: at, Task: "deploy"}}); err != nil {
		t.Fatalf("AppendEvents() error = %v", err)
	}
	if err := store.AppendLog(ctx, "run_live", []byte("remote says hello")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	wake <- live.Message{Type: "event"}

	// The deadline below is checked between reads, and a read on a stream that never delivers
	// blocks rather than returning, so the check alone cannot end the test. Closing the body when
	// the deadline passes makes the blocked read fail, which turns a hung test into a reported one.
	// Left unbounded this ran until the package timeout and took the whole suite down with it.
	reader := bufio.NewReader(res.Body)
	deadline := time.Now().Add(5 * time.Second)
	// Well above the five second deadline that is the real assertion. This only has to stop a
	// stream that never delivers from hanging the whole suite, which it used to do for fifteen
	// minutes. Set close to the deadline it fired on a stream that was merely slow, because the
	// drain ticks once a second and the race detector under a full suite is not fast.
	stopReading := time.AfterFunc(60*time.Second, func() { _ = res.Body.Close() })
	defer stopReading.Stop()
	var sawEvent, sawLog bool
	for !sawEvent || !sawLog {
		if time.Now().After(deadline) {
			t.Fatalf("stream never delivered store rows: event=%v log=%v", sawEvent, sawLog)
		}
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if strings.Contains(line, "deploy") {
			sawEvent = true
		}
		if strings.Contains(line, "remote says hello") {
			sawLog = true
		}
	}

	// A terminal store state ends the stream on the next drain.
	done := &run.Run{ID: "run_live", Status: run.StatusSucceeded, CreatedAt: time.Now()}
	if err := store.Save(ctx, done); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	wake <- live.Message{Type: "event"}
	sawEnd := false
	for time.Now().Before(deadline) {
		line, err := reader.ReadString('\n')
		if err != nil {
			break
		}
		if strings.Contains(line, "event: end") {
			sawEnd = true
			break
		}
	}
	if !sawEnd {
		t.Error("stream did not end after the run turned terminal")
	}
}

func TestRunStreamWithoutHub(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx,
		&run.Run{ID: "run_done", Status: run.StatusSucceeded, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// No streamer configured: the store poll alone serves the stream.
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/run_done/stream", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "event: end") {
		t.Errorf("no-hub stream = %d %q, want 200 with end", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs/ghost/stream", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("unknown run = %d, want 404", rec.Code)
	}
}

// TestRunStreamEndsOnShutdown checks a live stream releases the server when draining starts.
// Graceful shutdown waits for handlers to return and does not cancel a request's context, so a
// stream that only watches its request would keep an exiting process alive until the shutdown
// timeout expired. The run here never turns terminal, so nothing but the shutdown signal can end it.
func TestRunStreamEndsOnShutdown(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	if err := store.Save(ctx,
		&run.Run{ID: "run_live", Status: run.StatusRunning, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	shutdownCtx, drain := context.WithCancel(ctx)
	defer drain()
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithShutdown(shutdownCtx)).Handler()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	httpServer := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	served := make(chan error, 1)
	go func() { served <- httpServer.Serve(ln) }()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		"http://"+ln.Addr().String()+"/v1/runs/run_live/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest() error = %v", err)
	}
	// Resume from the very start of both cursors, the way a reconnecting browser does.
	//
	// The handler flushes its response headers before it reads the position it will stream from,
	// and by design a stream with no cursor starts at the current end. So an append landing in that
	// window is not replayed, and the read below waited for bytes that were never going to be sent.
	// With no deadline on the client that was an indefinite block: this test hung a full suite run
	// for ten minutes once and passed twenty times in isolation, because the window is narrow and
	// only a loaded machine widens it. Naming an explicit cursor removes the race rather than
	// hiding it behind a sleep.
	req.Header.Set("Last-Event-ID", "0:0")
	// A bound so a stream that stops sending fails this test in seconds instead of hanging the
	// suite. It is far above the real runtime, which is milliseconds.
	client := &http.Client{Timeout: 30 * time.Second}
	res, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET stream: %v", err)
	}
	defer func() { _ = res.Body.Close() }()
	// Read the first bytes so the handler is provably inside its loop before shutdown begins.
	if err := store.AppendLog(ctx, "run_live", []byte("streaming\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
	}
	reader := bufio.NewReader(res.Body)
	for {
		line, err := reader.ReadString('\n')
		if err != nil {
			t.Fatalf("read stream: %v", err)
		}
		if strings.Contains(line, "streaming") {
			break
		}
	}

	// The real shutdown timeout is fifteen seconds. Five is long enough that a slow machine will not
	// trip it and short enough that a stream ignoring the signal fails the test rather than hanging.
	const graceLimit = 5 * time.Second
	start := time.Now()
	drain()
	shutdownDone, cancel := context.WithTimeout(ctx, graceLimit)
	defer cancel()
	if err := httpServer.Shutdown(shutdownDone); err != nil {
		t.Fatalf("Shutdown() waited out its timeout with a stream open: %v", err)
	}
	if elapsed := time.Since(start); elapsed > graceLimit/2 {
		t.Errorf("Shutdown() took %v with a stream open, want prompt release", elapsed)
	}
	if err := <-served; err != nil && !errors.Is(err, http.ErrServerClosed) {
		t.Fatalf("Serve() error = %v", err)
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/fleet?window=5", nil))

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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/pipelines", strings.NewReader(
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/hosts/db01/runs", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "run_1") || !strings.Contains(body, `"count":1`) {
		t.Errorf("body %q missing history entry", body)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/hosts/ghost/runs", nil))
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/tasks?window=5", nil))
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
			Path: "/v1/runs/run_1/cancel", WantStatus: http.StatusAccepted,
		},
		{ // Test 1: A finished run conflicts.
			Name: "terminal", Store: seed(run.StatusSucceeded), Canceler: &fakeCanceler{ok: true},
			Path: "/v1/runs/run_1/cancel", WantStatus: http.StatusConflict,
		},
		{ // Test 2: An unknown run is not found.
			Name: "unknown", Store: run.NewMemStore(), Canceler: &fakeCanceler{ok: true},
			Path: "/v1/runs/none/cancel", WantStatus: http.StatusNotFound,
		},
		{ // Test 3: A run another process holds still accepts, the store carries the request.
			Name: "held elsewhere", Store: seed(run.StatusRunning), Canceler: &fakeCanceler{ok: false},
			Path: "/v1/runs/run_1/cancel", WantStatus: http.StatusAccepted,
		},
		{ // Test 4: No local canceler still accepts through the store.
			Name: "store only", Store: seed(run.StatusRunning), Canceler: nil,
			Path: "/v1/runs/run_1/cancel", WantStatus: http.StatusAccepted,
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
			store := run.NewMemStore()
			if err := store.Save(context.Background(), &run.Run{ID: "run_1", Tool: "ansible", Status: run.StatusFailed}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			handler := New(store, &fakeSubmitter{}, zap.NewNop(), opts...).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec,
				httptest.NewRequest(http.MethodPost, "/v1/runs/run_1/retry", nil))
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/schedules",
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/schedules", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "nightly") {
		t.Errorf("list = %d %q", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/schedules/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("get status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, "/v1/schedules/"+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Errorf("delete status = %d, want 200", rec.Code)
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/schedules/"+created.ID, nil))
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
				httptest.NewRequest(http.MethodPost, "/v1/schedules", strings.NewReader(test.Body)))
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
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/schedules", nil))
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

func TestAuthGate(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens)).Handler()

	get := func(path, bearer string) int {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Open mode: no tokens exist, everything passes.
	if code := get("/v1/runs", ""); code != http.StatusOK {
		t.Fatalf("open mode /runs = %d, want 200", code)
	}

	plain, tok, err := auth.New("ci")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	// The enforcement cache holds the open answer briefly; a fresh handler sees the token now.
	handler = New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens)).Handler()

	if code := get("/v1/runs", ""); code != http.StatusUnauthorized {
		t.Errorf("no token = %d, want 401", code)
	}
	if code := get("/v1/runs", "ymt_wrong"); code != http.StatusUnauthorized {
		t.Errorf("wrong token = %d, want 401", code)
	}
	if code := get("/v1/runs", plain); code != http.StatusOK {
		t.Errorf("good token = %d, want 200", code)
	}
	if code := get("/healthz", ""); code != http.StatusOK {
		t.Errorf("healthz = %d, want 200 exempt", code)
	}
	if code := get("/ui/", ""); code != http.StatusOK {
		t.Errorf("ui shell = %d, want 200 exempt", code)
	}

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/check", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("auth check without token = %d, want 401", rec.Code)
	}
	req := httptest.NewRequest(http.MethodPost, "/v1/auth/check", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Errorf("auth check with token = %d, want 204", rec.Code)
	}
}

func TestMetrics(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	for i, status := range []run.Status{run.StatusSucceeded, run.StatusSucceeded, run.StatusFailed} {
		if err := store.Save(context.Background(), &run.Run{
			ID: fmt.Sprintf("run_%d", i), Status: status, CreatedAt: time.Now(),
		}); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	handler := New(store, &fakeSubmitter{}, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`switchtender_runs_total{status="succeeded"} 2`,
		`switchtender_runs_total{status="failed"} 1`,
		"# TYPE switchtender_runs_total gauge",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics missing %q", want)
		}
	}
}

func TestAuthGateRoles(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()

	mint := func(role user.Role) string {
		u, err := user.New(string(role)+"-person", "pw", role)
		if err != nil {
			t.Fatalf("New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		plain, tok, err := auth.New("t-" + string(role))
		if err != nil {
			t.Fatalf("New token error = %v", err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("Save token error = %v", err)
		}
		return plain
	}
	viewer, operator, admin := mint(user.RoleViewer), mint(user.RoleOperator), mint(user.RoleAdmin)

	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users)).Handler()
	do := func(method, path, bearer string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"playbook":"p.yml"}`))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	tests := []struct {
		Method string
		Path   string
		Token  string
		Want   int
	}{
		{http.MethodGet, "/v1/runs", viewer, http.StatusOK},               // Test 0: Viewer reads.
		{http.MethodPost, "/v1/runs", viewer, http.StatusForbidden},       // Test 1: Viewer cannot launch.
		{http.MethodPost, "/v1/runs", operator, http.StatusAccepted},      // Test 2: Operator launches.
		{http.MethodPost, "/v1/projects", operator, http.StatusForbidden}, // Test 3: Operator cannot manage.
		{http.MethodPost, "/v1/projects", admin, http.StatusNotFound},     // Test 4: Admin passes the gate; feature off here.
		{http.MethodGet, "/v1/fleet", operator, http.StatusOK},            // Test 5: Operator reads.
		// A profile is personal data, so listing accounts takes an admin even though it is a read.
		{http.MethodGet, "/v1/users", viewer, http.StatusForbidden},   // Test 6: Viewer cannot list accounts.
		{http.MethodGet, "/v1/users", operator, http.StatusForbidden}, // Test 7: Operator cannot either.
		{http.MethodGet, "/v1/users", admin, http.StatusOK},           // Test 8: Admin lists accounts.
		{http.MethodGet, "/v1/audit", viewer, http.StatusForbidden},   // Test 9: The audit trail is admin-only to read.
	}
	for i, test := range tests {
		if got := do(test.Method, test.Path, test.Token); got != test.Want {
			t.Errorf("test %d: %s %s = %d, want %d", i, test.Method, test.Path, got, test.Want)
		}
	}
}

func TestAuthGateExpiredToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	plain, tok, err := auth.New("short-lived")
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	past := time.Now().Add(-time.Minute)
	tok.ExpiresAt = &past
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens)).Handler()
	req := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	req.Header.Set("Authorization", "Bearer "+plain)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expired token = %d, want 401", rec.Code)
	}
}

func TestReadOnlyRejectsMutations(t *testing.T) {
	t.Parallel()
	store := run.NewMemStore()
	handler := New(store, &fakeSubmitter{}, zap.NewNop(), WithReadOnly(true)).Handler()

	// A read passes through.
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/runs", nil))
	if rec.Code != http.StatusOK {
		t.Errorf("GET /runs status = %d, want 200", rec.Code)
	}

	// Every mutating method is refused with 403.
	for _, method := range []string{http.MethodPost, http.MethodDelete, http.MethodPut, http.MethodPatch} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(method, "/v1/runs", nil))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s /runs status = %d, want 403", method, rec.Code)
		}
	}
}

func TestMutationsAuthorizeReferencedObjects(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tokens := auth.NewMemStore()
	users := user.NewMemStore()
	grants := grant.NewMemStore()

	// mint creates a user with the given role and returns a bearer token and the user id.
	mint := func(name string, role user.Role) (string, string) {
		u, err := user.New(name, "pw", role)
		if err != nil {
			t.Fatalf("user.New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("users.Save() error = %v", err)
		}
		plain, tok, err := auth.New("t-" + name)
		if err != nil {
			t.Fatalf("auth.New() error = %v", err)
		}
		tok.UserID = u.ID
		if err := tokens.Save(ctx, tok); err != nil {
			t.Fatalf("tokens.Save() error = %v", err)
		}
		return plain, u.ID
	}
	ownerTok, ownerID := mint("owner", user.RoleOperator)
	otherTok, otherID := mint("other", user.RoleOperator)
	adminTok, _ := mint("boss", user.RoleAdmin)

	// A template both operators may use, but which references the secret credential only the owner
	// was granted. Launching it must still be gated on the credential, not just on the template.
	templates := template.NewMemStore()
	if err := templates.Save(ctx, &template.Template{
		ID: "tpl_secret", Name: "secret", Playbook: "p.yml",
		CredentialIDs: []string{"cred_secret"},
	}); err != nil {
		t.Fatalf("templates.Save() error = %v", err)
	}

	// The owner holds use on the secret credential, project, and template; the open credential has no
	// grant. Both operators may use the template, so the launch check turns on the credential grant.
	for _, object := range []string{"cred_secret", "proj_secret", "tpl_secret"} {
		if err := grants.Save(ctx, &grant.Grant{
			ID: grant.NewID(), Subject: ownerID, Object: object, Access: grant.AccessUse,
		}); err != nil {
			t.Fatalf("grants.Save() error = %v", err)
		}
	}
	if err := grants.Save(ctx, &grant.Grant{
		ID: grant.NewID(), Subject: otherID, Object: "tpl_secret", Access: grant.AccessUse,
	}); err != nil {
		t.Fatalf("grants.Save() error = %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_x"}}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithGrants(grants, false),
		WithTemplates(templates)).Handler()

	do := func(method, path, bearer, body string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+bearer)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Runs and pipelines are the mutation paths an operator may reach by role, so object grants are
	// enforced there. Inventory-source create is admin only at the role gate, so its object check is
	// defense in depth and not exercised through an operator here.
	runSecret := `{"playbook":"p.yml","credential_ids":["cred_secret"]}`
	runOpen := `{"playbook":"p.yml","credential_ids":["cred_open"]}`
	pipeSecret := `{"steps":[{"playbook":"p.yml"}],"project_id":"proj_secret"}`

	tests := []struct {
		Name   string
		Method string
		Path   string
		Token  string
		Body   string
		Want   int
	}{
		{"run other denied secret credential", http.MethodPost, "/v1/runs", otherTok, runSecret, http.StatusForbidden},         // Test 0.
		{"run owner uses granted credential", http.MethodPost, "/v1/runs", ownerTok, runSecret, http.StatusAccepted},           // Test 1.
		{"run admin bypasses grants", http.MethodPost, "/v1/runs", adminTok, runSecret, http.StatusAccepted},                   // Test 2.
		{"run ungranted credential defers to role", http.MethodPost, "/v1/runs", otherTok, runOpen, http.StatusAccepted},       // Test 3.
		{"pipeline other denied secret project", http.MethodPost, "/v1/pipelines", otherTok, pipeSecret, http.StatusForbidden}, // Test 4.
		{"pipeline owner uses granted project", http.MethodPost, "/v1/pipelines", ownerTok, pipeSecret, http.StatusAccepted},   // Test 5.
		{"launch other denied templated credential", http.MethodPost, "/v1/templates/tpl_secret/launch", otherTok, "",
			http.StatusForbidden}, // Test 6.
		{"launch owner uses granted credential", http.MethodPost, "/v1/templates/tpl_secret/launch", ownerTok, "",
			http.StatusAccepted}, // Test 7.
	}
	for i, test := range tests {
		if got := do(test.Method, test.Path, test.Token, test.Body); got != test.Want {
			t.Errorf("test %d (%s): status = %d, want %d", i, test.Name, got, test.Want)
		}
	}
}

// TestRunAccessScopedByGrant proves that under strict grants a viewer without a grant on a run's
// referenced objects cannot read it, and a matching grant restores access.
func TestRunAccessScopedByGrant(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	grants := grant.NewMemStore()
	runs := run.NewMemStore()

	viewer, err := user.New("viewer", "pw", user.RoleViewer)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, viewer); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	plain, tok, err := auth.New("t-viewer")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = viewer.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}
	if err := runs.Save(ctx, &run.Run{ID: "run_scoped", Tool: "ansible", Status: run.StatusFailed, ProjectID: "proj_secret"}); err != nil {
		t.Fatalf("runs.Save() error = %v", err)
	}

	handler := New(runs, &fakeSubmitter{}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithGrants(grants, true)).Handler()
	get := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, "/v1/runs/run_scoped", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	// Without a grant on the run's project, the viewer is denied under strict grants.
	if code := get(); code != http.StatusForbidden {
		t.Errorf("ungranted read = %d, want 403", code)
	}
	// A grant on the project restores read access.
	if err := grants.Save(ctx, &grant.Grant{
		ID: grant.NewID(), Subject: viewer.ID, Object: "proj_secret", Access: grant.AccessUse,
	}); err != nil {
		t.Fatalf("grants.Save() error = %v", err)
	}
	if code := get(); code != http.StatusOK {
		t.Errorf("granted read = %d, want 200", code)
	}
}

// TestRerunRun verifies a finished run reruns with its spec replayed and the refusals hold.
func TestRerunRun(t *testing.T) {
	t.Parallel()
	shard0 := 0
	three := 3
	parent := "run_parent"
	tests := []struct {
		Saved            *run.Run
		WantStatus       int
		WantBodyContains string
		WantShards       int
	}{{ // Test 0: A finished plain run reruns with its spec.
		Saved: &run.Run{
			ID: "run_1", Playbook: "site.yml", Inventory: "inv.ini", Status: run.StatusFailed,
			Tool: "ansible", Limit: "web*", DryRun: true, CredentialIDs: []string{"cred_1"},
		},
		WantStatus: http.StatusAccepted, WantBodyContains: "run_new",
	}, { // Test 1: A split parent reruns as a new split.
		Saved: &run.Run{
			ID: "run_1", Playbook: "site.yml", Inventory: "inv.ini", Status: run.StatusFailed,
			Kind: run.KindSplit, ShardCount: &three,
		},
		WantStatus: http.StatusAccepted, WantBodyContains: "run_new", WantShards: 3,
	}, { // Test 2: A running run refuses.
		Saved: &run.Run{
			ID: "run_1", Playbook: "site.yml", Inventory: "inv.ini", Status: run.StatusRunning,
		},
		WantStatus: http.StatusConflict, WantBodyContains: "has not finished",
	}, { // Test 3: A shard child refuses.
		Saved: &run.Run{
			ID: "run_1", Playbook: "site.yml", Inventory: "inv.ini", Status: run.StatusFailed,
			ParentID: &parent, ShardIndex: &shard0,
		},
		WantStatus: http.StatusConflict, WantBodyContains: "rerun the parent",
	}, { // Test 4: A pipeline parent refuses.
		Saved: &run.Run{
			ID: "run_1", Playbook: "release", Inventory: "inv.ini", Status: run.StatusFailed,
			Kind: run.KindPipeline,
		},
		WantStatus: http.StatusConflict, WantBodyContains: "from its workflow",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			store := run.NewMemStore()
			if err := store.Save(context.Background(), test.Saved); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
			handler := New(store, sub, zap.NewNop()).Handler()
			rec := httptest.NewRecorder()
			handler.ServeHTTP(rec,
				httptest.NewRequest(http.MethodPost, "/v1/runs/run_1/rerun", nil))
			if rec.Code != test.WantStatus {
				t.Fatalf("status = %d, want %d, body %s", rec.Code, test.WantStatus, rec.Body.String())
			}
			if !strings.Contains(rec.Body.String(), test.WantBodyContains) {
				t.Errorf("body %q does not contain %q", rec.Body.String(), test.WantBodyContains)
			}
			if test.WantShards != 0 && sub.gotShards != test.WantShards {
				t.Errorf("shards = %d, want %d", sub.gotShards, test.WantShards)
			}
			if test.WantStatus == http.StatusAccepted && test.WantShards == 0 {
				if sub.gotRun == nil || sub.gotRun.Limit != test.Saved.Limit || sub.gotRun.DryRun != test.Saved.DryRun {
					t.Errorf("submitted spec = %+v, want limit %q dry %v", sub.gotRun, test.Saved.Limit, test.Saved.DryRun)
				}
				if sub.gotRun != nil && (sub.gotRun.Source != "rerun" || sub.gotRun.RerunOf != "run_1") {
					t.Errorf("provenance = %q %q, want rerun run_1", sub.gotRun.Source, sub.gotRun.RerunOf)
				}
			}
		})
	}
}

// TestSchedulePreview verifies the cron preview returns firings and rejects bad specs.
func TestSchedulePreview(t *testing.T) {
	t.Parallel()
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop()).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/schedules/preview?cron=0+2+*+*+*", nil))
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "next") {
		t.Fatalf("preview = %d %s, want 200 with next", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/schedules/preview?cron=nope", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid preview = %d, want 400", rec.Code)
	}
}

// TestLaunchOverrides verifies prompt-on-launch overrides reach the submitted run and the
// inventory override cannot dodge validation.
func TestLaunchOverrides(t *testing.T) {
	t.Parallel()
	store := template.NewMemStore()
	tpl := &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml", Inventory: "inv.ini",
		ExtraVars: map[string]any{"env": "prod"},
	}
	if err := store.Save(context.Background(), tpl); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
	handler := New(run.NewMemStore(), sub, zap.NewNop(), WithTemplates(store)).Handler()

	rec := httptest.NewRecorder()
	body := `{"limit":"web*","dry_run":true,"extra_vars":{"env":"stage","extra":1}}`
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost,
		"/v1/templates/"+tpl.ID+"/launch", strings.NewReader(body)))
	if rec.Code != http.StatusCreated && rec.Code != http.StatusAccepted {
		t.Fatalf("launch = %d, body %s", rec.Code, rec.Body.String())
	}
	if sub.gotRun == nil {
		t.Fatal("no submitted run recorded")
	}
	if sub.gotRun.Limit != "web*" || !sub.gotRun.DryRun {
		t.Errorf("overrides = limit %q dry %v, want web* true", sub.gotRun.Limit, sub.gotRun.DryRun)
	}
	if sub.gotRun.ExtraVars["env"] != "stage" {
		t.Errorf("extra_vars env = %v, want stage (launch overrides template)", sub.gotRun.ExtraVars["env"])
	}
	if sub.gotRun.Source != "template" || sub.gotRun.SourceID != tpl.ID {
		t.Errorf("provenance = %q %q, want template %s", sub.gotRun.Source, sub.gotRun.SourceID, tpl.ID)
	}
}

// TestDoctor verifies broken references, dead schedules, and secretless credentials surface, and
// a clean control plane reports an all-clear with real counts.
func TestDoctor(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tpls := template.NewMemStore()
	if err := tpls.Save(ctx, &template.Template{
		ID: "tpl_1", Name: "deploy", Playbook: "site.yml",
		InventoryID: "inv_gone", CredentialIDs: []string{"cred_gone"},
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	scheds := schedule.NewMemStore()
	if err := scheds.Save(ctx, &schedule.Schedule{
		ID: "sch_1", Name: "nightly", Cron: "not a cron", TemplateID: "tpl_gone",
	}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTemplates(tpls), WithSchedules(scheds), WithInventories(inventory.NewMemStore()),
		WithProjects(project.NewMemStore()), WithCredentials(credential.NewMemStore(), nil)).Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/v1/doctor", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("doctor = %d, body %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	for _, want := range []string{"inv_gone", "cred_gone", "does not parse", "tpl_gone", "\"checked_templates\":1"} {
		if !strings.Contains(body, want) {
			t.Errorf("doctor body missing %q in %s", want, body)
		}
	}
}

// dedupeSubmitter saves each submission into a store under whatever idempotency key it carries,
// the way the dispatcher does, so a handler test sees the dedupe the store's unique key provides.
type dedupeSubmitter struct {
	// store receives every accepted submission.
	store run.Store
	// calls counts how many submissions reached it.
	calls int
}

// Submit records the call and saves the run, returning the run already holding its key when one does.
func (d *dedupeSubmitter) Submit(ctx context.Context, playbook, inventory string, opts ...run.SubmitOption) (*run.Run, error) {
	d.calls++
	r := &run.Run{
		ID: run.NewID(), Playbook: playbook, Inventory: inventory,
		Status: run.StatusPending, CreatedAt: time.Now(),
	}
	run.ApplyOptions(r, opts)
	if r.IdempotencyKey != "" {
		if existing, err := d.store.ByIdempotencyKey(ctx, r.IdempotencyKey); err == nil {
			return existing, nil
		}
	}
	if err := d.store.Save(ctx, r); err != nil {
		return nil, err
	}
	return r, nil
}

// SubmitSplit is not exercised by the dedupe test and refuses to run.
func (d *dedupeSubmitter) SubmitSplit(context.Context, string, string, int, ...run.SubmitOption) (*run.Run, error) {
	return nil, errors.New("not used")
}

// SubmitPipeline is not exercised by the dedupe test and refuses to run.
func (d *dedupeSubmitter) SubmitPipeline(context.Context, string, string, []run.PipelineStep, ...run.SubmitOption) (*run.Run, error) {
	return nil, errors.New("not used")
}

// TestRerunRunDedupes proves clicking rerun twice starts one run rather than two: both requests
// answer with the same run and only one rerun of the original exists afterwards.
func TestRerunRunDedupes(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	store := run.NewMemStore()
	saved := &run.Run{
		ID: "run_1", Playbook: "site.yml", Inventory: "inv.ini", Status: run.StatusFailed,
	}
	if err := store.Save(ctx, saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &dedupeSubmitter{store: store}
	handler := New(store, sub, zap.NewNop()).Handler()

	ids := make([]string, 2)
	for i := range ids {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_1/rerun", nil))
		if rec.Code != http.StatusAccepted {
			t.Fatalf("click %d status = %d, want %d, body %s",
				i, rec.Code, http.StatusAccepted, rec.Body.String())
		}
		var got run.Run
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("click %d decode: %v", i, err)
		}
		ids[i] = got.ID
	}
	if ids[0] != ids[1] {
		t.Errorf("second click returned %s, want the first click's run %s", ids[1], ids[0])
	}

	all, err := store.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	reruns := 0
	for _, r := range all {
		if r.RerunOf == "run_1" {
			reruns++
		}
	}
	if reruns != 1 {
		t.Errorf("reruns of run_1 = %d, want 1", reruns)
	}
}

// TestRerunReplaysEveryExecutionField pins that a rerun carries the whole spec of the run it
// replays, and fails when a field is added to run.Run without a decision about reruns.
//
// The list of fields a rerun replayed was written by hand and drifted from the run model, dropping
// the timeout and the notification targets: the rerun executed under the dispatcher's default cap
// instead of the one the run was given, and its terminal state reached the server-wide channels but
// not the team the original run paged. The diff below is against the saved run itself, so a new
// field either gets replayed or gets named in the ignore list.
func TestRerunReplaysEveryExecutionField(t *testing.T) {
	t.Parallel()
	saved := &run.Run{
		ID: "run_1", Playbook: "site.yml", Inventory: "inv.ini", Status: run.StatusFailed,
		Tool: "ansible", Command: "deploy.sh", DryRun: true, Limit: "web*",
		CredentialIDs: []string{"cred_1", "cred_2"}, ProjectID: "prj_1", InventoryID: "inv_1",
		Queue: "prod", Image: "registry.example/exec:1", PullCredentialID: "cred_pull",
		ExtraVars: map[string]any{"release": "9.2"}, Labels: map[string]string{"env": "prod"},
		Timeout: 900,
		Notifications: []run.NotifyTarget{
			{Kind: "slack", URL: "https://hooks.example/team", OnFailure: true},
		},
	}
	store := run.NewMemStore()
	if err := store.Save(context.Background(), saved); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
	handler := New(store, sub, zap.NewNop()).Handler()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/runs/run_1/rerun", nil))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d, body %s", rec.Code, http.StatusAccepted, rec.Body.String())
	}
	if sub.gotRun == nil {
		t.Fatal("no run was submitted")
	}
	// Everything a rerun is expected to differ on: its own identity and lifecycle, the provenance
	// that marks it a rerun, the dedupe key, and the shape fields that belong to the original.
	ignored := cmpopts.IgnoreFields(run.Run{},
		"ID", "Status", "CreatedAt", "StartedAt", "EndedAt", "ExitCode", "Error", "Warning",
		"Source", "SourceID", "Actor", "RerunOf", "IdempotencyKey", "ClaimedBy", "ClaimedAt",
		"CancelRequested", "Outputs", "CommitSHA", "Risk", "ParentID", "ShardIndex", "ShardCount",
		"Kind", "RetryOf", "Steps", "StepName", "StepIndex", "Attempt", "ProposedFrom", "Intent")
	if diff := cmp.Diff(saved, sub.gotRun, ignored, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("the rerun does not replay the run it reruns (-saved +submitted):\n%s", diff)
	}
}

// TestRunObjectsCoversThePullCredential pins that the registry credential pulling a run's execution
// image is one of the objects a caller has to be granted.
//
// It was added to the run model and to the fields a retry inherits without widening the object
// list, so every path built on runObjects, including reading a run and retrying its failed shards,
// let an actor use a registry credential they were never granted. The rerun handler named the field
// by hand and refused, which is how the two paths came to disagree.
func TestRunObjectsCoversThePullCredential(t *testing.T) {
	t.Parallel()
	rn := &run.Run{
		ID: "run_1", ProjectID: "prj_1", InventoryID: "inv_1",
		Image: "registry.example/exec:1", PullCredentialID: "cred_pull",
		CredentialIDs: []string{"cred_1"},
	}
	want := []string{"prj_1", "inv_1", "cred_pull", "cred_1"}
	if diff := cmp.Diff(want, runObjects(rn), cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
		t.Errorf("the objects a run uses (-want +got):\n%s", diff)
	}
}

// TestRetryAuthorizesThePullCredential pins that retrying a run's failed shards is refused when the
// actor is not granted the registry credential the retry would pull its execution image with.
func TestRetryAuthorizesThePullCredential(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	grants := grant.NewMemStore()
	runs := run.NewMemStore()

	operator, err := user.New("operator", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, operator); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	plain, tok, err := auth.New("t-operator")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = operator.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}
	three := 3
	if err := runs.Save(ctx, &run.Run{
		ID: "run_split", Tool: "ansible", Kind: run.KindSplit, ShardCount: &three,
		Status: run.StatusFailed, ProjectID: "prj_open",
		Image: "registry.example/exec:1", PullCredentialID: "cred_private",
	}); err != nil {
		t.Fatalf("runs.Save() error = %v", err)
	}
	// The actor is granted everything the run touches except the registry credential.
	if err := grants.Save(ctx, &grant.Grant{
		ID: grant.NewID(), Subject: operator.ID, Object: "prj_open", Access: grant.AccessUse,
	}); err != nil {
		t.Fatalf("grants.Save() error = %v", err)
	}

	handler := New(runs, &fakeSubmitter{}, zap.NewNop(), WithTokens(tokens), WithUsers(users),
		WithGrants(grants, true),
		WithRetrier(&fakeRetrier{run: &run.Run{ID: "run_retry", Status: run.StatusPending}})).Handler()
	retry := func() int {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodPost, "/v1/runs/run_split/retry", nil)
		req.Header.Set("Authorization", "Bearer "+plain)
		handler.ServeHTTP(rec, req)
		return rec.Code
	}

	if code := retry(); code != http.StatusForbidden {
		t.Errorf("retry without a grant on the registry credential = %d, want 403: the retry "+
			"inherits PullCredentialID, so it pulls an image the actor may not use", code)
	}
	if err := grants.Save(ctx, &grant.Grant{
		ID: grant.NewID(), Subject: operator.ID, Object: "cred_private", Access: grant.AccessUse,
	}); err != nil {
		t.Fatalf("grants.Save() error = %v", err)
	}
	if code := retry(); code == http.StatusForbidden {
		t.Error("retry with a grant on the registry credential is still refused")
	}
}

// failingAudits is an audit store whose appends always fail, standing in for a full disk or a
// locked database.
type failingAudits struct{ err error }

// Append always fails.
func (f *failingAudits) Append(context.Context, *audit.Entry) error { return f.err }

// AppendSpanBeat always fails.
func (f *failingAudits) AppendSpanBeat(context.Context, time.Time, int) (*audit.Entry, error) {
	return nil, f.err
}

// SpanBeats returns nothing.
func (f *failingAudits) SpanBeats(context.Context, int) ([]*audit.Entry, error) { return nil, nil }

// List returns nothing.
func (f *failingAudits) List(context.Context, int) ([]*audit.Entry, error) { return nil, nil }

// Chain returns nothing.
func (f *failingAudits) Chain(context.Context) ([]*audit.Entry, error) { return nil, nil }

// ChainScan streams nothing.
func (f *failingAudits) ChainScan(context.Context, int64, func(*audit.Entry) error) error { return nil }

// TestMutationRefusedWhenItCannotBeAudited pins that a change which cannot be written to the audit
// trail does not happen and is not reported as done.
//
// The append used to run in a goroutine whose error was logged and dropped, so a store failure meant
// the mutation succeeded, returned 200, and left no trace. The chain showed no gap either: a
// sequence number is assigned at append, so an entry that was never appended leaves no hole for
// anyone to notice. For a product whose claim is a provable record of what happened, silently not
// recording is the worst available outcome.
func TestMutationRefusedWhenItCannotBeAudited(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	runs := run.NewMemStore()

	admin, err := user.New("admin", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	plain, tok, err := auth.New("t-admin")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = admin.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}

	sub := &fakeSubmitter{run: &run.Run{ID: "run_new", Status: run.StatusPending}}
	handler := New(runs, sub, zap.NewNop(), WithTokens(tokens), WithUsers(users),
		WithAudit(&failingAudits{err: errors.New("disk full")})).Handler()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"playbook":"site.yml","inventory":"hosts.ini"}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503: a change that cannot be recorded must be refused, not "+
			"performed silently", rec.Code)
	}
	if sub.gotRun != nil {
		t.Error("the run was submitted even though the audit entry could not be written, so the " +
			"change happened with no record of it")
	}
}

// TestMutationReturnsAnAuditReceipt pins that a recorded mutation hands the caller its chain
// position, so a receipt holder can later demand that a chain contain it.
//
// A chain proves that what it holds was not altered. It cannot prove nothing is missing, because
// the same process decides both what happens and what gets written down. A receipt is what moves
// that from the server's word to the holder's evidence.
func TestMutationReturnsAnAuditReceipt(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	audits := audit.NewMemStore()

	admin, err := user.New("admin", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	plain, tok, err := auth.New("t-admin")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = admin.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "run_new"}}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithAudit(audits)).Handler()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"playbook":"site.yml","inventory":"hosts.ini"}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)

	receipt := rec.Header().Get(AuditReceiptHeader)
	if receipt == "" {
		t.Fatalf("no %s header on a recorded mutation", AuditReceiptHeader)
	}
	// The receipt has to name a link that is actually in the chain, or it proves nothing.
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 1 {
		t.Fatalf("chain holds %d entries, want 1", len(chain))
	}
	want := strconv.FormatInt(chain[0].Seq, 10) + ":" + chain[0].Hash
	if receipt != want {
		t.Errorf("receipt = %q, want %q: a receipt that does not name a real chain link cannot be "+
			"redeemed against the chain", receipt, want)
	}
	// A read is not a mutation and gets no receipt.
	readRec := httptest.NewRecorder()
	readReq := httptest.NewRequest(http.MethodGet, "/v1/runs", nil)
	readReq.Header.Set("Authorization", "Bearer "+plain)
	handler.ServeHTTP(readRec, readReq)
	if got := readRec.Header().Get(AuditReceiptHeader); got != "" {
		t.Errorf("a read returned receipt %q, want none", got)
	}
}

// TestRecordedEntryCarriesActorTypeAndContentDigest drives the audit chokepoint over the real HTTP
// path and checks the fields the chain now commits to: the entry records how the caller authenticated
// (a token), the account the token is bound to, and a digest of the change payload. It also proves a
// secret in the body does not enter that digest.
func TestRecordedEntryCarriesActorTypeAndContentDigest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	audits := audit.NewMemStore()
	creds := credential.NewMemStore()

	operator, err := user.New("deploy-agent", "pw", user.RoleOperator)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, operator); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	plain, tok, err := auth.New("agent")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = operator.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}
	// A second operator token must exist so the operator has grant to create a credential, which the
	// role gate allows for an operator only via a manage grant; simplest is an admin token minting.
	sealer := credential.NewSealer("k", "s")
	handler := New(run.NewMemStore(), &fakeSubmitter{run: &run.Run{ID: "r"}}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithAudit(audits),
		WithCredentials(creds, sealer)).Handler()

	// The operator submits a run; the entry should record the token authentication and the bound
	// account, and a digest of the body.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/runs",
		strings.NewReader(`{"playbook":"site.yml"}`))
	req.Header.Set("Authorization", "Bearer "+plain)
	req.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated && rec.Code != http.StatusOK && rec.Code != http.StatusAccepted {
		t.Fatalf("submit status = %d (body %s)", rec.Code, rec.Body.String())
	}
	chain, err := audits.Chain(ctx)
	if err != nil || len(chain) == 0 {
		t.Fatalf("Chain() = %v, %v", chain, err)
	}
	e := chain[len(chain)-1]
	if e.ActorType != actorTypeToken {
		t.Errorf("actor_type = %q, want %q", e.ActorType, actorTypeToken)
	}
	if e.OnBehalfOf != "deploy-agent" {
		t.Errorf("on_behalf_of = %q, want the bound account deploy-agent", e.OnBehalfOf)
	}
	if e.ContentDigest == "" {
		t.Error("the run submission recorded no content digest")
	}
	// The digest must be committed by the link.
	if audit.EntryHash(e) != e.Hash {
		t.Error("the recorded entry does not hash to its stored link")
	}
}

// TestWebhookSecretNeverReachesTheAuditChain pins that a webhook's token stays out of the audit
// trail.
//
// A webhook authenticates by a secret in its path, and the trigger store keeps only that secret's
// SHA-256 precisely because the token itself must never persist. Recording the raw path put it
// back, hash-linked into a chain that cannot be redacted without breaking verification, and carried
// into every bundle handed to a third party. A failed probe of a guessed path would have been
// written down just as permanently.
func TestWebhookSecretNeverReachesTheAuditChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	audits := audit.NewMemStore()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()

	// An install with at least one token is enforcing, which is when unauthenticated paths matter.
	admin, err := user.New("admin", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	_, tok, err := auth.New("t-admin")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = admin.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}

	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithAudit(audits)).Handler()

	const secret = "whsec_super_secret_value"
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/hooks/"+secret, nil))

	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	// A probe of a guessed token records nothing. The path is the credential, so anybody on the
	// network can present a guess, and each one used to write a permanent hash-linked entry. Fifty
	// probes made fifty entries, and since the append is fail-closed, filling the store that way
	// refuses every real mutation in the install. A hook that resolves to a trigger is recorded by
	// the handler instead, where the trigger is known.
	if len(chain) != 0 {
		t.Errorf("a guessed webhook token appended %d entries, so a stranger can fill the chain",
			len(chain))
	}
	for _, e := range chain {
		if strings.Contains(e.Path, secret) {
			t.Errorf("the webhook secret is in the audit chain at seq %d: %q. It is hash-linked, "+
				"so it cannot be removed without breaking verification, and it travels in every "+
				"bundle handed to a third party", e.Seq, e.Path)
		}
	}

	// Redaction keys on the path, not the method, so a mistyped verb cannot write the token either.
	for _, method := range []string{http.MethodPut, http.MethodDelete, http.MethodPatch} {
		probe := httptest.NewRequest(method, "/hooks/"+secret, nil)
		if got := auditPath(probe); strings.Contains(got, secret) {
			t.Errorf("%s on a hook path records %q, embedding a live webhook token in the chain",
				method, got)
		}
	}
	if got := auditPath(httptest.NewRequest(http.MethodPost, "//HOOKS/"+secret, nil)); strings.Contains(got, secret) {
		t.Errorf("an oddly spelled hook path records %q", got)
	}
}

// TestSignInDoesNotAppendToTheAuditChain pins that authentication attempts stay out of the chain.
//
// Sign-in mints no run and changes no configuration, but it is reachable by anyone on the network.
// Recording each attempt let a stranger append without bound to the structure the integrity story
// rests on, and because the append is fail-closed, an unhealthy audit store then locked every
// account out of signing in.
func TestSignInDoesNotAppendToTheAuditChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	audits := audit.NewMemStore()
	users := user.NewMemStore()
	tokens := auth.NewMemStore()
	admin, err := user.New("admin", "pw", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	if err := users.Save(ctx, admin); err != nil {
		t.Fatalf("users.Save() error = %v", err)
	}
	_, tok, err := auth.New("t-admin")
	if err != nil {
		t.Fatalf("auth.New() error = %v", err)
	}
	tok.UserID = admin.ID
	if err := tokens.Save(ctx, tok); err != nil {
		t.Fatalf("tokens.Save() error = %v", err)
	}
	handler := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTokens(tokens), WithUsers(users), WithAudit(audits)).Handler()

	for i := 0; i < 25; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/auth/login",
			strings.NewReader(`{"username":"admin","password":"wrong"}`)))
	}
	chain, err := audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if len(chain) != 0 {
		t.Errorf("25 failed sign-ins appended %d entries, so anyone reachable on the network can "+
			"grow the audit chain without bound", len(chain))
	}
}
