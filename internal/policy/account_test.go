package policy

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/license"
	"github.com/kordloom/switchtender/internal/run"
)

// ciAgent returns a run the agent token labeled ci-agent submitted, bound to account. Two teams can
// each mint a token with that label, which is the case the account criterion exists for.
func ciAgent(account string) *run.Run {
	r := agentRun("ci-agent", "bash", "./deploy.sh")
	r.Account = account
	return r
}

// unnamedAccount returns an agent run requested through an account whose name it does not carry.
func unnamedAccount() *run.Run {
	r := agentRun("ci-agent", "bash", "./deploy.sh")
	r.ActorUserID = "usr_team_a"
	return r
}

// personOn returns a run a signed-in person on account submitted.
func personOn(account string) *run.Run {
	r := personRun("bash", "psql -c 'drop database staging'")
	r.Account, r.ActorUserID = account, "usr_"+account
	return r
}

// TestTheAccountCriterion holds the account criterion to the account a run was requested under.
// Two agent tokens share the label ci-agent and are bound to different accounts: an exemption
// naming one account lets that account's agent through and holds the other's, a run whose account
// is unknown fails closed in both directions, and a hold or deny rule naming an account covers
// that account's runs and no one else's.
//
//nolint:funlen // Test function.
func TestTheAccountCriterion(t *testing.T) {
	t.Parallel()
	ciOnTeamA := exemption("ci on team a", func(p *Policy) {
		p.Actor, p.Account = "ci-agent", "team-a"
	})
	teamA := exemption("team a agents", func(p *Policy) { p.Account = "team-a" })
	// labelOnly reached the store without Validate, as nothing written today can.
	labelOnly := exemption("label only", func(p *Policy) { p.Actor = "ci-agent" })
	holdTeamA := &Policy{ID: "pol_hold", Name: "team a waits", Account: "team-a",
		MaxDestroy: DisabledMaxDestroy}
	denyTeamB := &Policy{ID: "pol_deny", Name: "team b never drops", Account: "team-b",
		CommandContains: "drop database", Effect: EffectDeny, MaxDestroy: DisabledMaxDestroy}
	tests := []struct {
		// Policies are the stored rules.
		Policies []*Policy
		// Run is the run judged.
		Run *run.Run
		// WantDenied names the rule that refuses the run, empty when none does.
		WantDenied string
		// WantHeldBy names the rule that holds the run, empty when nothing does.
		WantHeldBy string
		// WantNote is what the evidence records about the built-in hold, empty for nothing.
		WantNote string
	}{{ // Test 0: The exemption covers the agent on the account it names.
		Policies: []*Policy{ciOnTeamA}, Run: ciAgent("team-a"),
		WantNote: `requested by an agent bound to account "team-a", exempt from the default hold ` +
			`by policy "ci on team a"`,
	}, { // Test 1: The same label on another account is held, which is the whole fix.
		Policies: []*Policy{ciOnTeamA}, Run: ciAgent("team-b"),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 2: An exemption naming an account alone covers every agent bound to it.
		Policies: []*Policy{teamA},
		Run: func() *run.Run {
			r := ciAgent("team-a")
			r.Actor = "triage-bot"
			return r
		}(),
		WantNote: `requested by an agent bound to account "team-a", exempt from the default hold ` +
			`by policy "team a agents"`,
	}, { // Test 3: And no agent bound to another account.
		Policies: []*Policy{teamA}, Run: ciAgent("team-b"),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 4: A run that does not carry its account's name is not exempt.
		Policies: []*Policy{teamA}, Run: unnamedAccount(),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 5: Nor is an agent's run with no account at all.
		Policies: []*Policy{teamA}, Run: agentRun("ci-agent", "bash", "./deploy.sh"),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 6: An exemption naming the label alone exempts nothing, even for its own label.
		Policies: []*Policy{labelOnly}, Run: ciAgent("team-a"),
		WantHeldBy: AgentDefaultName, WantNote: AgentDefaultName,
	}, { // Test 7: A hold naming an account holds that account's person.
		Policies: []*Policy{holdTeamA}, Run: personOn("team-a"), WantHeldBy: "team a waits",
	}, { // Test 8: And no other account's.
		Policies: []*Policy{holdTeamA}, Run: personOn("team-b"),
	}, { // Test 9: A run whose account name is unknown is held by it, failing closed.
		Policies: []*Policy{holdTeamA},
		Run: func() *run.Run {
			r := personOn("team-a")
			r.Account = ""
			return r
		}(),
		WantHeldBy: "team a waits",
	}, { // Test 10: A run no account stands behind is not covered by a rule naming one.
		Policies: []*Policy{holdTeamA},
		Run:      &run.Run{Actor: "system:scheduler", ActorType: "system", Tool: "bash"},
	}, { // Test 11: A deny naming an account refuses that account's run.
		Policies: []*Policy{denyTeamB}, Run: personOn("team-b"), WantDenied: "team b never drops",
	}, { // Test 12: And not the same run on another account.
		Policies: []*Policy{denyTeamB}, Run: personOn("team-a"),
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := Denying(test.Policies, test.Run).Label(); got != test.WantDenied {
				t.Errorf("Denying() = %q, want %q", got, test.WantDenied)
			}
			if got := Requiring(test.Policies, test.Run).Label(); got != test.WantHeldBy {
				t.Errorf("Requiring() = %q, want %q", got, test.WantHeldBy)
			}
			if got := AgentNote(test.Policies, test.Run); got != test.WantNote {
				t.Errorf("AgentNote() = %q, want %q", got, test.WantNote)
			}
		})
	}
}

