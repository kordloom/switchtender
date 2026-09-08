package policy_test

import (
	"encoding/hex"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
)

// TestInForceDigestCoversEveryEnforcingField walks the rule fields one at a time and proves each one
// moves the digest.
//
// The digest is the only record that says what the rules were for a run nothing stopped. A field it
// does not cover is a rule an operator can change, run a change through, and change back, leaving two
// runs whose recorded rule sets are byte-identical while the gate between them was different. That is
// precisely the gap this record was added to close, so every enforcing field has to be in it.
func TestInForceDigestCoversEveryEnforcingField(t *testing.T) {
	t.Parallel()
	base := policy.Policy{
		ID: "pol_1", Name: "prod gate", Tool: run.ToolTerraform, CommandContains: "destroy",
		InventoryID: "inv_prod", ActorKind: policy.ActorKindAgent, Actor: "bot",
		MinRisk: run.RiskMedium, Effect: policy.EffectRequireApproval, ExcludeDryRun: true,
		MaxDestroy: 5, RequireDistinctApprover: true,
		CreatedAt: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC),
	}
	tests := []struct {
		// Change makes one edit to a copy of the base rule.
		Change func(p *policy.Policy)
		// WantDifferent is whether that edit must move the digest.
		WantDifferent bool
	}{{ // Test 0: Loosening the destroy threshold is a change to what the rule enforces.
		Change: func(p *policy.Policy) { p.MaxDestroy = 500 }, WantDifferent: true,
	}, { // Test 1: Turning the threshold off entirely is a change.
		Change:        func(p *policy.Policy) { p.MaxDestroy = policy.DisabledMaxDestroy },
		WantDifferent: true,
	}, { // Test 2: Pointing the rule at another tool is a change.
		Change: func(p *policy.Policy) { p.Tool = run.ToolBash }, WantDifferent: true,
	}, { // Test 3: Narrowing the command it matches is a change.
		Change: func(p *policy.Policy) { p.CommandContains = "apply" }, WantDifferent: true,
	}, { // Test 4: Repointing the inventory is a change.
		Change: func(p *policy.Policy) { p.InventoryID = "inv_dev" }, WantDifferent: true,
	}, { // Test 5: Changing which kind of actor it binds is a change.
		Change: func(p *policy.Policy) { p.ActorKind = policy.ActorKindHuman }, WantDifferent: true,
	}, { // Test 6: Changing the named principal is a change.
		Change: func(p *policy.Policy) { p.Actor = "other-bot" }, WantDifferent: true,
	}, { // Test 7: Raising the risk floor is a change.
		Change: func(p *policy.Policy) { p.MinRisk = run.RiskHigh }, WantDifferent: true,
	}, { // Test 8: Turning a hold into a refusal is a change.
		Change:        func(p *policy.Policy) { p.Effect = policy.EffectDeny; p.MaxDestroy = -1 },
		WantDifferent: true,
	}, { // Test 9: Letting dry runs through is a change.
		Change: func(p *policy.Policy) { p.ExcludeDryRun = false }, WantDifferent: true,
	}, { // Test 10: Dropping separation of duties is a change, and it is the one an operator would
		// most want back afterwards.
		Change: func(p *policy.Policy) { p.RequireDistinctApprover = false }, WantDifferent: true,
	}, { // Test 11: Renaming a rule is not a change to what it enforces, so the digest holds still.
		Change: func(p *policy.Policy) { p.Name = "renamed" }, WantDifferent: false,
	}, { // Test 12: Nor is a new id, which is what a rule moved between stores gets.
		Change: func(p *policy.Policy) { p.ID = "pol_moved" }, WantDifferent: false,
	}, { // Test 13: Nor is a different creation time.
		Change:        func(p *policy.Policy) { p.CreatedAt = time.Now() },
		WantDifferent: false,
	}}
	want := policy.InForce([]*policy.Policy{&base})
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			edited := base
			test.Change(&edited)
			got := policy.InForce([]*policy.Policy{&edited})
			if different := got.Digest != want.Digest; different != test.WantDifferent {
				t.Errorf("digest moved = %v, want %v after editing the rule to %+v",
					different, test.WantDifferent, edited)
			}
		})
	}
}

// TestInForceDescribesAnEmptyAndAGrowingSet pins the shape of the record itself.
//
// The count and the digest cover the whole set whether or not every rule is named, so an install with
// hundreds of rules does not put hundreds of strings on every run record. The cap is the part worth
// pinning: a set truncated without saying so would read as a smaller set than it was.
func TestInForceDescribesAnEmptyAndAGrowingSet(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Count is how many rules are in force.
		Count int
		// WantNamed is how many entries the rules list must carry.
		WantNamed int
		// WantOverflowLine is whether the last entry must say how many were left out.
		WantOverflowLine bool
	}{
		{Count: 0, WantNamed: 0},                            // Test 0: No rules at all.
		{Count: 1, WantNamed: 1},                            // Test 1: One rule.
		{Count: 39, WantNamed: 39},                          // Test 2: Just under the cap.
		{Count: 40, WantNamed: 40},                          // Test 3: Exactly the cap.
		{Count: 41, WantNamed: 41, WantOverflowLine: true},  // Test 4: One over the cap.
		{Count: 500, WantNamed: 41, WantOverflowLine: true}, // Test 5: Far over the cap.
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			policies := make([]*policy.Policy, 0, test.Count)
			for i := range test.Count {
				policies = append(policies, &policy.Policy{
					ID: fmt.Sprintf("pol_%03d", i), Name: fmt.Sprintf("rule %03d", i),
					MaxDestroy: policy.DisabledMaxDestroy,
				})
			}
			got := policy.InForce(policies)
			if got.Count != test.Count {
				t.Errorf("Count = %d, want %d: the count covers the whole set", got.Count, test.Count)
			}
			if len(got.Rules) != test.WantNamed {
				t.Fatalf("named %d rules, want %d", len(got.Rules), test.WantNamed)
			}
			if raw, err := hex.DecodeString(got.Digest); err != nil || len(raw) != 32 {
				t.Errorf("Digest = %q, want 32 hex-encoded bytes", got.Digest)
			}
			if test.WantOverflowLine {
				last := got.Rules[len(got.Rules)-1]
				if !strings.Contains(last, "more rules") ||
					!strings.Contains(last, fmt.Sprint(test.Count-40)) {
					t.Errorf("last named rule = %q, want it to say how many were left out", last)
				}
			}
			// The rules are named in a stable order, so two servers describing the same set write the
			// same record.
			if diff := cmp.Diff(got.Rules, policy.InForce(policies).Rules, cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("naming is not stable (-first +second):\n%s", diff)
			}
		})
	}
}

