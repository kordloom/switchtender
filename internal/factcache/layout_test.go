package factcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// wrappedFile is a cache file as ansible-core 2.19 and later write it: the fact document as JSON
// text under __payload__.
func wrappedFile(t *testing.T, doc string) string {
	t.Helper()
	raw, err := json.Marshal(map[string]string{"__payload__": doc})
	if err != nil {
		t.Fatalf("encode the wrapper: %v", err)
	}
	return string(raw)
}

// TestDirWritesBothLayouts checks a cached host is written where every Ansible release reads it:
// the bare host file ansible-core 2.18 and earlier read, and the schema-prefixed wrapper 2.19 and
// later read, both holding the same document.
func TestDirWritesBothLayouts(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir() error = %v", err)
	}
	t.Cleanup(d.Remove)
	facts := `{"ansible_distribution":"Debian"}`
	if _, err := d.Write([]Entry{
		{Host: "web1", Facts: json.RawMessage(facts), ModifiedAt: now},
		// A host whose name is web2's prefixed name keeps that file for its own facts.
		{Host: "web2", Facts: json.RawMessage(`{"a":2}`), ModifiedAt: now},
		{Host: "s1_web2", Facts: json.RawMessage(`{"a":3}`), ModifiedAt: now},
	}, now, 0); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	read := func(name string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(d.Path, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return string(raw)
	}
	if got := read("web1"); got != facts {
		t.Errorf("the bare file holds %s, want %s", got, facts)
	}
	if got, want := read("s1_web1"), wrappedFile(t, facts); got != want {
		t.Errorf("the prefixed file holds %s, want %s", got, want)
	}
	if got := read("s1_web2"); got != `{"a":3}` {
		t.Errorf("the host named s1_web2 holds %s, want its own facts", got)
	}
}

// TestDirCollectsTheSchemaPrefixedLayout checks what Collect reads back after a run of
// ansible-core 2.19 or later, which rewrites only the prefixed file. Before, every such file was
// refused as a host outside the inventory, so on a current Ansible the cache never kept a fact and
// never served one.
//
//nolint:funlen // Table of Ansible behaviors.
func TestDirCollectsTheSchemaPrefixedLayout(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Act plays Ansible's part on the directory after Write.
		Act func(t *testing.T, dir string)
		// WantUpdated are the hosts and documents Collect keeps.
		WantUpdated []Entry
		// WantCleared are the hosts Collect reports cleared.
		WantCleared []string
		// WantSkipped is how many files Collect refuses.
		WantSkipped int
	}{{ // Test 0: A rewrite of a cached host, tags dropped and numbers kept exact.
		Act: func(t *testing.T, dir string) {
			t.Helper()
			writeLater(t, dir, "s1_web1", wrappedFile(t, `{"ansible_distribution":"Ubuntu",`+
				`"big":12345678901234567890,"canary":{"value":"abc","tags":[{"path":"/srv/p.yml",`+
				`"__ansible_type":"Origin"}],"__ansible_type":"_AnsibleTaggedStr"}}`))
		},
		WantUpdated: []Entry{{Host: "web1", Facts: json.RawMessage(
			`{"ansible_distribution":"Ubuntu","big":12345678901234567890,"canary":"abc"}`), Bytes: 75}},
	}, { // Test 1: A host with nothing cached gathered for the first time.
		Act: func(t *testing.T, dir string) {
			t.Helper()
			writeLater(t, dir, "s1_fresh", wrappedFile(t, `{"a":1}`))
		},
		WantUpdated: []Entry{{Host: "fresh", Facts: json.RawMessage(`{"a":1}`), Bytes: 7}},
	}, { // Test 2: meta: clear_facts removes the prefixed file alone.
		Act: func(t *testing.T, dir string) {
			t.Helper()
			if err := os.Remove(filepath.Join(dir, "s1_web1")); err != nil {
				t.Fatal(err)
			}
		},
		WantCleared: []string{"web1"},
	}, { // Test 3: A host named like a prefixed file, written in the older layout, is that host.
		Act: func(t *testing.T, dir string) {
			t.Helper()
			writeLater(t, dir, "s1_db", `{"a":4}`)
		},
		WantUpdated: []Entry{{Host: "s1_db", Facts: json.RawMessage(`{"a":4}`), Bytes: 7}},
	}, { // Test 4: Both layouts rewritten, and the newer release's file is the one kept.
		Act: func(t *testing.T, dir string) {
			t.Helper()
			writeLater(t, dir, "web1", `{"layout":"bare"}`)
			writeLater(t, dir, "s1_web1", wrappedFile(t, `{"layout":"prefixed"}`))
		},
		WantUpdated: []Entry{{Host: "web1", Facts: json.RawMessage(`{"layout":"prefixed"}`),
			Bytes: 21}},
	}, { // Test 5: A wrapper around something other than an object is refused.
		Act: func(t *testing.T, dir string) {
			t.Helper()
			writeLater(t, dir, "s1_web1", wrappedFile(t, `[1,2]`))
		},
		WantSkipped: 1,
	}, { // Test 6: A prefixed file for a host the inventory does not hold is refused.
		Act: func(t *testing.T, dir string) {
			t.Helper()
			writeLater(t, dir, "s1_stranger", wrappedFile(t, `{"a":1}`))
		},
		WantSkipped: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			now := time.Now()
			d, err := NewDir(t.TempDir())
			if err != nil {
				t.Fatalf("NewDir() error = %v", err)
			}
			t.Cleanup(d.Remove)
			if _, err := d.Write([]Entry{
				{Host: "web1", Facts: json.RawMessage(`{"ansible_distribution":"Debian"}`),
					ModifiedAt: now},
			}, now, 0); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			test.Act(t, d.Path)
			got, err := d.Collect(func(h string) bool { return h != "stranger" })
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			// When each file was written is TestCollectStampsWhenAnsibleWroteTheFile's to check.
			if diff := cmp.Diff(test.WantUpdated, got.Updated, cmpopts.EquateEmpty(),
				cmpopts.IgnoreFields(Entry{}, "ModifiedAt")); diff != "" {
				t.Errorf("updated mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantCleared, got.Cleared, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("cleared mismatch (-want +got):\n%s", diff)
			}
			if len(got.Skipped) != test.WantSkipped {
				t.Errorf("skipped %q, want %d", got.Skipped, test.WantSkipped)
			}
		})
	}
}

// writeLater writes a cache file as a run would, with a modification time after the one Write
// recorded, so the change is seen on a file system with coarse timestamps.
func writeLater(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	later := time.Now().Add(time.Minute)
	if err := os.Chtimes(path, later, later); err != nil {
		t.Fatal(err)
	}
}
