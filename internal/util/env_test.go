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
