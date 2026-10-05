//go:build unix

package dispatch

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// Stand-ins for the cloud command line tools that are not installed, each writing exactly where the
// tool's own documentation says the command it imitates writes: under the directory or file the
// tool's variable names, and under the home directory when the variable is unset.
const (
	// fakeAWS imitates "aws configure set", which writes a key to the shared credentials file or the
	// config file, at AWS_SHARED_CREDENTIALS_FILE or AWS_CONFIG_FILE, otherwise under ~/.aws.
	fakeAWS = `#!/bin/sh
[ "$1" = configure ] && [ "$2" = set ] || exit 2
case "$3" in
aws_access_key_id|aws_secret_access_key|aws_session_token)
  file="${AWS_SHARED_CREDENTIALS_FILE:-$HOME/.aws/credentials}" ;;
*) file="${AWS_CONFIG_FILE:-$HOME/.aws/config}" ;;
esac
mkdir -p "$(dirname "$file")" && printf '[default]\n%s = %s\n' "$3" "$4" >> "$file"
`
	// fakeGcloud imitates "gcloud config set", which writes the active configuration in the
	// directory CLOUDSDK_CONFIG names, otherwise ~/.config/gcloud, beside the credential databases.
	fakeGcloud = `#!/bin/sh
[ "$1" = config ] && [ "$2" = set ] || exit 2
dir="${CLOUDSDK_CONFIG:-$HOME/.config/gcloud}"
mkdir -p "$dir/configurations" &&
  printf '[core]\n%s = %s\n' "$3" "$4" > "$dir/configurations/config_default"
`
	// fakeAz imitates "az config set", which writes the config file in the directory AZURE_CONFIG_DIR
	// names, otherwise ~/.azure, beside the MSAL token cache.
	fakeAz = `#!/bin/sh
[ "$1" = config ] && [ "$2" = set ] || exit 2
dir="${AZURE_CONFIG_DIR:-$HOME/.azure}"
mkdir -p "$dir" && printf '[core]\n%s\n' "$3" > "$dir/config"
`
	// fakeKubectl imitates kubectl's discovery cache, under KUBECACHEDIR, otherwise ~/.kube/cache.
	fakeKubectl = `#!/bin/sh
dir="${KUBECACHEDIR:-$HOME/.kube/cache}/discovery/fake"
mkdir -p "$dir" && printf '{}' > "$dir/servergroups.json"
`
	// botocoreProbe resolves the paths botocore reads its config and credentials from and writes a
	// login token through botocore's own cache, the way it saves one after aws login.
	botocoreProbe = `import botocore.session, botocore.utils
s = botocore.session.Session()
for name in ("config_file", "credentials_file"):
    print(s.get_config_variable(name))
botocore.utils.JSONFileCache(botocore.utils.get_login_token_cache_directory())["run"] = {"t": "x"}
`
)

// toolStateVars are the variables SwitchTender sets to move tool state into a run directory. The
// control run removes them, to show where the tool writes without them.
var toolStateVars = []string{"AWS_CONFIG_FILE", "AWS_SHARED_CREDENTIALS_FILE",
	"AWS_LOGIN_CACHE_DIRECTORY", "CLOUDSDK_CONFIG", "AZURE_CONFIG_DIR", "KUBECACHEDIR"}

// toolCommand returns the program to run for tool and its arguments: the installed tool when there
// is one, otherwise the stand-in written to bin. It reports whether the tool is the real one.
func toolCommand(t *testing.T, tool, bin string) ([]string, bool) {
	t.Helper()
	args := map[string][]string{
		"aws":     {"configure", "set", "region", "us-east-1"},
		"gcloud":  {"config", "set", "core/disable_usage_reporting", "true"},
		"az":      {"config", "set", "core.collect_telemetry=false"},
		"kubectl": {"--request-timeout=5s", "api-resources"},
	}[tool]
	if path, err := exec.LookPath(tool); err == nil {
		return append([]string{path}, args...), true
	}
	fake := map[string]string{"aws": fakeAWS, "gcloud": fakeGcloud, "az": fakeAz,
		"kubectl": fakeKubectl}[tool]
	path := filepath.Join(bin, tool)
	if err := os.WriteFile(path, []byte(fake), 0o700); err != nil {
		t.Fatal(err)
	}
	return append([]string{path}, args...), false
}

