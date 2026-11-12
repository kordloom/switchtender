package run

import (
	"fmt"
	"testing"
)

//nolint:funlen // Test function.
func TestRawPathSplit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Tool        string
		Path        string
		InventoryID string
		Shards      int
		WantResult  bool
	}{{ // Test 0: An Ansible split with no stored inventory lists a path.
		Tool: ToolAnsible, Path: "/etc/shells", Shards: 2, WantResult: true,
	}, { // Test 1: An empty tool is Ansible.
		Tool: "", Path: "hosts.ini", Shards: 3, WantResult: true,
	}, { // Test 2: A stored inventory replaces the path.
		Tool: ToolAnsible, Path: "hosts.ini", Shards: 2, InventoryID: "inv_fleet", WantResult: false,
	}, { // Test 3: One shard is not a split.
		Tool: ToolAnsible, Path: "hosts.ini", Shards: 1, WantResult: false,
	}, { // Test 4: No shards is not a split.
		Tool: ToolAnsible, Path: "hosts.ini", Shards: 0, WantResult: false,
	}, { // Test 5: Another tool runs once and lists nothing.
		Tool: ToolBash, Path: "hosts.ini", Shards: 4, WantResult: false,
	}, { // Test 6: A split that names no inventory has no path to open.
		Tool: ToolAnsible, Path: "", Shards: 2, WantResult: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := RawPathSplit(test.Tool, test.Path, test.InventoryID, test.Shards)
			if got != test.WantResult {
				t.Errorf("RawPathSplit(%q, %q, %q, %d) = %v, want %v",
					test.Tool, test.Path, test.InventoryID, test.Shards, got, test.WantResult)
			}
		})
	}
}
