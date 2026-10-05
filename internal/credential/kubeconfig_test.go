package credential

import (
	"encoding/base64"
	"errors"
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// TestKubeconfigInject covers the built-in kubeconfig kind: a document becomes one private file bound
// to the three variables Kubernetes tooling reads, and its secret values are named for masking while
// its ordinary structure is not.
func TestKubeconfigInject(t *testing.T) {
	t.Parallel()
	pem := "-----BEGIN RSA PRIVATE KEY-----\nMIIkey\n-----END RSA PRIVATE KEY-----\n"
	keyData := base64.StdEncoding.EncodeToString([]byte(pem))
	doc := "apiVersion: v1\nkind: Config\nclusters:\n- name: prod\n  cluster:\n" +
		"    server: https://k8s.example.com\nusers:\n- name: deployer\n  user:\n" +
		"    token: kube-token-s3cret\n    client-key-data: " + keyData + "\n" +
		"- name: eks\n  user:\n    exec:\n      command: aws\n      env:\n" +
		"      - name: AWS_SECRET_ACCESS_KEY\n        value: aws-s3cret-value\n" +
		"      - name: AWS_REGION\n        value: us-east-1\n"
	tests := []struct {
		Secret      string
		WantSecrets []string
		WantEnv     []string
		Want        error
	}{{ // Test 0: A full kubeconfig names its token, its client key, and the exec secret.
		Secret:      doc,
		WantSecrets: []string{"kube-token-s3cret", keyData, pem, "aws-s3cret-value"},
		WantEnv:     []string{"KUBECONFIG", "K8S_AUTH_KUBECONFIG", "KUBE_CONFIG_PATH"},
	}, { // Test 1: An empty value is refused.
		Secret: "  \n", Want: ErrBadField,
	}, { // Test 2: A value that is not a YAML mapping is refused.
		Secret: "- just\n- a list\n", Want: ErrBadField,
	}, { // Test 3: A value that is not YAML at all is refused.
		Secret: "key: [unclosed", Want: ErrBadField,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			inj, err := Inject(KindKubeconfig, test.Secret)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Inject() error = %v, want %v", err, test.Want)
			}
			if err != nil {
				return
			}
			if len(inj.Files) != 1 {
				t.Fatalf("Inject() files = %d, want 1", len(inj.Files))
			}
			f := inj.Files[0]
			if diff := cmp.Diff(test.WantEnv, f.EnvVars); diff != "" {
				t.Errorf("env vars mismatch (-want +got):\n%s", diff)
			}
			if !f.MaskByField {
				t.Error("a kubeconfig file is masked whole, which redacts its ordinary lines everywhere")
			}
			if f.Content != test.Secret {
				t.Errorf("content = %q, want the document", f.Content)
			}
			got := slices.Clone(inj.Secrets)
			if diff := cmp.Diff(test.WantSecrets, got, cmpopts.EquateEmpty(),
				cmpopts.SortSlices(func(a, b string) bool { return a < b })); diff != "" {
				t.Errorf("secrets mismatch (-want +got):\n%s", diff)
			}
			for _, plain := range []string{"kind: Config", "us-east-1", "https://k8s.example.com"} {
				if slices.Contains(inj.Secrets, plain) {
					t.Errorf("non-secret %q was named for masking", plain)
				}
			}
		})
	}
}

// TestKubeconfigIsABuiltinKind pins that the kind is listed, valid, and usable with any tool.
func TestKubeconfigIsABuiltinKind(t *testing.T) {
	t.Parallel()
	if !slices.Contains(Kinds(), KindKubeconfig) || !ValidKind(KindKubeconfig) {
		t.Error("kubeconfig is not a listed, valid kind")
	}
	if AnsibleOnly(KindKubeconfig) {
		t.Error("kubeconfig is marked Ansible only, but kubectl, helm, and Terraform read it too")
	}
}