// TestInForceDescribesWhatEachRuleDoes pins the human half of the record, which is what an auditor
// reads before they read anything else.
//
// A label that does not say the rule denies, or does not say a second person is required, describes a
// weaker control than the one that was actually in force.
func TestInForceDescribesWhatEachRuleDoes(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule to describe.
		Policy policy.Policy
		// WantRule is the exact description the record must carry.
		WantRule string
	}{{ // Test 0: A blanket hold.
		Policy:   policy.Policy{ID: "pol_1", Name: "hold", MaxDestroy: policy.DisabledMaxDestroy},
		WantRule: "hold: requires approval",
	}, { // Test 1: A refusal.
		Policy: policy.Policy{
			ID: "pol_2", Name: "refuse", Effect: policy.EffectDeny,
			MaxDestroy: policy.DisabledMaxDestroy,
		}, WantRule: "refuse: denies",
	}, { // Test 2: A plan-content threshold says the number, since the number is the control.
		Policy:   policy.Policy{ID: "pol_3", Name: "teardown", MaxDestroy: 5},
		WantRule: "teardown: requires approval over 5 destroys",
	}, { // Test 3: A threshold of zero says zero rather than reading as a blanket rule.
		Policy:   policy.Policy{ID: "pol_4", Name: "any", MaxDestroy: 0},
		WantRule: "any: requires approval over 0 destroys",
	}, { // Test 4: Separation of duties is named, because a hold one person can release is a
		// different control from one that needs two.
		Policy: policy.Policy{
			ID: "pol_5", Name: "two people", MaxDestroy: policy.DisabledMaxDestroy,
			RequireDistinctApprover: true,
		}, WantRule: "two people: requires approval, by someone other than the requester",
	}, { // Test 5: A rule with no name is named by its id, so no line of the record is blank.
		Policy:   policy.Policy{ID: "pol_6", MaxDestroy: policy.DisabledMaxDestroy},
		WantRule: "pol_6: requires approval",
	}, { // Test 6: A plan-content rule that also demands a second person says both.
		Policy: policy.Policy{
			ID: "pol_7", Name: "big teardown", MaxDestroy: 20, RequireDistinctApprover: true,
		}, WantRule: "big teardown: requires approval over 20 destroys, by someone other than the requester",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			p := test.Policy
			got := policy.InForce([]*policy.Policy{&p})
			if diff := cmp.Diff([]string{test.WantRule}, got.Rules); diff != "" {
				t.Errorf("rule description mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestInForceSkipsNilRulesWithoutCountingThem pins that a nil entry in the list, which a store that
// dropped a row could hand over, neither panics nor inflates the count an auditor reads.
func TestInForceSkipsNilRulesWithoutCountingThem(t *testing.T) {
	t.Parallel()
	real := &policy.Policy{ID: "pol_1", Name: "gate", MaxDestroy: policy.DisabledMaxDestroy}
	with := policy.InForce([]*policy.Policy{nil, real, nil})
	without := policy.InForce([]*policy.Policy{real})
	if with.Count != 1 {
		t.Errorf("Count = %d, want 1: a nil entry is not a rule", with.Count)
	}
	if with.Digest != without.Digest {
		t.Error("a nil entry changed the digest, so a store hiccup reads as a rule set change")
	}
	if diff := cmp.Diff(without.Rules, with.Rules, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("named rules mismatch (-want +got):\n%s", diff)
	}
	// An entirely nil list is the same record as no list at all.
	if policy.InForce([]*policy.Policy{nil, nil}).Digest != policy.InForce(nil).Digest {
		t.Error("a list of nothing but nil entries differs from an empty rule set")
	}
}

// TestInForceCountsDuplicateRules pins that adding a second copy of a rule is visible.
//
// Two identical rules are not one rule. An operator who duplicates a gate has changed the set, and a
// digest that collapsed duplicates would hide the reverse move, deleting one of a pair, entirely.
func TestInForceCountsDuplicateRules(t *testing.T) {
	t.Parallel()
	one := &policy.Policy{ID: "pol_1", Name: "gate", MaxDestroy: policy.DisabledMaxDestroy}
	two := &policy.Policy{ID: "pol_2", Name: "gate copy", MaxDestroy: policy.DisabledMaxDestroy}
	single := policy.InForce([]*policy.Policy{one})
	doubled := policy.InForce([]*policy.Policy{one, two})
	if doubled.Count != 2 {
		t.Errorf("Count = %d, want 2", doubled.Count)
	}
	if doubled.Digest == single.Digest {
		t.Error("duplicating a rule left the digest unchanged, so deleting one of a duplicated pair " +
			"would leave no trace either")
	}
}
