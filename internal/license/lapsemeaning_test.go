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

// TestAMalformedTermRefusesRatherThanRecursing pins that a license file nobody can parse cannot take
// the process down.
//
// Deciding whether a refusal counts as a lapse means asking what this license held while it was
// live. Asking that by calling back in with a time inside the term looked right and was not: a term
// whose Expires does not parse reads as expired at every instant, including the one chosen to be
// inside it, so the call recursed on identical arguments until the stack ran out. A license file is
// operator-supplied and a malformed one is ordinary, so that is a crash anybody could trigger by
// mistyping a date.
func TestAMalformedTermRefusesRatherThanRecursing(t *testing.T) {
	t.Parallel()
	broken := &License{Claims: Claims{
		V: 1, ID: "lic_broken", Org: "Example", Tier: TierPro,
		Issued: "not a date", Expires: "also not a date",
	}}
	now := time.Now()

	// Well past what any tier below Team holds, which is the branch that used to recurse.
	if err := allowPoliciesAt(broken, proPolicyCap+40, now); err == nil {
		t.Error("a license whose term does not parse was allowed an uncapped policy set")
	}
	// And the ordinary feature gate on the same license.
	if err := allowAt(broken, FeaturePolicyFull, now); err == nil {
		t.Error("a license whose term does not parse was allowed a paid feature")
	}
	// A count every tier holds is still fine, so the refusal is about the cap and not about the
	// license being unreadable.
	if err := allowPoliciesAt(broken, 1, now); err != nil {
		t.Errorf("one policy was refused on a malformed license: %v", err)
	}
}
