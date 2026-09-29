package importer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// TestChefReadsWhatTheDocumentedExportWrites pins the shapes the Chef importer accepts against the
// commands that produce them.
//
// The migration guide exports nodes with knife node show run once per node, which writes one JSON
// document after another. The importer refused that as a file holding more than one document, so the
// documented command produced a file the documented import rejected.
func TestChefReadsWhatTheDocumentedExportWrites(t *testing.T) {
	t.Parallel()
	const web = `{"name": "web01.prod", "chef_environment": "production", "run_list": ["role[web]"]}`
	const db = `{"name": "db01.prod", "chef_environment": "production", "run_list": ["role[db]"]}`
	tests := []struct {
		WantHosts []string
		Want      string
		Doc       string
	}{{ // Test 0: knife node show once per node, one document after another.
		Doc: web + "\n" + db + "\n", WantHosts: []string{"db01.prod", "web01.prod"},
	}, { // Test 1: The same with no newline between them.
		Doc: web + db, WantHosts: []string{"db01.prod", "web01.prod"},
	}, { // Test 2: knife search node, the matches under rows beside a count.
		Doc:       `{"results": 2, "rows": [` + web + `, ` + db + `]}`,
		WantHosts: []string{"db01.prod", "web01.prod"},
	}, { // Test 3: The array form still reads.
		Doc: "[" + web + ", " + db + "]", WantHosts: []string{"db01.prod", "web01.prod"},
	}, { // Test 4: Something that is not a node after the first is still refused, so a file with a
		// stray document appended is not imported in part.
		Doc: web + "\n" + `{"unrelated": true}`, Want: "more than one document",
	}, { // Test 5: Two arrays appended together are refused the same way.
		Doc: "[" + web + "]\n[" + db + "]", Want: "more than one document",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromChef([]byte(test.Doc), time.Now())
			if test.Want != "" {
				if err == nil || !strings.Contains(err.Error(), test.Want) {
					t.Fatalf("FromChef() error = %v, want one saying %q", err, test.Want)
				}
				return
			}
			if err != nil {
				t.Fatalf("FromChef() error = %v", err)
			}
			if len(plan.Inventories) != 1 {
				t.Fatalf("inventories = %d, want 1", len(plan.Inventories))
			}
			var got []string
			for _, host := range test.WantHosts {
				if strings.Contains(plan.Inventories[0].Content, host) {
					got = append(got, host)
				}
			}
			if diff := cmp.Diff(test.WantHosts, got); diff != "" {
				t.Errorf("hosts in the inventory mismatch (-want +got):\n%s\ninventory:\n%s", diff,
					plan.Inventories[0].Content)
			}
		})
	}
}
