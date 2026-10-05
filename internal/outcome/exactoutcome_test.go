package outcome_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// exactSecret is a password a run's failure text carries, which no committed or disclosed outcome
// may hold.
const exactSecret = "hunter2-outcome-secret"

// TestCommittedIsTheRedactedRecordOrItsSummary pins the bytes an outcome entry commits under the
// exact form, which are also the bytes a receipt discloses: the record redacted and canonicalized
// once, whole up to MaxDisclosedBytes, and its summary above that, naming the full record's size
// and SHA-256 and keeping what ran, how it ended, and what was approved.
func TestCommittedIsTheRedactedRecordOrItsSummary(t *testing.T) {
	t.Parallel()
	spec := "sha256:" + strings.Repeat("ab", 32)
	record := func(pad int) string {
		return fmt.Sprintf(`{"run_id":"run_c","status":"succeeded","exit_code":0,`+
			`"spec_digest":%q,"pad":%q}`, spec, strings.Repeat("x", pad))
	}
	fixed := len(reduce(t, record(0)))
	tests := []struct {
		Body        string
		WantSummary bool
	}{{ // Test 0: A record carrying a password assignment is committed redacted.
		Body: `{"run_id":"run_c","status":"failed","error":"login failed password=` +
			exactSecret + `"}`,
	}, { // Test 1: A record exactly at the limit is committed whole.
		Body: record(outcome.MaxDisclosedBytes - fixed),
	}, { // Test 2: A record one byte over the limit is committed as its summary.
		Body: record(outcome.MaxDisclosedBytes - fixed + 1), WantSummary: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			reduced := reduce(t, test.Body)
			committed, err := outcome.Committed([]byte(test.Body))
			if err != nil {
				t.Fatalf("Committed() error = %v", err)
			}
			if strings.Contains(string(committed), exactSecret) {
				t.Errorf("the committed outcome holds the password: %s", committed)
			}
			if !test.WantSummary {
				if !bytes.Equal(committed, reduced) {
					t.Errorf("Committed() = %d bytes, want the %d byte redacted record", len(committed),
						len(reduced))
				}
				return
			}
			rec, err := outcome.Parse(committed)
			if err != nil {
				t.Fatalf("the summary is not an outcome record: %v", err)
			}
			sum := sha256.Sum256(reduced)
			want := &outcome.Oversize{Limit: outcome.MaxDisclosedBytes, Size: len(reduced),
				SHA256: hex.EncodeToString(sum[:])}
			if rec.Oversize == nil || *rec.Oversize != *want {
				t.Errorf("summary oversize = %+v, want %+v", rec.Oversize, want)
			}
			if rec.RunID != "run_c" || rec.Status != "succeeded" || rec.SpecDigest != spec ||
				rec.ExitCode == nil {
				t.Errorf("the summary lost what ran and what was approved: %s", committed)
			}
			if len(committed) > 1024 {
				t.Errorf("the summary is %d bytes, which is not a summary", len(committed))
			}
		})
	}
}

// reduce returns a body's canonical redacted bytes.
func reduce(t *testing.T, body string) []byte {
	t.Helper()
	reduced, err := audit.CanonicalRedacted([]byte(body))
	if err != nil {
		t.Fatalf("CanonicalRedacted() error = %v", err)
	}
	return reduced
}

// TestAnOutcomeIsCommittedAsTheBytesItsReceiptDiscloses pins the exact form end to end. The outcome
// entry commits the redacted record, a receipt discloses those same bytes, and both this product's
// verifier and the open LoomSeal verifier check them as carried, so no verifier needs this
// product's redaction rules. The password the run's failure text carried is in neither.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAnOutcomeIsCommittedAsTheBytesItsReceiptDiscloses(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := exactRun(t)
	if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	entry := outcomeEntryOf(t, audits, r.ID)
	if !strings.HasPrefix(entry.ContentDigest, audit.ExactDigestPrefix) {
		t.Fatalf("the outcome is committed as %q, not under the exact form", entry.ContentDigest)
	}
	body, err := outcome.Body(ctx, runs, r)
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	committed, err := outcome.Committed(body)
	if err != nil {
		t.Fatalf("Committed() error = %v", err)
	}
	if !audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce, committed) ||
		!outcome.VerifyBody(entry.ContentDigest, entry.Nonce, body) {
		t.Error("the committed bytes do not reproduce the outcome entry's digest")
	}
	if audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce, body) {
		t.Error("the record as assembled, before redaction, reproduces the exact digest")
	}

	res, err := receipt.Build(ctx, runs, audits, id, "v-test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	if strings.Contains(string(res.Signed), exactSecret) {
		t.Error("the receipt discloses the password the run's failure text carried")
	}
	if got := disclosedOutcomeOf(t, res.Signed, r.ID); got != string(committed) {
		t.Errorf("the receipt discloses %q, want the committed bytes %q", got, committed)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OutcomePresent || !rep.OutcomeDigestOK || !rep.OK() {
		t.Errorf("the receipt's outcome does not verify: %+v", rep)
	}
	if report := verifyWithLoomSeal(t, loomsealRepo(t), res.Signed); !report.OK {
		t.Fatalf("loomseal refused a receipt with an exact-form outcome: %v", report.Problems)
	}
}

