package project

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/go-git/go-git/v5"
)

// syncSecretUser is the userinfo carried by the test repository URL. A deploy token used as the ssh
// login is the credential shape that survives ValidateRepoURL, so it is the shape the sync failure
// paths must strip before their text reaches a log, a run record, or an API response.
const syncSecretUser = "tok3nvalue"

// syncSecretRepoURL embeds syncSecretUser in a repository URL that passes validation.
const syncSecretRepoURL = "ssh://" + syncSecretUser + "@git.example.com/team/infra.git"

// syncRedactedRepoURL is syncSecretRepoURL with the userinfo removed, which is what the failure text
// is expected to name instead.
const syncRedactedRepoURL = "ssh://git.example.com/team/infra.git"

// TestSyncRedactsRepoURLInErrors drives both failing sync paths and confirms neither reports the
// repository URL's userinfo. Both errors interpolate the URL, so a call site that stopped redacting
// hands the embedded credential to whoever reads the failure.
func TestSyncRedactsRepoURLInErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		// Name labels the sync failure path under test.
		Name string
		// Fail provokes the failure and returns the error the caller would see.
		Fail func(t *testing.T, p *Project) error
		// WantPrefix is the error prefix proving the intended path produced the failure.
		WantPrefix string
	}{{ // Test 0: A checkout path that cannot hold a repository fails the clone.
		Name: "clone",
		Fail: func(t *testing.T, p *Project) error {
			t.Helper()
			cache := t.TempDir()
			s, err := NewSyncer(cache)
			if err != nil {
				t.Fatalf("NewSyncer() error = %v", err)
			}
			// A regular file where the checkout belongs fails the clone before it reaches the remote.
			if err := os.WriteFile(filepath.Join(cache, p.ID), []byte("not a directory"), 0o600); err != nil {
				t.Fatalf("WriteFile() error = %v", err)
			}
			_, err = s.Sync(p, "")
			return err
		},
		WantPrefix: "clone ",
	}, { // Test 1: An existing checkout with no origin remote fails the fetch.
		Name: "fetch",
		Fail: func(t *testing.T, p *Project) error {
			t.Helper()
			repo, err := git.PlainInit(t.TempDir(), false)
			if err != nil {
				t.Fatalf("PlainInit() error = %v", err)
			}
			return fetchAndReset(repo, p, nil)
		},
		WantPrefix: "fetch ",
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			p := &Project{ID: "proj_redact", Name: "infra", RepoURL: syncSecretRepoURL}

			err := test.Fail(t, p)
			if err == nil {
				t.Fatalf("test %d: error = nil, want the %s path to fail", testNum, test.Name)
			}
			got := err.Error()
			if !strings.HasPrefix(got, test.WantPrefix) {
				t.Fatalf("test %d: error = %q, want the %s failure", testNum, got, test.Name)
			}
			if strings.Contains(got, syncSecretUser) {
				t.Errorf("test %d: error leaked the url credential: %q", testNum, got)
			}
			if !strings.Contains(got, syncRedactedRepoURL) {
				t.Errorf("test %d: error = %q, want the repository named as %q",
					testNum, got, syncRedactedRepoURL)
			}
		})
	}
}
