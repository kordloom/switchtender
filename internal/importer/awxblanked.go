package importer

import (
	"bytes"
	"slices"
)

// awxSecretInputs are the inputs AWX's built-in credential types mark secret, from its managed
// types at 23.7.0. AWX deletes such an input when it holds no value, so one an export lists was set
// in AWX, whatever the export wrote for it.
var awxSecretInputs = []string{
	"authorize_password", "bearer_token", "become_password", "gpg_public_key", "oauth_token",
	"password", "secret", "security_token", "ssh_key_data", "ssh_key_unlock", "ssh_public_key_data",
	"ssl_ca_cert", "token", "vault_password",
}

// awxPrompted is the value AWX stores for a secret input it asks for at each launch.
const awxPrompted = "ASK"

// awxDeclaredSecrets returns, by type name, the inputs each credential type in the export marks
// secret, so a credential of a type that did not come across still has its own secrets read.
func awxDeclaredSecrets(types []awxCredentialType) map[string][]string {
	out := map[string][]string{}
	for _, ct := range types {
		for _, f := range ct.Inputs.Fields {
			if f.Secret && f.ID != "" {
				out[ct.Name] = append(out[ct.Name], f.ID)
			}
		}
	}
	return out
}

// awxSecretInput reports whether a credential input is one AWX keeps secret: a built-in secret
// input, or one the credential's exported type declares secret.
func awxSecretInput(key string, declared []string) bool {
	return slices.Contains(awxSecretInputs, key) || slices.Contains(declared, key)
}

// awxPromptedInputs returns, sorted, the secret inputs AWX asks for at each launch rather than
// stores, which it writes as the literal ASK.
func awxPromptedInputs(inputs map[string]any, declared []string) []string {
	var out []string
	for key, v := range inputs {
		if s, ok := v.(string); ok && s == awxPrompted && awxSecretInput(key, declared) {
			out = append(out, key)
		}
	}
	slices.Sort(out)
	return out
}

// awxBlankedSecrets reports whether an export writes every secret as an empty value, as upstream
// awxkit 17.1.0 through 23.7.0 does: it ends each object's export by writing every $encrypted$ as
// an empty string and keeping the key. Such a file holds no marker anywhere, and at least one
// credential lists a built-in secret input as an empty value, which AWX never stores.
//
// A credential is read by the same rule in every file, so this decides only what the preview says
// and whether a secret survey default the file wrote empty is listed. AWX keeps an empty survey
// default for a question that has none, so in such a file the two cannot be told apart.
func awxBlankedSecrets(data []byte, creds []awxCredential) bool {
	if bytes.Contains(data, []byte(awxEncrypted)) {
		return false
	}
	for _, c := range creds {
		for _, key := range awxSecretInputs {
			if v, ok := c.Inputs[key].(string); ok && v == "" {
				return true
			}
		}
	}
	return false
}

// warnBlankedSecrets says so in the preview when an export writes every secret as an empty value,
// and records it on the plan so each secret survey default the file wrote empty is listed.
func (p *Plan) warnBlankedSecrets(data []byte, export awxExport) {
	if !awxBlankedSecrets(data, export.Credentials) {
		return
	}
	p.awxBlanked = true
	p.warn("This export writes every secret as an empty value instead of $encrypted$, as upstream " +
		"awxkit through 23.7.0 does. AWX never keeps an empty credential secret, so each empty " +
		"secret input on a credential is read as one AWX held and listed below to enter again.")
}
