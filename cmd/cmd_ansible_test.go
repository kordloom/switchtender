package cmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/ansibleruntime"
)

// TestAnsibleCommands drives the ansible commands as an operator would: listing an empty runtime
// directory, printing a release's lock, and the refusals for a release that is not installed, one
// that is not supported, and a Python that is not there.
func TestAnsibleCommands(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantCode      int
		WantOut       []string
		WantErr       []string
		Args          []string
		UsesDirectory bool
	}{{ // Test 0: Listing a directory with nothing installed prints no runtimes.
		Args: []string{"ansible", "list", "--dir"}, UsesDirectory: true,
		WantOut: []string{`"runtimes":[]`, `"runtime_dir":"`},
	}, { // Test 1: The lock for a minor version is its pinned release, with hashes.
		Args:    []string{"ansible", "lock", "--version", "2.20"},
		WantOut: []string{"# ansible-core: 2.20.9", "ansible-core==2.20.9", "--hash=sha256:"},
	}, { // Test 2: Removing a release that is not installed fails and says so.
		Args: []string{"ansible", "remove", "--version", "2.20", "--dir"}, UsesDirectory: true,
		WantCode: 1, WantErr: []string{"not installed"},
	}, { // Test 3: Installing an unsupported release fails before touching anything.
		Args: []string{"ansible", "install", "--version", "2.15", "--dir"}, UsesDirectory: true,
		WantCode: 1, WantErr: []string{"unsupported ansible-core version", "Supported: 2.16.19"},
	}, { // Test 4: Installing with a Python that is not there fails, naming the range needed.
		Args:          []string{"ansible", "install", "--python", "/no/such/python3", "--dir"},
		UsesDirectory: true, WantCode: 1,
		WantErr: []string{"runs on Python 3.12 to 3.14", "/no/such/python3 was not found"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			args := test.Args
			dir := filepath.Join(t.TempDir(), "rt")
			if test.UsesDirectory {
				args = append(append([]string{}, args...), dir)
			}
			stdout, stderr, code := runCLI(t, args...)
			if code != test.WantCode {
				t.Fatalf("%v exited %d, want %d\nstdout: %s\nstderr: %s", args, code, test.WantCode,
					stdout, stderr)
			}
			for _, w := range test.WantOut {
				if !strings.Contains(stdout, w) {
					t.Errorf("%v printed %q, want it to contain %q", args, stdout, w)
				}
			}
			for _, w := range test.WantErr {
				if !strings.Contains(stderr, w) {
					t.Errorf("%v reported %q, want it to contain %q", args, stderr, w)
				}
			}
			if test.UsesDirectory {
				if _, err := os.Stat(filepath.Join(dir, "current")); !os.IsNotExist(err) {
					t.Errorf("%v made a runtime current: %v", args, err)
				}
			}
		})
	}
}

// TestAnsibleRoot pins where the ansible commands, serve, and worker look for the managed runtime:
// --dir, then the environment, then ansible/ beside the SQLite database, and for a PostgreSQL DSN
// switchtender/ansible in the account's configuration directory.
func TestAnsibleRoot(t *testing.T) {
	config, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("this account has no configuration directory: %v", err)
	}
	tests := []struct {
		WantRoot string
		Dir      string
		Env      string
		DB       string
	}{{ // Test 0: Beside the SQLite database by default.
		DB: "/var/lib/switchtender/switchtender.db", WantRoot: "/var/lib/switchtender/ansible",
	}, { // Test 1: In the configuration directory for a PostgreSQL DSN.
		DB:       "postgres://u:p@db/switchtender",
		WantRoot: filepath.Join(config, "switchtender", "ansible"),
	}, { // Test 2: The environment overrides the data directory.
		Env: "/srv/ansible", DB: "/var/lib/switchtender/switchtender.db", WantRoot: "/srv/ansible",
	}, { // Test 3: The flag overrides the environment.
		Dir: "/opt/rt", Env: "/srv/ansible", DB: "switchtender.db", WantRoot: "/opt/rt",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Setenv(ansibleruntime.RootEnv, test.Env)
			got, err := ansibleRoot(test.Dir, test.DB)
			if err != nil {
				t.Fatalf("ansibleRoot() error = %v", err)
			}
			if diff := cmp.Diff(test.WantRoot, got); diff != "" {
				t.Errorf("ansibleRoot() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
