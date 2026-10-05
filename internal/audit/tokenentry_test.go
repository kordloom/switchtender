package audit_test

import (
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/outcome"
)

// TestTokenEntryVerifiesInLoomSeal checks the entry a federated token issuance writes is one the
// format's own verifier accepts in both receipt shapes, the contiguous segment and the sparse tree,
// and that each discloses the id of the key that signed the run's token. The method is new to the
// chain, and a verifier that refused an unfamiliar one would make every federated run unprovable.
func TestTokenEntryVerifiesInLoomSeal(t *testing.T) {
	id := treeIdentity(t)
	at := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	const kid = "Yk1v6dS0fXv3r8Q2m2n0yQZ9Jtq6cZr2dJm4X8nQ0aA"
	path, err := outcome.TokenPath(outcome.TokenIssuance{
		RunID: "run_fed", CredentialID: "cred_aws", KeyID: kid,
		TokenID: "9f86d081884c7d659a2feaa0c55ad015", ExpiresAt: at.Add(15 * time.Minute),
	})
	if err != nil {
		t.Fatalf("TokenPath() error = %v", err)
	}
	var chain []*audit.Entry
	var prev *audit.Entry
	for i, e := range []*audit.Entry{
		{Actor: "deploy-session", ActorType: "session", Method: "POST", Path: "/v1/runs"},
		{Actor: "system:federation", ActorType: "system", OnBehalfOf: "deploy-session",
			Method: audit.MethodToken, Path: path},
		{Actor: "system:dispatcher", ActorType: "system", OnBehalfOf: "deploy-session",
			Method: audit.MethodRun, Path: "/runs/run_fed/outcome/succeeded"},
	} {
		e.ID = audit.NewID()
		e.At = at.Add(time.Duration(i) * time.Minute)
		audit.Link(prev, e)
		chain = append(chain, e)
		prev = e
	}
	linear, err := audit.BuildBundle(chain, id, "v-test", time.Now())
	if err != nil {
		t.Fatalf("BuildBundle() error = %v", err)
	}
	tree, err := audit.BuildTreeBundle(chain, map[int64]bool{2: true, 3: true}, id, "v-test",
		audit.BundleSubject{Type: "run", ID: "run_fed"}, time.Now())
	if err != nil {
		t.Fatalf("BuildTreeBundle() error = %v", err)
	}
	for name, doc := range map[string]*audit.Bundle{"contiguous": linear, "sparse": tree} {
		signed, err := audit.SignBundleDoc(doc, id.Private())
		if err != nil {
			t.Fatalf("%s: SignBundleDoc() error = %v", name, err)
		}
		if !strings.Contains(string(signed), kid) {
			t.Errorf("%s: the receipt does not disclose the key that signed the run's token", name)
		}
		if report := verifyWithLoomSeal(t, signed); !report.OK {
			t.Errorf("%s: loomseal refused a receipt carrying a token entry: %v", name,
				report.Problems)
		}
	}
}
