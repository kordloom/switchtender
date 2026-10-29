package policy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/run"
)

// ladderLicense returns a license at tier whose term ends at expires, relative to now.
func ladderLicense(tier string, expires time.Duration) *license.License {
	return &license.License{Claims: license.Claims{
		V: 1, ID: "lic_ladder", Org: "Example", Tier: tier, Hosts: "500",
		Issued:  time.Now().Add(-72 * time.Hour).Format(time.RFC3339),
		Expires: time.Now().Add(expires).Format(time.RFC3339),
	}}
}

// uncompiledRego returns a Rego program as a build without the paid compiler holds one that arrived
// from elsewhere: its bundle and digest intact, nothing to evaluate it with. The tier predicates read
// whether a policy carries a bundle, never what the bundle says.
func uncompiledRego() *RegoProgram {
	modules := []RegoModule{{
		File: "gate.rego", Source: "package switchtender\n\nhold contains \"held\" if true\n",
	}}
	return &RegoProgram{
		pkg: "data.switchtender", syntax: RegoSyntaxV1, modules: modules,
		digest:  RegoDigest("data.switchtender", RegoSyntaxV1, modules),
		timeout: DefaultRegoTimeout, warn: RegoWarnHold,
	}
}

// TestTheLadderSplitsOnOnePredicatePair holds every rule to one answer about its tier. Deny rules
// and the risk and reversibility floors are Pro. Rego, separation of duties, and actor, actor kind,
// and account scoping are Team. An exemption is neither, whatever it carries: it is the free way
// past the built-in agent hold, and Validate refuses the floors on one, so a rule no tier accepts is
// refused as invalid rather than sold.
func TestTheLadderSplitsOnOnePredicatePair(t *testing.T) {
	t.Parallel()
	rego := uncompiledRego()
	tests := []struct {
		// Policy is the rule classified.
		Policy Policy
		// WantPro is whether it uses a Pro rule.
		WantPro bool
		// WantTeam is whether it uses a Team rule.
		WantTeam bool
	}{{ // Test 0: A plain hold is Community.
		Policy: Policy{Tool: "bash"},
	}, { // Test 1: A deny rule is Pro.
		Policy: Policy{Effect: EffectDeny}, WantPro: true,
	}, { // Test 2: A risk floor is Pro.
		Policy: Policy{MinRisk: run.RiskHigh}, WantPro: true,
	}, { // Test 3: A reversibility floor is Pro.
		Policy: Policy{Reversibility: run.Irreversible}, WantPro: true,
	}, { // Test 4: Separation of duties is Team.
		Policy: Policy{RequireDistinctApprover: true}, WantTeam: true,
	}, { // Test 5: Scoping to agents as a class is Team.
		Policy: Policy{ActorKind: ActorKindAgent}, WantTeam: true,
	}, { // Test 6: Scoping to one named actor is Team.
		Policy: Policy{Actor: "deploy-agent"}, WantTeam: true,
	}, { // Test 7: Scoping to one account is Team.
		Policy: Policy{Account: "team-a"}, WantTeam: true,
	}, { // Test 8: A Rego policy is Team.
		Policy: Policy{Rego: rego}, WantTeam: true,
	}, { // Test 9: A deny rule scoped to an account uses both.
		Policy: Policy{Effect: EffectDeny, Account: "team-a"}, WantPro: true, WantTeam: true,
	}, { // Test 10: An exemption naming an account stays free.
		Policy: Policy{Effect: EffectExempt, Account: "team-a"},
	}, { // Test 11: An exemption naming an agent and its account stays free.
		Policy: Policy{Effect: EffectExempt, ActorKind: ActorKindAgent, Actor: "ci", Account: "a"},
	}, { // Test 12: An exemption carrying a risk floor is not Pro. Validate refuses it on every tier.
		Policy: Policy{Effect: EffectExempt, MinRisk: run.RiskHigh},
	}, { // Test 13: Nor is one carrying a reversibility floor.
		Policy: Policy{Effect: EffectExempt, Reversibility: run.Irreversible},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := test.Policy
			if got := p.ProRules(); got != test.WantPro {
				t.Errorf("ProRules() = %v, want %v for %+v", got, test.WantPro, p)
			}
			if got := p.TeamRules(); got != test.WantTeam {
				t.Errorf("TeamRules() = %v, want %v for %+v", got, test.WantTeam, p)
			}
			if got, want := p.Advanced(), test.WantPro || test.WantTeam; got != want {
				t.Errorf("Advanced() = %v, want %v: it is the pair's union", got, want)
			}
		})
	}
}

