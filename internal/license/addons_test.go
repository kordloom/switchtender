package license

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// fixtureKid names the key the pre-add-on license fixtures were signed with.
const fixtureKid = "fixture-preaddons"

// preAddonLicenses are license files signed by the build before add-ons entered the signed body,
// byte for byte as that build wrote them. One carries a host band and a tier, the other carries
// neither, so both optional-field paths of the preimage are pinned.
//
//nolint:gochecknoglobals // Not modified, simplifies testing.
var preAddonLicenses = []string{`{
  "claims": {
    "v": 1,
    "id": "lic_fixture_1",
    "org": "Acme",
    "tier": "team",
    "hosts": "250",
    "issued": "2026-09-01T00:00:00Z",
    "expires": "2027-09-01T00:00:00Z",
    "kid": "fixture-preaddons"
  },
  "sig": "/LY1GF8p6blaCkPOWZCWqxwZGSexmfbT0uOLI+34jPXOZYn1SYnaC8GjkbI8rSir8IrBa7jXHGjuf5Wja4VcBw=="
}`, `{
  "claims": {
    "v": 1,
    "id": "lic_fixture_2",
    "org": "Acme",
    "tier": "",
    "issued": "2026-09-01T00:00:00Z",
    "expires": "2027-09-01T00:00:00Z",
    "kid": "fixture-preaddons"
  },
  "sig": "W6F/S7bZcX/2z+fMhnXgxfa0PnhWl501mXuk36srN0GNJKwuDZuutuqhoaHLqvfAsDaMqV1++c6aPmXaSCkCBA=="
}`}

// fixtureKey returns the deterministic key the pre-add-on fixtures were signed with, registered
// under fixtureKid.
func fixtureKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	priv := ed25519.NewKeyFromSeed(bytes.Repeat([]byte{0x2a}, ed25519.SeedSize))
	RegisterKey(fixtureKid, priv.Public().(ed25519.PublicKey))
	return priv
}

// withClaim rewrites one field of a signed license file's claims, keeping the signature, and
// returns the forged file. A nil value deletes the field.
func withClaim(t *testing.T, raw []byte, field string, value any) []byte {
	t.Helper()
	var f map[string]json.RawMessage
	if err := json.Unmarshal(raw, &f); err != nil {
		t.Fatalf("unmarshal file: %v", err)
	}
	var c map[string]any
	if err := json.Unmarshal(f["claims"], &c); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	if value == nil {
		delete(c, field)
	} else {
		c[field] = value
	}
	cb, err := json.Marshal(c)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	f["claims"] = cb
	forged, err := json.Marshal(f)
	if err != nil {
		t.Fatalf("marshal file: %v", err)
	}
	return forged
}

