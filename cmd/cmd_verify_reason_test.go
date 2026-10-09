package cmd

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/receipt"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// writeReasonedReceipt runs an agent's change through an approval carrying a reason, applies change
// to the stores or the signed bundle, and writes the receipt. change may redact the reason or alter
// the disclosed text, re-signing as a producer editing its own export would.
func writeReasonedReceipt(t *testing.T, redact bool, alter func(*audit.Bundle)) string {
	t.Helper()
	ctx := context.Background()
	runs, audits, decisions := run.NewMemStore(), audit.NewMemStore(), decision.NewMemStore()
	t.Setenv("SWITCHTENDER_AUDIT_KEY", "")
	id, err := audit.LoadIdentity(t.TempDir())
	if err != nil {
		t.Fatalf("LoadIdentity() error = %v", err)
	}
	runner := roundhouse.RunnerFunc(
		func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
			return roundhouse.Result{ExitCode: 0}, nil
		},
	)
	d := dispatch.New(runs, runner, nil, dispatch.WithAudits(audits),
		dispatch.WithDecisions(decisions), dispatch.WithNoJanitor())
	t.Cleanup(d.Close)
	creation := &audit.Entry{ID: audit.NewID(), At: time.Now(), Actor: "deploy-bot",
		ActorType: "agent", OnBehalfOf: "dev-lead", Method: "POST", Path: "/v1/runs"}
	if err := audits.Append(ctx, creation); err != nil {
		t.Fatalf("Append() error = %v", err)
	}
	held, err := d.Submit(run.WithAuditReceipt(ctx, fmt.Sprintf("%d:%s", creation.Seq, creation.Hash)),
		"", "", run.WithTool(run.ToolBash), run.WithCommand("deploy"), run.WithActor("deploy-bot"),
		run.WithActorType("agent"), run.WithActorAccount("user_dev"), run.WithRequireApproval(true),
		run.WithInitiator(&run.Initiator{InitiatedBy: "deploy-bot", BoundTo: "dev-lead",
			ProvisionedBy: "org-admin", ProvisionedByType: "session"}))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	if _, err := d.DecideRun(ctx, held.ID, dispatch.RunDecision{Approve: true,
		Reason: "approved after the change review", By: outcome.Decider{Name: "ops-admin",
			Type: "session", OnBehalfOf: "ops-admin", AccountID: "user_ops"}}); err != nil {
		t.Fatalf("DecideRun() error = %v", err)
	}
	// The status turns terminal before the outcome is appended, so the wait is for the outcome entry
	// itself: a receipt built on the status alone can find no outcome to end at.
	for deadline := time.Now().Add(10 * time.Second); ; {
		committed := false
		if err := audits.ChainScan(ctx, 0, func(e *audit.Entry) error {
			if e.Method == audit.MethodRun &&
				strings.HasPrefix(e.Path, "/runs/"+held.ID+"/outcome/") {
				committed = true
			}
			return nil
		}); err != nil {
			t.Fatalf("ChainScan() error = %v", err)
		}
		if committed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never committed its outcome", held.ID)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if redact {
		records, rerr := decisions.ForRun(ctx, held.ID)
		if rerr != nil || len(records) != 1 {
			t.Fatalf("ForRun() = %d, %v", len(records), rerr)
		}
		if _, rerr := d.RedactReason(ctx, held.ID, records[0].ID, decision.CategoryPersonalData,
			outcome.Decider{Name: "privacy-admin", Type: "session"}); rerr != nil {
			t.Fatalf("RedactReason() error = %v", rerr)
		}
	}
	res, err := receipt.Build(ctx, runs, audits, id, "v-test", held.ID,
		receipt.Options{Decisions: decisions})
	if err != nil {
		t.Fatalf("receipt.Build() error = %v", err)
	}
	signed := res.Signed
	if alter != nil {
		var b audit.Bundle
		if err := json.Unmarshal(signed, &b); err != nil {
			t.Fatalf("parse receipt: %v", err)
		}
		alter(&b)
		b.Signatures = []any{}
		if signed, err = audit.SignBundleDoc(&b, id.Private()); err != nil {
			t.Fatalf("SignBundleDoc() error = %v", err)
		}
	}
	path := filepath.Join(t.TempDir(), "run.receipt")
	if err := os.WriteFile(path, signed, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return path
}

