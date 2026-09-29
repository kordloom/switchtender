package server

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// routePattern matches a route registered on the mux in server.go.
var routePattern = regexp.MustCompile(`mux\.Handle\("([A-Z]+) ([^"]+)"`)

// docRowPattern matches a method and path documented as a table row in docs/api.md.
var docRowPattern = regexp.MustCompile(`\|\s*([A-Z]+)\s*\|\s*` + "`" + `([^` + "`" + `]+)` + "`")

// docProsePaths are the registered routes docs/api.md covers in prose rather than as a table row,
// because neither is an API endpoint a caller programs against: one publishes the install's signing
// key for a relying party, and the other is the root redirect to the web UI.
var docProsePaths = map[string]bool{
	"/.well-known/loomseal.json": true,
	"/{$}":                       true,
}

// TestEveryRouteIsDocumented holds docs/api.md to the claim it opens with.
//
// The page says "Every endpoint the server exposes" and was missing twelve, including the two that
// serve a run's evidence document and its signed receipt, and the two that browse a project
// checkout. A customer's security review inventories the attack surface from this page, so a route
// it omits is a route nobody assesses and whose role requirement nobody checks. The claim is only
// worth making if something enforces it.
func TestEveryRouteIsDocumented(t *testing.T) {
	t.Parallel()
	source, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatalf("ReadFile(server.go) error = %v", err)
	}
	doc, err := os.ReadFile(filepath.Join("..", "..", "docs", "api.md"))
	if err != nil {
		t.Fatalf("ReadFile(api.md) error = %v", err)
	}

	documented := make(map[string]bool)
	for _, m := range docRowPattern.FindAllStringSubmatch(string(doc), -1) {
		documented[m[1]+" "+m[2]] = true
	}

	var missing []string
	for _, m := range routePattern.FindAllStringSubmatch(string(source), -1) {
		method, path := m[1], m[2]
		if docProsePaths[path] {
			continue
		}
		// The UI and its assets are a browser surface, not the JSON API this page documents.
		if strings.HasPrefix(path, "/ui/") {
			continue
		}
		if !documented[method+" "+path] {
			missing = append(missing, method+" "+path)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("docs/api.md claims to list every endpoint and omits %d:\n  %s",
			len(missing), strings.Join(missing, "\n  "))
	}
}