// botocorePython returns a Python interpreter that imports botocore, the library under Ansible's
// AWS modules, or empty when none is installed.
func botocorePython() string {
	candidates := []string{"python3"}
	if pb, err := exec.LookPath("ansible-playbook"); err == nil {
		if b, err := os.ReadFile(pb); err == nil {
			if line, _, _ := strings.Cut(string(b), "\n"); strings.HasPrefix(line, "#!") {
				candidates = append([]string{strings.Fields(strings.TrimPrefix(line, "#!"))[0]},
					candidates...)
			}
		}
	}
	for _, c := range candidates {
		if exec.Command(c, "-c", "import botocore").Run() == nil {
			return c
		}
	}
	return ""
}

// filesUnder lists the regular files below dir, relative to it.
func filesUnder(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}
			return err
		}
		if d.Type().IsRegular() {
			rel, _ := filepath.Rel(dir, p)
			out = append(out, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	return out
}

// toolLook is what one run of a tool left where.
type toolLook struct {
	// runDir is the run directory the dispatcher created.
	runDir string
	// inTools lists the files written under the run directory's tools directory.
	inTools []string
	// inHome lists the files written under the home directory the tool ran with.
	inHome []string
	// controlHome lists the files the same tool wrote under its home directory with the
	// redirection removed.
	controlHome []string
	// err records a tool that failed.
	err error
}

// runTool runs argv with the run's environment and home as its home directory, and again with the
// redirection removed and a second home, and reports what each left where.
func runTool(t *testing.T, argv []string, spec roundhouse.Spec, extra ...[]string) toolLook {
	t.Helper()
	look := toolLook{runDir: spec.RunDir}
	home, controlHome := t.TempDir(), t.TempDir()
	base := []string{"PATH=" + os.Getenv("PATH"), "CLOUDSDK_CORE_DISABLE_PROMPTS=1"}
	redirected := append(append(append([]string(nil), base...), "HOME="+home), spec.Env...)
	control := append(append([]string(nil), base...), "HOME="+controlHome)
	for _, kv := range spec.Env {
		name, _, _ := strings.Cut(kv, "=")
		if !containsString(toolStateVars, name) {
			control = append(control, kv)
		}
	}
	for _, cmd := range append([][]string{argv}, extra...) {
		for _, env := range [][]string{redirected, control} {
			c := exec.Command(cmd[0], cmd[1:]...)
			c.Env = env
			if out, err := c.CombinedOutput(); err != nil {
				look.err = errors.Join(look.err, fmt.Errorf("%v: %w: %s", cmd, err, out))
			}
		}
	}
	look.inTools = filesUnder(t, filepath.Join(spec.RunDir, "tools"))
	look.inHome = filesUnder(t, home)
	look.controlHome = filesUnder(t, controlHome)
	return look
}

// fakeKubeAPI serves just enough of the Kubernetes discovery API for kubectl api-resources.
func fakeKubeAPI(t *testing.T) *httptest.Server {
	t.Helper()
	bodies := map[string]string{
		"/version": `{"major":"1","minor":"30","gitVersion":"v1.30.0"}`,
		"/api":     `{"kind":"APIVersions","versions":["v1"],"serverAddressByClientCIDRs":[]}`,
		"/apis":    `{"kind":"APIGroupList","apiVersion":"v1","groups":[]}`,
		"/api/v1": `{"kind":"APIResourceList","groupVersion":"v1","resources":[{"name":"configmaps",` +
			`"singularName":"configmap","namespaced":true,"kind":"ConfigMap","verbs":["get","list"]}]}`,
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, ok := bodies[r.URL.Path]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestCredentialedRunsKeepToolStateInTheRunDirectory is the cache redirection test for every
// supported credential kind. Each kind's credential is materialized by the dispatcher for a run,
// and the tool that kind feeds runs with the environment the dispatcher built: the installed tool
// where there is one, kubectl and botocore here, otherwise a stand-in that writes where the tool's
// documentation says. What the tool writes must land under the run directory's tools directory and
// nowhere under the home directory, and the run directory must be gone when the run ends. Each case
// also runs the tool with the redirection removed, which writes under the home directory instead:
// that is the negative control, and it is what every credentialed run did before.
//
//nolint:funlen // Test function.
func TestCredentialedRunsKeepToolStateInTheRunDirectory(t *testing.T) {
	t.Parallel()
	kube := fakeKubeAPI(t)
	python := botocorePython()
	kubeconfig := "apiVersion: v1\nkind: Config\nclusters:\n- name: c\n  cluster:\n    server: " +
		kube.URL + "\ncontexts:\n- name: c\n  context:\n    cluster: c\n    user: u\n" +
		"current-context: c\nusers:\n- name: u\n  user:\n    token: kube-token-secret\n"
	tests := []struct {
		// Cred is the credential the run carries. A non-federated one's Secret is sealed here.
		Cred credential.Credential
		// Tool is the command line tool that kind feeds.
		Tool string
		// WantArea is where under the tools directory the tool's state belongs.
		WantArea string
	}{{ // Test 0: Static AWS keys.
		Cred: credential.Credential{Kind: credential.KindAWS,
			Secret: "access_key=AKIATOOLSTATE\nsecret_key=tool-state-secret"},
		Tool: "aws", WantArea: "aws",
	}, { // Test 1: AWS workload identity federation, which hands the tool a web identity token.
		Cred: credential.Credential{Kind: credential.KindAWSOIDC,
			Settings: map[string]string{"role_arn": "arn:aws:iam::123456789012:role/deploy"}},
		Tool: "aws", WantArea: "aws",
	}, { // Test 2: A Google service account.
		Cred: credential.Credential{Kind: credential.KindGCP,
			Secret: `{"type":"service_account","client_email":"x@p.iam.gserviceaccount.com",` +
				`"private_key":"-----BEGIN PRIVATE KEY-----\nabc\n-----END PRIVATE KEY-----\n"}`},
		Tool: "gcloud", WantArea: "gcloud",
	}, { // Test 3: Google workload identity federation.
		Cred: credential.Credential{Kind: credential.KindGCPOIDC,
			Settings: map[string]string{"provider": "projects/123456789/locations/global/" +
				"workloadIdentityPools/ci-pool/providers/switchtender"}},
		Tool: "gcloud", WantArea: "gcloud",
	}, { // Test 4: An Azure service principal.
		Cred: credential.Credential{Kind: credential.KindAzure,
			Secret: "client_id=app\nsecret=azure-secret\nsubscription_id=sub\ntenant_id=tenant"},
		Tool: "az", WantArea: "azure",
	}, { // Test 5: A Microsoft Entra federated identity.
		Cred: credential.Credential{Kind: credential.KindAzureOIDC,
			Settings: map[string]string{"client_id": "app-1", "tenant_id": "tenant-1"}},
		Tool: "az", WantArea: "azure",
	}, { // Test 6: A kubeconfig.
		Cred: credential.Credential{Kind: credential.KindKubeconfig, Secret: kubeconfig},
		Tool: "kubectl", WantArea: filepath.Join("kube", "cache"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Cred.Kind), func(t *testing.T) {
			t.Parallel()
			argv, real := toolCommand(t, test.Tool, t.TempDir())
			var extra [][]string
			if test.Tool == "aws" && python != "" {
				probe := filepath.Join(t.TempDir(), "probe.py")
				if err := os.WriteFile(probe, []byte(botocoreProbe), 0o600); err != nil {
					t.Fatal(err)
				}
				extra = append(extra, []string{python, probe})
			}
			c := test.Cred
			c.ID, c.Name = "cred_tool", "tool-state"
			if c.Secret != "" {
				sealed, err := fedSealer.Seal(c.Secret)
				if err != nil {
					t.Fatalf("Seal() error = %v", err)
				}
				c.Secret = sealed
			}
			looks := make(chan toolLook, 1)
			runner := roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
				_ io.Writer) (roundhouse.Result, error) {
				looks <- runTool(t, argv, spec, extra...)
				return roundhouse.Result{ExitCode: 0}, nil
			})
			fx := newFedFixture(t, runner, &c)
			r, err := fx.d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
				run.WithCommand("tool state"), run.WithCredentialIDs([]string{c.ID}))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			var look toolLook
			select {
			case look = <-looks:
			case <-time.After(waitBudget):
				t.Fatal("the run never reached its tool")
			}
			if final := waitTerminal(t, fx.store, r.ID); final.Status != run.StatusSucceeded {
				t.Fatalf("run status = %s: %s", final.Status, final.Error)
			}
			if look.err != nil {
				t.Fatalf("the tool failed (installed: %v): %v", real, look.err)
			}
			t.Logf("%s: installed tool %v, botocore %q, wrote %v", test.Tool, real, python, look.inTools)
			if len(look.inTools) == 0 {
				t.Errorf("the tool wrote nothing under the run directory's tools directory")
			}
			if len(extra) > 0 && !containsString(look.inTools,
				filepath.Join("aws", "login-cache", "run.json")) {
				t.Errorf("botocore's login cache did not land in the run directory: %v", look.inTools)
			}
			for _, f := range look.inTools {
				if !strings.HasPrefix(f, test.WantArea+string(filepath.Separator)) {
					t.Errorf("tool state %s is outside %s", f, test.WantArea)
				}
			}
			if len(look.inHome) > 0 {
				t.Errorf("the tool wrote under the home directory: %v", look.inHome)
			}
			if len(look.controlHome) == 0 {
				t.Errorf("without the redirection the tool wrote nothing under its home, so this " +
					"proves nothing about the redirection")
			}
			if _, err := os.Stat(look.runDir); !errors.Is(err, fs.ErrNotExist) {
				t.Errorf("the run directory %s outlived the run: %v", look.runDir, err)
			}
		})
	}
}

