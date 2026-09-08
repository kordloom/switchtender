package util

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestRunEnvironInstallsTheCredentialStrippingFilter proves the exported entry point a run's child
// process actually uses reads the real process environment and applies the strict filter.
//
// The filter functions are tested directly elsewhere, and they would keep passing if RunEnviron
// were wired to the lenient one: FilterConfigEnv keeps VAULT_TOKEN on purpose, so a constructor
// calling it instead of filterRunEnv hands the Vault token to every host run while every
// rule-level test stays green. This is the only test that can catch that, so it goes through
// os.Environ rather than a supplied slice.
//
// It cannot run in parallel: t.Setenv mutates the process environment, the state under test.
func TestRunEnvironInstallsTheCredentialStrippingFilter(t *testing.T) {
	tests := []struct {
		// Name is the variable set on the process.
		Name string
		// Value is what it is set to.
		Value string
		// WantInRun is whether a run's child process should still see it.
		WantInRun bool
		// WantInFetch is whether the secret-fetch child process should still see it.
		WantInFetch bool
	}{ // Test 0 to 3: The server's own credentials for reaching an external secret manager.
		{Name: EnvVaultToken, Value: "hvs.example", WantInRun: false, WantInFetch: true},
		{Name: EnvAWSAccessKeyID, Value: "AKIAEXAMPLE", WantInRun: false, WantInFetch: true},
		{Name: EnvAWSSecretAccessKey, Value: "aws-secret", WantInRun: false, WantInFetch: true},
		{Name: EnvAWSSessionToken, Value: "aws-session", WantInRun: false, WantInFetch: true},
		// Test 4 and 5: SwitchTender's own configuration reaches neither child.
		{Name: "SWITCHTENDER_ENCRYPTION_KEY", Value: "master-passphrase"},
		{Name: "SWITCHTENDER_WORKER_TOKEN", Value: "relay-token"},
		// Test 6: A variable added to the configuration later is stripped by having the prefix,
		// which is the property a denylist lacks.
		{Name: "SWITCHTENDER_SOMETHING_ADDED_LATER", Value: "whatever"},
		// Test 7: The host's own environment both children are meant to inherit.
		{Name: "AWS_REGION", Value: "us-east-1", WantInRun: true, WantInFetch: true},
	}
	for _, test := range tests {
		t.Setenv(test.Name, test.Value)
	}
	runEnv, fetchEnv := RunEnviron(), SecretFetchEnviron()

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			entry := test.Name + "=" + test.Value
			if got := slices.Contains(runEnv, entry); got != test.WantInRun {
				t.Errorf("RunEnviron() carries %s = %v, want %v; a run executes somebody else's "+
					"code and reads whatever it inherits", test.Name, got, test.WantInRun)
			}
			if got := slices.Contains(fetchEnv, entry); got != test.WantInFetch {
				t.Errorf("SecretFetchEnviron() carries %s = %v, want %v", test.Name, got,
					test.WantInFetch)
			}
		})
	}
}

// TestRunEnvironLeavesTheRestOfTheHostEnvironmentAlone proves the strip is narrow. A playbook is
// often meant to inherit the proxy, locale, and path settings of the host it runs on, so a filter
// that took more than it should would break working deployments rather than protect anything.
//
// It cannot run in parallel: t.Setenv mutates the process environment.
func TestRunEnvironLeavesTheRestOfTheHostEnvironmentAlone(t *testing.T) {
	t.Setenv("HTTPS_PROXY", "http://proxy:3128")
	t.Setenv("SWITCHTENDER_ENCRYPTION_KEY", "master-passphrase")

	runEnv := RunEnviron()
	if !slices.Contains(runEnv, "HTTPS_PROXY=http://proxy:3128") {
		t.Error("RunEnviron() dropped the proxy setting, so a playbook can no longer reach out")
	}
	for _, kv := range runEnv {
		if strings.HasPrefix(kv, configEnvPrefix) {
			name, _, _ := strings.Cut(kv, "=")
			t.Errorf("RunEnviron() kept %s, so a one-line shell run can print it", name)
		}
	}
}

// TestFilterEnvBoundaries pins the shapes an environment slice can hold that are not a plain
// name=value pair. os.Environ is not guaranteed to be well formed, and a filter that panicked or
// silently dropped a malformed entry would either kill the dispatcher or quietly change what a run
// inherits.
func TestFilterEnvBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Name labels the shape under test.
		Name string
		// In is the environment slice.
		In []string
		// WantResult is what a run should inherit.
		WantResult []string
	}{{ // Test 0: Nothing in, nothing out.
		Name: "empty", In: nil, WantResult: nil,
	}, { // Test 1: An entry with no equals sign is carried through, since it names nothing we strip.
		Name: "no equals", In: []string{"BARE"}, WantResult: []string{"BARE"},
	}, { // Test 2: An empty entry is not a credential and is left alone.
		Name: "empty entry", In: []string{""}, WantResult: []string{""},
	}, { // Test 3: A bare credential name with no value still goes, since the name is the match.
		Name: "credential name only", In: []string{"VAULT_TOKEN="}, WantResult: nil,
	}, { // Test 4: The prefix alone is SwitchTender configuration and goes.
		Name: "prefix only", In: []string{"SWITCHTENDER_"}, WantResult: nil,
	}, { // Test 5: A name that merely ends in a credential name is somebody else's variable.
		Name: "credential suffix", In: []string{"MY_VAULT_TOKEN=x"},
		WantResult: []string{"MY_VAULT_TOKEN=x"},
	}, { // Test 6: Matching is exact, so a lowercase spelling is not the server's own credential.
		Name: "lowercase credential", In: []string{"vault_token=x"},
		WantResult: []string{"vault_token=x"},
	}, { // Test 7: A value containing an equals sign keeps all of it.
		Name: "equals in value", In: []string{"A=b=c"}, WantResult: []string{"A=b=c"},
	}, { // Test 8: A duplicate credential entry is dropped every time it appears.
		Name: "repeated credential", In: []string{"VAULT_TOKEN=a", "PATH=/bin", "VAULT_TOKEN=b"},
		WantResult: []string{"PATH=/bin"},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			got := filterRunEnv(test.In)
			if len(got) != len(test.WantResult) {
				t.Fatalf("filterRunEnv(%q) = %q, want %q", test.In, got, test.WantResult)
			}
			for i := range got {
				if got[i] != test.WantResult[i] {
					t.Errorf("filterRunEnv(%q) = %q, want %q", test.In, got, test.WantResult)
					break
				}
			}
		})
	}
}
