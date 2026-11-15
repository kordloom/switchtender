package importer

import (
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
)

// blankedSentence is a fragment only the blanked-secrets preview sentence carries.
const blankedSentence = "writes every secret as an empty value instead of $encrypted$"

// blankedPreview is the whole blanked-secrets preview sentence, as the preview prints it.
const blankedPreview = "This export writes every secret as an empty value instead of " +
	"$encrypted$, as upstream awxkit through 23.7.0 does. AWX never keeps an empty credential " +
	"secret, so each empty secret input on a credential is read as one AWX held and listed below " +
	"to enter again."

// refusedSvcType is an exported custom credential type that does not come across, since its
// injector uses a filter, with one secret field and one that is not.
const refusedSvcType = `"credential_types": [{"name": "Svc API", "kind": "cloud",
  "inputs": {"fields": [{"id": "api_user", "label": "User"},
    {"id": "api_secret", "label": "Secret", "secret": true}]},
  "injectors": {"env": {"SVC_SECRET": "{{ api_secret | upper }}"}}}]`

// TestAWXReadsASecretInputByPresence pins that a credential's secret is read by whether the export
// lists the input, not by what it wrote there. AWX deletes a secret input that holds no value, so
// one listed as "", which upstream awxkit 17.1.0 through 23.7.0 writes for every secret, or as ASK,
// which AWX stores for one it asks for at launch, was held. Read by value, such a file lost its
// become password shell, picked the key kind for a password credential, and told the operator
// every credential held no secret.
//
//nolint:funlen // Test function.
func TestAWXReadsASecretInputByPresence(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Doc is the export's top level, less its braces. Its credential "c" is the one judged.
		Doc string
		// WantKind is the kind credential "c" imports as.
		WantKind credential.Kind
		// WantBecome reports whether "c (become)" is split out of it.
		WantBecome bool
		// WantHeld reports whether its line says its secret needs re-entering rather than that the
		// export lists none.
		WantHeld bool
		// WantPrompted is the fragment of the line saying AWX asked for it at launch, empty for none.
		WantPrompted string
		// WantBlanked reports whether the preview says the file writes every secret empty.
		WantBlanked bool
	}{{ // Test 0: A key written empty is a key AWX held.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"username": "deploy", "ssh_key_data": ""}}]`,
		WantKind: credential.KindSSHKey, WantHeld: true, WantBlanked: true,
	}, { // Test 1: A password written empty is a password AWX held.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"username": "deploy", "password": ""}}]`,
		WantKind: credential.KindSSHPassword, WantHeld: true, WantBlanked: true,
	}, { // Test 2: Both written empty, and the key wins as it does when both are marked.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"ssh_key_data": "", "password": ""}}]`,
		WantKind: credential.KindSSHKey, WantHeld: true, WantBlanked: true,
	}, { // Test 3: A become password written empty is split out as its own shell.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"password": "", "become_method": "sudo", "become_password": ""}}]`,
		WantKind: credential.KindSSHPassword, WantBecome: true, WantHeld: true, WantBlanked: true,
	}, { // Test 4: No become password listed makes no shell, and no secret listed holds none.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"username": "deploy", "become_method": "sudo"}}]`,
		WantKind: credential.KindSSHKey,
	}, { // Test 5: A Galaxy credential for the public server lists no secret, and its empty auth
		// URL is not one.
		Doc: `"credentials": [{"name": "c",
		  "credential_type": {"name": "Ansible Galaxy/Automation Hub API Token"},
		  "inputs": {"url": "https://galaxy.ansible.com", "auth_url": ""}}]`,
		WantKind: credential.KindToken,
	}, { // Test 6: A file holding the marker still reads an empty secret as held, and does not
		// say it writes every secret empty.
		Doc: `"credentials": [{"name": "marked", "credential_type": {"name": "Vault"},
		  "inputs": {"vault_password": "$encrypted$"}},
		  {"name": "c", "credential_type": {"name": "Machine"}, "inputs": {"password": ""}}]`,
		WantKind: credential.KindSSHPassword, WantHeld: true,
	}, { // Test 7: A secret field of an exported type that did not come across, written empty, is
		// held, and the file is not judged by a field that is not built in.
		Doc: refusedSvcType + `, "credentials": [{"name": "c",
		  "credential_type": {"name": "Svc API"}, "inputs": {"api_secret": ""}}]`,
		WantKind: credential.KindEnv, WantHeld: true,
	}, { // Test 8: A field that type does not mark secret, written empty, is not a secret.
		Doc: refusedSvcType + `, "credentials": [{"name": "c",
		  "credential_type": {"name": "Svc API"}, "inputs": {"api_user": ""}}]`,
		WantKind: credential.KindEnv,
	}, { // Test 9: A become password AWX asks for at launch is still split out, and said so.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"password": "$encrypted$", "become_password": "ASK"}}]`,
		WantKind: credential.KindSSHPassword, WantBecome: true, WantHeld: true,
		WantPrompted: "is set in AWX to ask for become_password at launch",
	}, { // Test 10: A credential whose only secret AWX asks for at launch holds one.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"username": "deploy", "password": "ASK"}}]`,
		WantKind: credential.KindSSHPassword, WantHeld: true,
		WantPrompted: "is set in AWX to ask for password at launch",
	}, { // Test 11: A null secret is unset.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"password": null, "become_password": null}}]`,
		WantKind: credential.KindSSHKey,
	}, { // Test 12: A secret that is not a string is unset.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"password": 0}}]`,
		WantKind: credential.KindSSHKey,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte("{"+test.Doc+"}"), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			joined := strings.Join(plan.Warnings, "\n")
			byName := map[string]*credential.Credential{}
			for _, c := range plan.Credentials {
				byName[c.Name] = c
			}
			c, ok := byName["c"]
			if !ok {
				t.Fatalf("credential c was not imported.\nwarnings:\n%s", joined)
			}
			if c.Kind != test.WantKind {
				t.Errorf("kind = %q, want %q", c.Kind, test.WantKind)
			}
			become, gotBecome := byName["c (become)"]
			if gotBecome != test.WantBecome {
				t.Errorf("become shell split out = %v, want %v.\nwarnings:\n%s", gotBecome,
					test.WantBecome, joined)
			}
			if gotBecome && become.Kind != credential.KindBecomePassword {
				t.Errorf("become shell kind = %q, want %q", become.Kind, credential.KindBecomePassword)
			}
			_, held := warningContaining(t, plan.Warnings, `credential "c" needs its secret re-entered`)
			_, none := warningContaining(t, plan.Warnings, `credential "c": the export lists no `+
				"secret for it, so there is nothing to re-enter")
			if held != test.WantHeld || none == test.WantHeld {
				t.Errorf("says re-enter = %v and says none listed = %v, want held %v.\nwarnings:\n%s",
					held, none, test.WantHeld, joined)
			}
			if w, ok := warningContaining(t, plan.Warnings, "had no secret in AWX"); ok {
				t.Errorf("a line still says what AWX held rather than what the export lists: %s", w)
			}
			_, prompted := warningContaining(t, plan.Warnings, "to ask for")
			if test.WantPrompted == "" && prompted {
				t.Errorf("a line says AWX asked for a secret at launch.\nwarnings:\n%s", joined)
			}
			if test.WantPrompted != "" {
				if _, ok := warningContaining(t, plan.Warnings, `credential "c"`, test.WantPrompted,
					"does not ask for a credential's secret"); !ok {
					t.Errorf("no line says %q.\nwarnings:\n%s", test.WantPrompted, joined)
				}
			}
			if got := slices.Contains(plan.Warnings, blankedPreview); got != test.WantBlanked {
				t.Errorf("blanked-secrets sentence = %v, want %v.\nwarnings:\n%s", got,
					test.WantBlanked, joined)
			}
		})
	}
}

