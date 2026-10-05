package runfiles

import (
	"path/filepath"
	"strings"
)

// toolsDir is the directory inside a run directory that holds the state cloud command line tools
// keep about the credentials a run handed them.
const toolsDir = "tools"

// toolState is one tool's credential-bearing state and what moves it.
type toolState struct {
	// Tool names the tool.
	Tool string
	// Triggers are the credential variables whose presence in a run's own environment means the run
	// hands the tool a credential.
	Triggers []string
	// Env lists each variable that relocates the tool's state, paired with the path it is given,
	// relative to the run directory's tools directory.
	Env [][2]string
}

// toolStates are the tools whose credential-bearing state SwitchTender knows how to move. Each
// variable is the one the tool itself documents for the purpose:
//
//   - AWS: AWS_CONFIG_FILE and AWS_SHARED_CREDENTIALS_FILE place the config and credentials files
//     every AWS SDK and the AWS CLI read and aws configure writes, and AWS_LOGIN_CACHE_DIRECTORY
//     places botocore's login token cache. The CLI's role credential cache and its SSO token cache
//     sit under ~/.aws and no variable moves them.
//   - Google Cloud: CLOUDSDK_CONFIG places the whole gcloud configuration directory, including the
//     credential and access token databases gcloud auth writes.
//   - Azure: AZURE_CONFIG_DIR places the az configuration directory, including its MSAL token
//     cache.
//   - kubectl: KUBECACHEDIR places the discovery and HTTP caches. The kubeconfig itself is already
//     a file in the run directory.
var toolStates = []toolState{{
	Tool: "aws",
	Triggers: []string{"AWS_ACCESS_KEY_ID", "AWS_SECRET_ACCESS_KEY", "AWS_SESSION_TOKEN",
		"AWS_ROLE_ARN", "AWS_WEB_IDENTITY_TOKEN_FILE"},
	Env: [][2]string{{"AWS_CONFIG_FILE", "aws/config"}, {"AWS_SHARED_CREDENTIALS_FILE",
		"aws/credentials"}, {"AWS_LOGIN_CACHE_DIRECTORY", "aws/login-cache"}},
}, {
	Tool: "gcloud",
	Triggers: []string{"GOOGLE_APPLICATION_CREDENTIALS", "GOOGLE_CREDENTIALS",
		"GOOGLE_OAUTH_ACCESS_TOKEN", "CLOUDSDK_AUTH_CREDENTIAL_FILE_OVERRIDE",
		"CLOUDSDK_AUTH_ACCESS_TOKEN_FILE", "GCP_SERVICE_ACCOUNT_FILE", "GCP_ACCESS_TOKEN"},
	Env: [][2]string{{"CLOUDSDK_CONFIG", "gcloud"}},
}, {
	Tool: "az",
	Triggers: []string{"AZURE_CLIENT_ID", "AZURE_CLIENT_SECRET", "AZURE_SECRET",
		"AZURE_FEDERATED_TOKEN_FILE", "ARM_CLIENT_ID", "ARM_CLIENT_SECRET", "ARM_OIDC_TOKEN_FILE_PATH"},
	Env: [][2]string{{"AZURE_CONFIG_DIR", "azure"}},
}, {
	Tool:     "kubectl",
	Triggers: []string{"KUBECONFIG", "K8S_AUTH_KUBECONFIG", "KUBE_CONFIG_PATH"},
	Env:      [][2]string{{"KUBECACHEDIR", "kube/cache"}},
}}

// ToolStateEnv returns the environment entries that point the known credential-bearing state of
// cloud command line tools into dir, the run's private directory, for each tool that env, the run's
// own environment, hands a credential. The tools create what they need below it, and it goes when
// the run directory does. A variable env already sets is left as env sets it, so a credential that
// names its own config file keeps it. An empty dir returns nothing.
//
// Only a run that hands a tool a credential is redirected. A run that carries none for a cloud
// keeps that tool's ordinary configuration in the executing account's home, which is where an
// operator who set up ambient access expects it.
func ToolStateEnv(dir string, env []string) []string {
	if dir == "" {
		return nil
	}
	set := map[string]bool{}
	for _, kv := range env {
		if name, value, ok := strings.Cut(kv, "="); ok && value != "" {
			set[name] = true
		}
	}
	var out []string
	for _, ts := range toolStates {
		if !anySet(set, ts.Triggers) {
			continue
		}
		for _, e := range ts.Env {
			if set[e[0]] {
				continue
			}
			out = append(out, e[0]+"="+filepath.Join(dir, toolsDir, filepath.FromSlash(e[1])))
		}
	}
	return out
}

// anySet reports whether any of names is in set.
func anySet(set map[string]bool, names []string) bool {
	for _, n := range names {
		if set[n] {
			return true
		}
	}
	return false
}
