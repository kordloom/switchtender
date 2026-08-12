package dispatch

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/invsource"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/run"
)

func TestValidateBareSource(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	execFile := filepath.Join(dir, "dyn.sh")
	if err := os.WriteFile(execFile, []byte("#!/bin/sh\necho hi\n"), 0o755); err != nil {
		t.Fatalf("write exec file: %v", err)
	}
	plainFile := filepath.Join(dir, "hosts.ini")
	if err := os.WriteFile(plainFile, []byte("[web]\nweb01\n"), 0o600); err != nil {
		t.Fatalf("write plain file: %v", err)
	}

	tests := []struct {
		Name string
		In   string
		Want error
	}{
		{Name: "empty", In: "", Want: invsource.ErrInvalidSource},                              // Test 0.
		{Name: "traversal relative", In: "../etc/passwd", Want: invsource.ErrInvalidSource},    // Test 1.
		{Name: "traversal absolute", In: dir + "/../../etc", Want: invsource.ErrInvalidSource}, // Test 2.
		{Name: "executable file", In: execFile, Want: invsource.ErrInvalidSource},              // Test 3.
		{Name: "plain inventory file", In: plainFile, Want: nil},                               // Test 4.
		{Name: "directory", In: dir, Want: nil},                                                // Test 5.
		{Name: "nonexistent", In: filepath.Join(dir, "ghost"), Want: nil},                      // Test 6.
	}
	for i, test := range tests {
		if err := validateBareSource(test.In); !errors.Is(err, test.Want) {
			t.Errorf("test %d (%s): validateBareSource(%q) error = %v, want %v",
				i, test.Name, test.In, err, test.Want)
		}
	}
}

// TestDumpSourceRefusesAProjectSourceThatEscapesTheCheckout drives dumpSource over a real project
// repository whose committed content reaches out of the checkout, and pins that a refused source
// never reaches the dumper.
//
// A project-backed source takes the project.WithinRepo path rather than the bare source guard, and
// nothing exercised it: replacing the call with a plain filepath.Join left the whole repository
// suite green. The path dumpSource produces is handed to ansible-inventory, which EXECUTES an
// inventory that is an executable file, so a repository committing a symlink out of the checkout
// picks a file on the control node to run as the executor. Recording the dumper calls is what makes
// the assertion real. A test that checked only the returned error would pass even after the file
// had already run.
func TestDumpSourceRefusesAProjectSourceThatEscapesTheCheckout(t *testing.T) {
	t.Parallel()
	repo := newEscapeRepo(t)

	tests := []struct {
		// Name describes the row.
		Name string
		// Source is the source path, relative to the project checkout.
		Source string
		// Want is the error dumpSource must return.
		Want error
	}{{ // Test 0: A committed symlink to a sibling directory holding a real file.
		Name: "a committed symlink to a sibling directory", Source: escapeRepoSecret,
		Want: project.ErrEscapesRepo,
	}, { // Test 1: A committed symlink to the filesystem root.
		Name: "a committed symlink to the filesystem root", Source: escapeRepoRooted,
		Want: project.ErrEscapesRepo,
	}, { // Test 2: Dot dot segments that climb out of the checkout onto a real file.
		Name: "a source that traverses out of the checkout", Source: escapeRepoTraversal,
		Want: project.ErrEscapesRepo,
	}, { // Test 3: An absolute path naming a real file.
		Name: "a source given as an absolute path", Source: escapeRepoAbsolute,
		Want: project.ErrEscapesRepo,
	}, { // Test 4: An ordinary committed inventory resolves into the checkout.
		Name: "an ordinary inventory inside the checkout", Source: escapeRepoInventory,
	}, { // Test 5: A path the sync never wrote passes through on the lexical result.
		Name: "a source the sync never wrote", Source: "inventories/never-written.ini",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d: %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cache := t.TempDir()
			syncer, err := project.NewSyncer(cache)
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			projects := project.NewMemStore()
			p := &project.Project{ID: "proj_src", Name: "infra", RepoURL: repo, Branch: "main"}
			if err := projects.Save(ctx, p); err != nil {
				t.Fatalf("Save(project) error = %v", err)
			}

			// dumpSource only reaches the dumper when the runner is an InventoryDumper, so the
			// recorder is both halves here.
			recorder := &dumpRecorder{}
			d := New(run.NewMemStore(), recorder, zap.NewNop(), WithProjects(projects, syncer))
			defer d.Close()

			src := &invsource.Source{
				ID: "src_1", Name: "dynamic", Source: test.Source,
				ProjectID: p.ID, InventoryID: "inv_1",
			}
			data, err := d.dumpSource(ctx, src)

			// Nothing the dumper received may reach outside the project cache, whichever way the
			// resolve went. This is asserted before the error, because the error is reported after
			// the dump would already have happened.
			if escaped := escapedPaths(cache, recorder.dumped...); len(escaped) != 0 {
				t.Errorf("ansible-inventory was handed %v, which reaches outside the project cache "+
					"%s and is executed when it is an executable file", escaped, cache)
			}
			if test.Want != nil {
				// The refusal has to land before the dump. ansible-inventory runs an executable
				// source, so an error reported afterward describes a command that already ran.
				if diff := cmp.Diff([]string(nil), recorder.dumped, cmpopts.EquateEmpty()); diff != "" {
					t.Errorf("the refused source was handed to ansible-inventory anyway, which "+
						"executes an executable inventory as the executor (-want +got):\n%s", diff)
				}
				if len(data) != 0 {
					t.Errorf("dumpSource() returned %d bytes on a refusal, want none", len(data))
				}
			}
			if !errors.Is(err, test.Want) {
				t.Fatalf("dumpSource() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				return
			}

			if len(recorder.dumped) != 1 {
				t.Fatalf("the dumper was called %d time(s), want 1: %v",
					len(recorder.dumped), recorder.dumped)
			}
			got := recorder.dumped[0]
			// The path ansible-inventory receives is rewritten into the isolated per-run checkout,
			// so it must never be the stored value and must never leave the project cache.
			if got == test.Source {
				t.Errorf("the dumper received the stored source %q unchanged, not a path inside "+
					"the checkout", got)
			}
			if want := test.Source; filepath.Base(got) != filepath.Base(want) {
				t.Errorf("the dumper received %q, want a path naming %q", got, want)
			}
		})
	}
}
