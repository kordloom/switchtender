package ansibleruntime

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/inventory"
)

// testHash is a well-formed SHA-256 for lock fixtures.
var testHash = strings.Repeat("ab", 32)

// testLock returns a valid lock for release whose body is body, with the headers ParseLock needs.
func testLock(release, body string) string {
	return "# ansible-core: " + release + "\n# python: 3.12-3.14\n" + body
}

// TestParseLock pins what a lock may hold: pinned, hashed requirements and nothing pip would
// install unchecked or from anywhere else.
func TestParseLock(t *testing.T) {
	t.Parallel()
	core := "ansible-core==2.21.4 \\\n    --hash=sha256:" + testHash + "\n"
	tests := []struct {
		Want         error
		WantInError  string
		WantRelease  string
		WantPython   string
		WantPackages []string
		WantHashes   int
		Content      string
	}{{ // Test 0: A pinned, hashed lock with a marker and comments parses.
		Content: testLock("2.21.4", core+"    # via switchtender\n"+
			"cffi==2.1.1 ; platform_python_implementation != 'PyPy' \\\n"+
			"    --hash=sha256:"+testHash+" \\\n    --hash=sha256:"+strings.Repeat("cd", 32)+"\n"),
		WantRelease: "2.21.4", WantPython: "3.12-3.14",
		WantPackages: []string{"ansible-core==2.21.4",
			"cffi==2.1.1 ; platform_python_implementation != 'PyPy'"},
		WantHashes: 3,
	}, { // Test 1: A requirement not pinned with == is refused.
		Content: testLock("2.21.4", core+"jinja2>=3.1 --hash=sha256:"+testHash+"\n"),
		Want:    ErrLock, WantInError: "is not pinned",
	}, { // Test 2: A requirement with no hash is refused.
		Content: testLock("2.21.4", core+"jinja2==3.1.6\n"),
		Want:    ErrLock, WantInError: "has no sha256 hash",
	}, { // Test 3: An index option line is refused.
		Content: testLock("2.21.4", "--index-url https://example.invalid/simple\n"+core),
		Want:    ErrLock, WantInError: "option line",
	}, { // Test 4: An editable requirement is refused.
		Content: testLock("2.21.4", core+"-e git+https://example.invalid/x#egg=x\n"),
		Want:    ErrLock, WantInError: "option line",
	}, { // Test 5: A URL requirement is refused.
		Content: testLock("2.21.4", core+"x @ https://example.invalid/x.whl --hash=sha256:"+
			testHash+"\n"),
		Want: ErrLock, WantInError: "is not pinned",
	}, { // Test 6: An environment variable reference is refused, since pip expands it.
		Content: testLock("2.21.4", core+"x==1 --hash=sha256:${HASH}\n"),
		Want:    ErrLock, WantInError: "environment variable",
	}, { // Test 7: A hash other than SHA-256 is refused.
		Content: testLock("2.21.4", core+"x==1 --hash=md5:0123456789abcdef0123456789abcdef\n"),
		Want:    ErrLock, WantInError: "only --hash=sha256:",
	}, { // Test 8: A per-requirement option beside a hash is refused.
		Content: testLock("2.21.4", core+"x==1 --hash=sha256:"+testHash+
			" --config-settings=a=b\n"),
		Want: ErrLock, WantInError: "only --hash=sha256:",
	}, { // Test 9: A pin that disagrees with the header is refused.
		Content: testLock("2.20.9", core),
		Want:    ErrLock, WantInError: "header names 2.20.9",
	}, { // Test 10: A lock without the release header is refused.
		Content: "# python: 3.12-3.14\n" + core,
		Want:    ErrLock, WantInError: "ansible-core: X.Y.Z",
	}, { // Test 11: A lock without the Python header is refused.
		Content: "# ansible-core: 2.21.4\n" + core,
		Want:    ErrLock, WantInError: "python: 3.N-3.M",
	}, { // Test 12: A package pinned twice is refused.
		Content: testLock("2.21.4", core+core),
		Want:    ErrLock, WantInError: "pinned twice",
	}, { // Test 13: A comment inside a continued requirement is refused.
		Content: testLock("2.21.4", "ansible-core==2.21.4 \\\n# sneaky\n    --hash=sha256:"+
			testHash+"\n"),
		Want: ErrLock, WantInError: "comment inside",
	}, { // Test 14: A lock ending inside a continuation is refused.
		Content: testLock("2.21.4", "ansible-core==2.21.4 \\\n"),
		Want:    ErrLock, WantInError: "ends inside",
	}, { // Test 15: A lock that does not pin ansible-core is refused.
		Content: testLock("2.21.4", "x==1 --hash=sha256:"+testHash+"\n"),
		Want:    ErrLock, WantInError: "does not pin ansible-core",
	}, { // Test 16: A malformed hash is refused.
		Content: testLock("2.21.4", "ansible-core==2.21.4 --hash=sha256:abc\n"),
		Want:    ErrLock, WantInError: "only --hash=sha256:",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			lock, err := ParseLock([]byte(test.Content))
			if !errors.Is(err, test.Want) {
				t.Fatalf("ParseLock() error = %v, want %v", err, test.Want)
			}
			if test.Want != nil {
				if !strings.Contains(err.Error(), test.WantInError) {
					t.Errorf("ParseLock() error = %q, want it to say %q", err, test.WantInError)
				}
				return
			}
			var pkgs []string
			hashes := 0
			for _, r := range lock.Requirements {
				p := r.Name + "==" + r.Version
				if r.Marker != "" {
					p += " ; " + r.Marker
				}
				pkgs = append(pkgs, p)
				hashes += len(r.Hashes)
			}
			got := struct {
				Release, Python string
				Packages        []string
				Hashes          int
			}{lock.Release, lock.PythonMin + "-" + lock.PythonMax, pkgs, hashes}
			want := struct {
				Release, Python string
				Packages        []string
				Hashes          int
			}{test.WantRelease, test.WantPython, test.WantPackages, test.WantHashes}
			if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ParseLock() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEmbeddedLocksAreTheTestedReleases pins the locks this binary carries to the releases the
// inventory conformance corpus runs against in CI, one lock per release, each parsing cleanly and
// pinning ansible-core with hashes.
func TestEmbeddedLocksAreTheTestedReleases(t *testing.T) {
	t.Parallel()
	locks, err := Locks()
	if err != nil {
		t.Fatalf("Locks() error = %v", err)
	}
	var got []string
	for _, l := range locks {
		got = append(got, l.Release)
		if pythonMinor(l.PythonMin) < 10 || pythonMinor(l.PythonMax) < pythonMinor(l.PythonMin) {
			t.Errorf("lock %s has Python range %s-%s", l.Release, l.PythonMin, l.PythonMax)
		}
	}
	if diff := cmp.Diff(inventory.TestedAnsibleCoreReleases, got); diff != "" {
		t.Errorf("embedded locks differ from inventory.TestedAnsibleCoreReleases (-want +got):\n%s"+
			"\nRun scripts/ansible-runtime-locks.py after moving the CI matrix.", diff)
	}
}

// TestLockFor pins how a requested version picks a lock: the newest by default, a minor version's
// pinned release, an exact pinned release, and nothing else.
func TestLockFor(t *testing.T) {
	t.Parallel()
	newest := inventory.TestedAnsibleCoreReleases[len(inventory.TestedAnsibleCoreReleases)-1]
	tests := []struct {
		Want        error
		WantRelease string
		WantInError string
		Version     string
	}{{ // Test 0: No version is the newest supported release.
		Version: "", WantRelease: newest,
	}, { // Test 1: A minor version is its pinned release.
		Version: "2.20", WantRelease: "2.20.9",
	}, { // Test 2: An exact pinned release is itself.
		Version: "2.18.19", WantRelease: "2.18.19",
	}, { // Test 3: A leading v is accepted.
		Version: "v2.19", WantRelease: "2.19.13",
	}, { // Test 4: Another patch of a supported minor is refused, naming the pinned one.
		Version: "2.20.8", Want: ErrVersion, WantInError: "installs ansible-core 2.20.9",
	}, { // Test 5: A minor below the range is refused, naming what is supported.
		Version: "2.15", Want: ErrVersion, WantInError: "Supported: 2.16.19",
	}, { // Test 6: A minor above the range is refused.
		Version: "2.22", Want: ErrVersion,
	}, { // Test 7: Something that is not a version is refused.
		Version: "latest", Want: ErrVersion,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			lock, err := LockFor(test.Version)
			if !errors.Is(err, test.Want) {
				t.Fatalf("LockFor(%q) error = %v, want %v", test.Version, err, test.Want)
			}
			if test.Want != nil {
				if !strings.Contains(err.Error(), test.WantInError) {
					t.Errorf("LockFor(%q) error = %q, want it to say %q", test.Version, err,
						test.WantInError)
				}
				return
			}
			if diff := cmp.Diff(test.WantRelease, lock.Release); diff != "" {
				t.Errorf("LockFor(%q) release mismatch (-want +got):\n%s", test.Version, diff)
			}
		})
	}
}
