package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestDistinctCaptures pins what the screenshot phase requires of its folder: at least one capture,
// and no two captures with the same bytes. A signed-out browser is sent to the sign-in form from
// every page, so it leaves one picture under every name, and the phase must not count that as
// evidence of anything.
func TestDistinctCaptures(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Files maps each file written into the folder to its bytes.
		Files map[string]string
		// Dirs lists the folders created inside the folder.
		Dirs []string
		// Missing leaves the folder itself uncreated.
		Missing bool
		// WantNames is the captures returned, in name order.
		WantNames []string
		// WantNamed is each piece of text the error must carry.
		WantNamed []string
		// Want is the error the check ends in.
		Want error
	}{{ // Test 0: Captures that all differ pass, in name order.
		Files:     map[string]string{"runs.png": "runs", "estate.png": "estate", "policies.png": "p"},
		WantNames: []string{"estate.png", "policies.png", "runs.png"},
	}, { // Test 1: Two captures with the same bytes fail, and both are named with their digest.
		Files: map[string]string{"runs.png": "form", "estate.png": "form", "policies.png": "p"},
		WantNamed: []string{"estate.png, runs.png share sha256 " +
			"07397d633f25a7101990a75864ae03d5a3b9ac07c4ed6accbc52cbfd7d7c13b4"},
		Want: ErrSameCapture,
	}, { // Test 2: Every capture the same, as a signed-out browser leaves them.
		Files: map[string]string{"runs.png": "form", "estate.png": "form", "policies.png": "form",
			"governed-run.png": "form"},
		WantNamed: []string{"estate.png, governed-run.png, policies.png, runs.png share sha256"},
		Want:      ErrSameCapture,
	}, { // Test 3: Two separate pairs are both named.
		Files:     map[string]string{"a.png": "one", "b.png": "one", "c.png": "two", "d.png": "two"},
		WantNamed: []string{"a.png, b.png share sha256", "c.png, d.png share sha256"},
		Want:      ErrSameCapture,
	}, { // Test 4: An empty folder holds no capture.
		Want: ErrNothingRead,
	}, { // Test 5: Files that are not captures do not count as one.
		Files: map[string]string{"notes.txt": "form", "report.md": "form"},
		Want:  ErrNothingRead,
	}, { // Test 6: Files that are not captures are not compared.
		Files:     map[string]string{"runs.png": "runs", "a.txt": "same", "b.txt": "same"},
		WantNames: []string{"runs.png"},
	}, { // Test 7: A folder named like a capture is not one.
		Files:     map[string]string{"runs.png": "runs"},
		Dirs:      []string{"estate.png"},
		WantNames: []string{"runs.png"},
	}, { // Test 8: A folder that does not exist is an error, not an empty pass.
		Missing: true,
		Want:    fs.ErrNotExist,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			dir := filepath.Join(t.TempDir(), "shots")
			if !test.Missing {
				if err := os.Mkdir(dir, 0o750); err != nil {
					t.Fatalf("Mkdir() error = %v", err)
				}
			}
			for _, name := range test.Dirs {
				if err := os.Mkdir(filepath.Join(dir, name), 0o750); err != nil {
					t.Fatalf("Mkdir(%s) error = %v", name, err)
				}
			}
			for name, body := range test.Files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
					t.Fatalf("WriteFile(%s) error = %v", name, err)
				}
			}
			got, err := distinctCaptures(dir)
			if !errors.Is(err, test.Want) {
				t.Fatalf("distinctCaptures() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantNames, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("distinctCaptures() names mismatch (-want +got):\n%s", diff)
			}
			for _, text := range test.WantNamed {
				if !strings.Contains(err.Error(), text) {
					t.Errorf("distinctCaptures() error = %q, want it to carry %q", err, text)
				}
			}
		})
	}
}

// TestCaptureUIHoldsCapturesToMoreThanAnExitCode runs the capture step of the screenshot phase with
// a stand-in for node, so what the phase concludes is tested with no cluster and no browser. The
// stand-in exits zero in every row, and an exit code alone is what the phase must not take as its
// evidence. The stand-in also records what it was started with, which shows the sign-in token
// travels in the environment and never in the arguments.
func TestCaptureUIHoldsCapturesToMoreThanAnExitCode(t *testing.T) {
	// Not parallel: it puts a stand-in for node first on PATH.
	const token = "swt_stand-in-not-a-real-token"
	tests := []struct {
		// Writes is the shell the stand-in runs to write captures into the folder its fourth
		// argument names.
		Writes string
		// WantResult is the evidence returned for the report.
		WantResult string
		// Want is the error the step ends in.
		Want error
	}{{ // Test 0: Captures that differ pass, and the evidence names them.
		Writes:     `printf runs > "$4/runs.png"; printf estate > "$4/estate.png"`,
		WantResult: "signed in as casey, no two captures alike: estate.png, runs.png",
	}, { // Test 1: One picture under two names fails.
		Writes: `printf form > "$4/runs.png"; printf form > "$4/estate.png"`,
		Want:   ErrSameCapture,
	}, { // Test 2: A script that exits zero and wrote nothing fails.
		Writes: "true",
		Want:   ErrNothingRead,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			// Not parallel: every row changes PATH.
			bin, record := t.TempDir(), t.TempDir()
			standIn := "#!/bin/sh\n" +
				"printf '%s\\n' \"$@\" > '" + filepath.Join(record, "argv") + "'\n" +
				"printf '%s' \"$" + shotsTokenEnv + "\" > '" + filepath.Join(record, "token") + "'\n" +
				"mkdir -p \"$4\"\n" + test.Writes + "\n"
			if err := os.WriteFile(filepath.Join(bin, "node"), []byte(standIn), 0o700); err != nil {
				t.Fatalf("WriteFile(node) error = %v", err)
			}
			t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
			h := &harness{repo: t.TempDir(), api: "http://127.0.0.1:1", ids: map[string]string{},
				human: actor{Name: "casey", Type: "user", Token: token}}
			got, err := h.captureUI(filepath.Join(bin, "shots.mjs"),
				filepath.Join(t.TempDir(), "shots"))
			if !errors.Is(err, test.Want) {
				t.Fatalf("captureUI() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantResult, got); diff != "" {
				t.Errorf("captureUI() evidence mismatch (-want +got):\n%s", diff)
			}
			argv, err := os.ReadFile(filepath.Join(record, "argv"))
			if err != nil {
				t.Fatalf("the stand-in recorded no arguments: %v", err)
			}
			if strings.Contains(string(argv), token) {
				t.Errorf("the token rode the arguments: %q", argv)
			}
			carried, err := os.ReadFile(filepath.Join(record, "token"))
			if err != nil {
				t.Fatalf("the stand-in recorded no environment: %v", err)
			}
			if string(carried) != token {
				t.Errorf("the environment carried %q, want the token", carried)
			}
		})
	}
}