// TestVerifyShowsTheReasonAndTheAgentBehindARun pins what the offline verifier prints for an
// agent's run approved with a reason: who asked, the account it acted under, who provisioned it,
// the reason it checked against the commitment, and how separation of duties applied. A reason that
// does not open its commitment fails the receipt and is not printed, and a redacted one says so.
func TestVerifyShowsTheReasonAndTheAgentBehindARun(t *testing.T) {
	tests := []struct {
		// Name says what the case proves.
		Name string
		// Redact redacts the reason before the receipt is built.
		Redact bool
		// Alter changes the disclosed reason and re-signs.
		Alter bool
		// Unknown adds a member this product never discloses and re-signs.
		Unknown bool
		// WantLines must all appear in the output.
		WantLines []string
		// DenyLines must not.
		DenyLines []string
		// WantErr must appear in the error, empty when the receipt verifies.
		WantErr string
	}{{ // Test 0: The honest receipt.
		Name: "honest",
		WantLines: []string{"initiated by   deploy-bot (agent)", "bound to       dev-lead",
			"provisioned by org-admin (session)",
			`reason   "approved after the change review" (opens the commitment the chain holds)`,
			"separation of duties not required: requester dev-lead (the agent's bound account), " +
				"decided by ops-admin, an independent account",
			"disclosed    ", ", 0 unchecked, 0 redacted",
			"INTACT, BUT UNIDENTIFIED: nothing has been altered"},
		DenyLines: []string{"unchecked  claim"},
	}, { // Test 1: A redacted reason is named as redacted, and the receipt still verifies.
		Name: "redacted", Redact: true,
		WantLines: []string{"reason   redacted (personal_data), its commitment stays on the chain"},
		DenyLines: []string{"approved after the change review"},
	}, { // Test 2: A changed reason fails the receipt and is never shown as the record.
		Name: "altered", Alter: true,
		DenyLines: []string{"approved by the change board", "opens the commitment"},
		WantErr:   "a disclosed decision is not what the chain committed",
	}, { // Test 3: A member nothing checks qualifies the verdict, which names it.
		Name: "unknown", Unknown: true,
		WantLines: []string{"INTACT, BUT UNIDENTIFIED, 1 disclosed record unchecked: nothing it " +
			"checked has been altered",
			"  unchecked  claim 0 note: this product does not disclose \"note\""},
		DenyLines: []string{"INTACT, BUT UNIDENTIFIED: nothing has been altered"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			var alter func(*audit.Bundle)
			if test.Unknown {
				alter = func(b *audit.Bundle) { b.Claims[0].Payload["note"] = "carried beside the link" }
			}
			if test.Alter {
				alter = func(b *audit.Bundle) {
					for i := range b.Claims {
						if _, ok := b.Claims[i].Payload["reason_text"]; ok {
							b.Claims[i].Payload["reason_text"] = "approved by the change board"
						}
					}
				}
			}
			path := writeReasonedReceipt(t, test.Redact, alter)
			verifyPubkey = ""
			var buf bytes.Buffer
			c := testCommand()
			c.SetOut(&buf)
			err := runVerify(c, []string{path})
			got := buf.String()
			switch {
			case test.WantErr == "" && err != nil:
				t.Fatalf("%s: runVerify() error = %v\n%s", test.Name, err, got)
			case test.WantErr != "" && (err == nil || !strings.Contains(err.Error(), test.WantErr)):
				t.Fatalf("%s: runVerify() error = %v, want %q\n%s", test.Name, err, test.WantErr, got)
			}
			for _, want := range test.WantLines {
				if !strings.Contains(got, want) {
					t.Errorf("%s: output is missing %q:\n%s", test.Name, want, got)
				}
			}
			for _, deny := range test.DenyLines {
				if strings.Contains(got, deny) {
					t.Errorf("%s: output says %q:\n%s", test.Name, deny, got)
				}
			}
		})
	}
}
