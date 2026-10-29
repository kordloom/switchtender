package license

import (
	"errors"
	"testing"
)

// TestAllowRefusesWhatTheBuildDoesNotCarry checks that a live Team license on a build without the
// worker code refuses workers as a missing tier, in the Community line, and that carrying the code
// is the only thing that changes the answer.
func TestAllowRefusesWhatTheBuildDoesNotCarry(t *testing.T) {
	before := Current()
	t.Cleanup(func() { Set(before) })
	Set(&License{Claims: Claims{
		V: 1, ID: "lic_test", Org: "test", Tier: TierTeam,
		Issued: "2026-01-01T00:00:00Z", Expires: "2099-01-01T00:00:00Z",
	}})
	Uncarry(FeatureWorkers)
	t.Cleanup(func() { Uncarry(FeatureWorkers) })

	err := Allow(FeatureWorkers)
	if !errors.Is(err, ErrRequiresTier) {
		t.Fatalf("Allow(FeatureWorkers) = %v, want ErrRequiresTier", err)
	}
	if got, want := err.Error(), Absent(FeatureWorkers).Error(); got != want {
		t.Errorf("Allow(FeatureWorkers) = %q, want %q", got, want)
	}

	// Negative control: carrying the code lets the license answer.
	Carry(FeatureWorkers)
	if err := Allow(FeatureWorkers); err != nil {
		t.Errorf("Allow(FeatureWorkers) with the code carried = %v, want nil", err)
	}

	if !PaidBuild() {
		t.Error("PaidBuild() = false with workers carried, want true")
	}

	// A built-in feature is carried with nothing registered, and that alone is not a paid build.
	Uncarry(FeatureSSO, FeaturePolicyFull, FeaturePolicyRules, FeatureRegister, FeatureWorkers,
		FeatureReconcile)
	if PaidBuild() {
		t.Error("PaidBuild() = true with nothing carried, want false")
	}
	if err := Allow(FeaturePostgresInit); err != nil {
		t.Errorf("Allow(FeaturePostgresInit) with nothing carried = %v, want nil", err)
	}
}
