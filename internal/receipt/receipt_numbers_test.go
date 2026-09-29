package receipt_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// TestReceiptSurvivesAnyNumberInTheSpec covers the availability of the artifact the whole product
// rests on, for a run submitted the way customers submit runs.
//
// A receipt discloses the run's spec so a verifier can recompute the digests the chain committed. The
// spec was disclosed as a parsed JSON tree, and the signature canonicalizes the bundle under a profile
// whose numbers are integers only, so one fractional extra var, a threshold of 3.14, made signing fail
// and the receipt impossible to produce. The run executed fine and its chain entries were intact; only
// the evidence could not be issued, and since the spec of a finished run never changes, it could never
// be issued. The offline sparse form worked, but it discloses no spec at all.
//
// A large integer was worse than an error. It round-tripped through a float on the way back out, so the
// disclosed spec no longer digested to what the chain committed and the receipt read as inconsistent:
// evidence that accuses itself of tampering because of a number's width.
//
// The spec is disclosed as the exact bytes the digest was taken over, which is both representable and
// faithful, so neither case can recur.
func TestReceiptSurvivesAnyNumberInTheSpec(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		// Name identifies the number's shape.
		Name string
		// Vars are the run's extra vars, as an operator supplied them.
		Vars map[string]any
		// Want is a distinctive substring of the value that must survive into the disclosed spec.
		Want string
	}{{ // Test 0: A fraction, which the signing profile cannot represent at all.
		Name: "fraction", Vars: map[string]any{"threshold": 3.14}, Want: "3.14",
	}, { // Test 1: An integer wider than the profile's range, which round-tripped through a float.
		Name: "wide integer", Vars: map[string]any{"batch": json.Number("9007199254740994")},
		Want: "9007199254740994",
	}, { // Test 2: A negative exponent, the other unrepresentable shape.
		Name: "exponent", Vars: map[string]any{"ratio": 1.5e-7}, Want: "1.5e-7",
	}, { // Test 3: An ordinary integer, which must keep working.
		Name: "plain integer", Vars: map[string]any{"retries": 3}, Want: "3",
	}}

	for testNum, test := range cases {
		t.Run(test.Name, func(t *testing.T) {
			runs, audits, id, r := heldWith(t, "approve", func(r *run.Run) {
				r.ExtraVars = test.Vars
			})

			res, err := receipt.Build(ctx, runs, audits, id, "test", r.ID, receipt.Options{})
			if err != nil {
				t.Fatalf("test %d: a run with a %s extra var cannot be receipted: %v",
					testNum, test.Name, err)
			}

			// The receipt has to verify, and its disclosed spec has to agree with what the chain
			// committed. A receipt that builds but reads as inconsistent is worse than one that fails.
			rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
			if err != nil {
				t.Fatalf("test %d: VerifyBundle: %v", testNum, err)
			}
			if !rep.SignatureOK {
				t.Errorf("test %d: the receipt's signature does not verify", testNum)
			}
			if !rep.ChainOK {
				t.Errorf("test %d: the receipt's chain does not verify", testNum)
			}
			if !rep.SpecPresent {
				t.Fatalf("test %d: the receipt disclosed no spec, so nobody can see what ran", testNum)
			}
			if !rep.SpecConsistent {
				t.Errorf("test %d: the disclosed spec does not match the digest the chain committed, "+
					"so the receipt accuses itself of tampering over a %s", testNum, test.Name)
			}

			// The value itself is readable, which is the point of disclosing the spec.
			if !strings.Contains(string(rep.SpecBody), test.Want) {
				t.Errorf("test %d: the disclosed spec does not carry %s:\n%s",
					testNum, test.Want, rep.SpecBody)
			}
		})
	}
}
