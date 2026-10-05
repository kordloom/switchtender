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

// TestTheGateMapIsWhatThePricingPageSells pins which tier each paid feature needs, and Pro's
// policy cap, to the tier cards a buyer reads. Moving a feature between tiers or changing the cap
// fails here until the pricing page and this table change together.
func TestTheGateMapIsWhatThePricingPageSells(t *testing.T) {
	t.Parallel()
	want := map[Feature]string{
		FeatureSSO:          TierPro,
		FeaturePolicyFull:   TierTeam,
		FeatureRegister:     TierTeam,
		FeatureWorkers:      TierTeam,
		FeaturePostgresInit: TierTeam,
		FeatureReconcile:    TierTeam,
	}
	if diff := cmp.Diff(want, featureTier); diff != "" {
		t.Errorf("the gate map moved; update the pricing page with it (-want +got):\n%s", diff)
	}
	if proPolicyCap != 5 {
		t.Errorf("Pro holds %d policies, and the pricing page sells five", proPolicyCap)
	}
	raw, err := os.ReadFile(filepath.Join("..", "..", "site", "pricing.html"))
	if err != nil {
		t.Fatalf("read pricing page: %v", err)
	}
	page := regexp.MustCompile(`\s+`).ReplaceAllString(string(raw), " ")
	tests := []struct {
		Tier       string
		WantPhrase string
	}{{ // Test 0: Pro's policy cap.
		Tier: "Pro", WantPhrase: "<b>Five approval policies</b> instead of one",
	}, { // Test 1: Pro's directory sign-in, FeatureSSO.
		Tier: "Pro", WantPhrase: "<b>Directory sign-in</b>",
	}, { // Test 2: FeaturePolicyFull.
		Tier: "Team", WantPhrase: "<b>The full policy engine</b>",
	}, { // Test 3: FeatureRegister.
		Tier: "Team", WantPhrase: "<b>The period change register</b>",
	}, { // Test 4: FeaturePostgresInit.
		Tier: "Team", WantPhrase: "<b>Creating a new PostgreSQL schema</b>",
	}, { // Test 5: FeatureWorkers.
		Tier: "Team", WantPhrase: "<b>Distributed workers</b>",
	}, { // Test 6: FeatureReconcile.
		Tier: "Team", WantPhrase: "<b>One-click drift reconcile</b>",
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
