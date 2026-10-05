//go:build integration

package integration_test

import (
	"bytes"
	"context"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/runfiles"
)

// runFilesImage is a small public image with bash, which a containerized bash run needs.
const runFilesImage = "debian:bookworm-slim"

// syncBuffer is an output sink a test can read while a run is still writing to it.
type syncBuffer struct {
	// mu guards buf.
	mu sync.Mutex
	// buf holds what was written.
	buf bytes.Buffer
}

// Write appends p.
func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

// String returns what was written so far.
func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// requireDocker stands the test down without a usable docker that shares this filesystem.
func requireDocker(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("docker"); err != nil {
		standDownOrFail(t, "docker is not on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		standDownOrFail(t, "the docker daemon is not running: %v", err)
	}
	requireHostBindMounts(t)
	pull, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if exec.CommandContext(pull, "docker", "image", "inspect", runFilesImage).Run() == nil {
		return
	}
	out, err := exec.CommandContext(pull, "docker", "pull", runFilesImage).CombinedOutput()
	if err != nil {
		standDownOrFail(t, "cannot pull %s: %v\n%s", runFilesImage, err, out)
	}
}

// containerRunFiles stages a credential in a fresh run directory and returns the directory, the
// root, and a spec for a containerized bash run of script that hands the run a kubeconfig, so its
// kubectl cache is pointed into the run directory the way the dispatcher points it.
func containerRunFiles(t *testing.T, script string) (*runfiles.Dir, string, roundhouse.Spec) {
	t.Helper()
	root := filepath.Join(t.TempDir(), "runfiles")
	dir, err := runfiles.Create(root, "it-container")
	if err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	t.Cleanup(func() { _ = dir.Remove() })
	cred, err := dir.WriteFile("cred-*", "staged-kubeconfig-secret")
	if err != nil {
		t.Fatal(err)
	}
	env := []string{"KUBECONFIG=" + cred, "RUNDIR=" + dir.Path()}
	env = append(env, runfiles.ToolStateEnv(dir.Path(), env)...)
	return dir, root, roundhouse.Spec{
		Tool: run.ToolBash, Command: script, Image: runFilesImage, Dir: t.TempDir(), Env: env,
		CredentialFiles: []string{cred}, RunDir: dir.Path(), RunFilesRoot: root,
	}
}

// requireNoToolStateOnHost fails when anything a containerized tool wrote reached the host's side
// of the run directory.
func requireNoToolStateOnHost(t *testing.T, dir *runfiles.Dir) {
	t.Helper()
	if _, err := os.Stat(filepath.Join(dir.Path(), "tools")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("tool state written inside the container reached the host: %v", err)
	}
}

// TestContainerRunKeepsToolStateInMemory runs a containerized tool against the real container
// runtime and proves the run directory inside the container is an in-memory filesystem, sized,
// nosuid and nodev, that runs a program placed in it, that holds the staged credential, and that
// keeps everything the tool writes off the host.
func TestContainerRunKeepsToolStateInMemory(t *testing.T) {
	requireDocker(t)
	script := `set -e
grep " $RUNDIR " /proc/mounts
cat "$KUBECONFIG"; echo
mkdir -p "$KUBECACHEDIR"
echo cached-token > "$KUBECACHEDIR/token"
cat "$KUBECACHEDIR/token"
printf '#!/bin/sh\necho helper-ran\n' > "$KUBECACHEDIR/helper"
chmod +x "$KUBECACHEDIR/helper"
"$KUBECACHEDIR/helper"
`
	dir, root, spec := containerRunFiles(t, script)
	runner := roundhouse.NewSelectiveRunner(true, "docker", "missing", false,
		roundhouse.DefaultContainerLimits())
	var out syncBuffer
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	res, err := runner.Run(ctx, spec, &out)
	if err != nil || res.ExitCode != 0 {
		t.Fatalf("Run() = %+v, %v\n%s", res, err, out.String())
	}
	got := out.String()
	mount := ""
	for _, line := range strings.Split(got, "\n") {
		if strings.HasPrefix(line, "tmpfs "+dir.Path()+" ") {
			mount = line
		}
	}
	if mount == "" {
		t.Fatalf("the run directory is not a tmpfs inside the container:\n%s", got)
	}
	for _, want := range []string{"nosuid", "nodev", "size=65536k", "mode=700"} {
		if !strings.Contains(mount, want) {
			t.Errorf("the mount %q lacks %s", mount, want)
		}
	}
	if strings.Contains(mount, "noexec") {
		t.Errorf("the mount %q is noexec, so a helper placed there cannot run", mount)
	}
	for _, want := range []string{"staged-kubeconfig-secret", "cached-token", "helper-ran"} {
		if !strings.Contains(got, want) {
			t.Errorf("the run's output lacks %q:\n%s", want, got)
		}
	}
	requireNoToolStateOnHost(t, dir)
	if err := dir.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.IsDir() {
			t.Errorf("%s is left under the root", e.Name())
		}
	}
}

// TestContainerTerminationLeavesNothingOnTheHost kills a run's container from outside while its
// tool holds a cached token, the way an out-of-memory kill or a crashed daemon ends one, and proves
// the token never reached the host and the run directory on the host is still removed by the
// executor, which never waits on the container dying to clean up.
func TestContainerTerminationLeavesNothingOnTheHost(t *testing.T) {
	requireDocker(t)
	script := `set -e
mkdir -p "$KUBECACHEDIR"
echo cached-token > "$KUBECACHEDIR/token"
echo "ready $HOSTNAME"
sleep 300
`
	dir, _, spec := containerRunFiles(t, script)
	runner := roundhouse.NewSelectiveRunner(true, "docker", "missing", false,
		roundhouse.DefaultContainerLimits())
	var out syncBuffer
	done := make(chan error, 1)
	go func() {
		_, err := runner.Run(context.Background(), spec, &out)
		done <- err
	}()
	var id string
	deadline := time.Now().Add(5 * time.Minute)
	for id == "" {
		for _, line := range strings.Split(out.String(), "\n") {
			if rest, ok := strings.CutPrefix(line, "ready "); ok {
				id = strings.TrimSpace(rest)
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("the container never reported ready:\n%s", out.String())
		}
		time.Sleep(50 * time.Millisecond)
	}
	if out, err := exec.Command("docker", "kill", id).CombinedOutput(); err != nil {
		t.Fatalf("docker kill %s: %v\n%s", id, err, out)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatal("the run did not end after its container was killed")
	}
	requireNoToolStateOnHost(t, dir)
	if err := dir.Remove(); err != nil {
		t.Fatalf("Remove() error = %v", err)
	}
	if _, err := os.Stat(dir.Path()); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the run directory survived its removal: %v", err)
	}
}
