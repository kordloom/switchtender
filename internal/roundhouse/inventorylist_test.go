package roundhouse

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestListInventoryFailsOnASourceNoPluginParses runs real ansible-inventory through the shared
// lister every composed inventory and host limit uses. A source no plugin parses is an error, not
// a listing that quietly lacks its hosts, while the empty and near-empty inputs the releases read
// as no hosts still list.
func TestListInventoryFailsOnASourceNoPluginParses(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath(defaultInventoryBinary); err != nil {
		if strings.TrimSpace(os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE")) != "" {
			t.Fatalf("ansible-inventory is required: %v", err)
		}
		t.Skip("ansible-inventory not installed")
	}
	lister, ok := NewAnsibleRunner().(InventoryLister)
	if !ok {
		t.Fatal("the Ansible runner does not list inventories")
	}
	tests := []struct {
		Content   string
		WantError bool
	}{{ // Test 0: A file neither the INI nor the YAML plugin reads is an error.
		Content: "[web\nh1\n", WantError: true,
	}, { // Test 1: A lone document marker is an error.
		Content: "---\n", WantError: true,
	}, { // Test 2: An empty file lists no hosts.
		Content: "",
	}, { // Test 3: A file of comments lists no hosts.
		Content: "# nothing here\n",
	}, { // Test 4: A plain INI file lists.
		Content: "[web]\nh1\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			path := filepath.Join(t.TempDir(), "input")
			if err := os.WriteFile(path, []byte(test.Content), 0o600); err != nil {
				t.Fatal(err)
			}
			out, err := lister.ListInventory(context.Background(), []string{path}, "")
			if test.WantError {
				if !errors.Is(err, ErrLaunch) {
					t.Fatalf("ListInventory() = %s, %v, want ErrLaunch", out, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("ListInventory() error = %v", err)
			}
		})
	}
}

// TestHeadAndTailKeepsBothEnds pins the cut a failed listing's error carries: the first line, which
// names the host and the variable, and the last lines both survive a long failure.
func TestHeadAndTailKeepsBothEnds(t *testing.T) {
	t.Parallel()
	long := "first line names web01 and its variable\n" + strings.Repeat("noise ", 2000) +
		"\nlast line says why"
	tests := []struct {
		In          string
		Limit       int
		WantResult  string
		WantHas     []string
		WantMaxSize int
	}{{ // Test 0: A short message is kept whole and trimmed.
		In: "  short \n", Limit: 64, WantResult: "short",
	}, { // Test 1: A long one keeps its head and its tail.
		In: long, Limit: 256, WantHas: []string{"first line names web01", "[...]", "last line says why"},
		WantMaxSize: 256 + len(" [...] "),
	}, { // Test 2: A cut never splits a character.
		In: strings.Repeat("é", 300), Limit: 101, WantMaxSize: 101 + len(" [...] "),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := headAndTail(test.In, test.Limit)
			if test.WantResult != "" && got != test.WantResult {
				t.Errorf("headAndTail() = %q, want %q", got, test.WantResult)
			}
			for _, want := range test.WantHas {
				if !strings.Contains(got, want) {
					t.Errorf("headAndTail() = %q, want it to hold %q", got, want)
				}
			}
			if test.WantMaxSize > 0 && len(got) > test.WantMaxSize {
				t.Errorf("headAndTail() is %d bytes, want at most %d", len(got), test.WantMaxSize)
			}
			if !strings.Contains(got, "�") && strings.ContainsRune(test.In, 'é') &&
				strings.Count(got, "é")*2+len(" [...] ") != len(got) {
				t.Errorf("headAndTail() = %q, which split a character", got)
			}
		})
	}
}
