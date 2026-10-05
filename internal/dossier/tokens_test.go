package dossier

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDossierNamesTheKeyThatSignedEachToken pins that the evidence for a federated run lists each
// identity token minted for it and for its steps, with the id of the key that signed it, and
// nothing about another run's token recorded beside it.
func TestDossierNamesTheKeyThatSignedEachToken(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	runs := run.NewMemStore()
	audits := audit.NewMemStore()
	r := &run.Run{ID: "run_fed", Tool: run.ToolBash, Command: "deploy", Status: run.StatusSucceeded,
		CreatedAt: time.Now()}
	if err := runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	expires := time.Date(2026, 10, 1, 9, 15, 0, 0, time.UTC)
	for _, ti := range []outcome.TokenIssuance{
		{RunID: "run_fed", CredentialID: "cred_aws", KeyID: "kid_own", TokenID: "jti_own"},
		{RunID: "run_other", CredentialID: "cred_aws", KeyID: "kid_other", TokenID: "jti_other"},
		{RunID: "run_fed_step", ParentRunID: "run_fed", CredentialID: "cred_gcp", KeyID: "kid_step",
			TokenID: "jti_step"},
	} {
		ti.IssuedAt, ti.ExpiresAt = expires.Add(-15*time.Minute), expires
		if err := outcome.CommitTokenIssuance(ctx, audits, ti, "system:federation"); err != nil {
			t.Fatalf("CommitTokenIssuance() error = %v", err)
		}
	}
	in, err := Collect(ctx, runs, audits, audit.Identity{}, "run_fed", time.Now())
	if err != nil {
		t.Fatalf("Collect() error = %v", err)
	}
	doc, err := Render(in)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	tests := []struct {
		Text        string
		WantPresent bool
	}{{ // Test 0: The section is there.
		Text: "Identity tokens issued", WantPresent: true,
	}, { // Test 1: The run's own token names its key.
		Text: ">kid_own<", WantPresent: true,
	}, { // Test 2: A step's token names its key.
		Text: ">kid_step<", WantPresent: true,
	}, { // Test 3: The expiry is shown.
		Text: "2026-10-01T09:15:00Z", WantPresent: true,
	}, { // Test 4: Another run's token is not this run's evidence.
		Text: "kid_other", WantPresent: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := strings.Contains(string(doc), test.Text); got != test.WantPresent {
				t.Errorf("the dossier holds %q: %v, want %v", test.Text, got, test.WantPresent)
			}
		})
	}
}
