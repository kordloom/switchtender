package outcome_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// The spec, binding, and digest of goldenRun with the fact cache off, taken from the code before
// the cache was part of the record. A run that does not use the cache must reduce to exactly these
// bytes, or every receipt and approval issued before the change would stop matching its run.
const (
	goldenSpec = `{"extra_vars":{"region":"eu"},"forks":5,"inventory_id":"inv_fleet",` +
		`"limit":"web","playbook":"site.yml","tags":["deploy"],"tool":"ansible"}`
	goldenBinding = "sha256:4c6b1e942f8fd6aa03ebd17a0aba4b0e745cb426c6b56c6af78aa1be6e65faef"
	goldenDigest  = "sha256:bcee801725ebd4bc013ee7ee1db0de3b2324a266bab9e22245efaeea9a7b3aa4"
)

// goldenRun returns the run the golden values were taken from, with the fact cache set as given.
// It carries a timeout even with the cache off, because a leftover timeout executes nothing.
func goldenRun(useCache bool, timeout int) *run.Run {
	return &run.Run{ID: "run_golden", Tool: run.ToolAnsible, Playbook: "site.yml",
		InventoryID: "inv_fleet", Limit: "web", Tags: []string{"deploy"},
		ExtraVars: map[string]any{"region": "eu"}, Forks: 5,
		UseFactCache: useCache, FactCacheTimeout: timeout}
}

// TestTheFactCacheIsBoundOnlyWhenItIsOn pins decision sixteen. A run that serves cached facts
// records the setting and its timeout in the spec an approval binds to and a receipt discloses, so
// turning the cache on or changing how old a served fact may be moves the binding. A run with the
// cache off reduces to the bytes it always did.
func TestTheFactCacheIsBoundOnlyWhenItIsOn(t *testing.T) {
	t.Parallel()
	tests := []struct {
		WantSpec     string
		WantFragment string
		Run          *run.Run
		WantGolden   bool
	}{{ // Test 0: The cache off, with a timeout left over, is byte-identical to the old record.
		Run: goldenRun(false, 600), WantGolden: true, WantSpec: goldenSpec,
	}, { // Test 1: The cache on records it with its timeout.
		Run: goldenRun(true, 600), WantFragment: `"fact_cache":{"timeout_seconds":600}`,
	}, { // Test 2: The cache on with no timeout says so rather than omitting the zero.
		Run: goldenRun(true, 0), WantFragment: `"fact_cache":{"timeout_seconds":0}`,
	}}
	bindings := map[string]int{}
	for testNum, test := range tests {
		spec, err := outcome.Spec(test.Run)
		if err != nil {
			t.Fatalf("test %d: Spec() error = %v", testNum, err)
		}
		binding, err := outcome.SpecBinding(test.Run)
		if err != nil {
			t.Fatalf("test %d: SpecBinding() error = %v", testNum, err)
		}
		digest, err := outcome.SpecDigest(test.Run)
		if err != nil {
			t.Fatalf("test %d: SpecDigest() error = %v", testNum, err)
		}
		if test.WantSpec != "" {
			if diff := cmp.Diff(test.WantSpec, string(spec)); diff != "" {
				t.Errorf("test %d: spec mismatch (-want +got):\n%s", testNum, diff)
			}
		}
		if test.WantGolden {
			if binding != goldenBinding || digest != goldenDigest {
				t.Errorf("test %d: binding %s digest %s, want the golden %s and %s", testNum,
					binding, digest, goldenBinding, goldenDigest)
			}
		} else if binding == goldenBinding || digest == goldenDigest {
			t.Errorf("test %d: the fact cache setting did not move the binding or the digest",
				testNum)
		}
		if test.WantFragment != "" && !strings.Contains(string(spec), test.WantFragment) {
			t.Errorf("test %d: spec %s does not carry %s", testNum, spec, test.WantFragment)
		}
		if prev, seen := bindings[binding]; seen {
			t.Errorf("test %d shares its binding with test %d", testNum, prev)
		}
		bindings[binding] = testNum
	}
}

// TestAReceiptDisclosingTheFactCacheVerifiesInLoomSeal is the cross-verification for the fact cache
// field. It is part of the spec a receipt discloses, so the format's own verifier has to accept a
// receipt carrying it.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAReceiptDisclosingTheFactCacheVerifiesInLoomSeal(t *testing.T) {
	repo := loomsealRepo(t)
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "operator", ActorType: "session",
		Method: "POST", Path: "/v1/templates/tpl_facts/launch",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	ended := time.Now()
	exit := 0
	r := goldenRun(true, 3600)
	r.ID, r.Status, r.CreatedAt, r.EndedAt, r.ExitCode = "run_facts", run.StatusSucceeded,
		time.Now(), &ended, &exit
	r.Actor, r.ActorType = "operator", "session"
	r.AuditReceipt = fmt.Sprintf("%d:%s", creation.Seq, creation.Hash)
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	res, err := receipt.Build(ctx, runs, audits, id, "v-test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	if len(res.Notes) > 0 {
		t.Fatalf("the receipt withheld its outcome: %v", res.Notes)
	}
	if !strings.Contains(string(res.Signed), "fact_cache") ||
		!strings.Contains(string(res.Signed), "timeout_seconds") {
		t.Fatal("the signed receipt does not state that the run served cached facts")
	}
	report := verifyWithLoomSeal(t, repo, res.Signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt stating the fact cache: %v", report.Problems)
	}
}
