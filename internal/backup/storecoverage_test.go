package backup_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/backup"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// notBackedUp are the stores a backup deliberately leaves out, with the reason. Run history and the
// audit chain are out of scope by design: the chain has its own signed, self-verifying export, and
// restoring history into a different install would put another install's record under this one's
// identity. Close is not a store.
var notBackedUp = map[string]string{
	"Runs":   "run history is out of scope; it is operational data, not configuration",
	"Audits": "the audit chain has its own signed export and must not be restored under a new identity",
	"Close":  "not a store",
}

// TestEveryStoreIsBackedUpOrDeliberatelyNot pins that a store the database exposes is either carried
// by a backup or named here as an exclusion.
//
// A backup is a hand-maintained parallel list of what matters. Adding a store to the product does
// not add it to the backup, and nothing fails when it is missed: the backup writes, the restore
// reads, and the summary counts everything it knew to count. The gap appears on the day somebody
// restores and finds the thing simply absent. This turns that into a failing test at the moment the
// store is added, when the person adding it can still decide.
func TestEveryStoreIsBackedUpOrDeliberatelyNot(t *testing.T) {
	t.Parallel()
	db := reflect.TypeOf(&sqlitestore.DB{})
	carried := map[string]bool{}
	stores := reflect.TypeOf(backup.Stores{})
	for i := 0; i < stores.NumField(); i++ {
		carried[stores.Field(i).Name] = true
	}

	var missing []string
	for i := 0; i < db.NumMethod(); i++ {
		name := db.Method(i).Name
		if carried[name] {
			continue
		}
		if _, deliberate := notBackedUp[name]; deliberate {
			continue
		}
		missing = append(missing, name)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("these stores are neither backed up nor listed as deliberate exclusions: %s.\n"+
			"Add each to backup.Stores and to gather/apply, or name it in notBackedUp with the "+
			"reason. A store that is silently absent is only discovered during a restore.",
			strings.Join(missing, ", "))
	}

	// The exclusion list guards itself: a name here that the database no longer exposes is stale,
	// and a stale exclusion would hide a real gap if the name were ever reused.
	for name := range notBackedUp {
		if _, ok := db.MethodByName(name); !ok {
			t.Errorf("notBackedUp names %q, which the database no longer exposes; remove it", name)
		}
	}
}
