package license

import (
	"errors"
	"testing"
	"time"
)

// TestALapseIsOnlyReportedForWhatTheLicenseCovered pins what ErrLapsed means, which is load bearing
// rather than cosmetic.
//
// Callers act on ErrLapsed by keeping a feature working: the policy file keeps serving the rules it
// was already serving, because taking a paid install offline the minute its term ran out is the one
// thing the terms promise never happens. That behavior is only safe while ErrLapsed means "this
// install had this working yesterday".
//
// Reporting the lapse before checking coverage broke that. An expired Pro license was told it had
// lapsed out of the Team-only policy engine, which it never had, and the caller then handed it
// over. An expired license bought strictly more than the same license bought while it was live,
// and the cheapest way to unlock Team was to buy Pro once and let it run out.
func TestALapseIsOnlyReportedForWhatTheLicenseCovered(t *testing.T) {
	t.Parallel()
	past := time.Now().Add(-24 * time.Hour).Format(time.RFC3339)
	future := time.Now().Add(24 * time.Hour).Format(time.RFC3339)
	issued := time.Now().Add(-72 * time.Hour).Format(time.RFC3339)
	now := time.Now()

	tests := []struct {
		Name       string
		Tier       string
		Expires    string
		Feature    Feature
		WantErr    bool
		WantLapsed bool
	}{{ // Test 0: Expired Pro, asked for a Team feature it never had. Not a lapse.
		Name: "expired pro reaching past its tier", Tier: TierPro, Expires: past,
		Feature: FeaturePolicyFull, WantErr: true, WantLapsed: false,
	}, { // Test 1: Expired Team, asked for the Team feature it did have. A lapse.
		Name: "expired team losing what it had", Tier: TierTeam, Expires: past,
		Feature: FeaturePolicyFull, WantErr: true, WantLapsed: true,
	}, { // Test 2: Live Pro reaching past its tier. Not a lapse, and refused.
		Name: "live pro reaching past its tier", Tier: TierPro, Expires: future,
		Feature: FeaturePolicyFull, WantErr: true, WantLapsed: false,
	}, { // Test 3: Live Team. Allowed.
		Name: "live team", Tier: TierTeam, Expires: future,
		Feature: FeaturePolicyFull, WantErr: false, WantLapsed: false,
	}, { // Test 4: Expired Pro, asked for the deny rules and floors it did have. A lapse, which is
		// what keeps a lapsed Pro install's deny rules enforcing from its policy file.
		Name: "expired pro losing its rules", Tier: TierPro, Expires: past,
		Feature: FeaturePolicyRules, WantErr: true, WantLapsed: true,
	}, { // Test 5: Live Pro. Allowed.
		Name: "live pro rules", Tier: TierPro, Expires: future,
		Feature: FeaturePolicyRules, WantErr: false, WantLapsed: false,
	}}

	for testNum, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			lic := &License{Claims: Claims{
				V: 1, ID: "lic", Org: "Example", Tier: test.Tier,
				Issued: issued, Expires: test.Expires,
			}}
			err := allowAt(lic, test.Feature, now)
			if test.WantErr != (err != nil) {
				t.Fatalf("test %d: allowAt() error = %v, want an error: %v", testNum, err, test.WantErr)
			}
			if got := errors.Is(err, ErrLapsed); got != test.WantLapsed {
				t.Errorf("test %d: errors.Is(err, ErrLapsed) = %v, want %v.\nA caller keeps a "+
					"feature working on ErrLapsed, so reporting it for a tier that never "+
					"included the feature hands out what was never bought: %v",
					testNum, got, test.WantLapsed, err)
			}
		})
	}
}

// TestAMalformedTermRefusesRatherThanRecursing pins that a license file nobody can parse cannot take
// the process down or buy anything.
//
// A term whose Expires does not parse reads as expired at every instant. An earlier gate decided
// whether a refusal counted as a lapse by calling back in with a time inside the term, which on
// such a license recursed on identical arguments until the stack ran out. A license file is
// operator-supplied and a malformed one is ordinary, so every gate must refuse it plainly.
func TestAMalformedTermRefusesRatherThanRecursing(t *testing.T) {
	t.Parallel()
	broken := &License{Claims: Claims{
		V: 1, ID: "lic_broken", Org: "Example", Tier: TierPro,
		Issued: "not a date", Expires: "also not a date",
	}}
	now := time.Now()

	// A feature the tier never covered.
	if err := allowAt(broken, FeaturePolicyFull, now); err == nil {
		t.Error("a license whose term does not parse was allowed a feature beyond its tier")
	}
	// A feature the tier covers, refused because the term cannot be read as live.
	if err := allowAt(broken, FeaturePolicyRules, now); !errors.Is(err, ErrLapsed) {
		t.Errorf("a Pro feature on a license whose term does not parse = %v, want a lapse", err)
	}
}