// TestKubeconfigShaped pins which custom types count as a kubeconfig the built-in kind could carry
// unchanged: one field, one file, its path in kubeconfig variables only. Every near miss is a type
// a switch would change, so each is held back.
func TestKubeconfigShaped(t *testing.T) {
	t.Parallel()
	shaped := func(edit func(*CredentialType)) *CredentialType {
		typ := &CredentialType{
			Name:          "Kubeconfig",
			Fields:        []Field{{Name: "kube_config", Secret: true, Multiline: true}},
			FileInjectors: map[string]string{"template": "{{ kube_config }}"},
			EnvInjectors:  map[string]string{"K8S_AUTH_KUBECONFIG": "{{ tower.filename }}"},
		}
		if edit != nil {
			edit(typ)
		}
		return typ
	}
	tests := []struct {
		Type      *CredentialType
		WantField string
		WantOK    bool
	}{{ // Test 0: The shape AWX users write: one secret field, its file path in K8S_AUTH_KUBECONFIG.
		Type: shaped(nil), WantField: "kube_config", WantOK: true,
	}, { // Test 1: A named file, its path in all three variables, the awx alias in one.
		Type: shaped(func(c *CredentialType) {
			c.FileInjectors = map[string]string{"template.kubeconfig": "\n{{kube_config}}  "}
			c.EnvInjectors = map[string]string{
				"KUBECONFIG":          "{{ tower.filename.kubeconfig }}",
				"K8S_AUTH_KUBECONFIG": "{{ awx.filename.kubeconfig }}",
				"KUBE_CONFIG_PATH":    " {{ tower.filename.kubeconfig }} ",
			}
		}), WantField: "kube_config", WantOK: true,
	}, { // Test 2: A second field nothing injects changes nothing a run receives.
		Type: shaped(func(c *CredentialType) {
			c.Fields = append(c.Fields, Field{Name: "notes"})
		}), WantField: "kube_config", WantOK: true,
	}, { // Test 3: An extra var handing over the path would be lost by a switch.
		Type: shaped(func(c *CredentialType) {
			c.ExtraVarInjectors = map[string]string{"kubeconfig": "{{ tower.filename }}"}
		}),
	}, { // Test 4: A variable the kind does not set would be lost by a switch.
		Type: shaped(func(c *CredentialType) {
			c.EnvInjectors["K8S_AUTH_CONTEXT"] = "prod"
		}),
	}, { // Test 5: The kubeconfig variable carrying more than the path is a different value.
		Type: shaped(func(c *CredentialType) {
			c.EnvInjectors = map[string]string{"KUBECONFIG": "{{ tower.filename }}:/etc/kube"}
		}),
	}, { // Test 6: A file holding more than the one field is a different document.
		Type: shaped(func(c *CredentialType) {
			c.FileInjectors = map[string]string{"template": "# managed\n{{ kube_config }}"}
		}),
	}, { // Test 7: Two files are not one kubeconfig.
		Type: shaped(func(c *CredentialType) {
			c.FileInjectors = map[string]string{
				"template.a": "{{ kube_config }}", "template.b": "{{ kube_config }}",
			}
			c.EnvInjectors = map[string]string{"KUBECONFIG": "{{ tower.filename.a }}"}
		}),
	}, { // Test 8: A file nothing points a kubeconfig variable at is not a kubeconfig credential.
		Type: shaped(func(c *CredentialType) {
			c.EnvInjectors = map[string]string{"CLOUD_CONFIG": "{{ tower.filename }}"}
		}),
	}, { // Test 9: A type that writes no file is not one.
		Type: shaped(func(c *CredentialType) {
			c.FileInjectors = nil
			c.EnvInjectors = map[string]string{"KUBECONFIG": "{{ kube_config }}"}
		}),
	}, { // Test 10: A file template naming a field the type does not declare is not one.
		Type: shaped(func(c *CredentialType) {
			c.FileInjectors = map[string]string{"template": "{{ other }}"}
		}),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			field, ok := test.Type.KubeconfigShaped()
			if ok != test.WantOK || field != test.WantField {
				t.Errorf("KubeconfigShaped() = %q, %v, want %q, %v", field, ok, test.WantField,
					test.WantOK)
			}
		})
	}
}