// TestAllowRulesAsksTeamBeforePro holds the gate every site shares to the ladder. A set needing both
// tiers names Team, the one that covers all of it. A lapse is reported only for rules the tier had,
// so a caller that keeps enforcing on a lapse never enforces what was never bought. Not parallel:
// the license is process state.
func TestAllowRulesAsksTeamBeforePro(t *testing.T) {
	held := license.Current()
	t.Cleanup(func() { license.Set(held) })
	tests := []struct {
		// License is the license in force, nil for Community.
		License *license.License
		// Set is the rule set asked about.
		Set []*Policy
		// WantText is a phrase the refusal carries, empty when allowed.
		WantText string
		// WantLapsed is whether the refusal is a lapse.
		WantLapsed bool
	}{{ // Test 0: A plain hold on Community.
		Set: []*Policy{{Tool: "bash"}},
	}, { // Test 1: A risk floor on Community names Pro.
		Set: []*Policy{{MinRisk: run.RiskHigh}}, WantText: "A Pro license is required",
	}, { // Test 2: A deny rule beside a distinct-approver rule on Community names Team.
		Set:      []*Policy{{Effect: EffectDeny}, {RequireDistinctApprover: true}},
		WantText: "A Team license is required",
	}, { // Test 3: Every Pro rule on a live Pro license.
		License: ladderLicense(license.TierPro, 24*time.Hour),
		Set: []*Policy{{Effect: EffectDeny}, {MinRisk: run.RiskHigh},
			{Reversibility: run.Irreversible}},
	}, { // Test 4: A distinct-approver rule on a live Pro license names Team and Pro.
		License: ladderLicense(license.TierPro, 24*time.Hour),
		Set:     []*Policy{{RequireDistinctApprover: true}},
		WantText: "Team license is required for the full policy engine; this install's Pro license " +
			"does not include it",
	}, { // Test 5: A deny rule on a lapsed Pro license is a lapse.
		License: ladderLicense(license.TierPro, -time.Hour),
		Set:     []*Policy{{Effect: EffectDeny}}, WantText: "lapsed", WantLapsed: true,
	}, { // Test 6: A deny rule beside an account-scoped one on a lapsed Pro is not: Pro never had
		// account scoping.
		License:  ladderLicense(license.TierPro, -time.Hour),
		Set:      []*Policy{{Effect: EffectDeny}, {Account: "team-a"}},
		WantText: "A Team license is required",
	}, { // Test 7: The same set on a lapsed Team license is a lapse.
		License: ladderLicense(license.TierTeam, -time.Hour),
		Set:     []*Policy{{Effect: EffectDeny}, {Account: "team-a"}}, WantText: "lapsed",
		WantLapsed: true,
	}, { // Test 8: An exemption naming an account on Community.
		Set: []*Policy{{Effect: EffectExempt, Account: "team-a"}},
	}}
	for testNum, test := range tests {
		license.Set(test.License)
		err := AllowRules(test.Set...)
		if test.WantText == "" {
			if err != nil {
				t.Errorf("test %d: AllowRules() = %v, want nil", testNum, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), test.WantText) {
			t.Errorf("test %d: AllowRules() = %v, want a refusal containing %q", testNum, err,
				test.WantText)
		}
		if got := errors.Is(err, license.ErrLapsed); got != test.WantLapsed {
			t.Errorf("test %d: errors.Is(err, ErrLapsed) = %v, want %v: %v", testNum, got,
				test.WantLapsed, err)
		}
	}
}

// TestALapsedProKeepsLoadingADenyRuleFromTheFile holds the lapse promise on the tier that now sells
// deny rules. A Pro install's policy file with a deny rule keeps loading after the term runs out: a
// lapse takes nothing. A rule Pro never covered is still refused under the same lapsed license, so
// the lapse is not a way to Team. Not parallel: the license is process state.
func TestALapsedProKeepsLoadingADenyRuleFromTheFile(t *testing.T) {
	held := license.Current()
	t.Cleanup(func() { license.Set(held) })
	path := filepath.Join(t.TempDir(), "policies.yml")
	write := func(body string) {
		t.Helper()
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatalf("write policy file: %v", err)
		}
	}
	write("policies:\n  - name: never destroy prod\n    tool: terraform\n    effect: deny\n")
	license.Set(ladderLicense(license.TierPro, 24*time.Hour))
	store, err := NewFileStore(path)
	if err != nil {
		t.Fatalf("NewFileStore() error = %v", err)
	}
	if _, err := store.List(context.Background()); err != nil {
		t.Fatalf("a live Pro license was refused a deny rule from its file: %v", err)
	}

	// Test 0: Midnight. The same file keeps loading. That the deny rule keeps refusing is the paid
	// rule engine's, and is tested with it.
	license.Set(ladderLicense(license.TierPro, -time.Hour))
	set, err := store.List(context.Background())
	if err != nil {
		t.Fatalf("a lapsed Pro license took the install offline: List() error = %v", err)
	}
	if len(set) != 1 || set[0].Name != "never destroy prod" {
		t.Errorf("List() = %v, want the deny rule the file declares", set)
	}

	// Test 1: A rule Pro never had is refused under the same lapsed license, and not as a lapse.
	write("policies:\n  - name: two people\n    require_distinct_approver: true\n")
	_, err = store.List(context.Background())
	if err == nil {
		t.Fatal("a lapsed Pro license was served separation of duties, which Pro never included")
	}
	if errors.Is(err, license.ErrLapsed) || strings.Contains(err.Error(), "lapsed") {
		t.Errorf("a Team rule under a lapsed Pro was refused as a lapse: %v", err)
	}
}
