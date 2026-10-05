package federation

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/credential"
)

// testRoleARN is the IAM role the AWS tests assume.
const testRoleARN = "arn:aws:iam::123456789012:role/deploy"

// testProvider is the workload identity pool provider the Google tests federate through.
const testProvider = "projects/123456789/locations/global/workloadIdentityPools/ci-pool/" +
	"providers/switchtender"

// TestParseSettings pins what each federated kind accepts, what it fills in by default, and what it
// refuses when the credential is saved.
func TestParseSettings(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Kind       credential.Kind
		Settings   map[string]string
		WantConfig Config
		Want       error
	}{{ // Test 0: AWS needs only a role and defaults the rest.
		Kind: credential.KindAWSOIDC, Settings: map[string]string{"role_arn": testRoleARN},
		WantConfig: Config{Kind: credential.KindAWSOIDC, RoleARN: testRoleARN,
			Audience: DefaultAWSAudience, TokenTTL: DefaultTokenTTL, Delivery: DeliveryFile},
	}, { // Test 1: AWS with the exchange delivery, a region, a session length, and a lifetime.
		Kind: credential.KindAWSOIDC, Settings: map[string]string{
			"role_arn": testRoleARN, "region": "us-west-2", "delivery": "exchange",
			"session_duration": "1800", "token_ttl": "5m", "environment": "prod",
			"session_name": "deploy-run",
		},
		WantConfig: Config{Kind: credential.KindAWSOIDC, RoleARN: testRoleARN, Region: "us-west-2",
			Audience: DefaultAWSAudience, TokenTTL: 5 * time.Minute, Delivery: DeliveryExchange,
			SessionDuration: 1800, Environment: "prod", SessionName: "deploy-run"},
	}, { // Test 2: A misspelled key is refused rather than ignored.
		Kind:     credential.KindAWSOIDC,
		Settings: map[string]string{"role_arn": testRoleARN, "rolearn": "x"},
		Want:     ErrSetting,
	}, { // Test 3: A role that is not an IAM role ARN is refused.
		Kind: credential.KindAWSOIDC, Settings: map[string]string{"role_arn": "deploy"}, Want: ErrSetting,
	}, { // Test 4: A session length means nothing to the file delivery, so it is refused there.
		Kind:     credential.KindAWSOIDC,
		Settings: map[string]string{"role_arn": testRoleARN, "session_duration": "1800"},
		Want:     ErrSetting,
	}, { // Test 5: An endpoint a token would travel to in the clear is refused.
		Kind: credential.KindAWSOIDC, Settings: map[string]string{
			"role_arn": testRoleARN, "sts_endpoint": "http://sts.example.com",
		},
		Want: ErrSetting,
	}, { // Test 6: Google takes the provider in its short form and fills the full name and audience.
		Kind: credential.KindGCPOIDC, Settings: map[string]string{
			"provider": testProvider, "service_account": "deployer@my-project.iam.gserviceaccount.com",
		},
		WantConfig: Config{Kind: credential.KindGCPOIDC, Provider: "//iam.googleapis.com/" + testProvider,
			Audience:       "https://iam.googleapis.com/" + testProvider,
			ServiceAccount: "deployer@my-project.iam.gserviceaccount.com", TokenTTL: DefaultTokenTTL,
			Delivery: DeliveryFile, STSEndpoint: defaultGCPSTSURL, IAMEndpoint: defaultGCPIAMURL},
	}, { // Test 7: A service account that is not one is refused.
		Kind:     credential.KindGCPOIDC,
		Settings: map[string]string{"provider": testProvider, "service_account": "deployer@gmail.com"},
		Want:     ErrSetting,
	}, { // Test 8: Azure needs its application and tenant.
		Kind: credential.KindAzureOIDC, Settings: map[string]string{"client_id": "abc"}, Want: ErrSetting,
	}, { // Test 9: Azure defaults the Entra exchange audience.
		Kind: credential.KindAzureOIDC, Settings: map[string]string{
			"client_id": "11111111-2222-3333-4444-555555555555", "tenant_id": "contoso.onmicrosoft.com",
		},
		WantConfig: Config{Kind: credential.KindAzureOIDC,
			ClientID: "11111111-2222-3333-4444-555555555555",
			TenantID: "contoso.onmicrosoft.com", Audience: DefaultAzureAudience, TokenTTL: DefaultTokenTTL,
			Delivery: DeliveryFile},
	}, { // Test 10: Azure has no exchange delivery.
		Kind: credential.KindAzureOIDC, Settings: map[string]string{
			"client_id": "a1", "tenant_id": "t1", "delivery": "exchange",
		},
		Want: ErrSetting,
	}, { // Test 11: The generic token needs an audience.
		Kind: credential.KindOIDCToken, Settings: nil, Want: ErrSetting,
	}, { // Test 12: A lifetime outside the bounds is refused.
		Kind:     credential.KindOIDCToken,
		Settings: map[string]string{"audience": "vault", "token_ttl": "3h"},
		Want:     ErrSetting,
	}, { // Test 13: An environment carrying a separator is refused before it reaches a subject.
		Kind:     credential.KindOIDCToken,
		Settings: map[string]string{"audience": "vault", "environment": "prod:approved:true"},
		Want:     ErrSetting,
	}, { // Test 14: A stored kind is not a federated one.
		Kind: credential.KindAWS, Settings: nil, Want: ErrSetting,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := ParseSettings(test.Kind, test.Settings)
			if !errors.Is(err, test.Want) {
				t.Fatalf("ParseSettings() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantConfig, got); diff != "" {
				t.Errorf("ParseSettings() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestEveryFederatedKindHasSettings pins that each federated kind the credential package declares
// has a settings list here, so a new kind cannot be saved with settings nothing knows how to read.
func TestEveryFederatedKindHasSettings(t *testing.T) {
	t.Parallel()
	for _, kind := range credential.FederatedKinds() {
		if len(SettingKeys(kind)) == 0 {
			t.Errorf("%s has no settings list", kind)
		}
	}
	if len(settingKeys) != len(credential.FederatedKinds()) {
		t.Errorf("settingKeys covers %d kinds and the credential package declares %d", len(settingKeys),
			len(credential.FederatedKinds()))
	}
}
