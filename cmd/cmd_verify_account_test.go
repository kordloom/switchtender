package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// receiptForDecision seeds a finished run whose approval is on the chain under the given decider,
// committed the way an approval commits it, and returns the path of the receipt written for it.
func receiptForDecision(t *testing.T, actor, actorType, onBehalfOf string) string {
	t.Helper()
	return receiptForDecisionUnder(t, actor, actorType, onBehalfOf, false)
}

// receiptForDecisionUnder is receiptForDecision with the approval committed under the legacy
// unkeyed digest form when unkeyed is set, the way an entry recorded before nonces was.
func receiptForDecisionUnder(t *testing.T, actor, actorType, onBehalfOf string, unkeyed bool) string {
	t.Helper()
	ctx := context.Background()
	dir := t.TempDir()
	db := filepath.Join(dir, "state.db")
	store, err := openBundle(db)
	if err != nil {
		t.Fatalf("openBundle() error = %v", err)
	}
	at := time.Date(2026, 8, 11, 12, 0, 0, 0, time.UTC)
	creation := &audit.Entry{
		ID: audit.NewID(), At: at, Actor: "casey", ActorType: "session", OnBehalfOf: "casey",
		Method: "POST", Path: "/v1/runs",
	}
	if err := store.Audits().Append(ctx, creation); err != nil {
		t.Fatalf("Append(creation) error = %v", err)
	}
	r := &run.Run{
		ID: "run_decided", Playbook: "site.yml", Inventory: "prod", Status: run.StatusRunning,
		Actor: "casey", AuditReceipt: audit.Receipt(creation), CreatedAt: at,
	}
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(running) error = %v", err)
	}
	body, _, err := outcome.DecisionBody(r, "approved")
	if err != nil {
		t.Fatalf("DecisionBody() error = %v", err)
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		t.Fatalf("ContentDigestOf() error = %v", err)
	}
	if unkeyed {
		digest, nonce = audit.UnkeyedDigestOf(body), ""
	}
	if err := store.Audits().Append(ctx, &audit.Entry{
		ID: audit.NewID(), At: at.Add(time.Minute), Actor: actor, ActorType: actorType,
		OnBehalfOf: onBehalfOf, Method: audit.MethodDecision, Path: "/runs/run_decided/decision/approved",
		ContentDigest: digest, Nonce: nonce,
	}); err != nil {
		t.Fatalf("Append(decision) error = %v", err)
	}
	r.Status = run.StatusSucceeded
	if err := store.Runs().Save(ctx, r); err != nil {
		t.Fatalf("Save(succeeded) error = %v", err)
	}
	if err := outcome.Commit(ctx, store.Audits(), store.Runs(), r, "system:dispatcher",
		nil); err != nil {
		t.Fatalf("outcome.Commit() error = %v", err)
	}
	_ = store.Close()

	path := filepath.Join(dir, "run.receipt")
	receiptRunDB, receiptRunOut, receiptSparse = db, path, false
	t.Cleanup(func() { receiptRunDB, receiptRunOut = defaultDBPath, "" })
	if err := runReceipt(testCommand(), []string{"run_decided"}); err != nil {
		t.Fatalf("runReceipt() error = %v", err)
	}
	return path
}

// TestVerifyShowsTheAccountBehindADecision pins the decision line of a verified receipt.
//
// A decision made with a token bound to an account now carries that account, the same one the
// request entry beside it carries, and the receipt discloses it. The line that says who decided
// named only the token's label, so two tokens sharing a label on different accounts read as the
// same approver in the one artifact handed to somebody outside. The account is left off where it
// would only repeat the name, as it does for a person's browser session, and the caller class an
// install serving open records is not printed twice.
func TestVerifyShowsTheAccountBehindADecision(t *testing.T) {
	tests := []struct {
		// Name labels the case.
		Name string
		// Actor, ActorType, and OnBehalfOf are the decider as the chain recorded it.
		Actor, ActorType, OnBehalfOf string
		// WantLine is the decision line verify prints, up to the spec digest.
		WantLine string
		// DenyText must not appear in the output.
		DenyText string
	}{{ // Test 0: A token bound to an account names the account.
		Name: "bound token", Actor: "laptop", ActorType: "token", OnBehalfOf: "alice",
		WantLine: "  approved by laptop (token) on behalf of alice, binding spec ",
	}, { // Test 1: A browser session is named for its own account, which is said once.
		Name: "browser session", Actor: "casey-admin", ActorType: "session", OnBehalfOf: "casey-admin",
		WantLine: "  approved by casey-admin (session), binding spec ",
		DenyText: "on behalf of",
	}, { // Test 2: An install serving open records the caller class, which is said once.
		Name: "open install", Actor: "unauthenticated", ActorType: "unauthenticated",
		WantLine: "  approved by unauthenticated, binding spec ",
		DenyText: "(unauthenticated)",
	}, { // Test 3: A decision recorded before decisions carried the account reads as it always did.
		Name: "before the account", Actor: "pat", ActorType: "session",
		WantLine: "  approved by pat (session), binding spec ",
		DenyText: "on behalf of",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			path := receiptForDecision(t, test.Actor, test.ActorType, test.OnBehalfOf)

			// The receipt itself discloses the account on the decision it carries.
			signed, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("ReadFile(receipt) error = %v", err)
			}
			var doc audit.Bundle
			if err := json.Unmarshal(signed, &doc); err != nil {
				t.Fatalf("Unmarshal(receipt) error = %v", err)
			}
			disclosed := false
			for _, c := range doc.Claims {
				if c.Payload["method"] != audit.MethodDecision {
					continue
				}
				disclosed = c.Payload["decision_body"] != nil
				if got, _ := c.Payload["on_behalf_of"].(string); got != test.OnBehalfOf {
					t.Errorf("the receipt's decision claim is on behalf of %q, want %q", got,
						test.OnBehalfOf)
				}
			}
			if !disclosed {
				t.Fatal("the receipt does not disclose the decision, so this case checks nothing")
			}

			verifyPubkey = ""
			var buf bytes.Buffer
			c := testCommand()
			c.SetOut(&buf)
			if err := runVerify(c, []string{path}); err != nil {
				t.Fatalf("runVerify() error = %v\n%s", err, buf.String())
			}
			got := buf.String()
			if !strings.Contains(got, test.WantLine) {
				t.Errorf("verify output is missing %q:\n%s", test.WantLine, got)
			}
			if test.DenyText != "" && strings.Contains(got, test.DenyText) {
				t.Errorf("verify output says %q:\n%s", test.DenyText, got)
			}
		})
	}
}
