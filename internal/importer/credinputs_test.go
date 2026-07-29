package importer

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
)

// TestMachineKindFollowsInputs verifies an AWX machine credential is classified by what it actually
// holds. AWX uses one machine type for both key and password login, so mapping the name alone turned
// every password credential into an SSH key credential, which then failed at run time.
func TestMachineKindFollowsInputs(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Inputs   map[string]any
		AWXType  string
		WantKind credential.Kind
	}{{ // Test 0: A key credential keeps the key kind.
		AWXType: "Machine", Inputs: map[string]any{"username": "deploy", "ssh_key_data": "$encrypted$"},
		WantKind: credential.KindSSHKey,
	}, { // Test 1: A password credential is a password credential, not a key one.
		AWXType: "Machine", Inputs: map[string]any{"username": "deploy", "password": "$encrypted$"},
		WantKind: credential.KindSSHPassword,
	}, { // Test 2: A key wins when AWX recorded both, since that is what AWX prefers.
		AWXType: "Machine",
		Inputs: map[string]any{
			"ssh_key_data": "$encrypted$", "password": "$encrypted$",
		},
		WantKind: credential.KindSSHKey,
	}, { // Test 3: No inputs at all keeps the historical default rather than guessing.
		AWXType: "Machine", Inputs: nil, WantKind: credential.KindSSHKey,
	}, { // Test 4: An empty value is not a configured value.
		AWXType: "Machine", Inputs: map[string]any{"password": "", "ssh_key_data": ""},
		WantKind: credential.KindSSHKey,
	}, { // Test 5: Source control follows the same rule.
		AWXType: "Source Control", Inputs: map[string]any{"password": "$encrypted$"},
		WantKind: credential.KindSSHPassword,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, exact := mapCredentialKind(test.AWXType, test.Inputs)
			if got != test.WantKind {
				t.Errorf("mapCredentialKind(%q, %v) = %q, want %q",
					test.AWXType, test.Inputs, got, test.WantKind)
			}
			if !exact {
				t.Errorf("mapCredentialKind(%q) reported an inexact mapping", test.AWXType)
			}
		})
	}
}

// TestPublicInputsNeverCarriesSecrets verifies only allowlisted, non-secret inputs are reported. The
// result is shown to operators and stored in the migration plan, so a secret reaching it would be a
// leak, and the allowlist rather than AWX's encrypted marker is what decides.
func TestPublicInputsNeverCarriesSecrets(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Inputs map[string]any
		Want   []string
	}{{ // Test 0: Non-secret settings survive, sorted, and secrets do not.
		Inputs: map[string]any{
			"username": "deploy", "become_method": "sudo",
			"password": "hunter2", "ssh_key_data": "-----BEGIN KEY-----",
		},
		Want: []string{"become_method=sudo", "username=deploy"},
	}, { // Test 1: AWX's marker is not a value worth reporting.
		Inputs: map[string]any{"username": "$encrypted$", "region": "us-east-1"},
		Want:   []string{"region=us-east-1"},
	}, { // Test 2: An unrecognized key is withheld, since it could be a custom type's secret.
		Inputs: map[string]any{"my_custom_api_key": "sk-live-1234", "host": "vc.example.com"},
		Want:   []string{"host=vc.example.com"},
	}, { // Test 3: Empty values are dropped rather than reported as blanks.
		Inputs: map[string]any{"username": "", "domain": "   "},
		Want:   nil,
	}, { // Test 4: Non-string values still render.
		Inputs: map[string]any{"validate_certs": false, "authorize": true},
		Want:   []string{"authorize=true", "validate_certs=false"},
	}, { // Test 5: No inputs at all is not an error.
		Inputs: nil, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := publicInputs(test.Inputs)
			if diff := cmp.Diff(test.Want, got); diff != "" {
				t.Errorf("publicInputs() mismatch (-want +got):\n%s", diff)
			}
			for _, pair := range got {
				if strings.Contains(pair, "hunter2") || strings.Contains(pair, "BEGIN KEY") ||
					strings.Contains(pair, "sk-live") {
					t.Errorf("publicInputs() leaked a secret: %q", pair)
				}
			}
		})
	}
}

// TestImportReportsNonSecretInputs verifies a real export's non-secret settings reach the operator.
// Without this the import dropped them silently and the operator had to open AWX to find out which
// user a playbook connected as.
func TestImportReportsNonSecretInputs(t *testing.T) {
	t.Parallel()
	data := []byte(`{"credentials":[
		{"name":"prod-ssh","credential_type":"Machine","inputs":{
			"username":"deploy","become_method":"sudo","password":"$encrypted$"}}]}`)
	plan, err := FromAWX(data, time.Date(2026, 7, 29, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatalf("FromAWX() error = %v", err)
	}
	if len(plan.Credentials) != 1 {
		t.Fatalf("credentials = %d, want 1", len(plan.Credentials))
	}
	if plan.Credentials[0].Kind != credential.KindSSHPassword {
		t.Errorf("kind = %q, want ssh_password since AWX recorded a password and no key",
			plan.Credentials[0].Kind)
	}
	joined := strings.Join(plan.Warnings, "\n")
	for _, want := range []string{"username=deploy", "become_method=sudo"} {
		if !strings.Contains(joined, want) {
			t.Errorf("warnings do not mention %q, got:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "$encrypted$") {
		t.Errorf("warnings echo AWX's encrypted marker:\n%s", joined)
	}
}
