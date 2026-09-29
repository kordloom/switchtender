package dispatch

import (
	"slices"
	"strings"
	"testing"
)

// TestADecodedCredentialFieldIsMasked covers a private key that survived redaction by changing shape.
//
// A GCP service-account credential is a JSON file, and its private_key is a PEM whose line breaks
// are escaped inside that JSON. Only the stored blob was registered with the masker, so it held the
// escaped spelling: the moment a playbook decoded the file and printed the key, the real PEM with
// real newlines matched nothing and went into the stored run log verbatim. The ssh_key path had
// already learned this lesson, for exactly the same reason, one file over.
func TestADecodedCredentialFieldIsMasked(t *testing.T) {
	t.Parallel()
	const pem = "-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ\n-----END PRIVATE KEY-----\n"
	// How it is stored: one JSON line with the newlines escaped.
	content := `{"type":"service_account","project_id":"acme-prod",` +
		`"private_key":"-----BEGIN PRIVATE KEY-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASCBKcwggSjAgEAAoIBAQ\n-----END PRIVATE KEY-----\n",` +
		`"client_email":"runner@acme-prod.iam.gserviceaccount.com"}`

	got := decodedJSONSecrets(content)
	if !slices.Contains(got, pem) {
		t.Errorf("the decoded private key is not registered with the masker, so a run that reads "+
			"the file and prints the key writes it into the log in the clear.\ngot: %q", got)
	}
	// Only secret material is registered. Handing the masker ordinary identifiers, a service
	// account's type or its project, would turn redaction into a search for common words that
	// scribbles over legitimate run output.
	for _, v := range got {
		if strings.EqualFold(v, "service_account") || strings.EqualFold(v, "acme-prod") {
			t.Errorf("registered %q, which is an identifier rather than a secret", v)
		}
	}

	// A file that is not a JSON object yields nothing rather than guessing.
	if out := decodedJSONSecrets("-----BEGIN OPENSSH PRIVATE KEY-----\nabc\n"); len(out) != 0 {
		t.Errorf("a non-JSON credential produced %q, want nothing", out)
	}
}
