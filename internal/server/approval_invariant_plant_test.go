package server

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// TestTheInvariantCatchesADecisionPlantedOnACallbackRoute plants each way a provisioning callback
// could start deciding into a copy of this package and shows the checks the invariant runs name it:
// a call to an approval primitive from the callback handler, a deciding handler bound to the
// AWX-compatible callback address through the variable that address registers, and a deciding
// handler on the template's own callback address. The copy with nothing planted is the control and
// reports nothing. Before the route reader read every file and resolved constant patterns and
// handler variables, the AWX-compatible address was invisible to it, and a deciding handler there
// passed.
func TestTheInvariantCatchesADecisionPlantedOnACallbackRoute(t *testing.T) {
	t.Parallel()
	root := filepath.Join("..", "..")
	relay := routeHandlers(t, root, filepath.Join(root, "internal", "relay"))
	tests := []struct {
		// File is the file the plant goes into, empty for the control.
		File string
		// Old is the text the plant replaces, and New what replaces it.
		Old, New string
		// WantProblem is what the invariant must report, empty for nothing.
		WantProblem string
	}{{  // Test 0: Nothing planted.
	}, { // Test 1: The callback handler calls an approval primitive.
		File: "callback_handlers.go", Old: "func firedPath(",
		New: "func (c *callbacks) planted(ctx context.Context, id string) {\n" +
			"\t_, _ = c.store.(interface{ Approve(context.Context, string) error }).Approve(ctx, id)\n" +
			"}\n\nfunc firedPath(",
		WantProblem: "internal/server/callback_handlers.go:callbacks.planted:Approve reaches an " +
			"approval primitive",
	}, { // Test 2: The AWX-compatible address is served by a deciding handler.
		File: "awx_callback_handlers.go", Old: "h := c.awx()",
		New:         "h := approveRunHandler(nil, nil, nil, nil)",
		WantProblem: "POST /api/v2/job_templates/{id}/callback is a callback or hook route served",
	}, { // Test 3: The template's own callback address is served by a deciding handler.
		File:        "server.go",
		Old:         `mux.Handle("POST /v1/templates/{id}/callback", callbacks.native())`,
		New:         `mux.Handle("POST /v1/templates/{id}/callback", rejectRunHandler(nil, nil))`,
		WantProblem: "POST /v1/templates/{id}/callback is a callback or hook route served",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			tmp := t.TempDir()
			dir := filepath.Join(tmp, "internal", "server")
			plantedPackage(t, dir, test.File, test.Old, test.New)
			problems := callbackSurfaceProblems(scanApprovalCalls(t, tmp),
				routeHandlers(t, root, dir), relay)
			if test.WantProblem == "" {
				if len(problems) != 0 {
					t.Errorf("the unplanted copy reports %q, want nothing", problems)
				}
				return
			}
			if !slices.ContainsFunc(problems, func(p string) bool {
				return strings.Contains(p, test.WantProblem)
			}) {
				t.Errorf("a decision planted in %s went unreported: got %q, want %q", test.File,
					problems, test.WantProblem)
			}
		})
	}
}

// plantedPackage copies this package's non-test Go files into dir, replacing old with planted in
// file. An empty file copies the package as it is.
func plantedPackage(t *testing.T, dir, file, old, planted string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create %s: %v", dir, err)
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read the package: %v", err)
	}
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		body, err := os.ReadFile(name)
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if name == file {
			if !strings.Contains(string(body), old) {
				t.Fatalf("%s no longer holds %q, so the plant has nowhere to go", name, old)
			}
			body = []byte(strings.Replace(string(body), old, planted, 1))
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o600); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
}
