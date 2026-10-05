package migration

import (
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// guardrailsPolicy loads one Rego policy, so its bundle is the one thing deciding.
const guardrailsPolicy = `rego:
  - name: guardrails
    files: [rego/guardrails.rego]
`

// denyBuild refuses the imported build template outright.
const denyBuild = `package switchtender

deny contains "the build template is frozen during the migration" if {
	input.run.playbook == "build.yml"
}
`

// holdBuild replaces the refusal with a hold, so the same launch now runs once a person agrees.
const holdBuild = `package switchtender

hold contains "the build template runs once a person has looked at it" if {
	input.run.playbook == "build.yml"
}

deny contains "nothing else is frozen" if {
	input.run.playbook == "frozen-forever.yml"
}
`

// regoBundle reads the bundle digest the server reports for a Rego policy.
func (in *install) regoBundle(s *server, name string) string {
	in.t.Helper()
	var page struct {
		// Policies are the policies in force.
		Policies []struct {
			// Name is the policy's name.
			Name string `json:"name"`
			// Rego is the Rego bundle, when it is one.
			Rego *struct {
				// SHA256 is the bundle digest.
				SHA256 string `json:"sha256"`
			} `json:"rego"`
		} `json:"policies"`
	}
	in.must(s, "admin", "GET", "/v1/policies", nil, 200).decode(in.t, &page)
	for _, p := range page.Policies {
		if p.Name == name && p.Rego != nil && len(p.Rego.SHA256) == 64 {
			return p.Rego.SHA256
		}
	}
	in.t.Fatalf("no Rego policy %q is in force", name)
	return ""
}

// TestRegoRefusalAndItsReplacementAreBothOnTheRecord is scenario eight. A Rego policy refuses the
// imported build template: nothing may execute, and the refusal has to carry the bundle that made
// it. The module is then edited to hold instead, without a restart, and the same launch, approved,
// has to run with evidence naming the new bundle and not the old one.
func TestRegoRefusalAndItsReplacementAreBothOnTheRecord(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite, Policy: guardrailsPolicy,
		Rego: map[string]string{"rego/guardrails.rego": denyBuild}})
	s := in.startServer("a")
	first := in.regoBundle(s, "guardrails")

	r := in.launch(s, "operator", "build", nil)
	if r.Status != 403 {
		t.Fatalf("the launch the Rego policy refuses = %d, want 403: %s", r.Status, r.Body)
	}
	for _, want := range []string{"guardrails", "frozen during the migration",
		"rego sha256:" + first[:12]} {
		if !strings.Contains(string(r.Body), want) {
			t.Errorf("the refusal %s does not name %q", r.Body, want)
		}
	}
	if runs := in.runsFrom(s, in.template(s, "build")); len(runs) != 0 {
		t.Fatalf("a refused launch created runs: %v", runs)
	}
	if got := in.marked("build"); len(got) != 0 {
		t.Fatalf("a refused launch executed on %v", got)
	}
	ev := in.checkEvidence(s)
	requireRefusalRecorded(t, ev, "guardrails", first)

	in.writePolicy(guardrailsPolicy, map[string]string{"rego/guardrails.rego": holdBuild})
	second := in.regoBundle(s, "guardrails")
	if second == first {
		t.Fatalf("editing the module left the bundle digest at %s", first)
	}
	rec := in.launched(s, "operator", "build", nil)
	held := in.waitStatus(s, rec.ID, "pending_approval")
	if !strings.Contains(describe(held.Raw), "rego sha256:"+second[:12]) {
		t.Errorf("the held run does not name the bundle that held it: %s", describe(held.Raw))
	}
	in.must(s, "approver", "POST", "/v1/runs/"+rec.ID+"/approve", nil, 200)
	in.waitStatus(s, rec.ID, "succeeded")
	if diff := cmp.Diff([]string{"web1"}, in.marked("build")); diff != "" {
		t.Errorf("hosts the approved run reached (-want +got):\n%s", diff)
	}

	ev = in.checkEvidence(s, rec.ID)
	got := ev.Receipts[rec.ID]
	requireRecord(t, got, recordWant{
		Launcher: "operator-laptop", OnBehalfOf: "operator", Approver: "approver-laptop",
		Playbook: "build.yml", Hosts: []string{"web1"},
		Rules: []string{"guardrails: decided by Rego package data.switchtender, bundle sha256:" + second},
	})
	if body := got.outcome(t).OutcomeBody; strings.Contains(body, first) {
		t.Errorf("the run's outcome still names the replaced bundle %s: %s", first, body)
	}
	requireRefusalRecorded(t, ev, "guardrails", first)
}

// requireRefusalRecorded fails the scenario unless the exported chain holds the refusal the policy
// made, naming the policy and the full digest of the bundle that decided it.
func requireRefusalRecorded(t *testing.T, ev *evidence, policyName, bundle string) {
	t.Helper()
	var refusals []auditEntry
	for _, e := range ev.Audit {
		if e.Method == "DECISION" && strings.Contains(e.Path, "/decision/refused") {
			refusals = append(refusals, e)
		}
	}
	if len(refusals) != 1 {
		t.Fatalf("the chain holds %d refusals, want the one the policy made: %s", len(refusals),
			describe(ev.Audit))
	}
	r := refusals[0]
	if !strings.HasSuffix(r.Path, "/decision/refused/rego/sha256:"+bundle+"/policy/"+policyName) {
		t.Errorf("the recorded refusal %s does not name policy %s and bundle %s", r.Path, policyName,
			bundle)
	}
	if r.Actor != "system:policy" || r.ActorType != "system" || r.OnBehalfOf != "operator-laptop" {
		t.Errorf("the refusal is recorded as %s (%s) on behalf of %q, want the policy engine on "+
			"behalf of the operator who asked", r.Actor, r.ActorType, r.OnBehalfOf)
	}
	if !strings.Contains(string(ev.Bundle), r.Path) {
		t.Errorf("the exported chain does not carry the refusal %s", r.Path)
	}
}
