package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// paidFunc is a planted paid function well over every floor.
const paidFunc = `func scoreRules(rules []Rule, facts map[string]string) (int, error) {
	total := 0
	for i, r := range rules {
		v, ok := facts[r.Key]
		if !ok {
			return 0, fmt.Errorf("rule %d: missing fact %q", i, r.Key)
		}
		if strings.EqualFold(v, r.Want) {
			total += r.Weight
		} else if r.Required {
			return 0, fmt.Errorf("rule %d: required fact %q is %q", i, r.Key, v)
		}
	}
	return total, nil
}
`

// writeFile writes body at path under root, creating parents.
func writeFile(t *testing.T, root, path, body string) {
	t.Helper()
	full := filepath.Join(root, filepath.FromSlash(path))
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestRun(t *testing.T) { //nolint:funlen // Test function.
	t.Parallel()
	tests := []struct {
		WantCode   int
		WantStdout string
		WantStderr string
		Args       func(paid, public string) []string
		Plant      bool
		EmptyList  bool
	}{{ // Test 0: No subcommand.
		Args:     func(_, _ string) []string { return nil },
		WantCode: exitUsage, WantStderr: "usage",
	}, { // Test 1: Unknown subcommand.
		Args:     func(_, _ string) []string { return []string{"scan"} },
		WantCode: exitUsage, WantStderr: "unknown command",
	}, { // Test 2: gen without its trees.
		Args:     func(_, _ string) []string { return []string{"gen"} },
		WantCode: exitUsage, WantStderr: "required",
	}, { // Test 3: check of the clean tree passes.
		Args:     func(_, public string) []string { return []string{"check", "-root", public} },
		WantCode: exitOK, WantStderr: "clean against",
	}, { // Test 4: check of a planted copy fails and names the file and line.
		Args:     func(_, public string) []string { return []string{"check", "-root", public} },
		Plant:    true,
		WantCode: exitFound, WantStdout: "internal/core/stolen.go:3: matches paid code (exact func)",
	}, { // Test 5: An empty list is refused rather than passing everything.
		Args:      func(_, public string) []string { return []string{"check", "-root", public} },
		EmptyList: true,
		WantCode:  exitUsage, WantStderr: "holds no fingerprints",
	}, { // Test 6: A missing list is a usage error.
		Args: func(_, public string) []string {
			return []string{"check", "-root", public, "-list", filepath.Join(public, "absent.txt")}
		},
		WantCode: exitUsage, WantStderr: "absent.txt",
	}, { // Test 7: gen fails when the paid function is already public.
		Args: func(paid, public string) []string {
			writeFile(t, public, "internal/core/early.go", "package core\n\n"+paidFunc)
			return []string{"gen", "-paid", paid, "-public", public, "-out", filepath.Join(public, "x.txt")}
		},
		WantCode: exitFound, WantStderr: "paid code already present in public tree",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			paid, public := t.TempDir(), t.TempDir()
			writeFile(t, paid, "rules/score.go", "package rules\n\n"+paidFunc)
			writeFile(t, public, "internal/core/core.go",
				"package core\n\nfunc Add(a, b int) int { return a + b }\n")
			writeFile(t, public, "internal/wallcheck/paid-fingerprints.txt", "")
			listPath := filepath.Join(public, "internal", "wallcheck", "paid-fingerprints.txt")
			var genErr bytes.Buffer
			if code := run([]string{"gen", "-paid", paid, "-public", public, "-out", listPath},
				&bytes.Buffer{}, &genErr); code != exitOK {
				t.Fatalf("setup gen exited %d: %s", code, genErr.String())
			}
			if test.EmptyList {
				writeFile(t, public, "internal/wallcheck/paid-fingerprints.txt", "# empty\n")
			}
			if test.Plant {
				writeFile(t, public, "internal/core/stolen.go", "package core\n\n"+paidFunc)
			}
			var stdout, stderr bytes.Buffer
			code := run(test.Args(paid, public), &stdout, &stderr)
			if diff := cmp.Diff(test.WantCode, code); diff != "" {
				t.Errorf("exit code mismatch (-want +got):\n%s\nstderr: %s", diff, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.WantStdout) {
				t.Errorf("stdout %q does not contain %q", stdout.String(), test.WantStdout)
			}
			if !strings.Contains(stderr.String(), test.WantStderr) {
				t.Errorf("stderr %q does not contain %q", stderr.String(), test.WantStderr)
			}
		})
	}
}
