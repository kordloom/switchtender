package mcp

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// apiPath matches an API path written in the tool definitions.
var apiPath = regexp.MustCompile(`"(/v1/[a-z/{}._-]*)"`)

// TestAgentToolsUseOnlyOrdinaryEndpoints holds the property the whole agent gate rests on.
//
// Every claim made for this surface reduces to one thing: an agent's request is an ordinary
// authenticated API request, so it meets the same authorization, the same approval policy, and the
// same audit append a person's does. A tool that reached a privileged or internal path would keep
// the tool list looking identical while stepping around all three, and nothing about the tool's
// name would say so.
//
// The reversibility gate is a live example of why the path matters. It is applied where a run is
// submitted, so an agent proposing a playbook that deletes an archive is held only because the
// proposal arrives at the same endpoint a person's does.
func TestAgentToolsUseOnlyOrdinaryEndpoints(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile("tools.go")
	if err != nil {
		t.Fatalf("read tools: %v", err)
	}
	seen := map[string]bool{}
	for _, m := range apiPath.FindAllStringSubmatch(string(body), -1) {
		seen[m[1]] = true
	}
	var got []string
	for path := range seen {
		got = append(got, path)
	}
	sort.Strings(got)

	// Runs and templates, and nothing else. An agent lists templates, proposes a run, and reads
	// what happened; every one of those is a public endpoint a person uses too.
	want := []string{"/v1/runs", "/v1/runs/", "/v1/templates", "/v1/templates/"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the agent surface reaches different endpoints (-want +got):\n%s\n"+
			"Every claim made for this gate assumes an agent's request is an ordinary API request. "+
			"A new path here has to be checked against that before it is added.", diff)
	}
	for _, path := range got {
		if strings.Contains(path, "internal") || strings.Contains(path, "admin") {
			t.Errorf("the agent surface reaches %q, which is not a path a person's session uses", path)
		}
	}
}
