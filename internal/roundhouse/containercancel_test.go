//go:build unix

package roundhouse

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fakeRuntime writes a stand-in container runtime onto PATH that appends every invocation to a log,
// so a canceled run can be observed doing what it claims to do. rmFailures is how many times "rm"
// reports failure before succeeding, which is the transient case the retry loop exists for.
func fakeRuntime(t *testing.T, rmFailures int) (logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	countPath := filepath.Join(dir, "rmcount")
	script := `#!/bin/sh
[ "$1" = stub-warmup ] && exit 0
echo "$@" >> ` + logPath + `
case "$1" in
  run)
    # Long enough that the cancel lands while it is running.
    sleep 30
    ;;
  stop)
    exit 0
    ;;
  rm)
    n=0
    [ -f ` + countPath + ` ] && n=$(cat ` + countPath + `)
    n=$((n+1))
    echo "$n" > ` + countPath + `
    [ "$n" -le ` + itoa(rmFailures) + ` ] && exit 1
    exit 0
    ;;
esac
exit 0
`
	bin := filepath.Join(dir, "docker")
	writeStub(t, bin, script)
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return logPath
}

// itoa avoids importing strconv for one call in a test helper.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// TestCanceledContainerIsStoppedThenRemovedWithRetries pins what a cancel actually does to a running
// container.
//
// Two defects lived here. The container was removed with no grace at all, while the byte-identical
// run on the host gets ten seconds after SIGTERM: a canceled terraform therefore died holding its
// state lock, and every later plan or apply blocked until somebody ran force-unlock by hand. And the
// retry loop was dead code, because a single channel closed in a defer told the remover to stop the
// instant the client returned, which on a cancel is immediate since the client is SIGKILLed by the
// context. A transient removal failure then left the container still running the playbook against
// production while the run was recorded as canceled, and --rm erased the evidence afterward.
func TestCanceledContainerIsStoppedThenRemovedWithRetries(t *testing.T) {
	logPath := fakeRuntime(t, 2) // the first two removals fail, as a busy daemon does

	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(300 * time.Millisecond)
		cancel()
	}()

	_, _ = c.Run(ctx, Spec{Tool: "bash", Command: "echo hi", Image: "alpine:3"}, os.Stderr)

	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read runtime log: %v", err)
	}
	calls := string(body)

	if !strings.Contains(calls, "stop --time") {
		t.Errorf("a canceled container was never stopped, so a tool holding a lock had no chance to "+
			"release it:\n%s", calls)
	}
	// The removal is retried past its first failure. One line is the bug; more than one is the loop
	// doing what its constants say it does.
	if n := strings.Count(calls, "rm -f"); n < 2 {
		t.Errorf("rm -f was attempted %d time(s), want retries: a transient failure leaves the "+
			"container running the playbook while the run reads as canceled\n%s", n, calls)
	}
}