// TestAWXHeldSecretReadsWhatTheExportLists pins awxHeldSecret by itself: a secret input listed as a
// string, a built-in one or one the credential's exported type declares, or any input carrying the
// marker, is a secret AWX held. Anything else is not.
func TestAWXHeldSecretReadsWhatTheExportLists(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Inputs are the credential's inputs.
		Inputs map[string]any
		// Declared are the inputs its exported type marks secret.
		Declared []string
		// WantHeld reports whether a secret is listed.
		WantHeld bool
	}{{ // Test 0: Asked for at launch.
		Inputs: map[string]any{"password": "ASK"}, WantHeld: true,
	}, { // Test 1: Written empty.
		Inputs: map[string]any{"password": ""}, WantHeld: true,
	}, { // Test 2: A Galaxy server's address is not a secret.
		Inputs: map[string]any{"url": "https://galaxy.ansible.com"},
	}, { // Test 3: A null password is unset.
		Inputs: map[string]any{"password": nil},
	}, { // Test 4: An empty user name is not a secret.
		Inputs: map[string]any{"username": ""},
	}, { // Test 5: A declared secret written empty is held.
		Inputs: map[string]any{"api_secret": ""}, Declared: []string{"api_secret"}, WantHeld: true,
	}, { // Test 6: The same input with no type declaring it is not.
		Inputs: map[string]any{"api_secret": ""},
	}, { // Test 7: The marker on any input is held, declared or not.
		Inputs: map[string]any{"kube_config": "$encrypted$"}, WantHeld: true,
	}, { // Test 8: No inputs at all.
		Inputs: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := awxHeldSecret(test.Inputs, test.Declared); got != test.WantHeld {
				t.Errorf("awxHeldSecret(%v, %v) = %v, want %v", test.Inputs, test.Declared, got,
					test.WantHeld)
			}
		})
	}
}

