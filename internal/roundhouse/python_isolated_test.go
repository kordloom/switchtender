package roundhouse

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestPythonArgsIsolated checks that only a dry run asks Python for isolated mode.
func TestPythonArgsIsolated(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// DryRun selects the dry-run argument list.
		DryRun bool
		// Want is the expected argument list.
		Want []string
	}{
		{Name: "dry run", DryRun: true, // Test 0: A dry run is isolated.
			Want: []string{"-I", "-m", "py_compile", "/t/s.py"}},
		{Name: "real run", DryRun: false, // Test 1: A real run is untouched.
			Want: []string{"/t/s.py"}},
	}
	for _, tc := range tests {
		got := pythonArgs("/t/s.py", tc.DryRun)
		if diff := cmp.Diff(tc.Want, got); diff != "" {
			t.Errorf("%s: pythonArgs mismatch (-want +got):\n%s", tc.Name, diff)
		}
	}
}

// TestPythonDryRunIsolated checks that a dry run imports no module from the project directory.
func TestPythonDryRunIsolated(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not on PATH")
	}
	tests := []struct {
		// Name labels the case and names the shadowing module file.
		Name string
		// Module is the file name planted in the project directory.
		Module string
	}{
		{Name: "py_compile", Module: "py_compile.py"}, // Test 0: Shadow the compiler module.
		{Name: "traceback", Module: "traceback.py"},   // Test 1: Shadow a module it imports.
	}
	for _, tc := range tests {
		dir := t.TempDir()
		marker := filepath.Join(dir, "marker")
		body := "open(" + `"` + marker + `"` + ", 'w').write('x')\n"
		if err := os.WriteFile(filepath.Join(dir, tc.Module), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		r := newPythonRunner(os.Environ())
		var out bytes.Buffer
		res, err := r.Run(context.Background(), Spec{Command: "x = 1", Dir: dir, DryRun: true}, &out)
		if err != nil {
			t.Fatalf("%s: Run: %v", tc.Name, err)
		}
		if res.ExitCode != 0 {
			t.Errorf("%s: exit %d, output %q", tc.Name, res.ExitCode, out.String())
		}
		if _, err := os.Stat(marker); err == nil {
			t.Errorf("%s: marker written, project module was imported", tc.Name)
		}
	}
}
