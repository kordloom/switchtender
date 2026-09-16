//go:build integration

package integration_test

import (
	"fmt"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// TestEstateAtAgainstARealFleet drives the point-in-time estate end to end against real hosts.
//
// Everything else proving this works is a unit test writing facts a test invented. This gathers
// real Ansible facts over real SSH from real machines, through the dispatcher's own save path, and
// then asks what the estate looked like. It is the only check that the whole chain holds: gather,
// bucket, store, bound, and reconstruct.
func TestEstateAtAgainstARealFleet(t *testing.T) {
	requireStack(t)
	buildImage(t)
	key, pub := keypair(t)
	hosts := startHosts(t, 3, pub)
	inventory := writeInventory(t, hosts, key)
	// gather_facts on, which is the whole point: this is what populates the state history.
	playbook := writePlaybook(t, `
---
- name: Gather the fleet
  hosts: all
  gather_facts: true
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60
`)
	base := startServer(t)

	before := time.Now().Add(-time.Hour)

	var created run.Run
	postJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q}`, playbook, inventory), &created)
	if got := waitTerminal(t, base, created.ID); got.Status != run.StatusSucceeded {
		t.Fatalf("gather run status = %q, want succeeded", got.Status)
	}

	type estate struct {
		At    time.Time       `json:"at"`
		Hosts []run.HostFacts `json:"hosts"`
		Total int             `json:"total"`
	}

	// Now: every host the run touched is in the estate, carrying facts the machine reported.
	var now estate
	getJSON(t, base+"/v1/estate", &now)
	if len(now.Hosts) != len(hosts) {
		t.Fatalf("estate holds %d hosts after gathering %d, want all of them: %+v",
			len(now.Hosts), len(hosts), now.Hosts)
	}
	for _, h := range now.Hosts {
		if len(h.Facts) == 0 {
			t.Errorf("host %s is in the estate with no facts, so the gather stored an empty set",
				h.Host)
		}
		if h.RunID != created.ID {
			t.Errorf("host %s names run %q, want the run that gathered it %q",
				h.Host, h.RunID, created.ID)
		}
		if h.GatheredAt.IsZero() {
			t.Errorf("host %s has no gather time, so it can never be placed in history", h.Host)
		}
	}

	// An hour ago: nothing had been observed, so the estate is empty rather than back-filled with
	// the readings taken since. Inventing those would describe an estate holding machines nobody
	// had looked at, which is the failure that makes a point-in-time answer worthless.
	var past estate
	getJSON(t, base+"/v1/estate?at="+before.UTC().Format(time.RFC3339), &past)
	if len(past.Hosts) != 0 {
		t.Errorf("estate an hour before the gather holds %d hosts, want none. A reading is being "+
			"projected backwards to a time it was not taken: %+v", len(past.Hosts), past.Hosts)
	}
}

// TestAChangeAndItsReversibilityAgainstARealFleet drives the change grouping and the reversibility
// grade against real runs on real hosts.
//
// The outcome that matters is mixed: a failure followed by a fix is what a real change looks like,
// and run by run it reads as one red row and one green row with nothing joining them.
func TestAChangeAndItsReversibilityAgainstARealFleet(t *testing.T) {
	requireStack(t)
	buildImage(t)
	key, pub := keypair(t)
	hosts := startHosts(t, 2, pub)
	inventory := writeInventory(t, hosts, key)
	base := startServer(t)

	ok := writePlaybook(t, `
---
- name: Succeed
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60
    - name: True
      ansible.builtin.command: /bin/true
      changed_when: false
`)
	bad := writePlaybook(t, `
---
- name: Fail
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60
    # Deliberately named with a YAML boolean. Unquoted False is a bool, not a string, so the task
    # reaches the callback named with one. That used to drop every event for this task silently.
    - name: False
      ansible.builtin.command: /bin/false
`)

	const change = "nginx-cve-2026-3311"
	// The failure first, then the fix, which is the order a real change happens in.
	for _, pb := range []string{bad, ok} {
		var created run.Run
		postJSON(t, base+"/v1/runs", fmt.Sprintf(
			`{"playbook":%q,"inventory":%q,"labels":{"change":%q}}`, pb, inventory, change), &created)
		waitTerminal(t, base, created.ID)
	}

	var got struct {
		Change   string     `json:"change"`
		Outcome  string     `json:"outcome"`
		OpenedAt time.Time  `json:"opened_at"`
		ClosedAt time.Time  `json:"closed_at"`
		Runs     []*run.Run `json:"runs"`
		Total    int        `json:"total"`
	}
	getJSON(t, base+"/v1/changes/"+change, &got)
	if got.Total != 2 {
		t.Fatalf("change holds %d runs, want the 2 that carried its label", got.Total)
	}
	if got.Outcome != "mixed" {
		t.Errorf("outcome = %q, want mixed: the change failed and was then put right, which is "+
			"neither a success nor a failure", got.Outcome)
	}
	if got.ClosedAt.IsZero() || !got.ClosedAt.After(got.OpenedAt) {
		t.Errorf("change opened %v and closed %v, want a close after the open once every member "+
			"has finished", got.OpenedAt, got.ClosedAt)
	}

	// Reversibility is graded on a real run, read back through the API the approver uses.
	var one run.Run
	getJSON(t, base+"/v1/runs/"+got.Runs[0].ID, &one)
	if one.Reversibility == nil {
		t.Fatal("a run read back carries no reversibility grade, so an approver sees only risk")
	}
	if one.Reversibility.Class != run.ReversibleCostly {
		t.Errorf("a playbook run graded %q, want %q: it changed state and undoing it means "+
			"running something else", one.Reversibility.Class, run.ReversibleCostly)
	}
	if one.Risk == nil {
		t.Error("the risk grade went missing when reversibility was added beside it")
	}
}
