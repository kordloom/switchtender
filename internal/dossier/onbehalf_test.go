package dossier

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDocumentsNameTheAccountBehindADecision pins who the dossier and the change register say
// decided a run.
//
// A decision made with a token bound to an account carries that account, and both documents named
// only the token's label, so two tokens sharing a label on different accounts read as one approver
// in the documents an auditor samples. The account is left off where it would only repeat the name,
// as it does for a person's browser session, and a decision written before decisions carried it
// reads as it always did.
//
//nolint:funlen // Test function.
func TestDocumentsNameTheAccountBehindADecision(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the case.
		Name string
		// Actor, ActorType, and OnBehalfOf are the decider as the chain recorded it.
		Actor, ActorType, OnBehalfOf string
		// WantCell is the dossier's actor cell on the decision row.
		WantCell string
		// WantDecision is the register's decision cell.
		WantDecision string
	}{{ // Test 0: A token bound to an account names the account.
		Name: "bound token", Actor: "laptop", ActorType: "token", OnBehalfOf: "alice",
		WantCell: "laptop on behalf of alice", WantDecision: "Approved by laptop on behalf of alice",
	}, { // Test 1: A browser session is named for its own account, which is said once.
		Name: "browser session", Actor: "casey", ActorType: "session", OnBehalfOf: "casey",
		WantCell: "casey", WantDecision: "Approved by casey",
	}, { // Test 2: An install serving open records the caller class its request carries.
		Name: "open install", Actor: "unauthenticated", ActorType: "unauthenticated",
		WantCell: "unauthenticated", WantDecision: "Approved by unauthenticated",
	}, { // Test 3: A decision recorded before decisions carried the account.
		Name: "before the account", Actor: "pat", ActorType: "session",
		WantCell: "pat", WantDecision: "Approved by pat",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			runs := run.NewMemStore()
			audits := audit.NewMemStore()
			id := "run_decided1"
			at := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)
			if err := runs.Save(ctx, &run.Run{
				ID: id, Playbook: "site.yml", Inventory: "hosts.ini", Status: run.StatusSucceeded,
				CreatedAt: at, Actor: "deploy-bot",
			}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			if err := audits.Append(ctx, &audit.Entry{
				ID: audit.NewID(), At: at.Add(time.Minute), Actor: test.Actor,
				ActorType: test.ActorType, OnBehalfOf: test.OnBehalfOf,
				Method: audit.MethodDecision, Path: "/runs/" + id + "/decision/approved",
			}); err != nil {
				t.Fatalf("Append() error = %v", err)
			}

			in, err := Collect(ctx, runs, audits, audit.Identity{}, id, at.Add(time.Hour))
			if err != nil {
				t.Fatalf("Collect() error = %v", err)
			}
			doc, err := Render(in)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			wantRow := "<tr><td>Approved</td><td>" + test.WantCell + "</td>"
			if !strings.Contains(string(doc), wantRow) {
				t.Errorf("dossier is missing the decision row %q:\n%s", wantRow, doc)
			}

			reg, err := CollectRegister(ctx, runs, audits, audit.Identity{}, at.Add(-time.Hour),
				at.Add(time.Hour), at.Add(time.Hour), 0)
			if err != nil {
				t.Fatalf("CollectRegister() error = %v", err)
			}
			page, err := RenderRegister(reg)
			if err != nil {
				t.Fatalf("RenderRegister() error = %v", err)
			}
			wantCell := "<td>" + test.WantDecision + " <span"
			if !strings.Contains(string(page), wantCell) {
				t.Errorf("register is missing the decision cell %q:\n%s", wantCell, page)
			}
		})
	}
}
