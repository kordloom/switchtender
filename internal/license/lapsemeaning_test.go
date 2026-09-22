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

// TestALapsedPolicyCapIsOnlyALapseUpToWhatTheTierHeld pins the same rule on the count cap.
//
// A Pro license holds five approval policies. An expired Pro asked to hold fifty never held fifty,
// so that refusal is not a lapse and a caller must not treat it as one. Asked to hold five, it did
// hold five yesterday, so that one is.
func TestALapsedPolicyCapIsOnlyALapseUpToWhatTheTierHeld(t *testing.T) {
	t.Parallel()
	lapsedPro := &License{Claims: Claims{
		V: 1, ID: "lic", Org: "Example", Tier: TierPro,
		Issued:  time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
		Expires: time.Now().Add(-24 * time.Hour).Format(time.RFC3339),
	}}
	now := time.Now()

	if err := allowPoliciesAt(lapsedPro, proPolicyCap, now); !errors.Is(err, ErrLapsed) {
		t.Errorf("holding %d policies on a lapsed Pro = %v, want a lapse: it held exactly this "+
			"many yesterday", proPolicyCap, err)
	}
	over := proPolicyCap + 45
	err := allowPoliciesAt(lapsedPro, over, now)
	if err == nil {
		t.Fatalf("a lapsed Pro was allowed %d policies", over)
	}
	if errors.Is(err, ErrLapsed) {
		t.Errorf("holding %d policies on a lapsed Pro was reported as a lapse, but a live Pro "+
			"holds %d: a caller that keeps serving on a lapse would serve a set this license "+
			"never held", over, proPolicyCap)
	}
}
