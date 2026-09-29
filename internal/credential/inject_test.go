package credential

import (
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestInject covers the built-in typed injectors: the env vars each cloud kind emits, the optional
// fields, the GCP file binding, missing required fields, and an unknown kind.
func TestInject(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name    string
		Kind    Kind
		Secret  string
		Want    Injection
		WantErr error
	}{{ // Test 0: AWS with every field, including the optional session token and region.
		Name:   "aws full",
		Kind:   KindAWS,
		Secret: "access_key=AKIA\nsecret_key=SEKRIT\nsession_token=TOK\nregion=us-east-1",
		Want: Injection{Env: []string{
			"AWS_ACCESS_KEY_ID=AKIA",
			"AWS_SECRET_ACCESS_KEY=SEKRIT",
			"AWS_SESSION_TOKEN=TOK",
			"AWS_DEFAULT_REGION=us-east-1",
			"AWS_REGION=us-east-1",
		}},
	}, { // Test 1: AWS with only the required fields.
		Name:   "aws minimal",
		Kind:   KindAWS,
		Secret: "ACCESS_KEY=AKIA\nSECRET_KEY=SEKRIT",
		Want: Injection{Env: []string{
			"AWS_ACCESS_KEY_ID=AKIA",
			"AWS_SECRET_ACCESS_KEY=SEKRIT",
		}},
	}, { // Test 2: AWS missing the secret key is a bad-field error.
		Name:    "aws missing secret",
		Kind:    KindAWS,
		Secret:  "access_key=AKIA",
		WantErr: ErrBadField,
	}, { // Test 3: Azure emits both the ARM_ and AZURE_ variables.
		Name:   "azure full",
		Kind:   KindAzure,
		Secret: "client_id=CID\nsecret=CSEC\nsubscription_id=SUB\ntenant_id=TEN",
		Want: Injection{Env: []string{
			"AZURE_CLIENT_ID=CID",
			"AZURE_SECRET=CSEC",
			"AZURE_SUBSCRIPTION_ID=SUB",
			"AZURE_TENANT=TEN",
			"ARM_CLIENT_ID=CID",
			"ARM_CLIENT_SECRET=CSEC",
			"ARM_SUBSCRIPTION_ID=SUB",
			"ARM_TENANT_ID=TEN",
		}},
	}, { // Test 4: Azure missing the tenant is a bad-field error.
		Name:    "azure missing tenant",
		Kind:    KindAzure,
		Secret:  "client_id=CID\nsecret=CSEC\nsubscription_id=SUB",
		WantErr: ErrBadField,
	}, { // Test 5: GCP writes the JSON to a file bound to both credential path variables.
		Name:   "gcp json",
		Kind:   KindGCP,
		Secret: `{"type":"service_account","project_id":"x"}`,
		Want: Injection{Files: []InjectionFile{{
			EnvVars: []string{"GOOGLE_APPLICATION_CREDENTIALS", "GCP_SERVICE_ACCOUNT_FILE"},
			Content: `{"type":"service_account","project_id":"x"}`,
		}}},
	}, { // Test 6: GCP that is not JSON is a bad-field error.
		Name:    "gcp not json",
		Kind:    KindGCP,
		Secret:  "access_key=AKIA",
		WantErr: ErrBadField,
	}, { // Test 7: VMware with the optional validate_certs field.
		Name:   "vmware full",
		Kind:   KindVMware,
		Secret: "host=vc01\nuser=root\npassword=pw\nvalidate_certs=false",
		Want: Injection{Env: []string{
			"VMWARE_HOST=vc01",
			"VMWARE_USER=root",
			"VMWARE_PASSWORD=pw",
			"VMWARE_VALIDATE_CERTS=false",
		}},
	}, { // Test 8: VMware missing the password is a bad-field error.
		Name:    "vmware missing password",
		Kind:    KindVMware,
		Secret:  "host=vc01\nuser=root",
		WantErr: ErrBadField,
	}, { // Test 9: An unregistered kind is a bad-kind error.
		Name:    "unknown kind",
		Kind:    Kind("nope"),
		Secret:  "x=y",
		WantErr: ErrBadKind,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got, err := Inject(test.Kind, test.Secret)
			if !errors.Is(err, test.WantErr) {
				t.Fatalf("Inject() error = %v, want %v", err, test.WantErr)
			}
			if test.WantErr != nil {
				return
			}
			if diff := cmp.Diff(test.Want, got); diff != "" {
				t.Errorf("Inject() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestValidKind confirms the fixed kinds and the registered typed kinds are valid, and an unknown
// kind is not.
func TestValidKind(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Kind Kind
		Want bool
	}{
		{KindSSHKey, true},
		{KindEnv, true},
		{KindToken, true},
		{KindAWS, true},
		{KindAzure, true},
		{KindGCP, true},
		{KindVMware, true},
		{Kind("nope"), false},
		{Kind(""), false},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Kind), func(t *testing.T) {
			t.Parallel()
			if got := ValidKind(test.Kind); got != test.Want {
				t.Errorf("ValidKind(%q) = %v, want %v", test.Kind, got, test.Want)
			}
		})
	}
}

// TestRegisterInjectorPanics confirms the registration guards reject an empty kind, a nil injector,
// and a duplicate. None of these mutate the registry, so they are safe to run in parallel.
func TestRegisterInjectorPanics(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name string
		Kind Kind
		Inj  Injector
	}{
		{"empty kind", Kind(""), awsInject},
		{"nil injector", Kind("custom_nil"), nil},
		{"duplicate", KindAWS, awsInject},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			defer func() {
				if recover() == nil {
					t.Errorf("RegisterInjector(%q) did not panic", test.Kind)
				}
			}()
			RegisterInjector(test.Kind, test.Inj)
		})
	}
}
