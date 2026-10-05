package review

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/review/forgetest"
	"github.com/kordloom/switchtender/internal/trigger"
)

// testToken is the forge token the fakes accept.
const testToken = "glpat-review-token-0000"

// newFake starts the fake for provider and returns it with a review configuration pointed at it.
func newFake(t *testing.T, provider string) (*forgetest.Forge, *trigger.Review) {
	t.Helper()
	if provider == trigger.ProviderGitLab {
		f := forgetest.NewGitLab(t, testToken, "infra/network")
		return f, &trigger.Review{Provider: provider, APIURL: f.APIURL(), Repository: "infra/network",
			CredentialID: "cred_vcs"}
	}
	f := forgetest.NewGitHub(t, testToken, "acme/infra")
	return f, &trigger.Review{Provider: provider, APIURL: f.APIURL(), Repository: "acme/infra",
		CredentialID: "cred_vcs"}
}

// TestClientFindsOnlyItsOwnComment proves the comment a review updates is one the token's own
// account posted. A pull request author can paste the marker into a comment of their own, and a
// client that matched on the marker alone would then try to edit somebody else's comment forever.
func TestClientFindsOnlyItsOwnComment(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fake, cfg := newFake(t, provider)
			client, err := NewClient(cfg, testToken, fake.Client())
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			marker := MarkerPrefix("tpl_1")
			fake.Seed(7, "outsider", marker+"run= at=99999999999999999999 -->\nforged")

			got, err := client.FindComment(ctx, 7, marker)
			if err != nil {
				t.Fatalf("FindComment() error = %v", err)
			}
			if got != nil {
				t.Fatalf("FindComment() = %+v, want nil: the only marked comment is somebody else's", got)
			}
			id, err := client.CreateComment(ctx, 7, marker+"run=run_1 at=1 -->\nfirst")
			if err != nil {
				t.Fatalf("CreateComment() error = %v", err)
			}
			found, err := client.FindComment(ctx, 7, marker)
			if err != nil || found == nil {
				t.Fatalf("FindComment() = %v, %v, want the comment just created", found, err)
			}
			if diff := cmp.Diff(id, found.ID); diff != "" {
				t.Errorf("FindComment() id mismatch (-want +got):\n%s", diff)
			}
			edited := marker + "run=run_2 at=2 -->\nsecond"
			if err := client.UpdateComment(ctx, 7, found.ID, edited); err != nil {
				t.Fatalf("UpdateComment() error = %v", err)
			}
			comments := fake.Comments(7)
			if len(comments) != 2 || comments[1].Edits != 1 ||
				!strings.Contains(comments[1].Body, "second") {
				t.Errorf("comments = %+v, want the seeded one and ours edited once", comments)
			}
		})
	}
}

// TestClientSetsStatusInEachForgesVocabulary pins the status states each forge receives, since
// GitLab has no error state and refuses one.
func TestClientSetsStatusInEachForgesVocabulary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider  string
		State     string
		WantState string
	}{{ // Test 0: GitHub takes error as it is.
		Provider: trigger.ProviderGitHub, State: StateError, WantState: "error",
	}, { // Test 1: GitLab takes error as failed.
		Provider: trigger.ProviderGitLab, State: StateError, WantState: "failed",
	}, { // Test 2: GitLab takes failure as failed.
		Provider: trigger.ProviderGitLab, State: StateFailure, WantState: "failed",
	}, { // Test 3: Both take success as success.
		Provider: trigger.ProviderGitLab, State: StateSuccess, WantState: "success",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			fake, cfg := newFake(t, test.Provider)
			client, err := NewClient(cfg, testToken, fake.Client())
			if err != nil {
				t.Fatalf("NewClient() error = %v", err)
			}
			st := Status{State: test.State, Context: "switchtender/network", Description: "d",
				TargetURL: "https://st.example.com/ui/runs/run_1"}
			if err := client.SetStatus(context.Background(), headSHA, st); err != nil {
				t.Fatalf("SetStatus() error = %v", err)
			}
			want := []forgetest.Status{{SHA: headSHA, State: test.WantState, Context: "switchtender/network",
				Description: "d", TargetURL: "https://st.example.com/ui/runs/run_1"}}
			if diff := cmp.Diff(want, fake.Statuses(headSHA)); diff != "" {
				t.Errorf("statuses mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestClientRefusalNamesNoToken proves a forge refusal is an ErrForge that never carries the token,
// since the error reaches the server log.
func TestClientRefusalNamesNoToken(t *testing.T) {
	t.Parallel()
	fake, cfg := newFake(t, trigger.ProviderGitHub)
	client, err := NewClient(cfg, "wrong-token-value-1234", fake.Client())
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	_, err = client.FindComment(context.Background(), 7, MarkerPrefix("tpl_1"))
	if !errors.Is(err, ErrForge) {
		t.Fatalf("FindComment() error = %v, want ErrForge", err)
	}
	if strings.Contains(err.Error(), "wrong-token-value-1234") {
		t.Errorf("the forge error carries the token: %v", err)
	}
	if fake.Unauthorized() == 0 {
		t.Error("the fake saw no unauthorized request, so the token was not checked")
	}
}

// TestNewClientRefusesAnUnusableConfiguration pins the configurations a client is never built
// from: no token, and an API base that is not https.
func TestNewClientRefusesAnUnusableConfiguration(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Review *trigger.Review
		Token  string
		Want   error
	}{{ // Test 0: No token.
		Review: &trigger.Review{Provider: "github", Repository: "acme/infra", CredentialID: "c"},
		Want:   ErrNoToken,
	}, { // Test 1: A plain http API base would send the token in the clear.
		Review: &trigger.Review{Provider: "gitlab", APIURL: "http://gitlab.example.com/api/v4",
			Repository: "infra/network", CredentialID: "c"}, Token: "t", Want: trigger.ErrBadReview,
	}, { // Test 2: No configuration at all.
		Token: "t", Want: trigger.ErrBadReview,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if _, err := NewClient(test.Review, test.Token, nil); !errors.Is(err, test.Want) {
				t.Errorf("NewClient() error = %v, want %v", err, test.Want)
			}
		})
	}
}
