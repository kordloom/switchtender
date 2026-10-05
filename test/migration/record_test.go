package migration

import (
	"sort"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
)

// recordWant is what a run's verified receipt must say: who or what authorized it, under which
// rules, with which credentials, against which hosts, and what executed.
type recordWant struct {
	// Status is the outcome the receipt records, succeeded when empty.
	Status string
	// Launcher is the actor that launched the run.
	Launcher string
	// LauncherType is the launcher's actor type, any when empty.
	LauncherType string
	// OnBehalfOf is the account the launcher acted for, any when empty.
	OnBehalfOf string
	// Approver is the actor whose decision released the run, none required when empty.
	Approver string
	// Playbook is what executed.
	Playbook string
	// Hosts are the hosts the outcome records the run reached.
	Hosts []string
	// CredentialIDs are the credentials the spec says the run executed with, by id.
	CredentialIDs []string
	// SealedVars are the secret answers the spec names, by variable.
	SealedVars []string
	// Rules are fragments each of which some rule in the recorded policy set must contain.
	Rules []string
}

// requireRecord fails the scenario unless the receipt's disclosed entries say what want says.
func requireRecord(t *testing.T, rec *receipt, want recordWant) {
	t.Helper()
	out := rec.outcome(t)
	status := want.Status
	if status == "" {
		status = "succeeded"
	}
	if got := str(out.Outcome["status"]); got != status {
		t.Errorf("the receipt records status %q, want %q", got, status)
	}
	if want.Playbook != "" {
		if got := str(out.Outcome["playbook"]); got != want.Playbook {
			t.Errorf("the receipt records playbook %q executing, want %q", got, want.Playbook)
		}
	}
	if want.Hosts != nil {
		if diff := cmp.Diff(want.Hosts, outcomeHosts(out)); diff != "" {
			t.Errorf("hosts the receipt records the run reaching (-want +got):\n%s", diff)
		}
	}
	if want.CredentialIDs != nil {
		got := stringsOf(out.Spec["credential_ids"])
		if diff := cmp.Diff(want.CredentialIDs, got, cmpopts.EquateEmpty()); diff != "" {
			t.Errorf("credentials the receipt records (-want +got):\n%s", diff)
		}
	}
	if want.SealedVars != nil {
		got := stringsOf(out.Spec["sealed_vars"])
		if diff := cmp.Diff(want.SealedVars, got); diff != "" {
			t.Errorf("secret answers the receipt's spec names (-want +got):\n%s", diff)
		}
	}
	rules := stringsOf(field(out.Outcome, "policy_set.rules"))
	for _, frag := range want.Rules {
		found := false
		for _, r := range rules {
			if strings.Contains(r, frag) {
				found = true
			}
		}
		if !found {
			t.Errorf("the recorded policy set %v has no rule containing %q", rules, frag)
		}
	}
	if want.Launcher != "" {
		c := rec.launch(t)
		if c.Actor != want.Launcher || (want.OnBehalfOf != "" && c.OnBehalfOf != want.OnBehalfOf) ||
			(want.LauncherType != "" && c.ActorType != want.LauncherType) {
			t.Errorf("the receipt's launch is %s %s by %s (%s) on behalf of %q, want %s (%s) on "+
				"behalf of %q", c.Method, c.Path, c.Actor, c.ActorType, c.OnBehalfOf, want.Launcher,
				want.LauncherType, want.OnBehalfOf)
		}
	}
	decisions := rec.find("/runs/" + rec.RunID + "/decision/")
	switch {
	case want.Approver == "" && len(decisions) > 0:
		t.Errorf("the receipt records a decision nobody was expected to make: %s", describe(decisions))
	case want.Approver != "" && len(decisions) != 1:
		t.Errorf("the receipt records %d decisions on %s, want one by %s: %s", len(decisions),
			rec.RunID, want.Approver, describe(decisions))
	case want.Approver != "":
		if c := decisions[0]; c.Actor != want.Approver || !strings.HasSuffix(c.Path, "/approved") {
			t.Errorf("the receipt's decision is %s by %s, want an approval by %s", c.Path, c.Actor,
				want.Approver)
		}
	}
}

// outcomeHosts returns the hosts an outcome records, sorted.
func outcomeHosts(c claim) []string {
	var hosts []string
	list, _ := c.Outcome["hosts"].([]any)
	for _, h := range list {
		if m, ok := h.(map[string]any); ok {
			hosts = append(hosts, str(m["host"]))
		}
	}
	sort.Strings(hosts)
	return hosts
}

// stringsOf converts a decoded JSON list of strings.
func stringsOf(v any) []string {
	list, _ := v.([]any)
	out := make([]string, 0, len(list))
	for _, x := range list {
		out = append(out, str(x))
	}
	return out
}
