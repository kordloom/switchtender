package receipt_test

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/run"
)

// federatedRun builds a finished run whose execution minted two identity tokens, its own and a
// step's, with an unrelated run's token recorded between them, and returns the stores and the run.
func federatedRun(t *testing.T) (run.Store, audit.Store, audit.Identity, *run.Run) {
	t.Helper()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity: %v", err)
	}
	creation := &audit.Entry{
		ID: audit.NewID(), At: time.Now(), Actor: "operator-one", ActorType: "session",
		Method: "POST", Path: "/v1/runs",
	}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("append creation: %v", err)
	}
	r := &run.Run{
		ID: "run_fed", Status: run.StatusRunning, CreatedAt: time.Now(), Tool: run.ToolBash,
		Command: "deploy", Actor: "operator-one", ActorType: "session",
		AuditReceipt: chainReceipt(creation),
	}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save run: %v", err)
	}
	for _, ti := range []outcome.TokenIssuance{
		{RunID: "run_fed", CredentialID: "cred_aws", KeyID: "kid_own", TokenID: "jti_own"},
		{RunID: "run_other", CredentialID: "cred_aws", KeyID: "kid_other", TokenID: "jti_other"},
		{RunID: "run_fed_step", ParentRunID: "run_fed", CredentialID: "cred_gcp", KeyID: "kid_step",
			TokenID: "jti_step"},
	} {
		ti.IssuedAt, ti.ExpiresAt, ti.OnBehalfOf = time.Now(), time.Now().Add(time.Hour), "operator-one"
		if err := outcome.CommitTokenIssuance(ctx, audits, ti, "system:federation"); err != nil {
			t.Fatalf("CommitTokenIssuance(%s) error = %v", ti.RunID, err)
		}
	}
	ended := time.Now()
	code := 0
	r.Status, r.ExitCode, r.EndedAt = run.StatusSucceeded, &code, &ended
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("save terminal run: %v", err)
	}
	if err := outcome.Commit(ctx, audits, runs, r, "system:test", nil); err != nil {
		t.Fatalf("Commit outcome: %v", err)
	}
	return runs, audits, id, r
}

// disclosedKeys returns the key ids of the token issuances a receipt discloses, by run.
func disclosedKeys(t *testing.T, signed []byte) map[string]string {
	t.Helper()
	var doc struct {
		// Claims are the disclosed entries.
		Claims []struct {
			// Payload is the entry.
			Payload map[string]any `json:"payload"`
		} `json:"claims"`
	}
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatalf("decode receipt: %v", err)
	}
	out := map[string]string{}
	for _, c := range doc.Claims {
		method, _ := c.Payload["method"].(string)
		path, _ := c.Payload["path"].(string)
		if method != audit.MethodToken {
			continue
		}
		ti, ok := outcome.ParseTokenPath(path)
		if !ok {
			t.Fatalf("a disclosed token entry's path %q does not read as an issuance", path)
		}
		out[ti.RunID] = ti.KeyID
	}
	return out
}

// TestReceiptNamesTheKeysThatSignedTheRunsTokens pins that a run's receipt carries the id of every
// key that signed a token for it, in both shapes. The contiguous shape carries every entry between
// the run's creation and its outcome, another run's included. The sparse shape carries only the
// run's own, a step's included because its entry names the run as its parent, and nothing of the
// other run, which is the shape's whole promise. It runs serially, since the fixture sets the
// environment the producer identity reads.
func TestReceiptNamesTheKeysThatSignedTheRunsTokens(t *testing.T) {
	tests := []struct {
		Sparse   bool
		WantKeys map[string]string
	}{{ // Test 0: The contiguous segment.
		Sparse: false,
		WantKeys: map[string]string{"run_fed": "kid_own", "run_other": "kid_other",
			"run_fed_step": "kid_step"},
	}, { // Test 1: The sparse tree.
		Sparse:   true,
		WantKeys: map[string]string{"run_fed": "kid_own", "run_fed_step": "kid_step"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			runs, audits, id, r := federatedRun(t)
			res, err := receipt.Build(context.Background(), runs, audits, id, "test", r.ID,
				receipt.Options{Sparse: test.Sparse})
			if err != nil {
				t.Fatalf("Build() error = %v", err)
			}
			if diff := cmp.Diff(test.WantKeys, disclosedKeys(t, res.Signed)); diff != "" {
				t.Errorf("disclosed signing keys (-want +got):\n%s", diff)
			}
		})
	}
}
