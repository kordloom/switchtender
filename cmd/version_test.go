package cmd

import "testing"

// TestResolveVersion confirms an ldflags-set release version wins over the build-info fallback, and
// the fallback yields a non-empty version for the dev placeholder. It mutates the package global, so
// it does not run in parallel.
func TestResolveVersion(t *testing.T) {
	old := Version
	defer func() { Version = old }()

	Version = "v9.9.9"
	if got := resolveVersion(); got != "v9.9.9" {
		t.Errorf("resolveVersion() = %q, want v9.9.9", got)
	}

	Version = "0.0.0-dev"
	if got := resolveVersion(); got == "" {
		t.Error("resolveVersion() with the dev placeholder returned empty")
	}
}
