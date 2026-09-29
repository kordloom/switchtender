package roundhouse

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestNoScriptSurvivesALaunch pins that the file holding a run's command never outlives the run.
//
// The script tools write the command to a private temp file instead of onto argv, because argv is
// world-readable on Linux and survives in a container's config long after the run. That moved the
// exposure rather than removing it if the file is left behind, and it was: writeScriptFile returned
// a cleanup alongside its error, and every one of its five callers returned on the error and
// dropped it. A write or close that failed therefore left the script on disk for good, and the
// script is the operator's command verbatim, including anything they inlined into it.
//
// The cleanup now happens inside writeScriptFile on its own failures, so there is no error path on
// which a caller has to remember. This walks the outcomes a caller can actually reach and asserts
// the directory is empty after each, which is the property that matters whichever branch produced
// it.
func TestNoScriptSurvivesALaunch(t *testing.T) {
	tests := []struct {
		Name    string
		Command string
		DryRun  bool
	}{
		{Name: "a script that succeeds", Command: "echo ok"},
		{Name: "a script that exits non-zero", Command: "exit 7"},
		{Name: "a script with a syntax error", Command: "if", DryRun: true},
		{Name: "a script that kills its own shell", Command: "kill -9 $$"},
		{Name: "a script holding an inline secret", Command: "export PGPASSWORD=hunter2; true"},
	}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			// TMPDIR is process-wide, so these cannot run in parallel with each other or with any
			// other test that writes a temp file.
			dir := t.TempDir()
			t.Setenv("TMPDIR", dir)

			var buf bytes.Buffer
			_, _ = newBashRunner(nil).Run(context.Background(), Spec{
				Tool: "bash", Command: test.Command, DryRun: test.DryRun,
			}, &buf)

			left, err := filepath.Glob(filepath.Join(dir, "switchtender-*"))
			if err != nil {
				t.Fatalf("test %d: glob: %v", testNum, err)
			}
			for _, path := range left {
				body, rerr := os.ReadFile(path)
				if rerr != nil {
					t.Errorf("test %d: %s was left behind and cannot be read: %v",
						testNum, filepath.Base(path), rerr)
					continue
				}
				t.Errorf("test %d: %s outlived the run holding the command verbatim:\n%s",
					testNum, filepath.Base(path), string(body))
			}
		})
	}
}

// TestWriteScriptFileLeavesNothingWhenItFails pins the contract directly, since the failure that
// caused the leak is not one a caller can provoke from outside.
//
// The file is created before it is written, so every failure after the create has to remove it.
// This drives the create failure, which is the one reachable from here, and asserts the shape the
// other branches now share: no path returned, and a cleanup that is safe to call and has nothing
// left to do.
func TestWriteScriptFileLeavesNothingWhenItFails(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("TMPDIR", dir)

	// A pattern carrying a path separator is rejected by os.CreateTemp before anything is written.
	path, cleanup, err := writeScriptFile("bad/pattern-*.sh", "echo hello")
	if err == nil {
		t.Fatal("writeScriptFile accepted a pattern os.CreateTemp rejects")
	}
	if path != "" {
		t.Errorf("writeScriptFile returned path %q alongside an error, and a caller that trusts "+
			"it would hand the tool a file that does not exist", path)
	}
	if cleanup == nil {
		t.Fatal("writeScriptFile returned a nil cleanup with its error, so a caller that defers " +
			"it panics")
	}
	cleanup() // Must be safe on an error return, not only on success.

	left, gerr := filepath.Glob(filepath.Join(dir, "*"))
	if gerr != nil {
		t.Fatalf("glob: %v", gerr)
	}
	if len(left) != 0 {
		t.Errorf("a failed writeScriptFile left %v behind", left)
	}

	// The success path still hands back a cleanup that removes the file, which is what the callers
	// defer, and the file is private while it exists.
	path, cleanup, err = writeScriptFile("switchtender-sh-*.sh", "echo hello")
	if err != nil {
		t.Fatalf("writeScriptFile() error = %v", err)
	}
	info, serr := os.Stat(path)
	if serr != nil {
		t.Fatalf("stat the script: %v", serr)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("the script is mode %o, want 600: it holds the command verbatim", mode)
	}
	cleanup()
	if _, serr := os.Stat(path); !os.IsNotExist(serr) {
		t.Errorf("the cleanup left the script at %s", path)
	}
}
