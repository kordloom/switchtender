package importer

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/kordloom/switchtender/internal/credential"
)

// TestMapCredentialKind covers the AWX credential-type to SwitchTender-kind mapping, including the
// typed cloud kinds, the exact machine and registry kinds, and the env fallback for unknown types.
func TestMapCredentialKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		AWXType   string
		WantKind  credential.Kind
		WantExact bool
	}{
		{"Amazon Web Services", credential.KindAWS, true},
		{"Microsoft Azure Resource Manager", credential.KindAzure, true},
		{"Google Compute Engine", credential.KindGCP, true},
		{"VMware vCenter", credential.KindVMware, true},
		{"Machine", credential.KindSSHKey, true},
		{"Source Control", credential.KindSSHKey, true},
		{"Vault", credential.KindVaultPassword, true},
		{"Container Registry", credential.KindRegistry, true},
		{"OpenShift or Kubernetes API Bearer Token", credential.KindToken, true},
		{"Red Hat Insights", credential.KindEnv, false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.AWXType), func(t *testing.T) {
			t.Parallel()
			gotKind, gotExact := mapCredentialKind(test.AWXType, nil)
			if gotKind != test.WantKind || gotExact != test.WantExact {
				t.Errorf("mapCredentialKind(%q) = %q, %v, want %q, %v",
					test.AWXType, gotKind, gotExact, test.WantKind, test.WantExact)
			}
		})
	}
}

// TestJSONScalarString covers the faithful rendering of JSON-decoded inventory and choice values.
// The old fmt.Sprintf("%v", ...) mangled non-strings: a float64 printed in scientific notation and
// lost precision past 2^53, an object printed as Go's map[k:v], and null printed as <nil>.
func TestJSONScalarString(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		In   any
		Want string
	}{
		{Name: "string", In: "plain", Want: "plain"},                 // Test 0.
		{Name: "bool", In: true, Want: "true"},                       // Test 1.
		{Name: "nil", In: nil, Want: ""},                             // Test 2.
		{Name: "json number int", In: json.Number("42"), Want: "42"}, // Test 3.
		{Name: "json number big", In: json.Number("90071992547409931"),
			Want: "90071992547409931"}, // Test 4: survives past 2^53, no float rounding.
		{Name: "json number decimal", In: json.Number("3.14"), Want: "3.14"}, // Test 5.
		{Name: "float no sci notation", In: 1000000.0, Want: "1000000"},      // Test 6.
		{Name: "object compact json", In: map[string]any{"a": "b"},
			Want: `{"a":"b"}`}, // Test 7: not Go's map[a:b].
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := jsonScalarString(test.In); got != test.Want {
				t.Errorf("jsonScalarString(%v) = %q, want %q", test.In, got, test.Want)
			}
		})
	}
}
