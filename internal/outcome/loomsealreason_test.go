package outcome_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// TestAReceiptDisclosingAFailureReasonVerifiesInLoomSeal is the cross-verification the failure
// reason has to survive. The record is not this product's private structure: it is disclosed inside
// a signed bundle a relying party checks with the format's own tool, so a field added to it is a
// change to what that tool is handed.
//
// The receipt built here is for the run the field exists for, failed at exit code 0 with the reason
// naming what went wrong, and it is verified by shelling out to the loomseal command rather than by
// importing a verifier, for the same reason the cross-check in internal/audit does: a verifier
// vendored into this repository would drift toward whatever this repository emits.
//
// The disclosure is asserted before the verdict. A receipt whose outcome could not be rebuilt
// withholds the body and says so in a note, and it would still verify, so checking only the verdict
// would pass over the case where the reason never reached the bundle at all.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with,
// and t.Setenv and t.Parallel cannot both apply to one test.
func TestAReceiptDisclosingAFailureReasonVerifiesInLoomSeal(t *testing.T) {
	repo := loomsealRepo(t)
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}

	// The request that created the run, and the receipt tying the run to it. Without both, a receipt
	// cannot place the run's start on the chain and never gets as far as the outcome.
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "casey", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}

	ended := time.Now()
	exit := 0
	r := &run.Run{
		ID: "run_zero_host", Status: run.StatusFailed, CreatedAt: time.Now(), EndedAt: &ended,
		Tool: run.ToolAnsible, Playbook: "site.yml", Inventory: "inv",
		Actor: "casey", ActorType: "session", ExitCode: &exit, Error: zeroHostReason,
		AuditReceipt: fmt.Sprintf("%d:%s", creation.Seq, creation.Hash),
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if err := runs.AppendLog(ctx, r.ID, []byte("PLAY RECAP ****\n")); err != nil {
		t.Fatalf("AppendLog() error = %v", err)
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
	if !strings.Contains(string(res.Signed), "no host was touched") {
		t.Fatal("the signed receipt does not disclose the reason the run failed, so verifying it " +
			"would prove nothing about the reason")
	}

	report := verifyWithLoomSeal(t, repo, res.Signed)
	if !report.OK {
		t.Fatalf("loomseal refused a receipt disclosing a run's failure reason: %v", report.Problems)
	}
}

// loomsealReport is the part of the format verifier's report this test asserts on.
type loomsealReport struct {
	// OK reports whether every check passed.
	OK bool `json:"ok"`
	// Problems lists why it did not.
	Problems []string `json:"problems"`
}

// verifyWithLoomSeal runs the format's own verifier over a signed bundle and returns its report.
func verifyWithLoomSeal(t *testing.T, repo string, signed []byte) loomsealReport {
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
	var rep loomsealReport
	if err := json.Unmarshal(out, &rep); err != nil {
		t.Fatalf("loomseal report is not JSON: %v\n%s", err, out)
	}
	return rep
}

// loomsealRepo locates the pinned loomseal checkout the cross-check runs the verifier from,
// skipping when there is none so the suite still runs for someone who cloned only this repository.
//
// It reads the one variable internal/audit's cross-check reads, which is also where the contract
// that the checkout sit at the exact version go.mod names is enforced. That check fails loudly
// rather than skipping, so a mismatched checkout is caught by the run that owns the pin instead of
// being restated here.
func loomsealRepo(t *testing.T) string {
	t.Helper()
	repo := os.Getenv("SWITCHTENDER_LOOMSEAL_REPO")
	if repo == "" {
		wd, err := os.Getwd()
		if err != nil {
			t.Fatalf("getwd: %v", err)
		}
		repo = filepath.Join(wd, "..", "..", "..", "loomseal")
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
