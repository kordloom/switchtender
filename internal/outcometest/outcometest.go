// Package outcometest holds what outcome record tests share across packages: running the LoomSeal
// verifier over a signed bundle, and sealing and approving runs the way a keyed install does so a
// receipt's approvers can be held to their seals.
package outcometest

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/binding"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/loomsealtest"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// modulePath is the module whose root the LoomSeal checkout sits beside.
const modulePath = "github.com/kordloom/switchtender"

// Report is the part of the format verifier's report the tests assert on.
type Report struct {
	// OK reports whether every check passed.
	OK bool `json:"ok"`
	// Problems lists why it did not.
	Problems []string `json:"problems"`
}

// VerifyWithLoomSeal runs the format's own verifier over a signed bundle and returns its report.
func VerifyWithLoomSeal(t *testing.T, repo string, signed []byte) Report {
	t.Helper()
	path := filepath.Join(t.TempDir(), "receipt.loomseal.json")
	if err := os.WriteFile(path, signed, 0o600); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	cmd := exec.Command("go", "run", ".", "verify", "--json", path)
	cmd.Dir = repo
	out, err := cmd.Output()
	if err != nil && len(out) == 0 {
		var stderr string
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			stderr = string(ee.Stderr)
		}
		// A refusal is a verdict and still reports on stdout, so only an empty result is a failure
		// to run at all.
		t.Fatalf("run loomseal verify: %v\n%s", err, stderr)
	}
	var rep Report
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("loomseal report is not JSON: %v\n%s", err, out)
	}
	// The format has two independent implementations, and this product's output has to satisfy the
	// format, not one implementation of it, so the reference verifier reads the same bytes.
	loomsealtest.RequireReferenceAgreement(t, repo, path, out)
	return rep
}

// LoomsealRepo locates the pinned loomseal checkout the cross-check runs the verifier from,
// skipping when there is none so the suite still runs for someone who cloned only this repository.
//
// It reads the one variable internal/audit's cross-check reads, which is also where the contract
// that the checkout sit at the exact version go.mod names is enforced. That check fails loudly
// rather than skipping, so a mismatched checkout is caught by the run that owns the pin instead of
// being restated here. With the variable unset it looks beside the module root, wherever in the tree
// the calling test runs from.
func LoomsealRepo(t *testing.T) string {
	t.Helper()
	repo := os.Getenv("SWITCHTENDER_LOOMSEAL_REPO")
	if repo == "" {
		repo = filepath.Join(moduleRoot(t), "..", "loomseal")
	}
	if _, err := os.Stat(filepath.Join(repo, "go.mod")); err != nil {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and no loomseal checkout was found, so " +
				"the disclosed outcome record goes unchecked against the reference verifier; set " +
				"SWITCHTENDER_LOOMSEAL_REPO")
		}
		t.Skip("loomseal checkout not found beside this repository; set SWITCHTENDER_LOOMSEAL_REPO " +
			"to point at one")
	}
	return repo
}

// moduleRoot walks up from the working directory to the directory holding this module's go.mod.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		raw, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil && bytes.HasPrefix(raw, []byte("module "+modulePath+"\n")) {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatalf("no go.mod for %s above the working directory", modulePath)
		}
		dir = parent
	}
}

// ReceiptKey derives the binding key an install signing with id seals its rows under, the way the
// command and the server derive it, so the dispatchers in a test seal what a receipt is held to.
func ReceiptKey(t *testing.T, id audit.Identity) *binding.Key {
	t.Helper()
	seed, err := hex.DecodeString(id.Seed)
	if err != nil {
		t.Fatalf("decode the identity seed: %v", err)
	}
	key, err := binding.DeriveKey(seed)
	if err != nil {
		t.Fatalf("DeriveKey() error = %v", err)
	}
	return key
}

// KeyedReceipt returns opts carrying the verifier a keyed install holds a receipt's approvers to,
// over runs and audits under the key id derives.
func KeyedReceipt(t *testing.T, runs run.Store, audits audit.Store, id audit.Identity,
	opts receipt.Options) receipt.Options {
	t.Helper()
	opts.Binder = dispatch.NewBindingVerifier(runs, audits, ReceiptKey(t, id))
	return opts
}

// ApproveSealed records an approval of the stored held run runID by by, settles the run into
// settle with that decision, stamps what was approved, and seals it the way the dispatcher seals an
// approved run. It returns the run as stored.
func ApproveSealed(t *testing.T, runs run.Store, audits audit.Store, id audit.Identity, runID string,
	by outcome.Decider, settle run.DecisionSettle) *run.Run {
	t.Helper()
	ctx := context.Background()
	r, err := runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	decisionID := audit.NewID()
	digest, err := outcome.CommitDecisionWith(ctx, audits, r, outcome.StepApproved, by, time.Now,
		outcome.DecisionExtras{ID: decisionID})
	if err != nil {
		t.Fatalf("CommitDecisionWith() error = %v", err)
	}
	if ok, err := runs.ClaimDecision(ctx, runID, decisionID, "claim"); err != nil || !ok {
		t.Fatalf("ClaimDecision() = %v, %v", ok, err)
	}
	if ok, err := runs.SettleDecision(ctx, runID, decisionID, settle); err != nil || !ok {
		t.Fatalf("SettleDecision() = %v, %v", ok, err)
	}
	if r, err = runs.Get(ctx, runID); err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	r.ApprovedSpecDigest = digest
	if r.ApprovedSpecBinding, err = outcome.SpecBinding(r); err != nil {
		t.Fatalf("SpecBinding() error = %v", err)
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return SealAsKeyed(t, runs, audits, id, runID)
}

// SealAsKeyed seals the stored row runID as a keyed process seals it, under the key id derives: a
// top-level run as submitted, or to the DECISION entry it names, and an approval step to the
// workflow state it was decided on and its DECISION entry. It returns the row as stored.
func SealAsKeyed(t *testing.T, runs run.Store, audits audit.Store, id audit.Identity,
	runID string) *run.Run {
	t.Helper()
	ctx := context.Background()
	r, err := runs.Get(ctx, runID)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", runID, err)
	}
	root := func() binding.Root {
		entry, eerr := audits.Entry(ctx, r.DecisionID)
		if eerr != nil {
			t.Fatalf("Entry(%s) error = %v", r.DecisionID, eerr)
		}
		return binding.RootOf(entry)
	}
	var rec binding.Record
	switch {
	case r.ParentID == nil && r.DecisionID == "":
		rec, err = binding.SubmitRecord(r)
	case r.ParentID == nil:
		rec, err = binding.RunRecord(r, root())
	default:
		parent, perr := runs.Get(ctx, *r.ParentID)
		if perr != nil {
			t.Fatalf("Get(%s) error = %v", *r.ParentID, perr)
		}
		state, serr := outcome.StepStateOf(ctx, runs, parent, r)
		if serr != nil {
			t.Fatalf("StepStateOf() error = %v", serr)
		}
		stateBinding, berr := state.Binding()
		if berr != nil {
			t.Fatalf("Binding() error = %v", berr)
		}
		rec, err = binding.GateRecord(parent, r, stateBinding, root())
	}
	if err != nil {
		t.Fatalf("record of %s: %v", runID, err)
	}
	r.SpecSeal = ReceiptKey(t, id).Seal(rec)
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save(%s) error = %v", runID, err)
	}
	return r
}
