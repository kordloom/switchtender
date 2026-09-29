package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestTheEngineTableMatchesTheImageItDescribes makes the claim above the table true.
//
// The table says what the shipped image carries, and says so in a comment that promises adding a
// tool to the Dockerfile without moving it here will fail. Nothing enforced that, which made the
// promise the same kind of unbacked claim this suite exists to catch. Two lists in two languages
// with nothing between them is how the release gate came to demand seven engines while installing
// one.
//
// The Dockerfile is the authority on what the image carries. A tool the table calls
// operator-provided must not be installed there, and a tool the table expects to answer must be.
func TestTheEngineTableMatchesTheImageItDescribes(t *testing.T) {
	t.Parallel()
	root, err := repoRoot()
	if err != nil {
		t.Fatalf("locate the repository root: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "Dockerfile"))
	if err != nil {
		t.Fatalf("read the Dockerfile: %v", err)
	}
	// Only the runtime stage matters. The build stage is a Go toolchain image, so looking at the
	// whole file would find a go compiler that never reaches the image that ships.
	dockerfile := string(raw)
	if i := strings.LastIndex(dockerfile, "\nFROM "); i >= 0 {
		dockerfile = dockerfile[i:]
	}
	// Comments are not instructions. The first version of this read them and found the words "go"
	// and "pwsh" in the note explaining why neither is installed, so it reported both as shipped by
	// an image that does not ship them.
	dockerfile = stripDockerComments(dockerfile)
	for _, engine := range advertisedEngines {
		t.Run(engine.Tool, func(t *testing.T) {
			t.Parallel()
			installed := namesBinary(dockerfile, engine.Binary)
			switch engine.Want {
			case engineOperatorProvided:
				if installed {
					t.Errorf("the table calls %s operator-provided and the runtime stage installs "+
						"%q. The image now carries it, so the run would be refused for the project "+
						"it needs rather than for the tool, and this check would fail against a "+
						"working install", engine.Tool, engine.Binary)
				}
			case engineExecutes, engineNeedsProject:
				if !installed {
					t.Errorf("the table expects %s to answer from the image and the runtime stage "+
						"does not install %q, so the run would be refused for a missing tool and "+
						"this check would fail against an image that is behaving correctly",
						engine.Tool, engine.Binary)
				}
			}
		})
	}
}

// repoRoot walks up from the test's directory to the module root, so the test does not depend on
// where it was invoked from.
func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for range 10 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			break
		}
		dir = parent
	}
	return "", os.ErrNotExist
}

// stripDockerComments removes comment lines, so a note about a tool is not read as installing it.
func stripDockerComments(dockerfile string) string {
	var kept []string
	for _, line := range strings.Split(dockerfile, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		kept = append(kept, line)
	}
	return strings.Join(kept, "\n")
}

// namesBinary reports whether the Dockerfile names this executable as a whole word.
//
// A substring match answers yes for "go" inside "gcompat" and for "tofu" inside a URL that only
// downloads it for another architecture, so the boundaries matter more than they look.
func namesBinary(dockerfile, binary string) bool {
	for i := 0; ; {
		j := strings.Index(dockerfile[i:], binary)
		if j < 0 {
			return false
		}
		start, end := i+j, i+j+len(binary)
		beforeOK := start == 0 || !isWordByte(dockerfile[start-1])
		afterOK := end == len(dockerfile) || !isWordByte(dockerfile[end])
		if beforeOK && afterOK {
			return true
		}
		i = end
	}
}

// isWordByte reports whether b can appear inside an identifier, so a match that touches one is a
// match inside a longer word rather than the word itself.
func isWordByte(b byte) bool {
	return b == '_' || b == '-' ||
		(b >= '0' && b <= '9') || (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
