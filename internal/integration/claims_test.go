//go:build integration

package integration_test

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/dcadolph/yardmaster/internal/run"
)

// waitStatus polls a run until it reaches the wanted status, failing if it turns terminal first or
// times out. It catches a run mid-execution before acting on it.
func waitStatus(t *testing.T, base, id string, want run.Status) {
	t.Helper()
	deadline := time.Now().Add(90 * time.Second)
	for {
		var r run.Run
		getJSON(t, base+"/v1/runs/"+id, &r)
		if r.Status == want {
			return
		}
		if r.Status.Terminal() {
			t.Fatalf("run %s reached %s before %s", id, r.Status, want)
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s never reached %s (stuck in %s)", id, want, r.Status)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestRealSSHPipelineDAGSkip proves the pipeline claim: when a step fails, a step that depends on it
// is skipped and never runs, and the pipeline finishes failed.
func TestRealSSHPipelineDAGSkip(t *testing.T) {
	requireStack(t)
	buildImage(t)
	key, pub := keypair(t)
	hosts := startHosts(t, 1, pub)
	inventory := writeInventory(t, hosts, key)
	boom := writePlaybook(t, `
---
- name: Fail on purpose
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60
    - name: Fail
      ansible.builtin.fail:
        msg: intentional failure
`)
	after := writePlaybook(t, `
---
- name: Should never run
  hosts: all
  gather_facts: false
  tasks:
    - name: Touch a marker
      ansible.builtin.copy:
        content: "should not exist\n"
        dest: /tmp/yardmaster-after
`)
	base := startServer(t)

	var parent run.Run
	postJSON(t, base+"/v1/pipelines", fmt.Sprintf(
		`{"inventory":%q,"steps":[`+
			`{"name":"boom","playbook":%q,"inventory":%q},`+
			`{"name":"after","playbook":%q,"inventory":%q,"depends_on":["boom"]}]}`,
		inventory, boom, inventory, after, inventory), &parent)

	done := waitTerminal(t, base, parent.ID)
	if done.Status != run.StatusFailed {
		t.Fatalf("pipeline status = %q, want failed", done.Status)
	}

	var steps struct {
		Steps []run.Run `json:"steps"`
	}
	getJSON(t, base+"/v1/runs/"+parent.ID+"/steps", &steps)
	// The failed step ran; the dependent step was skipped and created no run.
	if len(steps.Steps) != 1 {
		t.Fatalf("step runs = %d, want 1 (the dependent step must be skipped): %+v", len(steps.Steps), steps.Steps)
	}
	if steps.Steps[0].StepName != "boom" || steps.Steps[0].Status != run.StatusFailed {
		t.Errorf("step = %q %q, want boom failed", steps.Steps[0].StepName, steps.Steps[0].Status)
	}
}

// TestRealSSHCancel proves the cancellation claim: a long run is stopped by a cancel request and
// finishes canceled well before its own timeout, because the process tree is killed.
func TestRealSSHCancel(t *testing.T) {
	requireStack(t)
	buildImage(t)
	key, pub := keypair(t)
	hosts := startHosts(t, 1, pub)
	inventory := writeInventory(t, hosts, key)
	playbook := writePlaybook(t, `
---
- name: Sleep a long time
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60
    - name: Sleep
      ansible.builtin.command: sleep 300
`)
	base := startServer(t)

	var created run.Run
	postJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q}`, playbook, inventory), &created)

	// Wait until the run is executing the sleep, then cancel it.
	waitStatus(t, base, created.ID, run.StatusRunning)
	var ack map[string]string
	postJSON(t, base+"/v1/runs/"+created.ID+"/cancel", "", &ack)

	done := waitTerminal(t, base, created.ID)
	if done.Status != run.StatusCanceled {
		t.Fatalf("run status = %q, want canceled", done.Status)
	}
}

// TestRealSSHDrift proves the drift claim: a dry run reports the tasks that would change on a host,
// which surface on the drift endpoint.
func TestRealSSHDrift(t *testing.T) {
	requireStack(t)
	buildImage(t)
	key, pub := keypair(t)
	hosts := startHosts(t, 1, pub)
	inventory := writeInventory(t, hosts, key)
	playbook := writePlaybook(t, `
---
- name: Would change the host
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60
    - name: Ensure a marker that is not there yet
      ansible.builtin.copy:
        content: "drift\n"
        dest: /tmp/yardmaster-drift-marker
`)
	base := startServer(t)

	var created run.Run
	postJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q,"dry_run":true}`, playbook, inventory), &created)
	done := waitTerminal(t, base, created.ID)
	if !done.Status.Terminal() {
		t.Fatalf("dry run status = %q, want terminal", done.Status)
	}

	var drift struct {
		Hosts []struct {
			Host         string `json:"host"`
			DriftedTasks int    `json:"drifted_tasks"`
		} `json:"hosts"`
	}
	getJSON(t, base+"/v1/drift", &drift)
	drifted := 0
	for _, h := range drift.Hosts {
		if strings.HasPrefix(hosts[0].Name, h.Host) || h.Host == hosts[0].Name {
			drifted += h.DriftedTasks
		}
	}
	if drifted == 0 {
		t.Fatalf("drift endpoint reported no drifted tasks after a dry run: %+v", drift.Hosts)
	}
}
