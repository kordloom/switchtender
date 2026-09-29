package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// pinnedModule matches a go.mod requirement or a go get that names one fixed release of this module.
var pinnedModule = regexp.MustCompile(`github\.com/kordloom/switchtender[ @]v\d+\.\d+\.\d+`)

// TestTheSDKGuideBuildsTheCurrentRelease holds the extension guide to the release a reader builds
// against. It pinned v1.19.0 for months, which predates the token the server mints on its first
// public start, so a reader who followed it built a server whose API answered anyone on the network.
// Every protection at the door ships with the version, so the guide takes whatever is current.
func TestTheSDKGuideBuildsTheCurrentRelease(t *testing.T) {
	t.Parallel()
	body, err := os.ReadFile(filepath.Join("..", "..", "docs", "sdk.md"))
	if err != nil {
		t.Fatalf("read the SDK guide: %v", err)
	}
	if !strings.Contains(string(body), "go get github.com/kordloom/switchtender@latest") {
		t.Error("the SDK guide no longer fetches the current release with go get ...@latest")
	}
	if pins := pinnedModule.FindAllString(string(body), -1); len(pins) > 0 {
		t.Errorf("the SDK guide pins a fixed release, which goes stale and ships a server without "+
			"what later releases enforce: %v", pins)
	}
}
