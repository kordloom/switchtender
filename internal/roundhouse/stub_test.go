package roundhouse

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
	"testing"
	"time"
)

// writeStub writes an executable stub script and confirms it can actually be launched before
// returning. Parallel tests in this package fork children while stubs are being written; a child
// forked between the write's open and close inherits the descriptor, and executing the stub fails
// with ETXTBSY until that child execs. The throwaway launch retries through that window so callers
// never see the race. Scripts must treat an argv of "stub-warmup" as a side-effect-free no-op.
func writeStub(t *testing.T, path, script string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write stub: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		err := exec.Command(path, "stub-warmup").Run()
		if err == nil || !errors.Is(err, syscall.ETXTBSY) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("stub still text-busy after 10s: %v", err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
