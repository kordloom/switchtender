//go:build !windows

package ansibleruntime

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestLocateOrder pins the order a locator prefers Ansible in: the configured directory, then the
// managed runtime when a finished one is current, then PATH. "system" names PATH outright.
func TestLocateOrder(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantSource  string
		WantDir     string
		WantRelease string
		Bin         string
		Managed     bool
		Unfinished  bool
		NoRoot      bool
	}{{ // Test 0: A configured directory wins over an installed managed runtime.
		Bin: "/opt/ansible/bin", Managed: true, WantSource: SourceConfigured,
		WantDir: "/opt/ansible/bin",
	}, { // Test 1: With nothing configured, an installed managed runtime wins over PATH.
		Managed: true, WantSource: SourceManaged, WantDir: "MANAGED", WantRelease: "2.21.4",
	}, { // Test 2: With nothing configured or installed, PATH is used.
		WantSource: SourcePath,
	}, { // Test 3: "system" uses PATH even when a managed runtime is installed.
		Bin: SystemBin, Managed: true, WantSource: SourcePath,
	}, { // Test 4: A current runtime whose install never finished fails closed, never PATH.
		Unfinished: true, WantSource: SourceManaged,
	}, { // Test 5: No runtime directory at all is PATH.
		NoRoot: true, WantSource: SourcePath,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			root := t.TempDir()
			want := test.WantDir
			if test.Managed {
				want = filepath.Join(fakeRuntime(t, root, "2.21.4", true), "bin")
				if test.WantDir != "MANAGED" {
					want = test.WantDir
				}
			}
			if test.Unfinished {
				dir := fakeRuntime(t, root, "2.21.4", true)
				if err := os.Remove(filepath.Join(dir, markerName)); err != nil {
					t.Fatalf("remove marker: %v", err)
				}
			}
			if test.NoRoot {
				root = ""
			}
			got := NewLocator(test.Bin, root).Locate()
			gotView := struct{ Source, Dir, Release string }{got.Source, got.Dir, got.Release}
			wantView := struct{ Source, Dir, Release string }{test.WantSource, want,
				test.WantRelease}
			if diff := cmp.Diff(wantView, gotView); diff != "" {
				t.Errorf("Locate() mismatch (-want +got):\n%s", diff)
			}
			if got.Root != root {
				t.Errorf("Locate().Root = %q, want %q", got.Root, root)
			}
			if (got.Problem != "") != test.Unfinished {
				t.Errorf("Locate().Problem = %q, want one only for the unfinished runtime",
					got.Problem)
			}
		})
	}
}

// TestNilLocatorIsPath pins that a runner without a locator keeps finding Ansible on PATH.
func TestNilLocatorIsPath(t *testing.T) {
	t.Parallel()
	var l *Locator
	if got := l.Locate(); got.Source != SourcePath || got.Dir != "" {
		t.Errorf("nil Locate() = %+v, want PATH", got)
	}
}

// TestCommandsPath pins how a command is found: inside the located directory or not at all, never
// on PATH instead.
func TestCommandsPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bin := filepath.Join(fakeRuntime(t, root, "2.21.4", true), "bin")
	tests := []struct {
		Want     error
		WantPath string
		Commands Commands
		Name     string
	}{{ // Test 0: A command in the directory is found there.
		Commands: Commands{Dir: bin, Source: SourceManaged}, Name: "ansible-playbook",
		WantPath: filepath.Join(bin, "ansible-playbook"),
	}, { // Test 1: A command missing from the directory is missing, not looked up on PATH.
		Commands: Commands{Dir: bin, Source: SourceConfigured}, Name: "sh",
		Want: ErrCommandMissing,
	}, { // Test 2: A command PATH does not have is missing.
		Commands: Commands{Source: SourcePath}, Name: "switchtender-no-such-command",
		Want: ErrCommandMissing,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := test.Commands.Path(test.Name)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Path(%q) error = %v, want %v", test.Name, err, test.Want)
			}
			if diff := cmp.Diff(test.WantPath, got); diff != "" {
				t.Errorf("Path(%q) mismatch (-want +got):\n%s", test.Name, diff)
			}
		})
	}
}

// TestDataDirRoot pins where the managed runtime lives: the flag, then the environment, then
// ansible/ in the data directory.
func TestDataDirRoot(t *testing.T) {
	tests := []struct {
		WantRoot string
		Flag     string
		Env      string
		DataDir  string
	}{{ // Test 0: The data directory's ansible/ by default.
		DataDir: "/var/lib/switchtender", WantRoot: "/var/lib/switchtender/ansible",
	}, { // Test 1: The environment overrides the data directory.
		Env: "/srv/ansible", DataDir: "/var/lib/switchtender", WantRoot: "/srv/ansible",
	}, { // Test 2: The flag overrides the environment.
		Flag: "/opt/rt", Env: "/srv/ansible", DataDir: "/data", WantRoot: "/opt/rt",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Setenv(RootEnv, test.Env)
			if diff := cmp.Diff(test.WantRoot, DataDirRoot(test.Flag, test.DataDir)); diff != "" {
				t.Errorf("DataDirRoot() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
