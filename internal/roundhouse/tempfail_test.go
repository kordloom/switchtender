//go:build unix

package roundhouse

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestEveryTempFileFailureIsReportedAsALaunchError drives the branches that only run when the
// executor cannot write a temporary file, by pointing TMPDIR at a path that does not exist.
//
// These are the paths where a run's inline script, its extra variables, its environment file, and
// the Ansible callback plugin are materialized. Each one must refuse the run: a script file that
// silently failed to appear would launch the interpreter against a path that is not there, and an
// environment file that silently failed to appear would run the tool with none of its credentials
// while still reporting whatever the tool then did.
//
//nolint:funlen // Test function.
func TestEveryTempFileFailureIsReportedAsALaunchError(t *testing.T) {
	// No t.Parallel: it sets TMPDIR, which is process-global, and t.TempDir reads it.
	missing := filepath.Join(os.TempDir(), "switchtender-absent-tmpdir", "deeper")
	t.Setenv("TMPDIR", missing)

	t.Run("test 0", func(t *testing.T) {
		// Test 0: An inline script that cannot be written is a launch error, not a silent no-op.
		if _, _, err := writeScriptFile("switchtender-x-*.sh", "echo hi"); !errors.Is(err, ErrLaunch) {
			t.Errorf("writeScriptFile() error = %v, want ErrLaunch", err)
		}
	})

	t.Run("test 1", func(t *testing.T) {
		// Test 1: Extra vars that cannot be written off argv fail the run rather than running
		// without a variable that may gate a destructive task.
		spec := Spec{Playbook: "p.yml", ExtraVars: map[string]any{"k": "v"}}
		cleanup, err := materializeExtraVars(&spec)
		cleanup()
		if err == nil {
			t.Fatal("materializeExtraVars() = nil error, want the run refused")
		}
		if len(spec.ExtraVarsFiles) != 0 || spec.ExtraVars == nil {
			t.Errorf("the spec was mutated by a failed materialization: %+v", spec)
		}
	})

	t.Run("test 2", func(t *testing.T) {
		// Test 2: A callback plugin that cannot be materialized fails the run.
		if _, err := (&pluginCache{}).ensure(); err == nil {
			t.Error("ensure() = nil error, want the failure surfaced")
		}
	})

	t.Run("test 3", func(t *testing.T) {
		// Test 3: An Ansible run that asked for events refuses when the plugin cannot be written.
		res, err := newAnsibleRunner(WithBinary("true")).Run(context.Background(),
			Spec{Playbook: "p.yml", EventsPath: filepath.Join(os.TempDir(), "events.ndjson")},
			io.Discard)
		if !errors.Is(err, ErrLaunch) {
			t.Errorf("Run() error = %v, want ErrLaunch", err)
		}
		if res.ExitCode != -1 {
			t.Errorf("ExitCode = %d, want -1", res.ExitCode)
		}
	})

	t.Run("test 4", func(t *testing.T) {
		// Test 4: A registry config directory that cannot be created fails the run, rather than
		// falling back to the executor's shared config where the credential would persist.
		if _, cleanup, err := newRuntimeConfigDir(); err == nil {
			cleanup()
			t.Error("newRuntimeConfigDir() = nil error, want the failure surfaced")
		}
	})

	t.Run("test 5", func(t *testing.T) {
		// Test 5: An environment file that cannot be written fails the run, rather than running the
		// tool with none of its injected credentials.
		c := newContainerRunner("docker", "missing", false, nil, &pluginCache{},
			DefaultContainerLimits())
		_, cleanup, err := c.writeEnvFile(Spec{Env: []string{"TOKEN=abc"}}, nil)
		cleanup()
		if err == nil {
			t.Error("writeEnvFile() = nil error, want the failure surfaced")
		}
	})

	scriptTools := []struct {
		// Name says which runner is being driven.
		Name string
		// Run executes the runner under test.
		Run func(Spec, io.Writer) (Result, error)
	}{
		{"python", func(s Spec, w io.Writer) (Result, error) {
			return newPythonRunner(nil).Run(context.Background(), s, w)
		}},
		{"go", func(s Spec, w io.Writer) (Result, error) {
			return newGoRunner(nil).Run(context.Background(), s, w)
		}},
		{"powershell", func(s Spec, w io.Writer) (Result, error) {
			return newPwshRunner(nil).Run(context.Background(), s, w)
		}},
	}
	for i, tool := range scriptTools {
		t.Run(fmt.Sprintf("test %d", 6+i), func(t *testing.T) {
			// Test 6 to 8: A script tool whose source cannot be written refuses the run.
			res, err := tool.Run(Spec{Command: "print"}, io.Discard)
			if !errors.Is(err, ErrLaunch) {
				t.Errorf("%s: Run() error = %v, want ErrLaunch", tool.Name, err)
			}
			if res.ExitCode != -1 {
				t.Errorf("%s: ExitCode = %d, want -1", tool.Name, res.ExitCode)
			}
		})
	}

	t.Run("test 9", func(t *testing.T) {
		// Test 9: The container plan for a script tool refuses when the source cannot be written.
		_, cleanup, err := buildContainerPlan(Spec{Tool: run.ToolPython, Command: "print(1)"})
		cleanup()
		if !errors.Is(err, ErrLaunch) {
			t.Errorf("buildContainerPlan() error = %v, want ErrLaunch", err)
		}
	})
}

// TestPluginCacheReportsALostDirectory pins that removing the whole plugin directory, not only
// the file inside it, is reported rather than silently producing a directory Ansible cannot import
// from. A callback that fails to load costs a run its events with no error anywhere, which is the
// quiet failure this cache exists to avoid.
func TestPluginCacheReportsALostDirectory(t *testing.T) {
	t.Parallel()
	cache := &pluginCache{}
	dir, err := cache.ensure()
	if err != nil {
		t.Fatalf("ensure() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	if err := os.RemoveAll(dir); err != nil {
		t.Fatalf("RemoveAll() error = %v", err)
	}
	if _, err := cache.ensure(); err == nil {
		t.Error("ensure() with its directory removed = nil error, want the failure surfaced")
	}
}

// TestPluginCacheIsSafeUnderConcurrentRuns pins that the integrity check serializes. Two runs
// starting at the same moment both ask for the directory, and without the lock one could read the
// file while the other is halfway through rewriting it, handing Ansible a truncated plugin. Run
// under -race this also proves the shared fields are not written concurrently.
func TestPluginCacheIsSafeUnderConcurrentRuns(t *testing.T) {
	t.Parallel()
	cache := &pluginCache{}
	const workers = 16
	dirs := make(chan string, workers)
	errs := make(chan error, workers)
	start := make(chan struct{})
	for i := 0; i < workers; i++ {
		go func() {
			<-start
			dir, err := cache.ensure()
			dirs <- dir
			errs <- err
		}()
	}
	close(start)

	var first string
	for i := 0; i < workers; i++ {
		if err := <-errs; err != nil {
			t.Errorf("concurrent ensure() error = %v", err)
		}
		dir := <-dirs
		if first == "" {
			first = dir
			t.Cleanup(func() { _ = os.RemoveAll(first) })
			continue
		}
		if dir != first {
			t.Errorf("concurrent ensure() returned %q and %q, want one shared directory", first, dir)
		}
	}
	body, err := os.ReadFile(filepath.Join(first, pluginName+".py"))
	if err != nil {
		t.Fatalf("ReadFile() error = %v", err)
	}
	if string(body) != callbackPlugin {
		t.Error("the materialized plugin does not match the embedded copy after concurrent use")
	}
}
