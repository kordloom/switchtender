//go:build unix

package roundhouse

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/run"
)

// TestUnencodableExtraVarsRefuseTheRun pins that a variable that cannot be written to the vars file
// stops the run everywhere it is reached from. Running the playbook anyway would execute it without
// a variable that may be the condition gating a destructive task, and the run would be recorded as a
// success.
func TestUnencodableExtraVarsRefuseTheRun(t *testing.T) {
	t.Parallel()
	// A channel cannot be JSON encoded, which is the only way to make the encoding fail.
	bad := map[string]any{"bad": make(chan int)}

	res, err := newAnsibleRunner(WithBinary("true")).Run(context.Background(),
		Spec{Playbook: "p.yml", ExtraVars: bad}, io.Discard)
	if !errors.Is(err, ErrLaunch) {
		t.Errorf("host run error = %v, want ErrLaunch", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("host run ExitCode = %d, want -1", res.ExitCode)
	}

	_, cleanup, err := buildContainerPlan(Spec{Tool: run.ToolAnsible, Playbook: "p.yml",
		ExtraVars: bad})
	cleanup()
	if err == nil {
		t.Error("buildContainerPlan() = nil error, want the run refused")
	}

	// A script engine's own vars entry is the one place the failure is swallowed rather than
	// returned, so the entry is simply absent.
	if got := varsExtra(Spec{ExtraVars: bad}); got != nil {
		t.Errorf("varsExtra with an unencodable value = %v, want no entry", got)
	}
}

// TestTerraformStopsWhenInitFails pins that a failed initialization is reported as it stands rather
// than followed by an apply. Running apply against a working directory whose providers and backend
// never initialized is how a partial apply happens, and the state lock is not held to protect it.
func TestTerraformStopsWhenInitFails(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	logPath := filepath.Join(dir, "calls.log")
	stub := filepath.Join(dir, "terraform")
	writeStub(t, stub, "#!/bin/sh\n"+
		`[ "$1" = stub-warmup ] && exit 0`+"\n"+
		`echo "$@" >> `+logPath+"\n"+
		"case \"$1\" in\n  init) exit 3 ;;\nesac\nexit 0\n")

	res, err := (&terraformRunner{binary: stub}).Run(context.Background(),
		Spec{Tool: run.ToolTerraform, Command: "."}, io.Discard)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.ExitCode != 3 {
		t.Errorf("ExitCode = %d, want init's own 3", res.ExitCode)
	}
	calls, _ := os.ReadFile(logPath)
	if strings.Contains(string(calls), "apply") {
		t.Errorf("apply ran after init failed:\n%s", calls)
	}
}

// TestTerraformRefusesAWorkingDirectoryOutsideTheCheckout pins that the containment check gates the
// run itself, not only the plan. Without it a template could aim Terraform at any directory the
// server can read and apply state there.
func TestTerraformRefusesAWorkingDirectoryOutsideTheCheckout(t *testing.T) {
	t.Parallel()
	res, err := newTerraformRunner(nil).Run(context.Background(),
		Spec{Tool: run.ToolTerraform, Dir: t.TempDir(), Command: "../../etc"}, io.Discard)
	if !errors.Is(err, ErrBadWorkDir) {
		t.Errorf("Run() error = %v, want ErrBadWorkDir", err)
	}
	if res.ExitCode != -1 {
		t.Errorf("ExitCode = %d, want -1", res.ExitCode)
	}
}

// TestWorkDirRefusesABaseThatIsNotADirectory pins that a checkout path which cannot be resolved at
// all is refused rather than treated as absent. A base that does not exist is deliberately allowed,
// since a container plan is built before the checkout is there, but a base that exists and is not
// traversable is a broken state and must not fall through that allowance.
func TestWorkDirRefusesABaseThatIsNotADirectory(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	file := filepath.Join(root, "afile")
	if err := os.WriteFile(file, []byte("x"), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}

	got, err := toolWorkDir(filepath.Join(file, "checkout"), "infra")
	if !errors.Is(err, ErrBadWorkDir) {
		t.Errorf("toolWorkDir() = (%q, %v), want ErrBadWorkDir", got, err)
	}
}

// TestPwshRunnerAgainstAStubShell exercises the PowerShell runner's own body without pwsh installed,
// by pointing it at a stub. It pins that the script reaches the interpreter as a file, that the
// working directory and the vars entry are set, and that a non-zero exit is the tool's own result
// rather than an executor error.
func TestPwshRunnerAgainstAStubShell(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	stub := filepath.Join(dir, "pwsh")
	writeStub(t, stub, "#!/bin/sh\n"+
		`[ "$1" = stub-warmup ] && exit 0`+"\n"+
		"echo \"args=$*\"\n"+
		"echo \"vars=$SWITCHTENDER_VARS\"\n"+
		"echo \"pwd=$(pwd)\"\n"+
		"exit 4\n")

	var buf strings.Builder
	res, err := (&pwshRunner{binary: stub}).Run(context.Background(), Spec{
		Tool: run.ToolPowerShell, Command: "Write-Output hi", Dir: dir,
		ExtraVars: map[string]any{"region": "eu-north-1"},
	}, &buf)
	if err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if res.ExitCode != 4 {
		t.Errorf("ExitCode = %d, want the interpreter's own 4", res.ExitCode)
	}
	got := buf.String()
	for _, want := range []string{"-NoProfile", "-NonInteractive", "-File",
		`vars={"region":"eu-north-1"}`} {
		if !strings.Contains(got, want) {
			t.Errorf("output %q missing %q", got, want)
		}
	}
	// The interpreter ran in the project's own directory, compared after resolving symlinks since
	// the temp root is one on some platforms.
	realDir, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("EvalSymlinks() error = %v", err)
	}
	if !strings.Contains(got, "pwd="+realDir) {
		t.Errorf("output %q, want the working directory set to the checkout %q", got, realDir)
	}
}

