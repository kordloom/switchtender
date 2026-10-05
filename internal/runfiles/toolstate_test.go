package runfiles

import (
	"fmt"
	"path/filepath"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestToolStateEnv pins which tools a run's environment redirects and where: only a tool the run
// hands a credential, every relocating variable it documents, a variable the run already sets left
// alone, and nothing at all without a run directory.
func TestToolStateEnv(t *testing.T) {
	t.Parallel()
	dir := filepath.Join("/runs", "run-abc")
	tool := func(rel string) string { return filepath.Join(dir, toolsDir, filepath.FromSlash(rel)) }
	tests := []struct {
		// Dir is the run directory.
		Dir string
		// Env is the run's own environment.
		Env []string
		// WantEnv is what ToolStateEnv adds.
		WantEnv []string
	}{{ // Test 0: Static AWS keys move the AWS config, credentials, and login cache.
		Dir: dir, Env: []string{"AWS_ACCESS_KEY_ID=AKIA", "AWS_SECRET_ACCESS_KEY=s"},
		WantEnv: []string{"AWS_CONFIG_FILE=" + tool("aws/config"),
			"AWS_SHARED_CREDENTIALS_FILE=" + tool("aws/credentials"),
			"AWS_LOGIN_CACHE_DIRECTORY=" + tool("aws/login-cache")},
	}, { // Test 1: A web identity role counts as an AWS credential.
		Dir: dir, Env: []string{"AWS_ROLE_ARN=arn", "AWS_WEB_IDENTITY_TOKEN_FILE=/t"},
		WantEnv: []string{"AWS_CONFIG_FILE=" + tool("aws/config"),
			"AWS_SHARED_CREDENTIALS_FILE=" + tool("aws/credentials"),
			"AWS_LOGIN_CACHE_DIRECTORY=" + tool("aws/login-cache")},
	}, { // Test 2: A Google credential moves the gcloud configuration.
		Dir: dir, Env: []string{"GOOGLE_APPLICATION_CREDENTIALS=/c.json"},
		WantEnv: []string{"CLOUDSDK_CONFIG=" + tool("gcloud")},
	}, { // Test 3: An Azure service principal moves the az configuration.
		Dir: dir, Env: []string{"AZURE_CLIENT_ID=id", "AZURE_SECRET=s"},
		WantEnv: []string{"AZURE_CONFIG_DIR=" + tool("azure")},
	}, { // Test 4: A kubeconfig moves kubectl's cache.
		Dir: dir, Env: []string{"KUBECONFIG=/k"},
		WantEnv: []string{"KUBECACHEDIR=" + tool("kube/cache")},
	}, { // Test 5: A credential that names its own config file keeps it.
		Dir: dir, Env: []string{"AWS_ACCESS_KEY_ID=AKIA", "AWS_CONFIG_FILE=/mine"},
		WantEnv: []string{"AWS_SHARED_CREDENTIALS_FILE=" + tool("aws/credentials"),
			"AWS_LOGIN_CACHE_DIRECTORY=" + tool("aws/login-cache")},
	}, { // Test 6: A run handing no cloud a credential keeps every tool's own configuration.
		Dir: dir, Env: []string{"SWITCHTENDER_TOKEN=t", "AWS_REGION=us-east-1", "AWS_ACCESS_KEY_ID="},
		WantEnv: nil,
	}, { // Test 7: No run directory, nothing to point at.
		Dir: "", Env: []string{"KUBECONFIG=/k"}, WantEnv: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := ToolStateEnv(test.Dir, test.Env)
			if diff := cmp.Diff(test.WantEnv, got, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("ToolStateEnv() (-want +got):\n%s", diff)
			}
		})
	}
}
