package main

import (
	"os"
	"os/exec"
	"testing"
)

// TestEveryPackageVetsForWindows type-checks every package and its tests as Windows builds them.
//
// The release ships a Windows binary, but nothing built the tests for Windows, so a test calling a
// helper that only exists in a Unix file, or a syscall Windows does not have, compiled everywhere
// CI looked and broke the first Windows build of the tests. go vet compiles the test files too,
// which go build does not, so it is the cheapest check that reaches them.
func TestEveryPackageVetsForWindows(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to vet with")
	}
	cmd := exec.Command("go", "vet", "./...")
	// Cross builds turn cgo off on their own, but a CGO_ENABLED=1 inherited from a race run would
	// ask for a Windows C compiler this machine does not have.
	cmd.Env = append(os.Environ(), "GOOS=windows", "GOARCH=amd64", "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Errorf("GOOS=windows go vet ./... error = %v, want every package and test to build "+
			"for Windows:\n%s", err, out)
	}
}
