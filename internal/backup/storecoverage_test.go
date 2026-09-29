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
	"BeginReadSnapshot": "not a store; it is the seam a backup pins its one consistent instant with, " +
		"carried on Stores as the Snapshot field",
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

// summaryExtras are Summary count fields with no same-named Stores field, each with its reason.
// Memberships counts rows carried inside teams and orgs rather than a store of their own.
var summaryExtras = map[string]string{
	"Memberships": "team and org membership rows ride inside their parents",
}

// TestEveryStoreFieldIsCountedBySummary pins the next link of the chain storecoverage starts.
//
// storecoverage forces a store onto backup.Stores. This forces the Summary to count it, by name,
// and the report guard in cmd forces every Summary count into the operator-facing line. Without
// this link a store could sit on the struct, be gathered or not, and no count would ever say,
// which is how policies and credential types were restored invisibly.
func TestEveryStoreFieldIsCountedBySummary(t *testing.T) {
	t.Parallel()
	counts := map[string]bool{}
	sum := reflect.TypeOf(backup.Summary{})
	for i := 0; i < sum.NumField(); i++ {
		if sum.Field(i).Type.Kind() == reflect.Int {
			counts[sum.Field(i).Name] = true
		}
	}
	stores := reflect.TypeOf(backup.Stores{})
	for i := 0; i < stores.NumField(); i++ {
		name := stores.Field(i).Name
		if name == "Snapshot" {
			continue // The snapshot seam, not a store; storecoverage documents it.
		}
		if !counts[name] {
			t.Errorf("Stores.%s has no matching Summary count: whatever it holds is backed up "+
				"and restored with no number ever reported for it", name)
		}
	}
	for name := range summaryExtras {
		if !counts[name] {
			t.Errorf("summaryExtras names %s but Summary has no such count", name)
		}
	}
	for name := range counts {
		if _, ok := stores.FieldByName(name); !ok {
			if _, deliberate := summaryExtras[name]; !deliberate {
				t.Errorf("Summary.%s counts nothing on Stores and is not a named extra: a count "+
					"with no source drifts into a lie", name)
			}
		}
	}
}
