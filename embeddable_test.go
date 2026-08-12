package main

import (
	"os/exec"
	"strings"
	"testing"
)

// embeddablePackages are the packages an out-of-tree program is meant to import directly.
//
// Their whole purpose is to be a shared contract: a SwitchTender server produces the span beat feed
// and an outside witness consumes it, so both build against the same definitions rather than two
// copies that drift. That only works while these packages stay free of the rest of the tree, which
// is why each of them says so in its own package comment.
var embeddablePackages = []string{
	"./beatfeed",
	"./identity",
	"./spanbeat",
	"./witness",
}

// TestEmbeddablePackagesStayEmbeddable checks that nothing an outside program imports has reached
// back into the server's private packages.
//
// The rule was a convention held by hand and nothing enforced it. One ordinary looking import of an
// internal helper is all it takes, and the break does not show up here: the tree still compiles,
// every test still passes, and the damage lands on whoever tries to build a witness against a
// tagged version, months later and out of sight. This asks the toolchain for the real transitive
// dependency set rather than reading the import blocks, so a package pulled in two hops away is
// caught the same as a direct one.
func TestEmbeddablePackagesStayEmbeddable(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("no go toolchain to resolve dependencies with")
	}
	for _, pkg := range embeddablePackages {
		t.Run(strings.TrimPrefix(pkg, "./"), func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command("go", "list", "-deps", pkg).Output()
			if err != nil {
				t.Fatalf("go list -deps %s error = %v", pkg, err)
			}
			for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
				if strings.Contains(dep, "/switchtender/internal/") {
					t.Errorf("%s depends on %s, so an out-of-tree witness can no longer build "+
						"against it; move what it needs into the shared package instead", pkg, dep)
				}
			}
		})
	}
}