// TestUncredentialedRunKeepsToolsAtHome proves the redirection is only for a tool the run hands a
// credential: a run with no credential gets a private directory for its own files, and no tool's
// state is moved into it, so an operator's ambient tool configuration keeps working.
func TestUncredentialedRunKeepsToolsAtHome(t *testing.T) {
	t.Parallel()
	specs := make(chan roundhouse.Spec, 1)
	runner := roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
		_ io.Writer) (roundhouse.Result, error) {
		_, err := os.Stat(spec.RunDir)
		if err != nil {
			spec.RunDir = "missing: " + err.Error()
		}
		specs <- spec
		return roundhouse.Result{ExitCode: 0}, nil
	})
	root := t.TempDir()
	store := run.NewMemStore()
	d := New(store, runner, zap.NewNop(), WithNoJanitor(), WithClaimInterval(time.Millisecond),
		WithRunFilesRoot(root))
	defer d.Close()
	r, err := d.Submit(context.Background(), "", "", run.WithTool(run.ToolBash),
		run.WithCommand("aws s3 ls"))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	spec := <-specs
	waitTerminal(t, store, r.ID)
	if filepath.Dir(spec.RunDir) != root || spec.RunFilesRoot != root {
		t.Errorf("RunDir = %q under root %q, want a private directory under %s", spec.RunDir,
			spec.RunFilesRoot, root)
	}
	for _, kv := range spec.Env {
		name, _, _ := strings.Cut(kv, "=")
		if containsString(toolStateVars, name) {
			t.Errorf("a run with no credential had %s moved", kv)
		}
	}
	if _, err := os.Stat(spec.RunDir); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("the run directory outlived the run: %v", err)
	}
}
