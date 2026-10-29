package paid

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/kordloom/switchtender/internal/license"
)

// TestStubsRefuseNamingTheTier checks that every Community stub refuses with ErrRequiresTier and
// the same line license.Allow prints for an unlicensed install, naming the tier.
func TestStubsRefuseNamingTheTier(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		Name     string
		Call     func() error
		WantTier string
		WantGate string
	}{{ // Test 0: OIDC is Pro.
		Name: "oidc", WantTier: "Pro", WantGate: "directory sign-in",
		Call: func() error { _, err := CurrentDirectory().OIDC(ctx, OIDCConfig{}); return err },
	}, { // Test 1: SAML is Pro.
		Name: "saml", WantTier: "Pro", WantGate: "directory sign-in",
		Call: func() error { _, err := CurrentDirectory().SAML(ctx, SAMLConfig{}); return err },
	}, { // Test 2: LDAP is Pro.
		Name: "ldap", WantTier: "Pro", WantGate: "directory sign-in",
		Call: func() error { _, err := CurrentDirectory().LDAP(ctx, LDAPConfig{}); return err },
	}, { // Test 3: JWT is Pro.
		Name: "jwt", WantTier: "Pro", WantGate: "directory sign-in",
		Call: func() error { _, err := CurrentDirectory().JWT(ctx, JWTConfig{}); return err },
	}, { // Test 4: Rego is Team.
		Name: "rego", WantTier: "Team", WantGate: "the full policy engine",
		Call: func() error {
			_, err := CurrentRegoCompiler().Compile(nil, RegoSettings{})
			return err
		},
	}, { // Test 5: the register emitter is Team.
		Name: "emitter", WantTier: "Team", WantGate: "the period change register",
		Call: func() error { _, err := CurrentChangeRegister().NewEmitter(EmitterConfig{}); return err },
	}, { // Test 6: rendering the register is Team.
		Name: "render", WantTier: "Team", WantGate: "the period change register",
		Call: func() error {
			_, err := CurrentChangeRegister().Render(ctx, RegisterSource{}, RegisterPeriod{})
			return err
		},
	}, { // Test 7: building the register is Team.
		Name: "build", WantTier: "Team", WantGate: "the period change register",
		Call: func() error {
			_, err := CurrentChangeRegister().Build(ctx, RegisterSource{}, RegisterPeriod{})
			return err
		},
	}, { // Test 8: the worker is Team.
		Name: "worker", WantTier: "Team", WantGate: "running distributed workers",
		Call: func() error { return CurrentWorker().Run(ctx, WorkerConfig{}) },
	}, { // Test 9: reconcile is Team.
		Name: "reconcile", WantTier: "Team", WantGate: "one-click drift reconcile",
		Call: func() error { _, err := CurrentReconciler().Propose(ctx, ReconcileRequest{}); return err },
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			err := test.Call()
			if err == nil {
				t.Fatal("stub returned nil, which is the silent unlock the seam exists to prevent")
			}
			if !errors.Is(err, license.ErrRequiresTier) {
				t.Errorf("error does not wrap ErrRequiresTier: %v", err)
			}
			msg := err.Error()
			want := "A " + test.WantTier + " license is required for " + test.WantGate
			if !strings.Contains(msg, want) {
				t.Errorf("message %q does not contain %q", msg, want)
			}
			if !strings.Contains(msg, "this install runs Community") {
				t.Errorf("message %q does not say the install runs Community", msg)
			}
		})
	}
}

// TestRequiresMatchesLicenseAllow checks that the stub line is the unlicensed line license.Allow
// prints, word for word after the sentinel.
func TestRequiresMatchesLicenseAllow(t *testing.T) {
	t.Parallel()
	for _, f := range []license.Feature{license.FeatureSSO, license.FeaturePolicyFull,
		license.FeaturePolicyRules, license.FeatureRegister, license.FeatureWorkers,
		license.FeatureReconcile} {
		want := license.Unlicensed(f).Error()
		got := Requires(f).Error()
		if !strings.HasSuffix(got, ": "+want) {
			t.Errorf("Requires(%s) = %q, want suffix %q", f, got, want)
		}
	}
}

// TestSlotReturnsStubUntilRegistered checks that a slot answers with its stub, then with what was
// registered, and panics on a second registration.
func TestSlotReturnsStubUntilRegistered(t *testing.T) {
	t.Parallel()
	s := NewSlot[Worker]("test worker", stubWorker{})
	if s.Registered() {
		t.Fatal("fresh slot reports registered")
	}
	if err := s.Get().Run(context.Background(), WorkerConfig{}); !errors.Is(err, license.ErrRequiresTier) {
		t.Fatalf("stub did not refuse: %v", err)
	}
	s.Set(testWorker{})
	if !s.Registered() {
		t.Fatal("slot does not report registered after Set")
	}
	if err := s.Get().Run(context.Background(), WorkerConfig{}); err != nil {
		t.Fatalf("registered worker refused: %v", err)
	}
	// Negative control: a second registration panics rather than replacing the first.
	defer func() {
		if r := recover(); r == nil {
			t.Fatal("second registration did not panic")
		}
	}()
	s.Set(testWorker{})
}

// TestNothingRegisteredInTheCommunityBuild checks that the public tree alone registers nothing,
// which is what makes the public build Community.
func TestNothingRegisteredInTheCommunityBuild(t *testing.T) {
	t.Parallel()
	if license.PaidBuild() {
		t.Fatal("a paid implementation is registered in the public tree")
	}
}

// testWorker is a worker that accepts.
type testWorker struct{}

// Run accepts.
func (testWorker) Run(context.Context, WorkerConfig) error { return nil }

// TestSlotDeclaresWhatItBacks checks that registering a slot is what marks its features carried, so
// a build without the registration refuses them whatever license is installed.
func TestSlotDeclaresWhatItBacks(t *testing.T) {
	const f license.Feature = "test-slot-feature"
	t.Cleanup(func() { license.Uncarry(f) })
	s := NewSlot[Worker]("test carried worker", stubWorker{}, f)
	if license.Carried(f) {
		t.Fatal("feature carried before the slot registered")
	}
	s.Set(testWorker{})
	if !license.Carried(f) {
		t.Fatal("feature not carried after the slot registered")
	}
}
