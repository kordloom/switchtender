package credential

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/kordloom/switchtender/internal/util"
)

// KubeconfigEnvVars are the environment variables a kubeconfig credential's written path is bound
// to: KUBECONFIG for kubectl and helm, K8S_AUTH_KUBECONFIG for the kubernetes.core collection, and
// KUBE_CONFIG_PATH for Terraform's kubernetes and helm providers.
var KubeconfigEnvVars = []string{"KUBECONFIG", "K8S_AUTH_KUBECONFIG", "KUBE_CONFIG_PATH"}

// clientKeyData is the kubeconfig key holding a base64 private key. The shared classifier does not
// recognize the name, so it is named here.
const clientKeyData = "client-key-data"

// kubeconfigInject writes a kubeconfig document to a private file bound to KubeconfigEnvVars.
//
// The document is checked to be a YAML mapping, so a value pasted into the wrong kind fails here with
// a sentence rather than in kubectl with a parse error. Its secret values are named one by one for
// masking rather than the whole file, because the masker redacts each line of a value it is given and
// a kubeconfig's ordinary lines, such as "kind: Config", would then vanish from every run's output.
func kubeconfigInject(secret string) (Injection, error) {
	content := strings.TrimSpace(secret)
	if content == "" {
		return Injection{}, fmt.Errorf("%w: kubeconfig needs a kubeconfig document", ErrBadField)
	}
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(content), &doc); err != nil {
		return Injection{}, fmt.Errorf("%w: kubeconfig is not YAML: %v", ErrBadField, err)
	}
	if len(doc.Content) == 0 || doc.Content[0].Kind != yaml.MappingNode {
		return Injection{}, fmt.Errorf("%w: kubeconfig needs a kubeconfig document, a YAML mapping",
			ErrBadField)
	}
	return Injection{
		Files: []InjectionFile{{
			EnvVars: append([]string(nil), KubeconfigEnvVars...), Content: content + "\n",
			MaskByField: true,
		}},
		Secrets: kubeconfigSecrets(doc.Content[0]),
	}, nil
}

// kubeconfigSecrets returns the secret values a kubeconfig carries: every scalar under a key the
// shared classifier calls secret, such as token or password, every client-key-data value with its
// decoded PEM, every value that is a private key, and the value of an exec plugin environment entry
// whose name is secret.
func kubeconfigSecrets(node *yaml.Node) []string {
	var out []string
	var walk func(n *yaml.Node)
	walk = func(n *yaml.Node) {
		switch n.Kind {
		case yaml.MappingNode:
			out = append(out, execEnvSecret(n)...)
			for i := 0; i+1 < len(n.Content); i += 2 {
				key, val := n.Content[i].Value, n.Content[i+1]
				if val.Kind == yaml.ScalarNode && val.Value != "" &&
					(util.SecretKey(key) || key == clientKeyData) {
					out = append(out, val.Value)
					if decoded, err := base64.StdEncoding.DecodeString(val.Value); err == nil &&
						key == clientKeyData {
						out = append(out, string(decoded))
					}
					continue
				}
				walk(val)
			}
		case yaml.SequenceNode, yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c)
			}
		case yaml.ScalarNode:
			if strings.Contains(n.Value, "PRIVATE KEY-----") {
				out = append(out, n.Value)
			}
		}
	}
	walk(node)
	return out
}

// KubeconfigShaped reports whether a custom type does nothing but write one field's value to one
// file and hand that file's path to variables the built-in kubeconfig kind also sets, and returns
// the field that holds the document.
//
// A credential of such a type can move to the kubeconfig kind and lose nothing a run receives: the
// same document lands in a private file, every variable the type set still names it, and the kind
// sets the rest of KubeconfigEnvVars beside them. What changes is the masking. The type masks the
// whole secret field, so every line of the document is redacted wherever a tool prints it, and the
// kind masks only the secrets inside the document. A type that injects anything else, an extra var,
// another variable, or a file with more in it than the one field, is not reported, because moving
// it would change what its runs receive.
func (t *CredentialType) KubeconfigShaped() (string, bool) {
	if len(t.FileInjectors) != 1 || len(t.EnvInjectors) == 0 || len(t.ExtraVarInjectors) != 0 {
		return "", false
	}
	var file, field string
	for key, tmpl := range t.FileInjectors {
		name, ok := fileName(key)
		ref, sole := soleRef(tmpl)
		if !ok || !sole || ref.field == "" || !t.fieldSet()[ref.field] {
			return "", false
		}
		file, field = name, ref.field
	}
	for name, tmpl := range t.EnvInjectors {
		ref, sole := soleRef(tmpl)
		if !sole || ref.file != file || !slices.Contains(KubeconfigEnvVars, name) {
			return "", false
		}
	}
	return field, true
}

// soleRef parses a template that is a single {{ reference }} and nothing else but space around it.
func soleRef(tmpl string) (templateRef, bool) {
	text := strings.TrimSpace(tmpl)
	m := tokenPattern.FindStringSubmatch(text)
	if m == nil || m[0] != text {
		return templateRef{}, false
	}
	ref, err := parseRef(m[1])
	if err != nil {
		return templateRef{}, false
	}
	return ref, true
}

// execEnvSecret returns the value of an exec plugin environment entry, a mapping of name and value,
// when the name classifies as secret. The secret sits under the key "value", which says nothing, so
// the name beside it is what marks it.
func execEnvSecret(n *yaml.Node) []string {
	var name, value string
	for i := 0; i+1 < len(n.Content); i += 2 {
		switch n.Content[i].Value {
		case "name":
			name = n.Content[i+1].Value
		case "value":
			value = n.Content[i+1].Value
		}
	}
	if name != "" && value != "" && util.SecretKey(name) {
		return []string{value}
	}
	return nil
}
