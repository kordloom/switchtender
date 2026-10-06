package cmd

import (
	"errors"
	"fmt"
	"io/fs"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/forgelink"
)

// TestParseForgeOAuth reads --forge-oauth values into forge applications, filling in each forge's
// addresses and reading the secret from the environment or a file, never the command line.
func TestParseForgeOAuth(t *testing.T) {
	t.Parallel()
	env := map[string]string{"GH_SECRET": " gh-secret\n", "GL_SECRET": "gl-secret", "EMPTY": " "}
	files := map[string]string{"/run/secrets/ghe": "ghe-secret\n"}
	getenv := func(k string) string { return env[k] }
	readFile := func(p string) ([]byte, error) {
		if v, ok := files[p]; ok {
			return []byte(v), nil
		}
		return nil, fs.ErrNotExist
	}
	tests := []struct {
		In         []string
		WantResult []forgelink.App
		Want       error
	}{{ // Test 0: GitHub with every address left to its default.
		In: []string{"provider=github,client_id=gh-app,secret_env=GH_SECRET"},
		WantResult: []forgelink.App{{Provider: "github", WebURL: "https://github.com",
			APIURL: "https://api.github.com", ClientID: "gh-app", ClientSecret: "gh-secret"}},
	}, { // Test 1: GitHub Enterprise Server derives its API base from its web address.
		In: []string{"provider=github, client_id=ghe-app, secret_file=/run/secrets/ghe, " +
			"web_url=https://GHE.example.com/"},
		WantResult: []forgelink.App{{Provider: "github", WebURL: "https://GHE.example.com",
			APIURL: "https://ghe.example.com/api/v3", ClientID: "ghe-app",
			ClientSecret: "ghe-secret"}},
	}, { // Test 2: Two forges, GitLab and self-managed GitLab with an explicit API base.
		In: []string{"provider=gitlab,client_id=gl-app,secret_env=GL_SECRET",
			"provider=gitlab,client_id=gl2,secret_env=GL_SECRET,web_url=https://git.example.com," +
				"api_url=https://git.example.com/api/v4/"},
		WantResult: []forgelink.App{{Provider: "gitlab", WebURL: "https://gitlab.com",
			APIURL: "https://gitlab.com/api/v4", ClientID: "gl-app", ClientSecret: "gl-secret"},
			{Provider: "gitlab", WebURL: "https://git.example.com",
				APIURL: "https://git.example.com/api/v4", ClientID: "gl2", ClientSecret: "gl-secret"}},
	}, { // Test 3: No values configures no forge.
		In: nil, WantResult: []forgelink.App{},
	}, { // Test 4: An unknown provider is refused.
		In: []string{"provider=bitbucket,client_id=x,secret_env=GH_SECRET"}, Want: ErrUsage,
	}, { // Test 5: A secret written on the command line is refused as an unknown key.
		In: []string{"provider=github,client_id=x,client_secret=plain"}, Want: ErrUsage,
	}, { // Test 6: Both secret sources at once are refused.
		In:   []string{"provider=github,client_id=x,secret_env=GH_SECRET,secret_file=/run/secrets/ghe"},
		Want: ErrUsage,
	}, { // Test 7: An empty secret is refused.
		In: []string{"provider=github,client_id=x,secret_env=EMPTY"}, Want: ErrUsage,
	}, { // Test 8: A secret file that cannot be read is refused.
		In: []string{"provider=github,client_id=x,secret_file=/nope"}, Want: ErrUsage,
	}, { // Test 9: A missing client id is refused.
		In: []string{"provider=github,secret_env=GH_SECRET"}, Want: ErrUsage,
	}, { // Test 10: A plain http forge address is refused.
		In:   []string{"provider=gitlab,client_id=x,secret_env=GL_SECRET,web_url=http://git.example.com"},
		Want: ErrUsage,
	}, { // Test 11: The same forge twice is refused.
		In: []string{"provider=github,client_id=a,secret_env=GH_SECRET",
			"provider=github,client_id=b,secret_env=GH_SECRET,api_url=https://API.github.com/"},
		Want: ErrUsage,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := parseForgeOAuth(test.In, getenv, readFile)
			if !errors.Is(err, test.Want) {
				t.Fatalf("parseForgeOAuth() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantResult, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("parseForgeOAuth() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
