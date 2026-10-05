//go:build !windows

package roundhouse

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
)

// plantManaged lays out a finished managed runtime environment named env under root, with the fake
// Ansible commands writeFakeAnsible writes and the marker an install writes, makes it current when
// current is set, and returns its bin directory.
func plantManaged(t *testing.T, root, env, release string, current bool) string {
	t.Helper()
	bin := filepath.Join(root, env, "bin")
	writeFakeAnsible(t, bin, release)
	marker := `{"ansible_core":"` + release + `","python":"3.12.3","lock_sha256":"` +
		strings.Repeat("0", 64) + `"}`
	if err := os.WriteFile(filepath.Join(root, env, "switchtender-runtime.json"), []byte(marker),
		0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if current {
		if err := os.WriteFile(filepath.Join(root, "current"), []byte(env+"\n"), 0o644); err != nil {
			t.Fatalf("write current: %v", err)
		}
	}
	return bin
}

// waitForFile waits up to ten seconds for path to exist.
func waitForFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

// TestRemoveDoesNotPullTheRuntimeFromUnderARunningPlay pins that removing the managed runtime
// cannot break a play already running from it. ansible-playbook imports plugins, module_utils, and
// the modules it ships to hosts lazily from its environment, so deleting the environment mid-play
// fails the play partway, after some hosts have changed. Today Remove deletes it with no regard for
// runs using it, and nothing tells the operator.
func TestRemoveDoesNotPullTheRuntimeFromUnderARunningPlay(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "runtime")
	env := "2.21.4-" + strings.Repeat("0", 12)
	bin := plantManaged(t, root, env, "2.21.4", true)
	lib := filepath.Join(root, env, "lib")
	if err := os.MkdirAll(lib, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(lib, "module_utils.py"), []byte("# imported lazily\n"),
		0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	started := filepath.Join(base, "started")
	proceed := filepath.Join(base, "proceed")
	// The stand-in starts, waits for the test, and then reads a file from its environment the way
	// Ansible imports a plugin partway through a play.
	playbook := "#!/bin/sh\nhere=$(cd \"$(dirname \"$0\")/..\" && pwd)\n: > '" + started + "'\n" +
		"i=0\nwhile [ ! -e '" + proceed + "' ] && [ $i -lt 400 ]; do sleep 0.05; i=$((i+1)); done\n" +
		"cat \"$here/lib/module_utils.py\" || exit 3\n"
	if err := os.WriteFile(filepath.Join(bin, "ansible-playbook"), []byte(playbook),
		0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	runner := NewAnsibleRunner(WithAnsibleLocator(ansibleruntime.NewLocator("", root)))
	type outcome struct {
		res Result
		err error
		out string
	}
	done := make(chan outcome, 1)
	go func() {
		var out bytes.Buffer
		res, err := runner.Run(context.Background(), Spec{Playbook: "site.yml", Dir: base}, &out)
		done <- outcome{res: res, err: err, out: out.String()}
	}()
	waitForFile(t, started)
	removed, rmErr := ansibleruntime.Remove(root, "")
	if err := os.WriteFile(proceed, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	got := <-done
	if got.err != nil || got.res.ExitCode != 0 {
		t.Errorf("Remove() = %v, %v while a play ran from the runtime, and the play then failed "+
			"with exit %d (%v): %s", removed, rmErr, got.res.ExitCode, got.err, got.out)
	}
}

// TestARunsPlayIsTheAnsibleItsEvidenceNames pins that the Ansible a run's inventory cross-check
// read with, the one its evidence records, and the one its play runs with are the same. The host
// runner locates the commands afresh for every command, and the dispatcher records the version
// from the cross-check's read and the source from a later locate, so an install that finishes
// during a run splits them: the record says ansible-core 2.21.4 from the managed runtime, the play
// runs 2.20.9, and the cross-check never covered the Ansible that executed. The calls below are
// the ones the dispatcher makes, in its order.
func TestARunsPlayIsTheAnsibleItsEvidenceNames(t *testing.T) {
	t.Parallel()
	base := t.TempDir()
	root := filepath.Join(base, "runtime")
	envA := "2.21.4-" + strings.Repeat("0", 12)
	envB := "2.20.9-" + strings.Repeat("0", 12)
	binA := plantManaged(t, root, envA, "2.21.4", true)
	plantManaged(t, root, envB, "2.20.9", false)
	// A's ansible-inventory stands in for the moment `switchtender ansible install --version 2.20`
	// finishes in another process while the cross-check reads.
	inv := "#!/bin/sh\nif [ \"$1\" = --version ]; then echo 'ansible-inventory [core 2.21.4]'; " +
		"exit 0; fi\nprintf '" + envB + "\\n' > '" + filepath.Join(root, "current") + "'\n" +
		"echo '{\"_meta\": {\"hostvars\": {}}, \"all\": {\"children\": [\"ungrouped\"]}}'\n"
	if err := os.WriteFile(filepath.Join(binA, "ansible-inventory"), []byte(inv), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	checkDir := filepath.Join(base, "check")
	if err := os.MkdirAll(checkDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(checkDir, "rendered"), []byte("web1\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	runner := NewAnsibleRunner(WithAnsibleLocator(ansibleruntime.NewLocator("", root)))
	reader, ok := runner.(interface {
		ReadInventories(ctx context.Context, spec Spec, dir string, names []string) ([][]byte,
			string, error)
	})
	if !ok {
		t.Fatal("the host runner does not read inventories")
	}
	// The run locates its Ansible once and binds it to its context, as the dispatcher does, and
	// every call below is made with that one answer.
	recorded := runner.(AnsibleCommandsReporter).AnsibleCommands()
	ctx := WithAnsibleCommands(context.Background(), recorded)
	_, version, err := reader.ReadInventories(ctx, Spec{Dir: base}, checkDir, []string{"rendered"})
	if err != nil {
		t.Fatalf("ReadInventories() error = %v", err)
	}
	var out bytes.Buffer
	if _, err := runner.Run(ctx, Spec{Playbook: "site.yml", Dir: base}, &out); err != nil {
		t.Fatalf("Run() error = %v", err)
	}
	if recorded.Release != version {
		t.Errorf("the evidence records ansible-core %s read by the cross-check with source %s "+
			"release %s", version, recorded.Source, recorded.Release)
	}
	if !strings.Contains(out.String(), "playbook from "+binA) {
		t.Errorf("the cross-check and the evidence name %s, and the play ran %q", binA,
			strings.TrimSpace(out.String()))
	}
}

// TestARelativeConfiguredDirectoryIsNotTheProjects pins that a relative --ansible-bin or
// SWITCHTENDER_ANSIBLE_BIN names one directory for the whole server. The locator keeps the value
// as given, and os/exec evaluates a relative command path against the command's working directory,
// which for a play is the project checkout. So the play runs an ansible-playbook from the project
// repository, while doctor and the evidence read the version of a different file relative to the
// server's own working directory, or report Ansible missing.
func TestARelativeConfiguredDirectoryIsNotTheProjects(t *testing.T) {
	t.Parallel()
	project := t.TempDir()
	planted := filepath.Join(project, "venv", "bin")
	if err := os.MkdirAll(planted, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(planted, "ansible-playbook"),
		[]byte("#!/bin/sh\necho 'ansible-playbook from the project checkout'\n"), 0o755); err != nil {
		t.Fatalf("write: %v", err)
	}
	runner := NewAnsibleRunner(WithAnsibleLocator(ansibleruntime.NewLocator("venv/bin", "")))
	var out bytes.Buffer
	_, _ = runner.Run(context.Background(), Spec{Playbook: "site.yml", Dir: project}, &out)
	if strings.Contains(out.String(), "from the project checkout") {
		t.Errorf("a relative configured directory ran the project's own ansible-playbook: %q",
			strings.TrimSpace(out.String()))
	}
}
