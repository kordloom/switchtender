package binding

import (
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// uncovered names each run field a seal leaves out on purpose, with the reason. A field is either
// moved by the seal or named here, so a field added to run.Run fails the test until somebody
// decides which it is.
var uncovered = map[string]string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
	"Status": "moves through the row's life, so a submit seal carries only whether the row " +
		"was held (Record.Held), which the verifier sets from the check it makes rather than " +
		"from the row; a finished row is held by the result seal",
	"ExitCode":        "written when the run ends",
	"Error":           "written when the run ends",
	"Warning":         "written while the run is held or runs, and read only by people",
	"StartedAt":       "written when the run starts",
	"EndedAt":         "written when the run ends",
	"ClaimedBy":       "the lease, written at each claim",
	"ClaimedAt":       "the lease, written at each claim",
	"ClaimSecret":     "the lease, written at each claim",
	"CancelRequested": "written by a cancel after the row is built, and it only stops a run",
	"Outputs":         "written when the run ends and held by the result seal",
	"SpecSeal":        "the seal itself",
	"ResultSeal":      "the result seal itself",
	"DecisionID": "covered through the root: the verifier reads the entry it names, and the " +
		"record carries that entry's id and content digest",
	"DecisionClaim": "a claim carries its own seal, made by SealClaim",
	"AwaitingStep":  "workflow progress, written while the walk runs",
	"Attention":     "a note for people, written while the run waits",
	"HoldNote":      "says to people why the run was held; the hold is ApprovalRequested and HeldByPolicy",
	"PolicyNotes":   "says to people which rules matched; no rule or gate reads it back",
	"Risk":          "a display grade the server computes on read; rules grade the run themselves",
	"Notifications": "read after the run ends to announce it, by no rule or gate",
	"SealedVars": "ciphertext the spec binds by digest (SealedDigests), checked against that " +
		"digest before it is opened",
	"InventorySealed": "ciphertext the spec binds through InventorySnapshot, and wiped when the run ends",
	"PlanSealed":      "ciphertext the spec binds through PlanSHA256, and wiped when the run ends",
	"InventoryCheck":  "written while the run executes",
	"CommitSHA":       "the commit the project synced to, written at execution and held to PinnedCommit",
	"ImageDigest":     "the image digest resolved at execution",
	"ResolvedHosts":   "the hosts resolved at execution",
	"AuditReceipt":    "written after the run ends",
	"RetryOf":         "lineage shown to people; a retry is built from the row it names and sealed then",
	"RerunOf":         "lineage shown to people, read by no rule or gate",
	"IdempotencyKey":  "a dedupe key read at submit before the row exists, never after",
}

// testCoverageRun returns a top-level run with a time set and the fact cache on, so its timeout is
// read, which every other field is changed against.
func testCoverageRun() *run.Run {
	return &run.Run{ID: "run_cov", Playbook: "site.yml", Inventory: "hosts", Status: run.StatusPending,
		CreatedAt: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC), UseFactCache: true}
}

// TestEveryRunFieldIsCoveredOrNamed changes each exported field of a run to a value other than the
// fixture's and requires the submit seal to move, unless the field is named in uncovered. The seal
// covers every field a rule, a gate, or an approval check reads, so a field it misses is one a
// database writer can change under a valid seal.
func TestEveryRunFieldIsCoveredOrNamed(t *testing.T) {
	t.Parallel()
	k := testKey(t, 1)
	rec, err := SubmitRecord(testCoverageRun())
	if err != nil {
		t.Fatalf("SubmitRecord() error = %v", err)
	}
	stored := k.Seal(rec)
	typ := reflect.TypeFor[run.Run]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()
			changed := testCoverageRun()
			perturb(reflect.ValueOf(changed).Elem().Field(i))
			rec, err := SubmitRecord(changed)
			if err != nil {
				t.Fatalf("SubmitRecord() with %s changed error = %v", field.Name, err)
			}
			moved := !k.Verify(rec, stored)
			reason, named := uncovered[field.Name]
			switch {
			case !moved && !named:
				t.Errorf("run field %s changes without moving the seal and is not named in "+
					"uncovered: cover it in the record or name why it is left out", field.Name)
			case moved && named:
				t.Errorf("run field %s moves the seal but is named in uncovered (%s): remove it "+
					"from the list", field.Name, reason)
			}
		})
	}
}

// uncoveredEntry names each chain entry field a root leaves out on purpose, with the reason. A field
// is either moved by RootOf or named here, so a field added to audit.Entry fails the test until
// somebody decides which it is.
var uncoveredEntry = map[string]string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
	"At": "bound by the link only: the sealers seal before append and Link truncates the time, so " +
		"sealing it needs an encoding fixed on the stored form",
	"Seq":       "chain position, assigned at append and bounded by anchors",
	"PrevHash":  "chain position, assigned at append and bounded by anchors",
	"Hash":      "the public link, assigned at append and bounded by anchors",
	"Nonce":     "never exported; the content digest commits its hash",
	"InstallID": "a chain from a foreign install is refused at start",
}

