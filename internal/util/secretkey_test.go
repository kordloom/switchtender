package util

import (
	"fmt"
	"strings"
	"testing"
)

// TestSecretKeyBoundaries pushes the one classifier the audit digest, the inventory redactor, and
// the run-log masker all consult to its edges.
//
// Everything downstream of it fails open: a name it calls ordinary has its value committed to the
// chain digest, served in an inventory, disclosed in a receipt, and never handed to the masker, so
// the value lands in the stored run log verbatim. The cases below are the ones where "does this
// name look secret" is a judgment rather than an obvious yes.
func TestSecretKeyBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the variable or field name.
		In string
		// WantResult is whether the value stored under it is secret material.
		WantResult bool
	}{
		{In: "PASS", WantResult: true},                  // Test 0: The bare name, shouted.
		{In: "Pass", WantResult: true},                  // Test 1: And mixed.
		{In: "become_pass", WantResult: true},           // Test 2: A terminal _pass.
		{In: "_pass", WantResult: true},                 // Test 3: The suffix with an empty stem.
		{In: "pass_phrase", WantResult: false},          // Test 4: Split, so no stem matches.
		{In: "passphrase", WantResult: true},            // Test 5: Joined, and a listed stem.
		{In: "passes", WantResult: false},               // Test 6: Not a terminal _pass.
		{In: "pass_word", WantResult: false},            // Test 7: Same, and password split.
		{In: "compass", WantResult: false},              // Test 8: Ends in pass, is not one.
		{In: "encompass", WantResult: false},            // Test 9: Same.
		{In: "FIELDS", WantResult: true},                // Test 10: The secret bag, shouted.
		{In: "fields_count", WantResult: false},         // Test 11: Only the exact field matches.
		{In: "custom_fields", WantResult: false},        // Test 12: Same.
		{In: "authorization", WantResult: true},         // Test 13: A header value is the credential.
		{In: "Authorization", WantResult: true},         // Test 14: As written on a curl line.
		{In: "proxy_authorization", WantResult: true},   // Test 15: The stem matches anywhere.
		{In: "aws_secret_access_key", WantResult: true}, // Test 16: Contains secret.
		{In: "refresh_token", WantResult: true},         // Test 17: Contains token.
		{In: "tokenizer", WantResult: true},             // Test 18: Over-matching is the safe side.
		{In: "ssh_private_key", WantResult: true},       // Test 19: Key material.
		{In: "privatekey_pem", WantResult: true},        // Test 20: The unseparated spelling.
		{In: "apikey", WantResult: true},                // Test 21: Unseparated.
		{In: "api_key", WantResult: true},               // Test 22: Underscore separated.
		{In: "public_key", WantResult: false},           // Test 23: A public key is not a secret.
		{In: "key", WantResult: false},                  // Test 24: Too broad to be a stem.
		{In: "username", WantResult: false},             // Test 25: An identifier, not a credential.
		{In: "", WantResult: false},                     // Test 26: Nothing.
		{In: "   ", WantResult: false},                  // Test 27: Whitespace only.
		{In: "пароль", WantResult: false},               // Test 28: A stem list is English only.
		{In: "ansible_password ", WantResult: true},     // Test 29: A trailing space does not hide it.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := SecretKey(test.In); got != test.WantResult {
				t.Errorf("SecretKey(%q) = %v, want %v", test.In, got, test.WantResult)
			}
		})
	}
}

// TestSecretKeyOnAVeryLongName proves the classifier does not fall over on a name no human wrote.
// Names reach it from an uploaded inventory and a submitted extra var, so their length is somebody
// else's choice, and the stem has to be found wherever it sits.
func TestSecretKeyOnAVeryLongName(t *testing.T) {
	t.Parallel()
	pad := strings.Repeat("a", 100000)
	if !SecretKey(pad + "_password_" + pad) {
		t.Error("SecretKey missed a stem buried in a long name, so its value is served in the clear")
	}
	if SecretKey(pad) {
		t.Error("SecretKey called a long ordinary name secret")
	}
}

// TestSecretKeyMissesHyphenatedNames demonstrates a name whose value is a credential and which the
// classifier calls ordinary.
//
// The stem list carries apikey and api_key but not api-key, and privatekey and private_key but not
// private-key. Both assignment patterns accept a hyphen in a name, and an HTTP header is
// conventionally written with hyphens, so X-Api-Key: SECRET in a bash run's command line, in an
// inventory's content, or as a YAML key is read as an ordinary assignment. Its value is then
// committed to the audit content digest, served by the inventory reader, disclosed in a receipt and
// a dossier, and never handed to the run-log masker, so a set -x echoes it into the stored log. The
// Authorization stem was added for exactly this shape, which is what says hyphenated header names
// are in scope rather than out of it.
func TestSecretKeyMissesHyphenatedNames(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// In is the name as a header, a flag, or a YAML key writes it.
		In string
	}{
		{In: "x-api-key"},   // Test 0: The header form of an API key.
		{In: "api-key"},     // Test 1: The bare hyphenated spelling.
		{In: "private-key"}, // Test 2: The hyphenated spelling of key material.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if !SecretKey(test.In) {
				t.Errorf("SecretKey(%q) = false, so its value is committed and served in the clear",
					test.In)
			}
		})
	}
}

// TestRedactAssignmentsMissesHyphenatedHeaders is the reachable end of the same defect: the exact
// text an operator submits as a one-line shell run, with the credential still in it afterward.
func TestRedactAssignmentsMissesHyphenatedHeaders(t *testing.T) {
	t.Parallel()
	const secret = "SUPERSECRETAPIKEY"
	in := `curl -H "X-Api-Key: ` + secret + `" https://api.example.com/deploy`
	got, found := RedactAssignments(in, "«redacted»")
	if strings.Contains(got, secret) {
		t.Errorf("RedactAssignments() = %q, want the header value masked", got)
	}
	var reported bool
	for _, a := range found {
		if a.Value == secret || strings.Contains(a.Value, secret) {
			reported = true
		}
	}
	if !reported {
		t.Errorf("found = %v, want the value reported so the run-log masker can match it", found)
	}
}
