//go:build unix

package roundhouse

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/run"
)

// TestMountGuardResolvesSymlinksBeforeJudgingAPath is the mount-side twin of the containment the
// working directory check already performs.
//
// toolWorkDir resolves every symlink before deciding whether a directory sits inside the checkout,
// and its own comment gives the reason: "the container path builds its mount from this same value,
// so the same link mounted it into the container past the blocklist". filepath.Clean alone is a
// spelling change and follows nothing, so a name inside the checkout that is really a link to /etc,
// /root/.ssh, or the filesystem root would be judged on the name and allowed. The container runtime
// then resolves the source path for real and bind mounts the target.
//
// The Ansible plan mounts filepath.Dir(spec.Playbook), spec.Inventory, spec.PrivateKeyPath and every
// vault password path exactly as given, and none of them pass through toolWorkDir, so a committed
// symlink in a project repository is enough to place one.
func TestMountGuardResolvesSymlinksBeforeJudgingAPath(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	link := filepath.Join(root, "confdir")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	if err := checkMountPath(link); !errors.Is(err, ErrForbiddenMount) {
		t.Errorf("checkMountPath(%q) = %v, want ErrForbiddenMount; the path resolves to /etc",
			link, err)
	}
}

// TestARunCannotMountABlockedDirectoryThroughASymlink reaches the same defect the way a request
// reaches it, through the plan a run produces, and shows the resulting bind mount on the command
// line. This is what a project whose repository carries one committed link gets: the host's /etc
// mounted inside the container at a path the run chose.
func TestARunCannotMountABlockedDirectoryThroughASymlink(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	link := filepath.Join(root, "confdir")
	if err := os.Symlink("/etc", link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	spec := Spec{
		Tool: run.ToolAnsible, Dir: root, Playbook: filepath.Join(link, "site.yml"),
		Image: "alpine:3",
	}
	c := newContainerRunner("docker", "missing", false, nil, &pluginCache{},
		DefaultContainerLimits())
	plan, cleanup, err := buildContainerPlan(spec)
	if err != nil {
		cleanup()
		t.Fatalf("buildContainerPlan() error = %v", err)
	}
	defer cleanup()

	args, err := c.runArgs(spec, plan, "st-test", "")
	if !errors.Is(err, ErrForbiddenMount) {
		t.Errorf("runArgs() error = %v, want ErrForbiddenMount.\nargs: %s",
			err, strings.Join(args, " "))
	}
}

// TestMountGuardIsCaseInsensitiveWhereTheFilesystemIs pins the second spelling gap in the same guard.
// Matching the credential directory names byte for byte meant that on a host whose filesystem folds
// case, which is the default on macOS and on Windows, a run naming .SSH or .AWS reached exactly the
// files the guard exists to withhold.
func TestMountGuardIsCaseInsensitiveWhereTheFilesystemIs(t *testing.T) {
	t.Parallel()
	for _, path := range []string{
		"/Users/ops/.SSH/id_rsa", "/home/ops/.AWS/credentials", "/ETC/shadow", "/Root/.Ssh",
	} {
		if err := checkMountPath(path); !errors.Is(err, ErrForbiddenMount) {
			t.Errorf("checkMountPath(%q) = %v, want ErrForbiddenMount", path, err)
		}
	}
}

// TestMountGuardBoundaries pins the guard's decisions at every edge that is settled today, so the
// spelling gaps above are the only ones open and a future change to the matching cannot quietly take
// the ordinary paths with it. Everything a run legitimately mounts lives under a directory that
// cannot be refused as a whole tree, which is why the rule is a mixture of trees, exact roots, and
// names matched at any depth.
//
//nolint:funlen // Test function.
func TestMountGuardBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Path is the host path offered for mounting.
		Path string
		// Want is the refusal expected, or nil when the path is allowed.
		Want error
	}{
		{"empty is not a mount", "", nil},                                                 // Test 0.
		{"filesystem root", "/", ErrForbiddenMount},                                       // Test 1.
		{"root with a trailing slash", "//", ErrForbiddenMount},                           // Test 2.
		{"a tree with a trailing slash", "/etc/", ErrForbiddenMount},                      // Test 3.
		{"doubled separators still resolve", "//etc//shadow", ErrForbiddenMount},          // Test 4.
		{"dot segments do not hide a tree", "/srv/../etc", ErrForbiddenMount},             // Test 5.
		{"dot segments do not hide a socket", "/x/../run/d.sock", ErrForbiddenMount},      // Test 6.
		{"any unix socket, not only docker", "/tmp/podman.sock", ErrForbiddenMount},       // Test 7.
		{"a name merely ending in sock is fine", "/tmp/mysock", nil},                      // Test 8.
		{"a credential name mid-path", "/srv/a/.gnupg/b/c", ErrForbiddenMount},            // Test 9.
		{"gcloud is matched as two components", "/h/o/.config/gcloud", ErrForbiddenMount}, // Test 10.
		{"a lone .config is not a credential store", "/h/o/.config/app.toml", nil},        // Test 11.
		{"a checkout under home", "/home/ops/checkout", nil},                              // Test 12.
		{"the home directory itself", "/home", ErrForbiddenMount},                         // Test 13.
		{"the users directory itself", "/Users", ErrForbiddenMount},                       // Test 14.
		{"a system library tree root", "/lib64", ErrForbiddenMount},                       // Test 15.
		{"a path under a system root", "/usr/local/share/project", nil},                   // Test 16.
		{"a relative path is not judged as a root", "checkout", nil},                      // Test 17.
		{"a unicode component is ordinary", "/srv/prosjekt-æøå/site.yml", nil},            // Test 18.
		{"a very long ordinary path", "/srv/" + strings.Repeat("a", 4096), nil},           // Test 19.
		{"a very long path into a tree", "/etc/" + strings.Repeat("a", 4096),
			ErrForbiddenMount}, // Test 20.
		{"containerd state", "/var/lib/containerd/x", ErrForbiddenMount},         // Test 21.
		{"the var tree stays open beneath itself", "/var/lib/switchtender", nil}, // Test 22.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := checkMountPath(test.Path)
			if test.Want == nil {
				if err != nil {
					t.Errorf("%s: checkMountPath(%q) = %v, want it allowed", test.Name, test.Path, err)
				}
				return
			}
			if !errors.Is(err, test.Want) {
				t.Errorf("%s: checkMountPath(%q) = %v, want %v", test.Name, test.Path, err, test.Want)
			}
		})
	}
}