// testCoverageEntry returns a decision entry with every field set, which every other entry is
// changed against.
func testCoverageEntry() *audit.Entry {
	return &audit.Entry{ID: "aud_cov", At: time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC),
		Actor: "approver", ActorType: "session", OnBehalfOf: "ops-bot",
		Method: audit.MethodDecision, Path: "/runs/run_cov/decision/approved", ContentDigest: "cd",
		Nonce: "nonce", InstallID: "in_1", Seq: 3, PrevHash: "ph", Hash: "h"}
}

// TestEveryEntryFieldIsCoveredOrNamed changes each exported field of a chain entry and requires the
// root naming it to move, unless the field is named in uncoveredEntry. A field a root misses is one
// a database writer can rewrite on the chain under a valid seal.
func TestEveryEntryFieldIsCoveredOrNamed(t *testing.T) {
	t.Parallel()
	want := RootOf(testCoverageEntry())
	typ := reflect.TypeFor[audit.Entry]()
	for i := range typ.NumField() {
		field := typ.Field(i)
		if !field.IsExported() {
			continue
		}
		t.Run(field.Name, func(t *testing.T) {
			t.Parallel()
			changed := testCoverageEntry()
			perturb(reflect.ValueOf(changed).Elem().Field(i))
			moved := RootOf(changed) != want
			reason, named := uncoveredEntry[field.Name]
			switch {
			case !moved && !named:
				t.Errorf("entry field %s changes without moving the root and is not named in "+
					"uncoveredEntry: cover it in the root or name why it is left out", field.Name)
			case moved && named:
				t.Errorf("entry field %s moves the root but is named in uncoveredEntry (%s): "+
					"remove it from the list", field.Name, reason)
			}
		})
	}
}

// TestTheSubmitSealCoversTheHold pins the one part of a row's status a submit seal covers: whether
// the row was waiting for a decision. A row sealed held does not verify as queued, and the reverse,
// while any other status reads as queued, so a writer's move between them changes nothing.
func TestTheSubmitSealCoversTheHold(t *testing.T) {
	t.Parallel()
	k := testKey(t, 1)
	tests := []struct {
		// Sealed is the status the row is sealed with.
		Sealed run.Status
		// Checked is the status the record is rebuilt with.
		Checked run.Status
		// WantVerified reports whether the seal verifies.
		WantVerified bool
	}{{ // Test 0: A held row checked as held verifies.
		Sealed: run.StatusPendingApproval, Checked: run.StatusPendingApproval, WantVerified: true,
	}, { // Test 1: A held row checked as queued does not.
		Sealed: run.StatusPendingApproval, Checked: run.StatusPending, WantVerified: false,
	}, { // Test 2: A queued row checked as held does not.
		Sealed: run.StatusPending, Checked: run.StatusPendingApproval, WantVerified: false,
	}, { // Test 3: A queued row checked as queued verifies.
		Sealed: run.StatusPending, Checked: run.StatusPending, WantVerified: true,
	}, { // Test 4: A running row reads as queued.
		Sealed: run.StatusRunning, Checked: run.StatusPending, WantVerified: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			sealed, checked := testCoverageRun(), testCoverageRun()
			sealed.Status, checked.Status = test.Sealed, test.Checked
			rec, err := SubmitRecord(sealed)
			if err != nil {
				t.Fatalf("SubmitRecord() error = %v", err)
			}
			again, err := SubmitRecord(checked)
			if err != nil {
				t.Fatalf("SubmitRecord() error = %v", err)
			}
			if got := k.Verify(again, k.Seal(rec)); got != test.WantVerified {
				t.Errorf("sealed %s, checked %s: Verify() = %t, want %t", test.Sealed,
					test.Checked, got, test.WantVerified)
			}
		})
	}
}

// perturb changes v to a value other than the one it holds. A nil pointer, slice, or map gains a
// changed element, and a struct has every exported field changed.
func perturb(v reflect.Value) {
	switch v.Kind() {
	case reflect.String:
		v.SetString(v.String() + "x")
	case reflect.Bool:
		v.SetBool(!v.Bool())
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		v.SetInt(v.Int() + 1)
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		v.SetUint(v.Uint() + 1)
	case reflect.Float32, reflect.Float64:
		v.SetFloat(v.Float() + 1)
	case reflect.Pointer:
		if v.IsNil() {
			v.Set(reflect.New(v.Type().Elem()))
		}
		perturb(v.Elem())
	case reflect.Slice:
		elem := reflect.New(v.Type().Elem()).Elem()
		perturb(elem)
		v.Set(reflect.Append(v, elem))
	case reflect.Map:
		if v.IsNil() {
			v.Set(reflect.MakeMap(v.Type()))
		}
		key := reflect.New(v.Type().Key()).Elem()
		perturb(key)
		val := reflect.New(v.Type().Elem()).Elem()
		perturb(val)
		v.SetMapIndex(key, val)
	case reflect.Interface:
		v.Set(reflect.ValueOf("x"))
	case reflect.Struct:
		if v.Type() == reflect.TypeFor[time.Time]() {
			v.Set(reflect.ValueOf(v.Interface().(time.Time).Add(time.Hour)))
			return
		}
		for i := range v.NumField() {
			if v.Type().Field(i).IsExported() {
				perturb(v.Field(i))
			}
		}
	}
}
