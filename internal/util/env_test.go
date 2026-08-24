package util

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// TestFilterConfigEnvRemovesEverySwitchTenderVariable checks that the environment handed to a run
// carries none of SwitchTender's own configuration.
//
// The server reads its master encryption key and salt from the environment, and the whole
// environment used to reach every host-mode child process. An operator allowed to submit a one-line
// shell run could print the key, and the key plus the salt opens every stored credential and any
// backup file. The run is the thing those values protect.
//
// The whole prefix goes rather than a list of names, so a secret added to the configuration later is
// stripped by having the prefix instead of by somebody remembering to add it here.
func TestFilterConfigEnvRemovesEverySwitchTenderVariable(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Entry    string
		WantKept bool
	}{ // Test 0 to 5: The secrets that must never reach a run.
		{"SWITCHTENDER_ENCRYPTION_KEY=master-passphrase", false},
		{"SWITCHTENDER_ENCRYPTION_SALT=deployment-salt", false},
		{"SWITCHTENDER_WORKER_TOKEN=relay-token", false},
		{"SWITCHTENDER_ADMIN_PASSWORD=hunter2", false},
		{"SWITCHTENDER_OIDC_CLIENT_SECRET=oidc-secret", false},
		{"SWITCHTENDER_AUDIT_KEY=signing-seed", false},
		// Test 6 and 7: Non-secret configuration goes too, since a run reads what we set per run.
		{"SWITCHTENDER_URL=https://switchtender.example.com", false},
		{"SWITCHTENDER_SOMETHING_ADDED_LATER=whatever", false},
		// Test 8 to 11: The host's own environment is left alone, because a playbook is often meant
		// to inherit it.
		{"PATH=/usr/bin", true},
		{"HOME=/home/deploy", true},
		{"AWS_ACCESS_KEY_ID=AKIAEXAMPLE", true},
		{"HTTPS_PROXY=http://proxy:3128", true},
	}
	in := make([]string, 0, len(tests))
	for _, test := range tests {
		in = append(in, test.Entry)
	}
	got := FilterConfigEnv(in)

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			kept := slices.Contains(got, test.Entry)
			switch {
			case test.WantKept && !kept:
				t.Errorf("%q was stripped, so a run no longer inherits the host environment",
					test.Entry)
			case !test.WantKept && kept:
				name, _, _ := strings.Cut(test.Entry, "=")
				t.Errorf("%s reaches the run, so anything the run executes can print it", name)
			}
		})
	}
}

// TestRunEnvStripsTheServersOwnCredentials checks the credentials SwitchTender reaches an external
// secret manager with never reach a run, while the secret-fetch command keeps them.
//
// These carry no SwitchTender prefix, so the prefix scrub left them in place and every host run
// inherited them. Submitting a run needs only the operator role and no credential access, so one
// line of shell returned the Vault token that opens every secret the server can reach, and the
// masker held only the run's own credentials so it landed in the stored log verbatim.
//
// The two environments genuinely differ: a fetch command authenticates to the store with the same
// token, so stripping it there would break the feature rather than protect anything.
func TestRunEnvStripsTheServersOwnCredentials(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Entry       string
		WantInRun   bool
		WantInFetch bool
	}{ // Test 0 to 3: The server's own credentials for reaching a secret manager.
		{"VAULT_TOKEN=hvs.example", false, true},
		{"AWS_SECRET_ACCESS_KEY=secret", false, true},
		{"AWS_SESSION_TOKEN=session", false, true},
		{"AWS_ACCESS_KEY_ID=AKIAEXAMPLE", false, true},
		// Test 4: SwitchTender's own configuration reaches neither.
		{"SWITCHTENDER_ENCRYPTION_KEY=master", false, false},
		// Test 5 to 7: The host environment both legitimately need.
		{"PATH=/usr/bin", true, true},
		{"AWS_REGION=us-east-1", true, true},
		{"HTTPS_PROXY=http://proxy:3128", true, true},
	}
	in := make([]string, 0, len(tests))
	for _, test := range tests {
		in = append(in, test.Entry)
	}
	runEnv, fetchEnv := filterRunEnv(in), FilterConfigEnv(in)

	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := slices.Contains(runEnv, test.Entry); got != test.WantInRun {
				t.Errorf("%q in a run environment = %v, want %v", test.Entry, got, test.WantInRun)
			}
			if got := slices.Contains(fetchEnv, test.Entry); got != test.WantInFetch {
				t.Errorf("%q in a secret-fetch environment = %v, want %v",
					test.Entry, got, test.WantInFetch)
			}
		})
	}
}