// TestAnOutcomeUnderTheOlderFormStillVerifies pins that an outcome committed before the exact form
// keeps verifying: its entry committed the record as assembled under the keyed form, its receipt
// discloses the record as assembled, and this product's verifier reduces it as it always did.
//
// It does not run in parallel: it pins the audit key environment for the identity it signs with.
func TestAnOutcomeUnderTheOlderFormStillVerifies(t *testing.T) {
	ctx := context.Background()
	runs, audits, id, r := exactRun(t)
	body, err := outcome.Body(ctx, runs, r)
	if err != nil {
		t.Fatalf("Body() error = %v", err)
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	if err := audits.Append(ctx, &audit.Entry{ID: audit.NewID(), At: time.Now(),
		Actor: "system:test", ActorType: "system", OnBehalfOf: r.Actor, Method: audit.MethodRun,
		Path: "/runs/" + r.ID + "/outcome/" + string(r.Status), ContentDigest: digest,
		Nonce: nonce}); err != nil {
		t.Fatalf("Append(outcome) error = %v", err)
	}
	if !outcome.VerifyBody(digest, nonce, body) {
		t.Error("an older outcome entry no longer verifies against its rebuilt record")
	}
	res, err := receipt.Build(ctx, runs, audits, id, "v-test", r.ID, receipt.Options{})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	if got := disclosedOutcomeOf(t, res.Signed, r.ID); got != string(body) {
		t.Errorf("the receipt discloses %q, want the record as assembled", got)
	}
	rep, err := audit.VerifyBundle(res.Signed, id.KeyID())
	if err != nil {
		t.Fatalf("VerifyBundle() error = %v", err)
	}
	if !rep.OutcomeDigestOK || !rep.OK() {
		t.Errorf("a receipt with an older outcome no longer verifies: %+v", rep)
	}
}

// exactRun stores a failed run whose failure text carries a password, with the request that created
// it on the chain, and returns the stores and the identity a receipt signs with.
func exactRun(t *testing.T) (run.Store, audit.Store, audit.Identity, *run.Run) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	creation := &audit.Entry{ID: audit.NewID(), At: time.Now(), Actor: "deploy-bot",
		ActorType: "session", Method: "POST", Path: "/v1/runs"}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	ended := time.Now()
	exit := 2
	r := &run.Run{
		ID: "run_exact", Status: run.StatusFailed, CreatedAt: time.Now(), EndedAt: &ended,
		Tool: run.ToolBash, Command: "deploy", Actor: "deploy-bot", ActorType: "session",
		ExitCode: &exit, Error: "login failed password=" + exactSecret,
		AuditReceipt: fmt.Sprintf("%d:%s", creation.Seq, creation.Hash),
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return runs, audits, id, r
}

// outcomeEntryOf returns the outcome entry the chain holds for a run.
func outcomeEntryOf(t *testing.T, audits audit.Store, runID string) *audit.Entry {
	t.Helper()
	var found *audit.Entry
	err := audits.ChainScan(context.Background(), 0, func(e *audit.Entry) error {
		if e.Method == audit.MethodRun && strings.HasPrefix(e.Path, "/runs/"+runID+"/outcome/") {
			found = e
		}
		return nil
	})
	if err != nil || found == nil {
		t.Fatalf("no outcome entry for %s on the chain (err %v)", runID, err)
	}
	return found
}

// disclosedOutcomeOf returns the outcome text a signed receipt discloses for a run.
func disclosedOutcomeOf(t *testing.T, signed []byte, runID string) string {
	t.Helper()
	var doc audit.Bundle
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatalf("parse the receipt: %v", err)
	}
	for _, c := range doc.Claims {
		path, _ := c.Payload["path"].(string)
		if !strings.HasPrefix(path, "/runs/"+runID+"/outcome/") {
			continue
		}
		text, ok := c.Payload["outcome_body"].(string)
		if !ok {
			t.Fatalf("the receipt's outcome claim discloses no outcome text: %v", c.Payload)
		}
		return text
	}
	t.Fatalf("the receipt has no outcome claim for %s", runID)
	return ""
}