// TestValidateTheExemptionAccount holds where an exemption must name an account: an exemption that
// names an agent's label without the account its token is bound to is refused with a reason, since
// the label repeats across accounts. One naming the account alone, or neither, is still allowed,
// and a rule that holds may name a label alone, since it fails safe.
func TestValidateTheExemptionAccount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule validated.
		Policy Policy
		// Want is the error expected, nil when the rule is accepted.
		Want error
	}{{ // Test 0: A label without its account is refused.
		Policy: Policy{Effect: EffectExempt, Actor: "ci-agent", MaxDestroy: DisabledMaxDestroy},
		Want:   ErrExemptionAccount,
	}, { // Test 1: The label with its account is accepted.
		Policy: Policy{Effect: EffectExempt, Actor: "ci-agent", Account: "team-a",
			MaxDestroy: DisabledMaxDestroy},
	}, { // Test 2: The account alone covers every agent bound to it.
		Policy: Policy{Effect: EffectExempt, Account: "team-a", MaxDestroy: DisabledMaxDestroy},
	}, { // Test 3: Neither stays legal, and is documented as turning the default off.
		Policy: Policy{Effect: EffectExempt, MaxDestroy: DisabledMaxDestroy},
	}, { // Test 4: A hold naming a label alone fails safe, so it needs no account.
		Policy: Policy{Actor: "ci-agent", MaxDestroy: DisabledMaxDestroy},
	}, { // Test 5: A deny may name an account.
		Policy: Policy{Effect: EffectDeny, Account: "team-b", MaxDestroy: DisabledMaxDestroy},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			err := test.Policy.Validate()
			if !errors.Is(err, test.Want) || (test.Want == nil && err != nil) {
				t.Fatalf("Validate() = %v, want %v", err, test.Want)
			}
			if err != nil && !strings.Contains(err.Error(), "not unique across accounts") {
				t.Errorf("Validate() = %q, want the refusal to say why", err)
			}
		})
	}
}

