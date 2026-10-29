package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// TestTheBuildMatchesThePhasesAskedFor holds the harness to refusing, before any cluster exists, a
// run whose build cannot pass its phases.
func TestTheBuildMatchesThePhasesAskedFor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Want      error
		WantTags  string
		WantMain  string
		Paid      bool
		SkipTeam  bool
		HasModule bool
	}{{ // Test 0: The Community tier on the Community build.
		Paid: false, SkipTeam: true, WantTags: "", WantMain: ".", Want: nil,
	}, { // Test 1: The Team phases on the Community build could only fail.
		Paid: false, SkipTeam: false, WantTags: "", WantMain: ".", Want: ErrTeamNeedsPaid,
	}, { // Test 2: The paid build from a checkout with the module.
		Paid: true, SkipTeam: false, HasModule: true, WantTags: "paid",
		WantMain: "./internal/paid/impl/cmd/switchtender", Want: nil,
	}, { // Test 3: The paid build from a public clone, where the module is an empty directory.
		Paid: true, SkipTeam: false, HasModule: false, WantTags: "paid",
		WantMain: "./internal/paid/impl/cmd/switchtender", Want: ErrPaidModuleMissing,
	}, { // Test 4: The Community phases alone on the paid build.
		Paid: true, SkipTeam: true, HasModule: true, WantTags: "paid",
		WantMain: "./internal/paid/impl/cmd/switchtender", Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			repo := t.TempDir()
			if err := os.MkdirAll(filepath.Join(repo, paidModule), 0o755); err != nil {
				t.Fatal(err)
			}
			if test.HasModule {
				main := filepath.Join(repo, paidModule, "cmd", "switchtender")
				if err := os.MkdirAll(main, 0o755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(main, "main.go"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			tags, main := buildTarget(test.Paid)
			if tags != test.WantTags || main != test.WantMain {
				t.Errorf("build target is (%q, %q), want (%q, %q)", tags, main, test.WantTags,
					test.WantMain)
			}
			if err := checkBuild(repo, test.Paid, test.SkipTeam); !errors.Is(err, test.Want) {
				t.Errorf("checkBuild error is %v, want %v", err, test.Want)
			}
		})
	}
}
