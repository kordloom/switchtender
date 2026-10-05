package factcache

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestCollectStampsWhenAnsibleWroteTheFile checks that each collected host carries the time Ansible
// wrote its cache file, in either layout.
//
// The fact cache timeout is measured from when facts were gathered. The collected entries came back
// unstamped and were stamped with the time the run ended, so a long run handed the next one facts
// as old as itself while calling them new, and an older gather that finished later was stamped as
// the newest.
func TestCollectStampsWhenAnsibleWroteTheFile(t *testing.T) {
	t.Parallel()
	wrote := time.Date(2026, 10, 4, 6, 4, 39, 0, time.UTC)
	tests := []struct {
		// File is the cache file Ansible writes.
		File string
		// Body is what it writes.
		Body string
	}{{ // Test 0: The layout ansible-core 2.18 and earlier write.
		File: "web01", Body: `{"ansible_distribution":"Debian"}`,
	}, { // Test 1: The schema-prefixed layout ansible-core 2.19 and later write.
		File: "s1_web01", Body: `{"__payload__":"{\"ansible_distribution\":\"Debian\"}"}`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			d, err := NewDir(t.TempDir())
			if err != nil {
				t.Fatalf("NewDir() error = %v", err)
			}
			t.Cleanup(d.Remove)
			if _, err := d.Write([]Entry{{Host: "web01",
				Facts:      json.RawMessage(`{"ansible_distribution":"Alpine"}`),
				ModifiedAt: wrote.Add(-time.Hour)}}, wrote, 0); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			path := filepath.Join(d.Path, test.File)
			if err := os.WriteFile(path, []byte(test.Body), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Chtimes(path, wrote, wrote); err != nil {
				t.Fatal(err)
			}
			got, err := d.Collect(func(string) bool { return true })
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			if len(got.Updated) != 1 {
				t.Fatalf("Collect() updated %d hosts, want web01 alone", len(got.Updated))
			}
			if diff := cmp.Diff(wrote, got.Updated[0].ModifiedAt.UTC()); diff != "" {
				t.Errorf("collected ModifiedAt mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
