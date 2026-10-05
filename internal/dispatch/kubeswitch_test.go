package dispatch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
)

// TestKubeconfigTypeMasksEveryLineUntilSwitched pins what an import leaves alone and what the
// one-step switch changes. A credential of an imported type shaped like a kubeconfig masks every
// line of the document when a tool prints it, ordinary structure included, because its secret field
// is masked whole. The same document switched to the built-in kubeconfig kind masks only the
// secrets inside it. Either way the token never reaches the log.
func TestKubeconfigTypeMasksEveryLineUntilSwitched(t *testing.T) {
	t.Parallel()
	imported := &credential.CredentialType{
		ID: "ctype_awxkube", Name: "Kubeconfig", Origin: credential.OriginAWX,
		Fields:        []credential.Field{{Name: "kube_config", Secret: true, Multiline: true}},
		FileInjectors: map[string]string{"template": "{{ kube_config }}"},
		EnvInjectors:  map[string]string{"K8S_AUTH_KUBECONFIG": "{{ tower.filename }}"},
	}
	if _, ok := imported.KubeconfigShaped(); !ok {
		t.Fatal("the imported type is not shaped like a kubeconfig, so this proves nothing")
	}
	tests := []struct {
		Credential  func(t *testing.T, fx *fileFixture) *credential.Credential
		WantShown   []string
		WantMasked  []string
		WantEnvVars int
	}{{ // Test 0: As imported, every line of the document is masked.
		Credential: func(t *testing.T, fx *fileFixture) *credential.Credential {
			values, err := json.Marshal(map[string]string{"kube_config": fileKubeconfig})
			if err != nil {
				t.Fatal(err)
			}
			return &credential.Credential{ID: "cred_k", Name: "prod-kube", TypeID: imported.ID,
				Secret: seal(t, fx, string(values))}
		},
		WantMasked:  []string{fileSecret, "apiVersion: v1", "kind: Config", "name: deployer"},
		WantEnvVars: 1,
	}, { // Test 1: Switched to the built-in kind, only the token is masked.
		Credential: func(t *testing.T, fx *fileFixture) *credential.Credential {
			return &credential.Credential{ID: "cred_k", Name: "prod-kube",
				Kind: credential.KindKubeconfig, Secret: seal(t, fx, fileKubeconfig)}
		},
		WantShown:   []string{"apiVersion: v1", "kind: Config", "name: deployer"},
		WantMasked:  []string{fileSecret},
		WantEnvVars: len(credential.KubeconfigEnvVars),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			fx := newFileFixture(t)
			if err := fx.types.Save(ctx, imported); err != nil {
				t.Fatal(err)
			}
			if err := fx.creds.Save(ctx, test.Credential(t, fx)); err != nil {
				t.Fatal(err)
			}
			envVars := make(chan int, 1)
			runner := roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
				out io.Writer) (roundhouse.Result, error) {
				var path string
				n := 0
				for _, name := range credential.KubeconfigEnvVars {
					for _, kv := range spec.Env {
						if v, ok := strings.CutPrefix(kv, name+"="); ok {
							path = v
							n++
						}
					}
				}
				envVars <- n
				// A tool that prints its kubeconfig, as kubectl config view --raw does.
				body, err := os.ReadFile(path)
				if err != nil {
					return roundhouse.Result{ExitCode: 1}, err
				}
				_, _ = out.Write(body)
				return roundhouse.Result{ExitCode: 0}, nil
			})
			store := run.NewMemStore()
			d := New(store, runner, nil, fx.options()...)
			defer d.Close()
			r, err := d.Submit(ctx, "", "", run.WithTool(run.ToolBash),
				run.WithCommand("kubectl config view --raw"),
				run.WithCredentialIDs([]string{"cred_k"}))
			if err != nil {
				t.Fatalf("Submit() error = %v", err)
			}
			if got := waitTerminal(t, store, r.ID); got.Status != run.StatusSucceeded {
				t.Fatalf("status = %q (%s), want succeeded", got.Status, got.Error)
			}
			if n := <-envVars; n != test.WantEnvVars {
				t.Errorf("the file reached %d kubeconfig variables, want %d", n, test.WantEnvVars)
			}
			body, err := store.Log(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, line := range test.WantMasked {
				if strings.Contains(string(body), line) {
					t.Errorf("the log shows %q, which this credential masks: %q", line, body)
				}
			}
			for _, line := range test.WantShown {
				if !strings.Contains(string(body), line) {
					t.Errorf("the log hides %q, which only the custom type masks: %q", line, body)
				}
			}
		})
	}
}

// seal seals plain with the fixture's sealer.
func seal(t *testing.T, fx *fileFixture, plain string) string {
	t.Helper()
	sealed, err := fx.sealer.Seal(plain)
	if err != nil {
		t.Fatal(err)
	}
	return sealed
}
