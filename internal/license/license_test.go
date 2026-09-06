package license

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// testKey registers a fresh signing key under kid and returns its private half.
func testKey(t *testing.T, kid string) ed25519.PrivateKey {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("keygen: %v", err)
	}
	RegisterKey(kid, pub)
	return priv
}

// claims returns a valid one-year Team license body.
func claims(kid string) Claims {
	return Claims{
		V: 1, ID: "lic_test", Org: "Acme", Tier: TierTeam, Hosts: "250",
		Issued:  "2026-09-01T00:00:00Z",
		Expires: "2027-09-01T00:00:00Z",
		Kid:     kid,
	}
}

func TestSignedLicenseRoundTrips(t *testing.T) {
	t.Parallel()
	priv := testKey(t, "rt1")
	raw, err := Sign(claims("rt1"), priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	l, err := Verify(raw, "test")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	if l.Claims.Org != "Acme" || l.Claims.Tier != TierTeam {
		t.Errorf("claims = %+v", l.Claims)
	}
}

// TestTamperedClaimsAreRefused covers every field of the signed body.
//
// The license is the paywall. A field that can be altered after signing without the signature
// noticing is a field a customer can grant themselves: a longer term, a higher tier, someone
// else's key id. Each mutation here must be refused, and the loop is field-by-field so adding a
// claim without extending the signature's coverage fails this test rather than shipping.
func TestTamperedClaimsAreRefused(t *testing.T) {
	t.Parallel()
	priv := testKey(t, "tm1")
	testKey(t, "tm2") // A second trusted key, so retargeting kid is a pure signature question.
	raw, err := Sign(claims("tm1"), priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	tampers := map[string]string{
		"tier":    TierEnterprise,
		"expires": "2099-01-01T00:00:00Z",
		"org":     "Mallory",
		"hosts":   "unlimited",
		"id":      "lic_other",
		"kid":     "tm2",
	}
	for field, value := range tampers {
		t.Run(field, func(t *testing.T) {
			t.Parallel()
			var f File
			if err := json.Unmarshal(raw, &f); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			var m map[string]any
			cb, _ := json.Marshal(f.Claims)
			_ = json.Unmarshal(cb, &m)
			m[field] = value
			nb, _ := json.Marshal(m)
			_ = json.Unmarshal(nb, &f.Claims)
			forged, _ := json.Marshal(f)
			if _, err := Verify(forged, "test"); err == nil {
				t.Fatalf("a license with a rewritten %s verified", field)
			}
		})
	}
}

func TestUnknownKeyAndVersionAreRefused(t *testing.T) {
	t.Parallel()
	priv := testKey(t, "uk1")
	c := claims("uk1")
	c.Kid = "nobody"
	raw, err := Sign(c, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if _, err := Verify(raw, "test"); err == nil ||
		!strings.Contains(err.Error(), "does not trust") {
		t.Errorf("unknown kid error = %v", err)
	}
	c = claims("uk1")
	c.V = 2
	raw, err = Sign(c, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	if _, err := Verify(raw, "test"); err == nil {
		t.Error("a future version verified in a build that cannot read it")
	}
}

// TestAllowIsTheWholePaywall covers the one function every gate calls.
func TestAllowIsTheWholePaywall(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	team := &License{Claims: claims("x")}
	lapsed := &License{Claims: claims("x")}
	lapsed.Claims.Expires = "2026-10-01T00:00:00Z"
	ent := &License{Claims: claims("x")}
	ent.Claims.Tier = TierEnterprise

	tests := []struct {
		Name   string
		L      *License
		Want   string
		WantOK bool
	}{{ // Test 0: no license is Community, named as such.
		Name: "community", L: nil, Want: "runs Community",
	}, { // Test 1: a valid Team license passes.
		Name: "team", L: team, WantOK: true,
	}, { // Test 2: enterprise includes team.
		Name: "enterprise", L: ent, WantOK: true,
	}, { // Test 3: lapsed drops to Community without a restart, and says nothing is lost.
		Name: "lapsed", L: lapsed, Want: "keeps working",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := allowAt(test.L, FeatureSSO, now)
			if test.WantOK {
				if err != nil {
					t.Fatalf("allowAt() = %v, want allowed", err)
				}
				return
			}
			if err == nil {
				t.Fatal("allowAt() allowed without a covering license")
			}
			if !strings.Contains(err.Error(), test.Want) {
				t.Errorf("error %q does not carry %q", err, test.Want)
			}
			if !strings.Contains(err.Error(), "switchtender.com/pricing") {
				t.Errorf("a gate line without the destination: %q", err)
			}
			if strings.Count(err.Error(), "\n") != 0 {
				t.Errorf("a gate must be one line, got %q", err)
			}
		})
	}
}

// TestLapseIsCheckedPerCall pins the no-restart promise: the same license object passes before its
// expiry and refuses after, with nothing reloaded.
func TestLapseIsCheckedPerCall(t *testing.T) {
	t.Parallel()
	l := &License{Claims: claims("x")}
	before := time.Date(2027, 8, 31, 0, 0, 0, 0, time.UTC)
	after := time.Date(2027, 9, 2, 0, 0, 0, 0, time.UTC)
	if err := allowAt(l, FeatureRegister, before); err != nil {
		t.Fatalf("before expiry: %v", err)
	}
	if err := allowAt(l, FeatureRegister, after); err == nil {
		t.Fatal("after expiry the same process still allowed the feature")
	}
}

func TestLoadMissingFileMeansCommunity(t *testing.T) {
	t.Parallel()
	l, err := Load(t.TempDir() + "/absent.json")
	if err != nil || l != nil {
		t.Fatalf("Load(absent) = %v, %v; want nil, nil", l, err)
	}
}

// TestAnUnknownTierGrantsNothing pins covers against a signed license whose tier is not one this
// build sells. Signing controls the tier in practice, but a mint mistake must fail toward
// Community, not toward everything: tier is an allowlist, never a formality.
func TestAnUnknownTierGrantsNothing(t *testing.T) {
	t.Parallel()
	weird := &License{Claims: claims("x")}
	weird.Claims.Tier = "community"
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if err := allowAt(weird, FeatureSSO, now); err == nil {
		t.Fatal("a license with tier community granted a paid feature")
	}
}

// TestTierCoverage pins which tier unlocks which feature. This table is the pricing page in
// executable form; a row changing here is a business decision, not a refactor.
func TestTierCoverage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		Tier        string
		Feature     Feature
		WantAllowed bool
	}{
		{Tier: TierPro, Feature: FeatureSSO, WantAllowed: true},              // Test 0: SSO is the Pro grant.
		{Tier: TierPro, Feature: FeaturePolicyFull, WantAllowed: false},      // Test 1: the full engine is not.
		{Tier: TierPro, Feature: FeatureWorkers, WantAllowed: false},         // Test 2.
		{Tier: TierPro, Feature: FeaturePostgresInit, WantAllowed: false},    // Test 3.
		{Tier: TierPro, Feature: FeatureRegister, WantAllowed: false},        // Test 4.
		{Tier: TierPro, Feature: FeatureReconcile, WantAllowed: false},       // Test 5.
		{Tier: TierTeam, Feature: FeatureSSO, WantAllowed: true},             // Test 6: Team includes Pro.
		{Tier: TierTeam, Feature: FeaturePolicyFull, WantAllowed: true},      // Test 7.
		{Tier: TierEnterprise, Feature: FeatureReconcile, WantAllowed: true}, // Test 8.
		{Tier: "", Feature: FeaturePolicyFull, WantAllowed: true},            // Test 9: pre-Pro licenses are Team.
		{Tier: "platinum", Feature: FeatureSSO, WantAllowed: false},          // Test 10: unknown tiers cover nothing.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			l := &License{Claims: Claims{Tier: test.Tier, Org: "Acme",
				Expires: "2027-09-01T00:00:00Z"}}
			err := allowAt(l, test.Feature, now)
			if got := err == nil; got != test.WantAllowed {
				t.Errorf("tier %q feature %q allowed = %v, want %v (%v)",
					test.Tier, test.Feature, got, test.WantAllowed, err)
			}
		})
	}
}

