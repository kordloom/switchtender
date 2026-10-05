//go:build unix

package roundhouse

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// digestRuntime writes a stand-in container runtime that succeeds at every run and answers an image
// inspect with repoDigests, logging each invocation, and returns its path and the log's path.
func digestRuntime(t *testing.T, repoDigests string) (bin, logPath string) {
	t.Helper()
	dir := t.TempDir()
	logPath = filepath.Join(dir, "calls.log")
	script := `#!/bin/sh
[ "$1" = stub-warmup ] && exit 0
echo "$@" >> ` + logPath + `
if [ "$1" = image ] && [ "$2" = inspect ]; then
  echo '` + repoDigests + `'
fi
exit 0
`
	bin = filepath.Join(dir, "docker")
	writeStub(t, bin, script)
	return bin, logPath
}

// TestAContainerRunRecordsTheDigestItPulled pins that a container run names the image bytes it
// executed. A tag can be moved to another image at any time, so a run bound only to a tag records the
// digest the runtime holds for it after pulling, and a run pinned by digest records that digest.
func TestAContainerRunRecordsTheDigestItPulled(t *testing.T) {
	t.Parallel()
	pinned := "sha256:" + strings.Repeat("d", 64)
	pulled := "sha256:" + strings.Repeat("e", 64)
	other := "sha256:" + strings.Repeat("f", 64)
	tests := []struct {
		// Name labels the case.
		Name string
		// Image is the reference the run executes in.
		Image string
		// RepoDigests is what the runtime reports for the image.
		RepoDigests string
		// WantDigest is the digest the run records.
		WantDigest string
		// WantInspect reports whether the runtime is asked.
		WantInspect bool
	}{{ // Test 0: A tag records the digest the runtime holds for that repository.
		Name: "tag", Image: "registry.example.com/runner:2",
		RepoDigests: `["mirror.example.com/runner@` + other + `","registry.example.com/runner@` + pulled + `"]`,
		WantDigest:  pulled, WantInspect: true,
	}, { // Test 1: A reference pinned by digest records that digest without asking.
		Name: "pinned", Image: "registry.example.com/runner:2@" + pinned, RepoDigests: `[]`,
		WantDigest: pinned, WantInspect: false,
	}, { // Test 2: A runtime that cannot say leaves no digest rather than a guess.
		Name: "unknown", Image: "registry.example.com/runner:2", RepoDigests: `[]`,
		WantDigest: "", WantInspect: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			bin, logPath := digestRuntime(t, test.RepoDigests)
			c := newContainerRunner(bin, "missing", false, nil, &pluginCache{}, DefaultContainerLimits())
			spec := Spec{Tool: "bash", Command: "true", Image: test.Image}
			res, err := c.Run(context.Background(), spec, io.Discard)
			if err != nil {
				t.Fatalf("Run() error = %v", err)
			}
			if res.ImageDigest != test.WantDigest {
				t.Errorf("ImageDigest = %q, want %q", res.ImageDigest, test.WantDigest)
			}
			calls, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("read the runtime log: %v", err)
			}
			if inspected := strings.Contains(string(calls), "image inspect"); inspected != test.WantInspect {
				t.Errorf("the runtime was asked = %v, want %v:\n%s", inspected, test.WantInspect, calls)
			}
		})
	}
}
