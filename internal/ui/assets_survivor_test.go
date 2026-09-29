package ui

import (
	"bytes"
	"compress/gzip"
	"errors"
	"fmt"
	"io/fs"
	"testing"
	"testing/fstest"
)

// lcgNoise returns n bytes of deterministic pseudo-random data, which gzip cannot shrink. The
// sequence is fixed so a failure reproduces exactly rather than depending on the run.
func lcgNoise(n int) []byte {
	b := make([]byte, n)
	x := uint32(0x12345678)
	for i := range b {
		x = x*1664525 + 1013904223
		b[i] = byte(x >> 24)
	}
	return b
}

// rawGzipLen returns the length gzip produces for body at the level gzipBody uses, so a test can
// tell the boundary cases apart from the ones well inside it.
func rawGzipLen(t *testing.T, body []byte) int {
	t.Helper()
	var buf bytes.Buffer
	zw, err := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if err != nil {
		t.Fatalf("gzip writer: %v", err)
	}
	if _, err := zw.Write(body); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Len()
}

// TestGzipBodyRefusesAResultThatIsNotSmaller walks the benefit rule across the point where it
// changes, including the case where compressing costs exactly as much as it saves.
//
// A body that gzips to the same size as itself is the boundary the rule is written at. Serving it
// compressed buys nothing and costs both ends a compression pass plus a Content-Encoding the client
// must undo, so the rule is that the gzip body is used only when it is strictly smaller. The
// existing boundary test covered bodies that compress well and bodies that do not compress at all,
// which a rule written as "larger" rather than "not smaller" passes just as happily.
func TestGzipBodyRefusesAResultThatIsNotSmaller(t *testing.T) {
	t.Parallel()
	// Noise with a run of identical leading bytes. The run gives deflate a little to work with, so
	// stepping its length walks the compressed size down through the raw size one byte at a time.
	const size = 1024
	var sawBoundary bool
	for run := 0; run <= 200; run++ {
		body := lcgNoise(size)
		for i := range run {
			body[i] = 'A'
		}
		raw := rawGzipLen(t, body)
		got := gzipBody(body)
		switch {
		case raw >= len(body):
			sawBoundary = true
			if got != nil {
				t.Errorf("run of %d: gzip is %d bytes against %d raw, so it must not be used, "+
					"got a %d byte body", run, raw, len(body), len(got))
			}
		case got == nil:
			t.Errorf("run of %d: gzip is %d bytes against %d raw and was refused anyway", run, raw,
				len(body))
		case len(got) >= len(body):
			t.Errorf("run of %d: served a %d byte gzip body for %d raw bytes", run, len(got),
				len(body))
		}
	}
	// Without this the sweep could pass by never reaching the boundary at all, which would make the
	// case above decoration rather than a check.
	if !sawBoundary {
		t.Error("no body in the sweep gzipped to its own size or larger, so the rule's boundary " +
			"was never exercised")
	}
}

// errReadDirFS is a tree whose ReadDir fails for one directory, standing in for an embedded subtree
// that cannot be listed.
type errReadDirFS struct {
	fstest.MapFS
	// failDir is the directory name whose listing fails.
	failDir string
	// err is the failure ReadDir reports.
	err error
}

// ReadDir lists a directory, failing for the one this tree is set to fail on.
func (f errReadDirFS) ReadDir(name string) ([]fs.DirEntry, error) {
	if name == f.failDir {
		return nil, f.err
	}
	return f.MapFS.ReadDir(name)
}

// TestNewAssetHandlerPanicsWhenTheTreeCannotBeWalked pins that a walk failure stops the build rather
// than being skipped.
//
// The tree is embedded, so a directory that will not list is a build time fault and the handler is
// documented as panicking on one. Swallowing it is worse than the panic it replaces: the process
// comes up serving whatever files the walk did reach, and an asset that silently is not there is a
// page that renders without its stylesheet or a script that 404s, discovered by a user rather than
// by the build. Nothing covered the error the walk itself hands the callback, only the read failure
// after it, so dropping that check left every test green.
func TestNewAssetHandlerPanicsWhenTheTreeCannotBeWalked(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		FailDir   string
		WantPanic bool
	}{{ // Test 0: A subtree that will not list stops the build.
		Name: "unlistable subtree", FailDir: "fonts", WantPanic: true,
	}, { // Test 1: The root failing to list stops the build too.
		Name: "unlistable root", FailDir: ".", WantPanic: true,
	}, { // Test 2: The same tree with nothing failing builds, so the panic is the failure and not
		// the fixture.
		Name: "nothing fails", FailDir: "none",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tree := errReadDirFS{
				MapFS: fstest.MapFS{
					"js/01-boot.js":    {Data: []byte("const BOOT = 1;\n")},
					"app.css":          {Data: []byte("a{}\n")},
					"fonts/body.woff2": {Data: []byte("font")},
				},
				failDir: test.FailDir,
				err:     errors.New("listing refused"),
			}
			var panicked bool
			func() {
				defer func() {
					if recover() != nil {
						panicked = true
					}
				}()
				newAssetHandler(tree)
			}()
			if panicked != test.WantPanic {
				t.Errorf("%s: panicked = %t, want %t: a tree that cannot be walked must stop the "+
					"build rather than come up serving whatever the walk reached", test.Name,
					panicked, test.WantPanic)
			}
		})
	}
}
