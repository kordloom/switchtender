package importer

import (
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
