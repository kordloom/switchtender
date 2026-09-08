package license

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestProcessLicenseCarriesTheGate checks the path a running server actually takes: a license is
// installed into the process once, and every later gate question reads that one. The rule itself is
// covered against an explicit license elsewhere; what is covered here is the plumbing between them,
// where a gate fails open silently if Set and Allow ever stop agreeing.
//
// These cases share the process-wide license, so they do not run in parallel with each other.
func TestProcessLicenseCarriesTheGate(t *testing.T) {
	before := Current()
	t.Cleanup(func() { Set(before) })

	tests := []struct {
		Install   *License
		Feature   Feature
		Name      string
		WantAllow bool
	}{{ // Test 0: No license is Community, and a paid feature is refused.
		Name: "community refuses a paid feature", Install: nil,
		Feature: FeatureSSO, WantAllow: false,
	}, { // Test 1: A live Team license admits a feature Team covers.
		Name:    "team admits its own feature",
		Install: &License{Claims: Claims{Org: "acme", Tier: TierTeam, Expires: future()}},
		Feature: FeatureSSO, WantAllow: true,
	}, { // Test 2: A lapsed license drops the install back to Community without a restart.
		Name:    "a lapsed license stops covering",
		Install: &License{Claims: Claims{Org: "acme", Tier: TierTeam, Expires: past()}},
		Feature: FeatureSSO, WantAllow: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			Set(test.Install)

			if got := Current(); got != test.Install {
				t.Fatalf("Current() = %v, want the license just installed", got)
			}
			err := Allow(test.Feature)
			if gotAllow := err == nil; gotAllow != test.WantAllow {
				t.Fatalf("Allow(%v) error = %v, want allowed = %v", test.Feature, err, test.WantAllow)
			}
			if err == nil {
				return
			}
			// The refusal is the entire user experience of hitting a gate, so it must name
			// the feature and point somewhere, never just say no.
			if !strings.Contains(err.Error(), "switchtender.com/pricing") {
				t.Errorf("refusal does not point at pricing: %v", err)
			}
		})
	}
}

// TestProcessPolicyCapFollowsTheInstalledLicense checks the policy cap against the process license
// rather than an explicit one, because the cap is enforced on a live server through this path.
func TestProcessPolicyCapFollowsTheInstalledLicense(t *testing.T) {
	before := Current()
	t.Cleanup(func() { Set(before) })

	tests := []struct {
		Install *License
		Name    string
		Total   int
		WantOK  bool
	}{{ // Test 0: Community holds one policy.
		Name: "community holds one", Install: nil, Total: 1, WantOK: true,
	}, { // Test 1: Community refuses a second.
		Name: "community refuses two", Install: nil, Total: 2, WantOK: false,
	}, { // Test 2: Pro holds five.
		Name:    "pro holds five",
		Install: &License{Claims: Claims{Org: "acme", Tier: TierPro, Expires: future()}},
		Total:   5, WantOK: true,
	}, { // Test 3: Pro refuses a sixth and names Team as the way out.
		Name:    "pro refuses six",
		Install: &License{Claims: Claims{Org: "acme", Tier: TierPro, Expires: future()}},
		Total:   6, WantOK: false,
	}, { // Test 4: Team is uncapped.
		Name:    "team is uncapped",
		Install: &License{Claims: Claims{Org: "acme", Tier: TierTeam, Expires: future()}},
		Total:   500, WantOK: true,
	}, { // Test 5: A lapsed Team license is capped like Community.
		Name:    "a lapsed team license is capped",
		Install: &License{Claims: Claims{Org: "acme", Tier: TierTeam, Expires: past()}},
		Total:   2, WantOK: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			Set(test.Install)
			err := AllowPolicies(test.Total)
			if gotOK := err == nil; gotOK != test.WantOK {
				t.Fatalf("AllowPolicies(%d) error = %v, want allowed = %v", test.Total, err, test.WantOK)
			}
		})
	}
}

// TestPathForPlacesTheLicenseBesideItsData checks where an install looks for its license. Getting
// this wrong strands a paying customer on Community after a restore that carried the database but
// looked for the license somewhere else.
func TestPathForPlacesTheLicenseBesideItsData(t *testing.T) {
	tests := []struct {
		Name     string
		Env      string
		DB       string
		WantPath string
	}{{ // Test 0: A SQLite file keeps its license in the same directory.
		Name: "sqlite file", DB: "/var/lib/switchtender/state.db",
		WantPath: "/var/lib/switchtender/switchtender-license.json",
	}, { // Test 1: A bare filename has no directory, so the license sits in the working directory.
		Name: "bare filename", DB: "state.db", WantPath: "./switchtender-license.json",
	}, { // Test 2: A DSN has no directory at all, so the working directory is used.
		Name: "postgres dsn", DB: "postgres://user@host:5432/db",
		WantPath: "./switchtender-license.json",
	}, { // Test 3: The environment override beats every other rule.
		Name: "env override", Env: "/etc/switchtender/license.json",
		DB: "/var/lib/switchtender/state.db", WantPath: "/etc/switchtender/license.json",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			if test.Env != "" {
				t.Setenv("SWITCHTENDER_LICENSE", test.Env)
			} else {
				t.Setenv("SWITCHTENDER_LICENSE", "")
			}
			if got := PathFor(test.DB); got != test.WantPath {
				t.Errorf("PathFor(%q) = %q, want %q", test.DB, got, test.WantPath)
			}
		})
	}
}

// TestLoadDistinguishesAbsentFromUnreadable pins the difference between the two ways a license can
// fail to arrive. An absent file is the ordinary Community install and must not be an error, while
// a file that exists and cannot be trusted must be, so a corrupted or tampered license is reported
// rather than quietly treated as no license at all.
func TestLoadDistinguishesAbsentFromUnreadable(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	l, err := Load(filepath.Join(dir, "absent.json"))
	if l != nil || err != nil {
		t.Errorf("Load(absent) = %v, %v; want nil, nil: no license file is a Community install", l, err)
	}

	corrupt := filepath.Join(dir, "corrupt.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(corrupt); err == nil {
		t.Error("Load of an unreadable license returned no error; a corrupt license must be reported")
	}

	unreadable := filepath.Join(dir, "unreadable.json")
	if err := os.WriteFile(unreadable, []byte("{}"), 0o000); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := Load(unreadable); err == nil && os.Getuid() != 0 {
		t.Error("Load of a permission-denied license returned no error; it must be reported")
	}
}

// future is a license expiry comfortably ahead of any test run, in the RFC 3339 form the claims
// carry. A term this package cannot parse reads as expired, so the format is load-bearing.
func future() string { return time.Now().Add(365 * 24 * time.Hour).Format(time.RFC3339) }

// past is a license expiry comfortably behind any test run, in the same form.
func past() string { return time.Now().Add(-24 * time.Hour).Format(time.RFC3339) }