// TestAWXBlankedSecretsNeedsNoMarkerAndAnEmptySecret pins the file-level reading behind the preview
// sentence. A file says it writes every secret empty only when it holds no $encrypted$ anywhere and
// a credential lists a built-in secret input as exactly "". A notification's empty password does
// not count, since AWX keeps one for a notification that has none.
func TestAWXBlankedSecretsNeedsNoMarkerAndAnEmptySecret(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Doc is the export's top level, less its braces.
		Doc string
		// WantBlanked reports whether the file is read as one that writes every secret empty.
		WantBlanked bool
	}{{ // Test 0: An empty password and no marker.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"password": ""}}]`,
		WantBlanked: true,
	}, { // Test 1: An empty vault password counts the same.
		Doc: `"credentials": [{"name": "v", "credential_type": {"name": "Vault"},
		  "inputs": {"vault_password": "", "vault_id": "prod"}}]`,
		WantBlanked: true,
	}, { // Test 2: A marker anywhere else in the file, here a survey default, says it is not.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"password": ""}}],
		  "job_templates": [{"name": "j", "playbook": "p.yml", "survey_spec": {"spec": [
		    {"variable": "pw", "type": "password", "default": "$encrypted$"}]}}]`,
	}, { // Test 3: An empty user name is not a secret.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"username": ""}}]`,
	}, { // Test 4: A null secret is not an empty one.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"password": null}}]`,
	}, { // Test 5: Whitespace is not what the old client writes.
		Doc: `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"password": "  "}}]`,
	}, { // Test 6: A notification's empty password does not count.
		Doc: `"notification_templates": [{"name": "hook", "notification_type": "webhook",
		  "notification_configuration": {"url": "https://hooks.example.com/x", "headers": {},
		    "http_method": "POST", "username": "", "password": ""}}],
		  "credentials": [{"name": "c", "credential_type": {"name": "Machine"},
		  "inputs": {"username": "deploy"}}]`,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte("{"+test.Doc+"}"), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			if plan.awxBlanked != test.WantBlanked {
				t.Errorf("awxBlanked = %v, want %v", plan.awxBlanked, test.WantBlanked)
			}
			if got := slices.Contains(plan.Warnings, blankedPreview); got != test.WantBlanked {
				t.Errorf("blanked-secrets sentence = %v, want %v.\nwarnings:\n%s", got,
					test.WantBlanked, strings.Join(plan.Warnings, "\n"))
			}
		})
	}
}

