package dossier

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/run"
)

// TestDossierShowsADecisionMadeFromAComment proves the dossier says when a decision came from a
// pull request comment, naming the comment and its author by the forge's numeric ids and the body
// by its SHA-256, and says nothing of the kind for a decision made in the queue.
func TestDossierShowsADecisionMadeFromAComment(t *testing.T) {
	t.Parallel()
	sum := strings.Repeat("ab", 32)
	tests := []struct {
		Comment  *decision.Comment
		WantText []string
		WantNot  string
	}{{ // Test 0: A GitHub comment is named with its ids and fingerprint.
		Comment: &decision.Comment{Forge: "github", APIURL: "https://api.github.com",
			Repository: "acme/infra", PullRequest: 7, CommentID: 901, AuthorID: 1001,
			BodySHA256: sum},
		WantText: []string{"Decided from a pull request comment", "acme/infra #7",
			"comment 901 by github account 1001", "body SHA-256 " + sum},
	}, { // Test 1: A GitLab note is named as a merge request comment.
		Comment: &decision.Comment{Forge: "gitlab", APIURL: "https://gitlab.com/api/v4",
			Repository: "infra/network", PullRequest: 12, CommentID: 902, AuthorID: 1002,
			BodySHA256: sum},
		WantText: []string{"Decided from a merge request comment", "infra/network #12",
			"comment 902 by gitlab account 1002"},
	}, { // Test 2: A decision made in the queue says nothing about a comment.
		WantNot: "Decided from a",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			at := time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
			doc, err := Render(&Input{
				Run: &run.Run{ID: "run_comment1", Playbook: "site.yml", Inventory: "hosts.ini",
					Status: run.StatusSucceeded, CreatedAt: at},
				Decisions: []*decision.Record{{ID: "aud_1", Kind: decision.KindDecision,
					DecisionID: "aud_1", RunID: "run_comment1", Verdict: "approved", At: at,
					Actor: "approver-one", ActorType: "forge_comment", Comment: test.Comment}},
			})
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			for _, want := range test.WantText {
				if !strings.Contains(string(doc), want) {
					t.Errorf("dossier lacks %q", want)
				}
			}
			if test.WantNot != "" && strings.Contains(string(doc), test.WantNot) {
				t.Errorf("dossier says %q for a decision made in the queue", test.WantNot)
			}
		})
	}
}
