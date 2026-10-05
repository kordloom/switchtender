package trigger_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/trigger"
)

// TestReviewValidate pins which review configurations are refused where they are written.
func TestReviewValidate(t *testing.T) {
	t.Parallel()
	ok := func(mod func(*trigger.Review)) *trigger.Review {
		r := &trigger.Review{
			Provider: trigger.ProviderGitHub, Repository: "acme/infra", CredentialID: "cred_1",
		}
		mod(r)
		return r
	}
	tests := []struct {
		Review      *trigger.Review
		WantBaseURL string
		Want        error
	}{{ // Test 0: A GitHub review with the public API.
		Review: ok(func(*trigger.Review) {}), WantBaseURL: "https://api.github.com",
	}, { // Test 1: A self-managed GitLab with a nested group.
		Review: ok(func(r *trigger.Review) {
			r.Provider, r.Repository = trigger.ProviderGitLab, "platform/infra/network"
			r.APIURL = "https://gitlab.example.com/api/v4/"
		}),
		WantBaseURL: "https://gitlab.example.com/api/v4",
	}, { // Test 2: An unknown provider.
		Review: ok(func(r *trigger.Review) { r.Provider = "bitbucket" }), Want: trigger.ErrBadReview,
	}, { // Test 3: A GitHub repository is exactly owner/name.
		Review: ok(func(r *trigger.Review) { r.Repository = "acme/infra/extra" }),
		Want:   trigger.ErrBadReview,
	}, { // Test 4: No repository.
		Review: ok(func(r *trigger.Review) { r.Repository = "" }), Want: trigger.ErrBadReview,
	}, { // Test 5: A traversal in the repository.
		Review: ok(func(r *trigger.Review) { r.Repository = "../admin" }), Want: trigger.ErrBadReview,
	}, { // Test 6: No token credential.
		Review: ok(func(r *trigger.Review) { r.CredentialID = "" }), Want: trigger.ErrBadReview,
	}, { // Test 7: A plain http API would send the token in the clear.
		Review: ok(func(r *trigger.Review) { r.APIURL = "http://ghe.example.com/api/v3" }),
		Want:   trigger.ErrBadReview,
	}, { // Test 8: Credentials in the API URL are refused.
		Review: ok(func(r *trigger.Review) { r.APIURL = "https://u:p@ghe.example.com/api/v3" }),
		Want:   trigger.ErrBadReview,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Review.Validate()
			if !errors.Is(err, test.Want) {
				t.Fatalf("Validate() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			if diff := cmp.Diff(test.WantBaseURL, test.Review.BaseURL()); diff != "" {
				t.Errorf("BaseURL() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestVerifyToken pins GitLab's token check: the secret itself, nothing else, never an empty one.
func TestVerifyToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Secret    string
		Header    string
		WantMatch bool
	}{{ // Test 0: The secret matches.
		Secret: "whs_abc", Header: "whs_abc", WantMatch: true,
	}, { // Test 1: A different value does not.
		Secret: "whs_abc", Header: "whs_abd",
	}, { // Test 2: An empty header does not.
		Secret: "whs_abc",
	}, { // Test 3: An empty secret never matches, even an empty header.
		Header: "",
	}, { // Test 4: A prefix of the secret does not.
		Secret: "whs_abc", Header: "whs_ab",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := trigger.VerifyToken(test.Secret, test.Header); got != test.WantMatch {
				t.Errorf("VerifyToken(%q, %q) = %v, want %v", test.Secret, test.Header, got, test.WantMatch)
			}
		})
	}
}
