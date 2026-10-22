package binding

import (
	"reflect"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// uncovered names each run field a seal leaves out on purpose, with the reason. A field is either
// moved by the seal or named here, so a field added to run.Run fails the test until somebody
// decides which it is.
var uncovered = map[string]string{ //nolint:gochecknoglobals // Not modified, simplifies testing.
	"Status":          "moves through the row's life; a finished row is held by the result seal",
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