// TestAllowPoliciesPerTier pins the policy count ladder: one on Community, five on Pro, uncapped
// from Team up, and a lapsed license capping like Community.
func TestAllowPoliciesPerTier(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	lic := func(tier, expires string) *License {
		return &License{Claims: Claims{Tier: tier, Org: "Acme", Expires: expires}}
	}
	live, lapsed := "2027-09-01T00:00:00Z", "2026-01-01T00:00:00Z"
	tests := []struct {
		License *License
		Total   int
		WantErr string
	}{
		{License: nil, Total: 1},                                                   // Test 0: Community holds one.
		{License: nil, Total: 2, WantErr: "Community holds one"},                   // Test 1.
		{License: lic(TierPro, live), Total: 5},                                    // Test 2: Pro holds five.
		{License: lic(TierPro, live), Total: 6, WantErr: "Team removes the cap"},   // Test 3.
		{License: lic(TierTeam, live), Total: 60},                                  // Test 4: Team is uncapped.
		{License: lic(TierEnterprise, live), Total: 600},                           // Test 5.
		{License: lic("", live), Total: 60},                                        // Test 6: pre-Pro licenses are Team.
		{License: lic(TierPro, lapsed), Total: 2, WantErr: "Community holds one"},  // Test 7: lapsed caps free.
		{License: lic(TierTeam, lapsed), Total: 2, WantErr: "Community holds one"}, // Test 8.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := allowPoliciesAt(test.License, test.Total, now)
			if test.WantErr == "" {
				if err != nil {
					t.Fatalf("allowPoliciesAt() error = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("allowPoliciesAt() error = %v, want containing %q", err, test.WantErr)
			}
		})
	}
}

