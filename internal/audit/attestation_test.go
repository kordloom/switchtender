package audit

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// testHeldAttestation returns a witness attestation with every member filled, so a comparison that
// skipped any one of them is caught when only that member differs.
func testHeldAttestation() *WitnessAttestation {
	return &WitnessAttestation{
		ID: "att_000000000001", Seq: 4, Link: strings.Repeat("a", 64), Kind: AttestationClaim,
		Size: 0, Root: "", KeyID: "sha256:" + strings.Repeat("b", 64),
		PublicKey: "11qYAYKxCrfVS/7TyWQHOg7hcvPapiMlrwIaaPcHURo=", Role: "witness",
		At: "2026-10-21T12:00:00.5Z", Sig: "c2lnbmF0dXJl",
		Witness: "sha256:" + strings.Repeat("c", 64),
	}
}

func TestValidAttestationKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Kind is the kind checked.
		Kind string
		// WantValid is whether the kind is one the format defines.
		WantValid bool
	}{{ // Test 0: A claim attestation.
		Kind: AttestationClaim, WantValid: true,
	}, { // Test 1: A head attestation.
		Kind: AttestationHead, WantValid: true,
	}, { // Test 2: The empty kind.
		Kind: "", WantValid: false,
	}, { // Test 3: A kind differing only in case.
		Kind: "Claim", WantValid: false,
	}, { // Test 4: An anchor type is not an attestation kind.
		Kind: AnchorRFC3161, WantValid: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := ValidAttestationKind(test.Kind); got != test.WantValid {
				t.Errorf("ValidAttestationKind(%q) = %v, want %v", test.Kind, got, test.WantValid)
			}
		})
	}
}

func TestNewAttestationID(t *testing.T) {
	t.Parallel()
	seen := map[string]bool{}
	for range 64 {
		id := NewAttestationID()
		if !strings.HasPrefix(id, "att_") || len(id) != len("att_")+12 {
			t.Fatalf("NewAttestationID() = %q, want att_ and 12 hex", id)
		}
		if seen[id] {
			t.Fatalf("NewAttestationID() repeated %q", id)
		}
		seen[id] = true
	}
}

// resolveHeldCase is one save settled against the held record.
type resolveHeldCase struct {
	// Field is the one member the saved record differs in.
	Field string
	// WantID is the id the saved record carries afterward.
	WantID string
	// Want is the error the settlement must return.
	Want error
}

// TestResolveHeldAttestationComparesEveryMember walks every member of WitnessAttestation by
// reflection, so a member added later is covered without anyone remembering to list it here. A
// record differing from the held one in any member but ID is a conflict and keeps its own id, and
// a record differing only in ID is the same answer and takes the held id. The held record is never
// changed.
func TestResolveHeldAttestationComparesEveryMember(t *testing.T) {
	t.Parallel()
	typ := reflect.TypeOf(WitnessAttestation{})
	tests := []resolveHeldCase{{ // Test 0: Only the id differs, so it is the held answer again.
		Field: "ID", WantID: "att_000000000001", Want: nil,
	}}
	for i := range typ.NumField() {
		name := typ.Field(i).Name
		if name == "ID" {
			continue
		}
		// Test N: Only this member differs, so it is a different answer at the held coordinate.
		tests = append(tests, resolveHeldCase{
			Field: name, WantID: "att_000000000002", Want: ErrAttestationConflict,
		})
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Field), func(t *testing.T) {
			t.Parallel()
			held := testHeldAttestation()
			want := *held
			a := testHeldAttestation()
			a.ID = "att_000000000002"
			if test.Field != "ID" {
				field := reflect.ValueOf(a).Elem().FieldByName(test.Field)
				switch field.Kind() {
				case reflect.String:
					field.SetString(field.String() + "x")
				case reflect.Int64:
					field.SetInt(field.Int() + 1)
				default:
					t.Fatalf("member %s has kind %s, which this test does not know how to vary",
						test.Field, field.Kind())
				}
			}
			err := ResolveHeldAttestation(held, a)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ResolveHeldAttestation() error = %v, want %v", err, test.Want)
			}
			if a.ID != test.WantID {
				t.Errorf("record id = %s, want %s", a.ID, test.WantID)
			}
			if diff := cmp.Diff(&want, held); diff != "" {
				t.Errorf("the held record changed (-want +got):\n%s", diff)
			}
		})
	}
}