// TestAddonsAreSigned pins the add-on list inside the signature and the preimage of every license
// signed before it existed.
//
// An add-on is a paid grant, so one a holder can write in after signing is one they can sell
// themselves. A license signed before add-ons existed must still verify byte for byte, or the
// release that adds them would drop every paying install to Community.
func TestAddonsAreSigned(t *testing.T) {
	t.Parallel()
	priv := fixtureKey(t)
	withAddon := Claims{
		V: 1, ID: "lic_addon", Org: "Acme", Tier: TierTeam, Hosts: "1000",
		Issued: "2026-09-01T00:00:00Z", Expires: "2027-09-01T00:00:00Z", Kid: fixtureKid,
		Addons: []string{AddonHostedWitness},
	}
	signedWith, err := Sign(withAddon, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	plain := withAddon
	plain.Addons = nil
	signedWithout, err := Sign(plain, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	tests := []struct {
		Name   string
		Raw    []byte
		WantOK bool
	}{{ // Test 0: A pre-add-on Team license with a host band verifies unchanged.
		Name: "pre-add-on team", Raw: []byte(preAddonLicenses[0]), WantOK: true,
	}, { // Test 1: A pre-add-on license with no tier and no band verifies unchanged.
		Name: "pre-add-on legacy", Raw: []byte(preAddonLicenses[1]), WantOK: true,
	}, { // Test 2: A license signed with an add-on verifies.
		Name: "signed add-on", Raw: signedWith, WantOK: true,
	}, { // Test 3: An add-on written into a license signed without one is refused.
		Name: "add-on added", Raw: withClaim(t, signedWithout, "addons", []string{AddonHostedWitness}),
	}, { // Test 4: An add-on written into a pre-add-on license is refused.
		Name: "add-on added to a pre-add-on license",
		Raw:  withClaim(t, []byte(preAddonLicenses[0]), "addons", []string{AddonHostedWitness}),
	}, { // Test 5: A signed add-on renamed is refused.
		Name: "add-on edited", Raw: withClaim(t, signedWith, "addons", []string{"hosted-witness-plus"}),
	}, { // Test 6: A second add-on appended to the signed list is refused.
		Name: "add-on appended",
		Raw:  withClaim(t, signedWith, "addons", []string{AddonHostedWitness, "custody"}),
	}, { // Test 7: A signed add-on list removed is refused, so the list cannot be traded away either.
		Name: "add-on removed", Raw: withClaim(t, signedWith, "addons", nil),
	}, { // Test 8: A signed add-on list emptied is refused.
		Name: "add-on emptied", Raw: withClaim(t, signedWith, "addons", []string{}),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			_, err := Verify(test.Raw, "test")
			if test.WantOK && err != nil {
				t.Fatalf("Verify() error = %v, want verified", err)
			}
			if !test.WantOK && err == nil {
				t.Fatal("Verify() verified a license whose add-ons were edited after signing")
			}
		})
	}
}

// TestHostedWitnessAddonCoverage pins who may use the hosted witness: Enterprise by its tier, Team
// by the add-on, and nobody below Team whatever the license lists.
func TestHostedWitnessAddonCoverage(t *testing.T) {
	t.Parallel()
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	live, lapsed := "2027-09-01T00:00:00Z", "2026-10-01T00:00:00Z"
	lic := func(tier, expires string, addons ...string) *License {
		return &License{Claims: Claims{Tier: tier, Org: "Acme", Expires: expires, Addons: addons}}
	}
	tests := []struct {
		Name        string
		License     *License
		Feature     Feature
		WantAllowed bool
		WantLapsed  bool
		WantErr     string
	}{{ // Test 0: Community is refused, and told both ways to buy it.
		Name: "community", License: nil, Feature: FeatureHostedWitness,
		WantErr: "requires an Enterprise license or the Team add-on; this install runs Community",
	}, { // Test 1: Team without the add-on is refused.
		Name: "team", License: lic(TierTeam, live), Feature: FeatureHostedWitness,
		WantErr: "this install's Team license does not include it",
	}, { // Test 2: Team with the add-on is allowed.
		Name: "team add-on", License: lic(TierTeam, live, AddonHostedWitness),
		Feature: FeatureHostedWitness, WantAllowed: true,
	}, { // Test 3: A pre-Pro license reads as Team, so the add-on applies.
		Name: "legacy add-on", License: lic("", live, AddonHostedWitness),
		Feature: FeatureHostedWitness, WantAllowed: true,
	}, { // Test 4: Enterprise includes it without the add-on.
		Name: "enterprise", License: lic(TierEnterprise, live), Feature: FeatureHostedWitness,
		WantAllowed: true,
	}, { // Test 5: Enterprise with the add-on listed is allowed too.
		Name: "enterprise add-on", License: lic(TierEnterprise, live, AddonHostedWitness),
		Feature: FeatureHostedWitness, WantAllowed: true,
	}, { // Test 6: Pro cannot carry the add-on.
		Name: "pro add-on", License: lic(TierPro, live, AddonHostedWitness),
		Feature: FeatureHostedWitness, WantErr: "this install's Pro license does not include it",
	}, { // Test 7: An unknown tier cannot carry it either.
		Name: "unknown tier add-on", License: lic("platinum", live, AddonHostedWitness),
		Feature: FeatureHostedWitness, WantErr: "does not include it",
	}, { // Test 8: An add-on name this build does not sell grants nothing.
		Name: "unknown add-on", License: lic(TierTeam, live, "hosted-witness-plus"),
		Feature: FeatureHostedWitness, WantErr: "does not include it",
	}, { // Test 9: The add-on grants only its own feature.
		Name: "add-on other feature", License: lic(TierPro, live, AddonHostedWitness),
		Feature: FeaturePolicyFull, WantErr: "requires a Team license; this install's Pro license",
	}, { // Test 10: A lapsed Team license with the add-on names the lapse.
		Name: "lapsed team add-on", License: lic(TierTeam, lapsed, AddonHostedWitness),
		Feature: FeatureHostedWitness, WantLapsed: true, WantErr: "lapsed on",
	}, { // Test 11: A lapsed Team license without the add-on never had it, so it is no lapse.
		Name: "lapsed team", License: lic(TierTeam, lapsed), Feature: FeatureHostedWitness,
		WantErr: "does not include it",
	}, { // Test 12: A lapsed Pro license listing the add-on never had it either.
		Name: "lapsed pro add-on", License: lic(TierPro, lapsed, AddonHostedWitness),
		Feature: FeatureHostedWitness, WantErr: "does not include it",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := allowAt(test.License, test.Feature, now)
			if test.WantAllowed {
				if err != nil {
					t.Fatalf("allowAt() error = %v, want allowed", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("allowAt() allowed %s without a covering license", test.Feature)
			}
			if got := errors.Is(err, ErrLapsed); got != test.WantLapsed {
				t.Errorf("errors.Is(err, ErrLapsed) = %v, want %v: %v", got, test.WantLapsed, err)
			}
			if !strings.Contains(err.Error(), test.WantErr) {
				t.Errorf("allowAt() error = %q, want it to contain %q", err, test.WantErr)
			}
			if !strings.Contains(err.Error(), "switchtender.com/pricing") {
				t.Errorf("a gate line without the destination: %q", err)
			}
		})
	}
}

// TestCoversFollowsTheAddonRule pins the exported coverage rule the hosted witness's enrollment
// policy applies to a license other than the process's own: Team by the add-on, Enterprise by its
// tier, and Pro never, whatever it lists.
func TestCoversFollowsTheAddonRule(t *testing.T) {
	t.Parallel()
	lic := func(tier string, addons ...string) *License {
		return &License{Claims: Claims{Tier: tier, Addons: addons}}
	}
	tests := []struct {
		Name        string
		License     *License
		Feature     Feature
		WantCovered bool
	}{{ // Test 0: Team with the add-on covers the hosted witness.
		Name: "team add-on", License: lic(TierTeam, AddonHostedWitness),
		Feature: FeatureHostedWitness, WantCovered: true,
	}, { // Test 1: Team without the add-on does not.
		Name: "team", License: lic(TierTeam), Feature: FeatureHostedWitness,
	}, { // Test 2: Pro does not, even listing the add-on.
		Name: "pro add-on", License: lic(TierPro, AddonHostedWitness),
		Feature: FeatureHostedWitness,
	}, { // Test 3: Enterprise covers it without the add-on.
		Name: "enterprise", License: lic(TierEnterprise), Feature: FeatureHostedWitness,
		WantCovered: true,
	}, { // Test 4: A license with no tier reads as Team, so the add-on applies.
		Name: "legacy add-on", License: lic("", AddonHostedWitness),
		Feature: FeatureHostedWitness, WantCovered: true,
	}, { // Test 5: The add-on grants only its own feature.
		Name: "add-on other feature", License: lic(TierPro, AddonHostedWitness),
		Feature: FeaturePolicyFull,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			if got := test.License.Covers(test.Feature); got != test.WantCovered {
				t.Errorf("Covers(%s) = %v, want %v", test.Feature, got, test.WantCovered)
			}
		})
	}
}

// TestAddonsRoundTrip checks a signed add-on survives the trip through disk and reaches coverage,
// through the real signing path rather than a hand-built struct.
func TestAddonsRoundTrip(t *testing.T) {
	t.Parallel()
	priv := fixtureKey(t)
	c := Claims{
		V: 1, ID: "lic_addon_rt", Org: "Acme", Tier: TierTeam, Hosts: "250",
		Issued: "2026-09-01T00:00:00Z", Expires: "2027-09-01T00:00:00Z", Kid: fixtureKid,
		Addons: []string{AddonHostedWitness},
	}
	raw, err := Sign(c, priv)
	if err != nil {
		t.Fatalf("Sign() error = %v", err)
	}
	l, err := Verify(raw, "test")
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	now := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	if err := allowAt(l, FeatureHostedWitness, now); err != nil {
		t.Errorf("a verified Team license with the add-on was refused: %v", err)
	}
}
