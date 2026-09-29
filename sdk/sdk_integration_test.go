// This file drives an SDK-registered tool through the full server stack: submitted over HTTP,
// validated, executed by the real dispatcher, and read back through the status and log endpoints.
// It is the in-process proof of the plugin story a third-party extension relies on.
package sdk_test

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/sdk"
)

// runView is the slice of the run JSON the test reads back from the API.
type runView struct {
	// ID is the run identifier assigned at submit.
	ID string `json:"id"`
	// Status is the run's lifecycle state.
	Status string `json:"status"`
	// ExitCode is the process exit code once the run is terminal.
	ExitCode int `json:"exit_code"`
}

// TestRegisteredToolEndToEnd registers a tool through the SDK and confirms a run naming it is
// accepted by the API, executed by the dispatcher, and lands with the runner's output in the run
// log and its exit code on the run. It does not call t.Parallel: it writes the tool registries.
func TestRegisteredToolEndToEnd(t *testing.T) {
	sdk.RegisterTool("sdkext-e2e", sdk.ToolRunnerFunc(
		func(_ context.Context, spec sdk.ToolSpec, out io.Writer) (sdk.ToolResult, error) {
			_, _ = fmt.Fprintf(out, "e2e plugin output: %s\n", spec.Command)
			return sdk.ToolResult{ExitCode: 0}, nil
		}))

	store := run.NewMemStore()
	d := dispatch.New(store, roundhouse.NewAnsibleRunner(), zap.NewNop())
	defer d.Close()
	srv := httptest.NewServer(server.New(store, d, zap.NewNop()).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/runs", "application/json",
		strings.NewReader(`{"tool":"sdkext-e2e","command":"ping"}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusAccepted {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("submit status = %d, want 202 (body %s)", resp.StatusCode, body)
	}
	var submitted runView
	if err := json.NewDecoder(resp.Body).Decode(&submitted); err != nil {
		t.Fatalf("decode submit reply: %v", err)
	}

	final := waitTerminal(t, srv.URL, submitted.ID)
	if final.Status != string(run.StatusSucceeded) {
		t.Fatalf("status = %q, want %q", final.Status, run.StatusSucceeded)
	}
	if final.ExitCode != 0 {
		t.Errorf("exit code = %d, want 0", final.ExitCode)
	}

	log := fetchLog(t, srv.URL, submitted.ID)
	if !strings.Contains(log, "e2e plugin output: ping") {
		t.Errorf("log = %q, want the registered runner's output", log)
	}
}

// waitTerminal polls the run over the API until it reaches a terminal status or the deadline
// passes, and returns the final view.
func waitTerminal(t *testing.T, baseURL, id string) runView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/v1/runs/" + id)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		var view runView
		err = json.NewDecoder(resp.Body).Decode(&view)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("decode run: %v", err)
		}
		if run.Status(view.Status).Terminal() {
			return view
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never reached a terminal status, last %q", id, view.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// fetchLog returns the run's captured output from the log endpoint.
func fetchLog(t *testing.T, baseURL, id string) string {
	t.Helper()
	resp, err := http.Get(baseURL + "/v1/runs/" + id + "/logs")
	if err != nil {
		t.Fatalf("get log: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	return string(body)
}
