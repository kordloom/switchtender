package cmd

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kordloom/switchtender/internal/util"
)

// TestTheStartCommandSourcesTheConfigItNames pins the dot command init prints. A config path given
// as an absolute path was printed as .//etc/switchtender.env, which exists nowhere: the start command
// failed, and the server it went on to start ran without the encryption key. Each case is run through
// a real shell, which is what the reader does with it.
func TestTheStartCommandSourcesTheConfigItNames(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh to run the printed command")
	}
	dir := t.TempDir()
	tests := []struct {
		// Name is the file, relative to dir or absolute.
		Name string
	}{{ // Test 0: The default, a bare name in the working directory.
		Name: "switchtender.env",
	}, { // Test 1: A relative path with a directory in it.
		Name: "conf/switchtender.env",
	}, { // Test 2: An absolute path, the one that broke.
		Name: filepath.Join(dir, "abs", "switchtender.env"),
	}, { // Test 3: A path with a space, which the command quotes.
		Name: "my conf/switchtender.env",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			work := t.TempDir()
			path := test.Name
			full := path
			if !filepath.IsAbs(path) {
				full = filepath.Join(work, path)
			}
			if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
				t.Fatalf("MkdirAll() error = %v", err)
			}
			if err := os.WriteFile(full, []byte("SWITCHTENDER_TEST_SOURCED=yes\n"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			script := "set -a; . " + util.ShellQuote(sourceablePath(path)) +
				"; set +a; printf %s \"$SWITCHTENDER_TEST_SOURCED\""
			cmd := exec.Command("sh", "-c", script)
			cmd.Dir = work
			cmd.Env = []string{"PATH=/usr/bin:/bin"}
			out, err := cmd.CombinedOutput()
			if err != nil || string(out) != "yes" {
				t.Errorf("the printed command for %q failed: %v %q", path, err, out)
			}
		})
	}
}