// TestAWXBlankedSurveyDefaultIsListed pins the survey line. A file that writes every secret empty
// writes a password question's set default the same way, and AWX keeps an empty default for one
// that has none, so the import cannot tell which it was and lists it. A file that holds the marker
// wrote a set default as the marker, so an empty default there is no default.
func TestAWXBlankedSurveyDefaultIsListed(t *testing.T) {
	t.Parallel()
	const blankedCred = `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
	  "inputs": {"password": ""}}]`
	const markedCred = `"credentials": [{"name": "c", "credential_type": {"name": "Machine"},
	  "inputs": {"password": "$encrypted$"}}]`
	survey := func(question string) string {
		return `"job_templates": [{"name": "j", "playbook": "p.yml",
		  "survey_spec": {"spec": [` + question + `]}}]`
	}
	const line = `template "j" secret field "pw" arrives without its default: this export writes a ` +
		"set default as an empty value, the same as no default, so set it on the template if " +
		"launches relied on one"
	tests := []struct {
		// Doc is the export's top level, less its braces.
		Doc string
		// WantLine reports whether the survey line is listed.
		WantLine bool
	}{{ // Test 0: An empty password default in a file that writes every secret empty.
		Doc:      blankedCred + `, ` + survey(`{"variable": "pw", "type": "password", "default": ""}`),
		WantLine: true,
	}, { // Test 1: The same default in a file that holds the marker is no default.
		Doc: markedCred + `, ` + survey(`{"variable": "pw", "type": "password", "default": ""}`),
	}, { // Test 2: A password question with no default at all is not listed.
		Doc: blankedCred + `, ` + survey(`{"variable": "pw", "type": "password"}`),
	}, { // Test 3: A question that is not a password keeps its empty default.
		Doc: blankedCred + `, ` + survey(`{"variable": "pw", "type": "text", "default": ""}`),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			plan, err := FromAWX([]byte("{"+test.Doc+"}"), importNow)
			if err != nil {
				t.Fatalf("FromAWX() error = %v", err)
			}
			joined := strings.Join(plan.Warnings, "\n")
			if got := slices.Contains(plan.Warnings, line); got != test.WantLine {
				t.Errorf("survey line = %v, want %v.\nwarnings:\n%s", got, test.WantLine, joined)
			}
			if w, ok := warningContaining(t, plan.Warnings, `secret field "pw"`,
				"because an export never carries a secret readably"); ok {
				t.Errorf("an empty default is reported as one the export carried: %s", w)
			}
		})
	}
}

// TestAWXSecretInputsAreNeverPublic pins that no input read as a secret is one whose value the
// report prints. The vault label is neither: it is carried on its own.
func TestAWXSecretInputsAreNeverPublic(t *testing.T) {
	t.Parallel()
	var overlap []string
	for _, key := range awxSecretInputs {
		if slices.Contains(awxPublicInputs, key) {
			overlap = append(overlap, key)
		}
	}
	if diff := cmp.Diff([]string(nil), overlap); diff != "" {
		t.Errorf("secret inputs the report would print (-want +got):\n%s", diff)
	}
	for name, list := range map[string][]string{
		"awxSecretInputs": awxSecretInputs, "awxPublicInputs": awxPublicInputs,
	} {
		if slices.Contains(list, "vault_id") {
			t.Errorf("%s holds vault_id, which is neither a secret nor printed as a setting", name)
		}
	}
}