// TestAccountScopingIsTeamExceptOnAnExemption holds the account criterion to the tier the actor
// criterion has: a hold or a deny scoped to an account is the full engine, and an exemption naming
// one stays Community.
func TestAccountScopingIsTeamExceptOnAnExemption(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Policy is the rule classified.
		Policy Policy
		// WantFull is whether it is the full engine.
		WantFull bool
	}{{ // Test 0: A hold scoped to an account.
		Policy: Policy{Account: "team-a"}, WantFull: true,
	}, { // Test 1: A deny scoped to an account.
		Policy: Policy{Account: "team-a", Effect: EffectDeny}, WantFull: true,
	}, { // Test 2: An exemption naming an account.
		Policy: Policy{Account: "team-a", Effect: EffectExempt}, WantFull: false,
	}, { // Test 3: An exemption naming an agent and its account.
		Policy: Policy{Actor: "ci-agent", Account: "team-a", Effect: EffectExempt}, WantFull: false,
	}, { // Test 4: A plain hold, the control.
		Policy: Policy{Tool: "bash"}, WantFull: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := test.Policy.Advanced(); got != test.WantFull {
				t.Errorf("Advanced() = %v, want %v", got, test.WantFull)
			}
		})
	}
}

// TestInForceNamesTheExemptionAccount holds the rule set every run records to naming the account an
// exemption covers, and its digest to moving when the account does, so an exemption repointed at
// another account is a visible change.
func TestInForceNamesTheExemptionAccount(t *testing.T) {
	t.Parallel()
	onA := exemption("ci", func(p *Policy) { p.Actor, p.Account = "ci-agent", "team-a" })
	onB := exemption("ci", func(p *Policy) { p.Actor, p.Account = "ci-agent", "team-b" })
	want := []string{`ci: lets an agent's run proceed without the default hold, for agents ` +
		`bound to account "team-a"`}
	if diff := cmp.Diff(want, InForce([]*Policy{onA}).Rules); diff != "" {
		t.Errorf("rules mismatch (-want +got):\n%s", diff)
	}
	if InForce([]*Policy{onA}).Digest == InForce([]*Policy{onB}).Digest {
		t.Error("repointing the exemption at another account left the digest unchanged")
	}
}

// TestAPolicyFileExemptionNamesItsAccount holds the policy file to the same rule the API keeps: an
// exemption naming an agent's label without its account does not load, one naming both loads on
// Community, and a hold scoped to an account needs the license the API asks for. It drops the
// package's Team license around itself, so it cannot run in parallel with tests that read it.
func TestAPolicyFileExemptionNamesItsAccount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "policies.yml")
	team := license.Current()
	t.Cleanup(func() { license.Set(team) })
	tests := []struct {
		// Body is the policy file.
		Body string
		// Team is whether the install holds the Team license.
		Team bool
		// Want is the error the load wraps, nil when it loads.
		Want error
		// WantLicense is whether the refusal is the license's.
		WantLicense bool
	}{{ // Test 0: An exemption naming a label without its account does not load.
		Body: "policies:\n  - name: ci\n    effect: exempt\n    actor: ci-agent\n",
		Want: ErrExemptionAccount,
	}, { // Test 1: With its account it loads on Community.
		Body: "policies:\n  - name: ci\n    effect: exempt\n    actor: ci-agent\n" +
			"    account: team-a\n",
	}, { // Test 2: A hold scoped to an account is refused on Community.
		Body:        "policies:\n  - name: team a waits\n    account: team-a\n",
		WantLicense: true,
	}, { // Test 3: The same hold loads under Team, the control for test 2.
		Body: "policies:\n  - name: team a waits\n    account: team-a\n", Team: true,
	}}
	for testNum, test := range tests {
		license.Set(nil)
		if test.Team {
			license.Set(team)
		}
		if err := os.WriteFile(path, []byte(test.Body), 0o600); err != nil {
			t.Fatalf("test %d: write policy file: %v", testNum, err)
		}
		store, err := NewFileStore(path)
		if err == nil {
			var set []*Policy
			if set, err = store.List(context.Background()); err == nil && set[0].Account != "team-a" {
				t.Errorf("test %d: loaded account = %q, want team-a", testNum, set[0].Account)
			}
		}
		switch {
		case test.WantLicense:
			if err == nil || !strings.Contains(err.Error(), "switchtender.com/pricing") {
				t.Errorf("test %d: error = %v, want the license refusal", testNum, err)
			}
		case !errors.Is(err, test.Want) || (test.Want == nil && err != nil):
			t.Errorf("test %d: error = %v, want %v", testNum, err, test.Want)
		}
	}
}
