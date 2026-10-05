package inventory

import (
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestDiffListings pins the differences the cross-check reports, each a sentence naming the host,
// group, or variable, with values typed and secrets named but never shown.
func TestDiffListings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Native   string
		Ansible  string
		WantDiff []string
	}{{ // Test 0: The same inventory has no differences.
		Native: "[web]\nw1 port=80\n", Ansible: "[web]\nw1 port=80\n",
	}, { // Test 1: A host in one view only.
		Native: "[web]\nw1\nw2\n", Ansible: "[web]\nw1\n",
		WantDiff: []string{"host w2 is in the native view and not in Ansible's",
			"group web has host w2 in the native view and not in Ansible's"},
	}, { // Test 2: A variable typed differently.
		Native: "[web]\nw1 port=80\n", Ansible: "[web]\nw1 port=\"'80'\"\n",
		WantDiff: []string{`host w1 variable port is 80 (int) in the native view and "80" ` +
			`(string) in Ansible's`},
	}, { // Test 3: A secret is named and never shown.
		Native: "[web]\nw1 ansible_password=hunter2\n", Ansible: "[web]\nw1 ansible_password=other\n",
		WantDiff: []string{"host w1 variable ansible_password differs (the value is secret and " +
			"not shown)"},
	}, { // Test 4: A group and a child group in one view only.
		Native: "[p:children]\nc\n[c]\nh\n", Ansible: "[p]\nh\n[c]\nh\n",
		WantDiff: []string{"group p has host h in Ansible's view and not in the native one",
			"group p has child group c in the native view and not in Ansible's"},
	}, { // Test 5: A reserved name is not compared, since releases disagree about it.
		Native: "[web]\nw1\n", Ansible: "[web]\nw1 omit=z\n",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			n, err := ResolveNative(test.Native)
			if err != nil {
				t.Fatal(err)
			}
			a, err := ResolveNative(test.Ansible)
			if err != nil {
				t.Fatal(err)
			}
			got := DiffListings(n, a)
			if diff := cmp.Diff(test.WantDiff, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("DiffListings() mismatch (-want +got):\n%s", diff)
			}
			for _, line := range got {
				if strings.Contains(line, "hunter2") || strings.Contains(line, "other") {
					t.Errorf("a difference shows a secret: %s", line)
				}
			}
		})
	}
}

// TestDiffListingsIsBounded pins that a disagreement about a whole fleet stays readable.
func TestDiffListingsIsBounded(t *testing.T) {
	t.Parallel()
	n, err := ResolveNative("[web]\nh[1:100]\n")
	if err != nil {
		t.Fatal(err)
	}
	a, err := ResolveNative("[web]\nx[1:100]\n")
	if err != nil {
		t.Fatal(err)
	}
	got := DiffListings(n, a)
	if len(got) != maxDifferences+1 || !strings.HasPrefix(got[maxDifferences], "and ") {
		t.Errorf("DiffListings() gave %d lines ending %q, want %d and a count of the rest",
			len(got), got[len(got)-1], maxDifferences+1)
	}
}
