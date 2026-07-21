package extplugin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/ai"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/secretsource"
	"github.com/kordloom/switchtender/internal/server"
)

// buildPlugin compiles the test extension binary into a fresh directory and returns that
// directory, so each test run loads exactly one plugin.
func buildPlugin(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	cmd := exec.Command("go", "build", "-o", filepath.Join(dir, "helloplugin"), "./testdata/helloplugin")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build test plugin: %v\n%s", err, out)
	}
	return dir
}

// TestLoadEmptyDir confirms an empty directory value loads nothing and returns a working close.
func TestLoadEmptyDir(t *testing.T) {
	t.Parallel()
	closePlugins, err := Load("", zap.NewNop())
	if err != nil {
		t.Fatalf("Load(\"\") error: %v", err)
	}
	closePlugins()
}

// TestLoadMissingDir confirms a directory that cannot be read fails startup with ErrLoad.
func TestLoadMissingDir(t *testing.T) {
	t.Parallel()
	if _, err := Load(filepath.Join(t.TempDir(), "absent"), zap.NewNop()); !errors.Is(err, ErrLoad) {
		t.Errorf("Load(missing dir) error = %v, want ErrLoad", err)
	}
}

// TestLoadSkipsBrokenEntries confirms a non-executable file and an executable that is not a
// plugin are both skipped without failing the load.
func TestLoadSkipsBrokenEntries(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("not a plugin"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "impostor"), []byte("#!/bin/sh\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	closePlugins, err := Load(dir, zap.NewNop())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	closePlugins()
}

// TestLoadRegistersEverySeam builds the real plugin binary, loads it, and drives every seam
// across the process boundary: the tool through the router and the full HTTP server, the
// notifier through the dispatcher with redaction observed from inside the plugin, and the AI
// provider and both secret engines through their packages. It does not call t.Parallel: it
// writes every registry and sets an environment variable the plugin process inherits.
//
//nolint:funlen // One load drives all five seams; splitting would rebuild and reload per seam.
func TestLoadRegistersEverySeam(t *testing.T) {
	notifyFile := filepath.Join(t.TempDir(), "notified")
	t.Setenv("EXTTEST_NOTIFY_FILE", notifyFile)

	closePlugins, err := Load(buildPlugin(t), zap.NewNop())
	if err != nil {
		t.Fatalf("Load error: %v", err)
	}
	defer closePlugins()

	// Seam 1: the tool validates and routes, with output, extra vars, dry run, and exit code
	// crossing the wire.
	if !run.ValidTool("exttest-hello") {
		t.Fatal("ValidTool(exttest-hello) = false, want the plugin tool accepted")
	}
	var out bytes.Buffer
	res, err := roundhouse.NewAnsibleRunner().Run(context.Background(), roundhouse.Spec{
		Tool: "exttest-hello", Command: "ping", DryRun: true,
		ExtraVars: map[string]any{"exit": 7},
	}, &out)
	if err != nil {
		t.Fatalf("router Run error: %v", err)
	}
	if res.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7 from the plugin's extra var", res.ExitCode)
	}
	if !strings.Contains(out.String(), "plugin says: ping") || !strings.Contains(out.String(), "dry run") {
		t.Errorf("output = %q, want the plugin's command echo and dry run note", out.String())
	}

	// Seam 2: the static secret source resolves through the plugin.
	if got, err := secretsource.Resolve(context.Background(), "exttest-static", "cfg"); err != nil || got != "static:cfg" {
		t.Errorf("Resolve = %q, %v, want static:cfg", got, err)
	}

	// Seam 3: the dynamic secret source mints through the plugin and its lease revokes once.
	value, lease, err := secretsource.ResolveLeased(context.Background(), "exttest-dyn", "role")
	if err != nil || value != "minted:role" {
		t.Fatalf("ResolveLeased = %q, %v, want minted:role", value, err)
	}
	if lease.Kind() != "exttest-dyn" {
		t.Errorf("lease kind = %q, want exttest-dyn", lease.Kind())
	}
	if err := lease.Revoke(context.Background()); err != nil {
		t.Fatalf("first Revoke error: %v", err)
	}
	if err := lease.Revoke(context.Background()); err == nil {
		t.Error("second Revoke succeeded, want an unknown-lease error proving plugin lease state")
	}

	// Seam 4: the AI provider completes through the plugin with settings passed through.
	provider, err := ai.New("exttest-ai", "test-model", "", "")
	if err != nil {
		t.Fatalf("ai.New error: %v", err)
	}
	reply, err := provider.Complete(context.Background(), "sys", "user")
	if err != nil {
		t.Fatalf("Complete error: %v", err)
	}
	if want := "ai:test-model:sys:user"; reply != want {
		t.Errorf("Complete = %q, want %q", reply, want)
	}

	// Seam 5: a run submitted over HTTP executes on the plugin tool and its terminal state
	// reaches the plugin notifier with extra vars redacted.
	store := run.NewMemStore()
	d := dispatch.New(store, roundhouse.NewAnsibleRunner(), zap.NewNop())
	defer d.Close()
	srv := httptest.NewServer(server.New(store, d, zap.NewNop()).Handler())
	defer srv.Close()

	resp, err := http.Post(srv.URL+"/v1/runs", "application/json",
		strings.NewReader(`{"tool":"exttest-hello","command":"end-to-end","extra_vars":{"exit":0}}`))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("submit status = %d, want 202 (body %s)", resp.StatusCode, body)
	}
	var submitted struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &submitted); err != nil {
		t.Fatalf("decode submit reply: %v", err)
	}

	status := waitTerminal(t, srv.URL, submitted.ID)
	if status != string(run.StatusSucceeded) {
		t.Fatalf("run status = %q, want succeeded", status)
	}
	note := waitFile(t, notifyFile)
	if want := submitted.ID + "|0"; note != want {
		t.Errorf("notifier recorded %q, want %q (run id with extra vars redacted)", note, want)
	}
}

// waitTerminal polls the run over the API until it reaches a terminal status or the deadline
// passes, and returns the final status.
func waitTerminal(t *testing.T, baseURL, id string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/v1/runs/" + id)
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		var view struct {
			Status string `json:"status"`
		}
		err = json.NewDecoder(resp.Body).Decode(&view)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("decode run: %v", err)
		}
		if run.Status(view.Status).Terminal() {
			return view.Status
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never reached a terminal status, last %q", id, view.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// waitFile polls for the notifier's file until it appears or the deadline passes, and returns its
// contents.
func waitFile(t *testing.T, path string) string {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			return string(data)
		}
		if time.Now().After(deadline) {
			t.Fatalf("notifier file %s never appeared", path)
		}
		time.Sleep(50 * time.Millisecond)
	}
}
