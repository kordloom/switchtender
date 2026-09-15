package cmd

import (
	"os"
	"regexp"
	"sort"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
)

// kindTableRow matches a credential kind named in the first column of a documentation table.
var kindTableRow = regexp.MustCompile("(?m)^\\s*\\|\\s*`([a-z_]+)`\\s*\\|")

// TestTheSecretTutorialNamesTheKindsTheCodeHas holds the tutorial's table of credential kinds
// against the kinds the binary registers, by name.
//
// A count test already covers this file, but only the count. That passes while a kind is renamed,
// or while the table lists a kind that was removed and omits the one that replaced it. The reader
// of this tutorial is choosing from the table and then typing what they chose, so a wrong name
// there is a dead end at the first step, and the count stays right the whole time.
func TestTheSecretTutorialNamesTheKindsTheCodeHas(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile("../docs/tutorial-set-a-secret.md")
	if err != nil {
		t.Fatalf("cannot read the secret tutorial: %v", err)
	}

	var documented []string
	for _, m := range kindTableRow.FindAllStringSubmatch(string(raw), -1) {
		documented = append(documented, m[1])
	}
	if len(documented) == 0 {
		t.Fatal("no credential kinds found in the tutorial, so this test is asserting nothing. " +
			"The table's shape changed and this pattern needs to change with it")
	}

	want := make([]string, 0, len(credential.Kinds()))
	for _, k := range credential.Kinds() {
		want = append(want, string(k))
	}
	sort.Strings(want)
	sort.Strings(documented)

	if diff := cmp.Diff(want, documented); diff != "" {
		t.Errorf("the tutorial's kinds do not match the binary's (-code +doc):\n%s", diff)
	}
}
