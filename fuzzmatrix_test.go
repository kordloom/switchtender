package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestFuzzMatrixCoversEveryTarget holds the fuzz workflow's matrix to the module's actual fuzz
// functions, in both directions.
//
// The matrix is a hand-list in yaml that nothing compiled: five targets were listed while eleven
// existed, so six parsers, among them the credential injector and the MCP protocol reader, were
// never fuzzed by the job that exists to fuzz them. A new Fuzz function must fail this test until
// the workflow runs it, and a matrix row naming a deleted function must fail until it is removed,
// because a green weekly fuzz over the wrong list reads as coverage it is not.
func TestFuzzMatrixCoversEveryTarget(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join(".github", "workflows", "fuzz.yml"))
	if err != nil {
		t.Fatalf("read fuzz.yml: %v (if the workflow moved, move this guard's path with it)", err)
	}
	row := regexp.MustCompile(`\{\s*pkg:\s*(\S+),\s*fn:\s*(Fuzz\w+)\s*\}`)
	inMatrix := map[string]string{}
	for _, m := range row.FindAllStringSubmatch(string(raw), -1) {
		inMatrix[m[2]] = strings.TrimPrefix(m[1], "./")
	}
	if len(inMatrix) == 0 {
		t.Fatal("no matrix rows parsed from fuzz.yml: the row shape changed and this guard is " +
			"matching nothing")
	}

	decl := regexp.MustCompile(`(?m)^func (Fuzz\w+)\(`)
	inSource := map[string]string{}
	err = filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "site" || name == "assets" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		body, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		for _, m := range decl.FindAllStringSubmatch(string(body), -1) {
			inSource[m[1]] = filepath.Dir(path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(inSource) == 0 {
		t.Fatal("no Fuzz functions found in the module: the scan is matching nothing")
	}

	for fn, dir := range inSource {
		if _, ok := inMatrix[fn]; !ok {
			t.Errorf("%s in %s is not in fuzz.yml's matrix: the weekly job never fuzzes it", fn, dir)
		}
	}
	for fn, pkg := range inMatrix {
		dir, ok := inSource[fn]
		if !ok {
			t.Errorf("fuzz.yml lists %s in %s but no such fuzz function exists", fn, pkg)
			continue
		}
		if dir != pkg {
			t.Errorf("fuzz.yml runs %s in %s but it lives in %s", fn, pkg, dir)
		}
	}
}
