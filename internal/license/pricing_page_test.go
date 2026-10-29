package license

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestTheGateMapIsWhatThePricingPageSells pins which tier each paid feature needs, which add-on
// sells a feature below that tier, and Pro's policy cap, to the tier cards a buyer reads. Moving a
// feature between tiers, selling it as an add-on, or changing the cap fails here until the pricing
// page and this table change together.
func TestTheGateMapIsWhatThePricingPageSells(t *testing.T) {
	t.Parallel()
	want := map[Feature]string{
		FeatureSSO:           TierPro,
		FeaturePolicyFull:    TierTeam,
		FeaturePolicyRules:   TierPro,
		FeatureRegister:      TierTeam,
		FeatureWorkers:       TierTeam,
		FeatureReconcile:     TierTeam,
		FeatureHostedWitness: TierEnterprise,
	}
	if diff := cmp.Diff(want, featureTier); diff != "" {
		t.Errorf("the gate map moved; update the pricing page with it (-want +got):\n%s", diff)
	}
	wantAddons := map[Feature]string{FeatureHostedWitness: AddonHostedWitness}
	if diff := cmp.Diff(wantAddons, featureAddon); diff != "" {
		t.Errorf("the add-on map moved; update the pricing page with it (-want +got):\n%s", diff)
	}
	if addonTier != TierTeam {
		t.Errorf("add-ons are sold from %s, and the pricing page sells them on Team", addonTier)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "site", "pricing.html"))
	if err != nil {
		t.Fatalf("read pricing page: %v", err)
	}
	page := regexp.MustCompile(`\s+`).ReplaceAllString(string(raw), " ")
	tests := []struct {
		Tier       string
		WantPhrase string
	}{{ // Test 0: Community holds plain rules uncapped, since no tier counts them.
		Tier: "Community", WantPhrase: "<b>Plain rules, as many as you need</b>",
	}, { // Test 1: Pro's directory sign-in, FeatureSSO.
		Tier: "Pro", WantPhrase: "<b>Directory sign-in</b>",
	}, { // Test 2: FeaturePolicyFull.
		Tier: "Team", WantPhrase: "<b>The full policy engine</b>",
	}, { // Test 3: FeatureRegister.
		Tier: "Team", WantPhrase: "<b>The period change register</b>",
	}, { // Test 4: FeatureWorkers.
		Tier: "Team", WantPhrase: "<b>Distributed workers</b>",
	}, { // Test 5: FeatureReconcile.
		Tier: "Team", WantPhrase: "<b>One-click drift reconcile</b>, from detection to a held, approvable fix. " +
			"Terraform and OpenTofu reconcile the exact plan the check kept. Ansible reruns the " +
			"playbook on the drifted host, bound to the check that found it, a rerun you can also " +
			"launch by hand on Community",
	}, { // Test 6: FeaturePolicyRules, which Pro sells and the gate names in the same words.
		Tier: "Pro", WantPhrase: "<b>Deny rules and risk and reversibility floors</b>",
	}, { // Test 7: FeatureHostedWitness, which Team buys as an add-on.
		Tier: "Team", WantPhrase: "<b>Hosted witness add-on</b>, $3,000 a year",
	}, { // Test 8: FeatureHostedWitness, which Enterprise includes.
		Tier: "Enterprise", WantPhrase: "<b>Hosted witness, included</b>",
	}, { // Test 9: The Pro card names the paid build.
		Tier: "Pro", WantPhrase: "The paid build, unlocked by a signed license file the product verifies offline.",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			start := strings.Index(page, `<div class="tier-name">`+test.Tier+`</div>`)
			if start < 0 {
				t.Fatalf("the pricing page has no %s card", test.Tier)
			}
			end := strings.Index(page[start:], "</article>")
			if end < 0 {
				t.Fatalf("the %s card never closes", test.Tier)
			}
			if !strings.Contains(page[start:start+end], test.WantPhrase) {
				t.Errorf("the %s card no longer says %q", test.Tier, test.WantPhrase)
			}
		})
	}
}
