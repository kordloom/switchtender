package factcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// anyHost admits every host, for tests that are not about the inventory filter.
func anyHost(string) bool { return true }

// TestDirRoundTrip writes cached facts, plays Ansible's part by rewriting one file, adding one, and
// deleting one, and checks Collect reports exactly those changes.
func TestDirRoundTrip(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir() error = %v", err)
	}
	t.Cleanup(d.Remove)
	n, err := d.Write([]Entry{
		{Host: "kept", Facts: json.RawMessage(`{"os":"Debian"}`), ModifiedAt: now},
		{Host: "regathered", Facts: json.RawMessage(`{"os":"old"}`), ModifiedAt: now},
		{Host: "cleared", Facts: json.RawMessage(`{"os":"gone"}`), ModifiedAt: now},
	}, now, 0)
	if err != nil || n != 3 {
		t.Fatalf("Write() = %d, %v, want 3 written", n, err)
	}
	info, err := os.Stat(filepath.Join(d.Path, "kept"))
	if err != nil {
		t.Fatalf("stat cache file: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("cache file mode = %v, want 0600", info.Mode().Perm())
	}
	raw, rerr := os.ReadFile(filepath.Join(d.Path, "kept"))
	if rerr != nil || string(raw) != `{"os":"Debian"}` {
		t.Fatalf("cache file holds %q, %v, want the cached document Ansible reads", raw, rerr)
	}

	// What Ansible's jsonfile plugin does during a run: rewrite a host it gathered, write a host it
	// had nothing for, and delete a host a play cleared.
	later := time.Now().Add(time.Minute)
	regathered := filepath.Join(d.Path, "regathered")
	if err := os.WriteFile(regathered, []byte("{\n    \"os\": \"new\"\n}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(regathered, later, later); err != nil {
		t.Fatal(err)
	}
	fresh := filepath.Join(d.Path, "fresh")
	if err := os.WriteFile(fresh, []byte(`{"os":"Alpine"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(d.Path, "cleared")); err != nil {
		t.Fatal(err)
	}

	got, err := d.Collect(anyHost)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	want := Collected{
		Updated: []Entry{
			{Host: "fresh", Facts: json.RawMessage(`{"os":"Alpine"}`), Bytes: 15},
			{Host: "regathered", Facts: json.RawMessage(`{"os":"new"}`), Bytes: 12},
		},
		Cleared: []string{"cleared"},
	}
	// When each file was written is TestCollectStampsWhenAnsibleWroteTheFile's to check.
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty(),
		cmpopts.IgnoreFields(Entry{}, "ModifiedAt")); diff != "" {
		t.Errorf("Collect() mismatch (-want +got):\n%s", diff)
	}
	d.Remove()
	if _, err := os.Stat(d.Path); !os.IsNotExist(err) {
		t.Errorf("Remove() left %s behind: %v", d.Path, err)
	}
}

// TestDirWriteHonorsTimeout checks the fact cache timeout: facts older than it are not written, so
// the run gathers them again rather than reading stale ones.
func TestDirWriteHonorsTimeout(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	tests := []struct {
		WantHosts []string
		Timeout   time.Duration
	}{{ // Test 0: No timeout serves every cached host however old.
		Timeout: 0, WantHosts: []string{"day-old", "fresh", "hour-old"},
	}, { // Test 1: A two hour timeout drops the day-old host.
		Timeout: 2 * time.Hour, WantHosts: []string{"fresh", "hour-old"},
	}, { // Test 2: A ten minute timeout keeps only the fresh host.
		Timeout: 10 * time.Minute, WantHosts: []string{"fresh"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d, err := NewDir(t.TempDir())
			if err != nil {
				t.Fatalf("NewDir() error = %v", err)
			}
			t.Cleanup(d.Remove)
			if _, err := d.Write([]Entry{
				{Host: "fresh", Facts: json.RawMessage(`{}`), ModifiedAt: now.Add(-time.Minute)},
				{Host: "hour-old", Facts: json.RawMessage(`{}`), ModifiedAt: now.Add(-time.Hour)},
				{Host: "day-old", Facts: json.RawMessage(`{}`), ModifiedAt: now.Add(-24 * time.Hour)},
			}, now, test.Timeout); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			files, err := os.ReadDir(d.Path)
			if err != nil {
				t.Fatal(err)
			}
			var hosts, wrapped []string
			for _, f := range files {
				if host, ok := strings.CutPrefix(f.Name(), "s1_"); ok {
					wrapped = append(wrapped, host)
					continue
				}
				hosts = append(hosts, f.Name())
			}
			if diff := cmp.Diff(test.WantHosts, hosts, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("written hosts mismatch (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(test.WantHosts, wrapped, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("hosts written in the schema-prefixed layout (-want +got):\n%s", diff)
			}
		})
	}
}

// TestDirCollectRefuses checks what Collect will not take: a host the inventory does not name, a
// document past the size bound, and a file that is not a JSON object. An unchanged file is simply
// not reported.
func TestDirCollectRefuses(t *testing.T) {
	t.Parallel()
	now := time.Now()
	d, err := NewDir(t.TempDir())
	if err != nil {
		t.Fatalf("NewDir() error = %v", err)
	}
	t.Cleanup(d.Remove)
	if _, err := d.Write([]Entry{{Host: "same", Facts: json.RawMessage(`{"a":1}`), ModifiedAt: now}},
		now, 0); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(d.Path, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("stranger", `{"a":1}`)
	write("huge", `{"x":"`+strings.Repeat("a", MaxFactsBytes)+`"}`)
	write("list", `[1,2]`)
	write("ok", `{"a":1}`)

	inInventory := func(h string) bool { return h != "stranger" }
	got, err := d.Collect(inInventory)
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	var hosts []string
	for _, e := range got.Updated {
		hosts = append(hosts, e.Host)
	}
	if diff := cmp.Diff([]string{"ok"}, hosts); diff != "" {
		t.Errorf("Collect() updated hosts mismatch (-want +got):\n%s", diff)
	}
	if len(got.Skipped) != 3 || len(got.Cleared) != 0 {
		t.Errorf("Collect() skipped %q cleared %q, want three skips and nothing cleared",
			got.Skipped, got.Cleared)
	}
}

// TestValidHost pins the host names that may become cache files.
func TestValidHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name      string
		WantValid bool
	}{
		{Name: "web01.example.com", WantValid: true},       // Test 0: An ordinary name.
		{Name: "10.0.0.5", WantValid: true},                // Test 1: An address.
		{Name: "", WantValid: false},                       // Test 2: Empty.
		{Name: "..", WantValid: false},                     // Test 3: The parent directory.
		{Name: "a/b", WantValid: false},                    // Test 4: A path separator.
		{Name: ".hidden", WantValid: false},                // Test 5: A dot file.
		{Name: "nul\x00byte", WantValid: false},            // Test 6: A NUL byte.
		{Name: strings.Repeat("h", 256), WantValid: false}, // Test 7: Too long.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := ValidHost(test.Name); got != test.WantValid {
				t.Errorf("ValidHost(%q) = %v, want %v", test.Name, got, test.WantValid)
			}
		})
	}
}
