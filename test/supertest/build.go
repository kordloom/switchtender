package main

import (
	"fmt"
	"os"
	"path/filepath"
)

// paidModule is where the private paid module is mounted in a checkout that has it. A public clone
// holds an empty directory here.
const paidModule = "internal/paid/impl"

// buildTarget returns the build tags and the main package for the binary and the image. The paid
// build has its own entry point inside the paid module, so no public file names that module.
func buildTarget(paid bool) (tags, main string) {
	if paid {
		return "paid", "./" + paidModule + "/cmd/switchtender"
	}
	return "", "."
}

// checkBuild refuses a run whose phases cannot pass on the build it would test, before any cluster
// is made: the Team phases on the Community build, or a paid build from a checkout without the
// paid module.
func checkBuild(repo string, paid, skipTeam bool) error {
	if !paid {
		if !skipTeam {
			return fmt.Errorf("%w: pass -paid from a checkout with %s mounted, or run with -skip-team "+
				"to test the Community tier alone", ErrTeamNeedsPaid, paidModule)
		}
		return nil
	}
	_, main := buildTarget(true)
	if _, err := os.Stat(filepath.Join(repo, main, "main.go")); err != nil {
		return fmt.Errorf("%w: -paid builds %s, and %s holds no main.go: run git submodule update "+
			"--init %s and copy its build/go.mod and build/go.sum over the root", ErrPaidModuleMissing,
			main, filepath.Join(repo, main), paidModule)
	}
	return nil
}