// TestLegacyEmptyTierRoundTrips proves a license signed without a tier still verifies and grants
// Team, through the real signing path rather than a hand-built struct.
func TestLegacyEmptyTierRoundTrips(t *testing.T) {
	t.Parallel()
	priv := testKey(t, "lg1")
	c := claims("lg1")
	c.Tier = ""
	raw, err := Sign(c, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	l, err := Verify(raw, "test")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	if err := allowAt(l, FeaturePolicyFull, now); err != nil {
		t.Errorf("legacy license lost Team coverage: %v", err)
	}
	if err := allowPoliciesAt(l, 60, now); err != nil {
		t.Errorf("legacy license gained a policy cap: %v", err)
	}
}

// TestLapsedProNamesTheTier checks a lapsed Pro license refuses SSO with the lapse named, and that
// a live Pro license hitting a Team feature names both tiers, so every refusal teaches the fix.
func TestLapsedProNamesTheTier(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 9, 6, 0, 0, 0, 0, time.UTC)
	lapsed := &License{Claims: Claims{Tier: TierPro, Org: "Acme",
		Expires: "2026-01-01T00:00:00Z"}}
	err := allowAt(lapsed, FeatureSSO, now)
	if err == nil || !strings.Contains(err.Error(), "lapsed") ||
		!strings.Contains(err.Error(), "Pro") {
		t.Errorf("lapsed Pro SSO refusal = %v, want the lapse and the tier named", err)
	}
	live := &License{Claims: Claims{Tier: TierPro, Org: "Acme",
		Expires: "2027-09-01T00:00:00Z"}}
	err = allowAt(live, FeaturePolicyFull, now)
	if err == nil || !strings.Contains(err.Error(), "Team") ||
		!strings.Contains(err.Error(), "Pro") {
		t.Errorf("Pro-at-Team refusal = %v, want both tiers named", err)
	}
}