// TestRunArgsMountsTheEventsSidecarAndThePluginDirectory pins the events wiring for a container run.
// The plugin directory is shared read-only because the container only imports from it, and the
// sidecar is shared writable because the plugin writes the run's events into it. Getting either
// direction wrong loses a run's events with no error.
func TestRunArgsMountsTheEventsSidecarAndThePluginDirectory(t *testing.T) {
	t.Parallel()
	cache := &pluginCache{}
	c := newContainerRunner("docker", "missing", false, nil, cache, DefaultContainerLimits())
	events := filepath.Join(t.TempDir(), "events.ndjson")
	spec := Spec{Tool: run.ToolBash, Command: "echo hi", Dir: t.TempDir(),
		Image: "alpine:3", EventsPath: events}

	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		cleanup()
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()
	args, err := c.runArgs(spec, plan, "st-test", "")
	if err != nil {
		t.Fatalf("runArgs() error = %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(cache.dir) })

	joined := strings.Join(args, " ")
	if want := "-v " + cache.dir + ":" + cache.dir + ":ro"; !strings.Contains(joined, want) {
		t.Errorf("args %q missing the read-only plugin mount %q", joined, want)
	}
	if want := "-v " + events + ":" + events; !strings.Contains(joined, want) {
		t.Errorf("args %q missing the sidecar mount %q", joined, want)
	}
	if strings.Contains(joined, events+":"+events+":ro") {
		t.Error("the events sidecar is mounted read-only, so the callback cannot write the run's " +
			"events into it")
	}
}

// TestRunArgsRefusesWhenThePluginCannotBeMaterialized pins that a run asking for events fails rather
// than starting a container with no callback. A container that started without it would run the
// playbook to completion while the run's event stream stayed empty, which reads as a hung run.
func TestRunArgsRefusesWhenThePluginCannotBeMaterialized(t *testing.T) {
	t.Parallel()
	// A cache whose one-time setup already failed, which is what an executor with no writable
	// temp directory produces.
	cache := &pluginCache{}
	cache.once.Do(func() { cache.err = errors.New("no temp directory") })
	c := newContainerRunner("docker", "missing", false, nil, cache, DefaultContainerLimits())
	spec := Spec{Tool: run.ToolBash, Command: "echo hi", Dir: t.TempDir(),
		Image: "alpine:3", EventsPath: filepath.Join(t.TempDir(), "events.ndjson")}

	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		cleanup()
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()

	if _, err := c.runArgs(spec, plan, "st-test", ""); err == nil {
		t.Error("runArgs() = nil error, want the run refused when the callback cannot be written")
	}
	if _, _, err := c.writeEnvFile(spec, nil); err == nil {
		t.Error("writeEnvFile() = nil error, want the run refused")
	}
}

// TestCancelBeforeStartIsHarmless pins the guard in the cancellation hook. The hook signals the
// child's process group by its pid, and a context canceled before the process exists has no pid to
// signal, so it must return quietly rather than signaling process group zero, which on Unix is the
// caller's own group and would take down the executor.
func TestCancelBeforeStartIsHarmless(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("true")
	configureProcessGroup(cmd)
	if cmd.Cancel == nil {
		t.Fatal("configureProcessGroup installed no cancel hook")
	}
	if err := cmd.Cancel(); err != nil {
		t.Errorf("cancel before start error = %v, want it to do nothing", err)
	}
}

// TestExtensionRunnersReceiveTheSpecTheyWereGiven pins the extension dispatch, including the part
// worth knowing: a registered tool is handed the Spec unchanged, image and all, and the router does
// not containerize it or refuse it the way it would a built-in. An extension that wants isolation has
// to arrange it, since the router's container gate covers the built-in engines only.
//
// It does not call t.Parallel: it writes the package runner registry, so it runs in the sequential
// phase before the parallel tests that read the registry resume.
func TestExtensionRunnersReceiveTheSpecTheyWereGiven(t *testing.T) {
	var got Spec
	RegisterRunner("gapsprobe", RunnerFunc(func(_ context.Context, spec Spec, _ io.Writer) (Result, error) {
		got = spec
		return Result{ExitCode: 0}, nil
	}))

	spec := Spec{Tool: "gapsprobe", Command: "deploy", Image: "alpine:3",
		Env: []string{"TOKEN=x"}}
	res, err := NewAnsibleRunner().Run(context.Background(), spec, io.Discard)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("extension route: exit=%d err=%v", res.ExitCode, err)
	}
	if diff := cmp.Diff(spec, got); diff != "" {
		t.Errorf("the extension runner received a different spec (-want +got):\n%s", diff)
	}
	if !slices.Contains(got.Env, "TOKEN=x") {
		t.Error("the run's injected environment did not reach the extension runner")
	}
}
