package policy

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestFileStoreRefusesRulesTheLicenceDoesNotCover covers a gate that was one file edit away from not
// existing.
//
// The policy file hot-reloads, and the license check lived only in serve's startup path. So a
// Community install could start with a plain file, have deny rules, risk floors and actor scoping
// appended to it afterward, and run the full policy engine uncapped for as long as the process
// lived. Nothing re-read the license because nothing re-checked it.
//
// The refusal is an error rather than a quiet drop of the rules it cannot license. Dropping them
// would ungate the runs those rules exist to hold, and the dispatcher already treats an unreadable
// policy set as a refusal to run: disruptive once, in the direction that does not execute something
// an approver was meant to see.
func TestFileStoreRefusesRulesTheLicenceDoesNotCover(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "policies.yml")

	// A plain rule: holds a tool for approval, which every tier may do.
	plain := "policies:\n  - name: hold bash\n    tool: bash\n"
	// The same file after somebody adds a deny rule, which is Team.
	advanced := plain + "  - name: never destroy prod\n    tool: terraform\n    effect: deny\n"

	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write policy file: %v", err)
		}
	}

	write(plain)
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}

	// Drop to Community for the rest, restoring the package license whatever happens.
	team := license.Current()
	license.Set(nil)
	t.Cleanup(func() { license.Set(team) })

	// A plain file is fine on Community, which is the case that must keep working.
	if _, err := store.List(context.Background()); err != nil {
		t.Fatalf("a Community install was refused a plain policy file: %v", err)
	}

	// Now the edit that used to buy the full engine for free.
	write(advanced)
	_, err = store.List(context.Background())
	if err == nil {
		t.Fatal("a Community install reloaded a file carrying a deny rule and served it, so " +
			"editing the file after startup is all it takes to run the full policy engine uncapped")
	}
	if !strings.Contains(err.Error(), "switchtender.com/pricing") {
		t.Errorf("the refusal does not point anywhere: %v", err)
	}
}
