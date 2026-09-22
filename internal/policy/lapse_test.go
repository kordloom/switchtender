package policy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/license"
)

// TestALapsedLicenseKeepsAPolicyFileInstallRunning covers the difference between a gate and a kill
// switch.
//
// The license check on every read is right, and refusing rather than quietly dropping rules is
// right, because a dropped deny rule ungates the runs it was written to hold. Together they had a
// third consequence nobody chose: the refusal also fires when the term simply ran out. The
// dispatcher reads an unreadable policy set as a reason not to run and serve will not start on one,
// so a paid install with a policy file went completely dark the minute its license expired. Not
// degraded to Community, not capped: every run refused and no restart.
//
// The terms rule that out in as many words. A lapsed license takes nothing, we do not reserve a
// right to disable the software, and we will not brick a running install over a billing dispute.
// That is one of the seven commitments the terms call contractual, so this is the product breaking
// a promise it sells on, not a rough edge.
//
// An install that never bought the tier is a different case and still refused, which is the gate
// doing its job on something the operator never had working.
func TestALapsedLicenseKeepsAPolicyFileInstallRunning(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policies.yml")

	// Deny rules and risk floors are Team. Several of them, so the count cap is crossed too and
	// both gates are exercised by the same file.
	advanced := "policies:\n" +
		"  - name: hold bash\n    tool: bash\n" +
		"  - name: never destroy prod\n    tool: terraform\n    effect: deny\n" +
		"  - name: hold ansible\n    tool: ansible\n"
	if err := os.WriteFile(path, []byte(advanced), 0o600); err != nil {
		t.Fatalf("write policy file: %v", err)
	}

	held := license.Current()
	t.Cleanup(func() { license.Set(held) })

	// A live Team license: the state this install was in yesterday.
	live := &license.License{Claims: license.Claims{
		V: 1, ID: "lic_live", Org: "Example", Tier: "team", Hosts: "500",
		Issued:  time.Now().Add(-48 * time.Hour).Format(time.RFC3339),
		Expires: time.Now().Add(24 * time.Hour).Format(time.RFC3339),
		Kid:     "k1",
	}}
	license.Set(live)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	before, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("a live Team license was refused its own policy file: %v", err)
	}
	if len(before) != 3 {
		t.Fatalf("got %d policies under a live license, want 3", len(before))
	}

	// Midnight. Same file, same rules, same install; only the clock moved.
	lapsed := &license.License{Claims: license.Claims{
		V: 1, ID: "lic_lapsed", Org: "Example", Tier: "team", Hosts: "500",
		Issued:  time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
		Expires: time.Now().Add(-1 * time.Hour).Format(time.RFC3339),
		Kid:     "k1",
	}}
	license.Set(lapsed)

	after, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("a lapsed license took the install offline: List() error = %v.\n"+
			"Every run is refused and serve will not start, which is the bricking the terms "+
			"promise never happens", err)
	}
	// The rules keep applying. Serving the set minus its advanced rules would be worse than the
	// outage it replaces: the deny rule holding production would stop applying, silently, at the
	// moment the invoice lapsed.
	if len(after) != len(before) {
		t.Fatalf("got %d policies after the lapse, want %d: dropping rules on expiry ungates "+
			"exactly the runs those rules exist to hold", len(after), len(before))
	}
	var denies int
	for _, p := range after {
		if p.Advanced() {
			denies++
		}
	}
	if denies == 0 {
		t.Error("the advanced rules stopped being served after the lapse, so a deny rule written " +
			"to hold production silently stopped applying")
	}

	// The gate still bites where nothing was ever bought.
	license.Set(nil)
	if _, err := store.List(context.Background()); err == nil {
		t.Error("a Community install with no license at all was served the full engine: the " +
			"lapse path must not become a way around the gate")
	} else if strings.Contains(err.Error(), "lapsed") {
		t.Errorf("a never-licensed install was refused as though its term ran out: %v", err)
	}
}
