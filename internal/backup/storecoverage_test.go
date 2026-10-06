package backup_test

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/backup"
	"github.com/kordloom/switchtender/internal/pgstore"
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
	"FactCache": "cached facts are runtime state a run regathers, and a fact document carries the " +
		"remote user's environment, which a portable file handed between installs should not",
	"FederationKeys": "the federation signing keys are sealed under this install's encryption key " +
		"and regenerate on their own; restoring them into another install would let it sign run " +
		"identity tokens a cloud trusts as this one",
	"BeginReadSnapshot": "not a store; it is the seam a backup pins its one consistent instant with, " +
		"carried on Stores as the Snapshot field",
	"ReviewReports": "a pull request review report record is runtime state about runs, " +
		"which are out of scope, and restoring one into another install would have it post " +
		"to a pull request again",
	"Decisions": "decision records are run history: an approver's reason belongs to its run and " +
		"its chain entry, both out of scope, and restoring it into another install would attach " +
		"reasons to commitments that install's chain never made",
	"Attention": "worker reports and raised attention alerts are runtime state the fleet rebuilds " +
		"within seconds of starting, and restoring another install's alerts would silence ones " +
		"this install never raised",
	"ForgeLinks": "a forge account link is proven through the forge's own sign-in and recorded on " +
		"this install's chain when it is made, and restoring one would let a pull request comment " +
		"act as an account with no record of the link on the chain, so each person links again",
}

// TestEveryStoreIsBackedUpOrDeliberatelyNot pins that a store either database exposes is either
// carried by a backup or named here as an exclusion.
//
// A backup is a hand-maintained parallel list of what matters. Adding a store to the product does
// not add it to the backup, and nothing fails when it is missed: the backup writes, the restore
// reads, and the summary counts everything it knew to count. The gap appears on the day somebody
// restores and finds the thing simply absent. This turns that into a failing test at the moment the
// store is added, when the person adding it can still decide. Both databases are read, since a
// store added to one first is as absent from a backup as one added to neither.
func TestEveryStoreIsBackedUpOrDeliberatelyNot(t *testing.T) {
	t.Parallel()
	carried := map[string]bool{}
	stores := reflect.TypeOf(backup.Stores{})
	for i := 0; i < stores.NumField(); i++ {
		carried[stores.Field(i).Name] = true
	}
	exposed := map[string]bool{}
	for _, db := range []reflect.Type{reflect.TypeOf(&sqlitestore.DB{}), reflect.TypeOf(&pgstore.DB{})} {
		var missing []string
		for i := 0; i < db.NumMethod(); i++ {
			name := db.Method(i).Name
			exposed[name] = true
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
			t.Errorf("these %s stores are neither backed up nor listed as deliberate exclusions: "+
				"%s.\nAdd each to backup.Stores and to gather/apply, or name it in notBackedUp with "+
				"the reason. A store that is silently absent is only discovered during a restore.",
				db, strings.Join(missing, ", "))
		}
	}

	// The exclusion list guards itself: a name here that neither database exposes is stale, and a
	// stale exclusion would hide a real gap if the name were ever reused.
	for name := range notBackedUp {
		if !exposed[name] {
			t.Errorf("notBackedUp names %q, which neither database exposes; remove it", name)
		}
	}
}

// summaryExtras are Summary count fields with no same-named Stores field, each with its reason.
// Memberships counts rows carried inside teams and orgs rather than a store of their own.
var summaryExtras = map[string]string{
	"Memberships":             "team and org membership rows ride inside their parents",
	"NotificationAttachments": "attachment rows ride inside the notification target they belong to",
	"AWXBindings":             "awx callback bindings ride in the template store beside the templates",
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