// TestMountSetDeduplicatesAndFailsClosed pins the collector the guard is reached through. A duplicate
// path must mount once, since a runtime refuses two bind mounts at the same destination, and one
// refused path must fail the whole set rather than being dropped so the rest can proceed: a run that
// silently lost its inventory mount would start and then behave as if the file did not exist.
func TestMountSetDeduplicatesAndFailsClosed(t *testing.T) {
	t.Parallel()
	m := newMountSet()
	for _, path := range []string{"", "/srv/checkout", "/srv/checkout", "/srv/other"} {
		if err := m.add(path, true); err != nil {
			t.Fatalf("add(%q) error = %v", path, err)
		}
	}
	want := []string{"-v", "/srv/checkout:/srv/checkout:ro", "-v", "/srv/other:/srv/other:ro"}
	if got := strings.Join(m.args(), " "); got != strings.Join(want, " ") {
		t.Errorf("args = %q, want %q", got, strings.Join(want, " "))
	}

	// A writable mount drops the read-only suffix, which is what a Terraform working directory needs.
	rw := newMountSet()
	if err := rw.add("/srv/infra", false); err != nil {
		t.Fatalf("add() error = %v", err)
	}
	if got := strings.Join(rw.args(), " "); got != "-v /srv/infra:/srv/infra" {
		t.Errorf("writable args = %q, want no :ro suffix", got)
	}

	// A refused path is not recorded, so nothing downstream can mount it anyway.
	blocked := newMountSet()
	if err := blocked.add("/etc/shadow", true); !errors.Is(err, ErrForbiddenMount) {
		t.Errorf("add(/etc/shadow) error = %v, want ErrForbiddenMount", err)
	}
	if len(blocked.args()) != 0 {
		t.Errorf("a refused path was still recorded: %v", blocked.args())
	}
}
