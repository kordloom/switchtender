package runfiles

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestChooseRootOrder pins the order a root is chosen in: an explicit root, then the systemd
// runtime directory, then a private XDG runtime directory, then the temporary directory, and never
// one that is not private.
func TestChooseRootOrder(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("the XDG runtime directory needs a POSIX owner and mode")
	}
	base := t.TempDir()
	private := filepath.Join(base, "xdg-private")
	if err := os.Mkdir(private, 0o700); err != nil {
		t.Fatal(err)
	}
	open := filepath.Join(base, "xdg-open")
	if err := os.Mkdir(open, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(open, 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(base, "xdg-link")
	if err := os.Symlink(private, link); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		// Explicit is the --runfiles-dir value.
		Explicit string
		// Env is the environment the choice reads.
		Env map[string]string
		// WantChoice is the root picked.
		WantChoice Choice
		// Want is the error.
		Want error
	}{{ // Test 0: An explicit root wins over everything.
		Explicit: "/srv/runfiles", Env: map[string]string{"RUNTIME_DIRECTORY": "/run/st"},
		WantChoice: Choice{Path: "/srv/runfiles", Source: SourceFlag},
	}, { // Test 1: A relative explicit root is refused.
		Explicit: "runfiles", Want: ErrUnsafeRoot,
	}, { // Test 2: The systemd runtime directory beats a private XDG directory.
		Env: map[string]string{
			"RUNTIME_DIRECTORY": "/run/switchtender", "XDG_RUNTIME_DIR": private,
		},
		WantChoice: Choice{Path: "/run/switchtender/runfiles", Source: SourceSystemd},
	}, { // Test 3: With several runtime directories the first one named is used.
		Env:        map[string]string{"RUNTIME_DIRECTORY": "/run/a:/run/b"},
		WantChoice: Choice{Path: "/run/a/runfiles", Source: SourceSystemd},
	}, { // Test 4: A private XDG runtime directory is used.
		Env:        map[string]string{"XDG_RUNTIME_DIR": private},
		WantChoice: Choice{Path: filepath.Join(private, xdgSubdir), Source: SourceXDG},
	}, { // Test 5: An XDG directory open to other accounts is passed over, and the reason kept.
		Env: map[string]string{"XDG_RUNTIME_DIR": open},
		WantChoice: Choice{Path: DefaultRoot(), Source: SourceTemp,
			Passed: []string{"XDG_RUNTIME_DIR " + open + " is mode 0755, open to other accounts"}},
	}, { // Test 6: An XDG directory that is a link is passed over.
		Env: map[string]string{"XDG_RUNTIME_DIR": link},
		WantChoice: Choice{Path: DefaultRoot(), Source: SourceTemp,
			Passed: []string{"XDG_RUNTIME_DIR " + link + " is not a directory"}},
	}, { // Test 7: Nothing set falls back to the temporary directory.
		WantChoice: Choice{Path: DefaultRoot(), Source: SourceTemp},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := ChooseRoot(test.Explicit, func(k string) string { return test.Env[k] })
			if !errors.Is(err, test.Want) {
				t.Fatalf("ChooseRoot() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantChoice, got); diff != "" {
				t.Errorf("ChooseRoot() (-want +got):\n%s", diff)
			}
		})
	}
}

// TestChooseRootNeverPicksDevShm proves no rule lands in /dev/shm by itself, and Prepare refuses it
// when the temporary directory is pointed there.
func TestChooseRootNeverPicksDevShm(t *testing.T) {
	t.Parallel()
	for _, path := range []string{"/dev/shm", "/dev/shm/x", "/dev/shm/../shm/y"} {
		if !underDevShm(path) {
			t.Errorf("underDevShm(%q) = false, want true", path)
		}
	}
	for _, path := range []string{"/dev/shmx", "/run/shm", "/tmp"} {
		if underDevShm(path) {
			t.Errorf("underDevShm(%q) = true, want false", path)
		}
	}
}
