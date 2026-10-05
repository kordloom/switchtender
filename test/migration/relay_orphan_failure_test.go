package migration

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/relay"
)

const (
	// rlyHoldSeconds is how long the play's one task waits before it changes its host. It is
	// longer than the lease lifetime plus a janitor tick, so the control node has recorded the run
	// interrupted well before the change lands.
	rlyHoldSeconds = 60
	// rlyOrphanPlaybook records the shell's process id and that the task began, waits, and then
	// changes the host, all inside one task. Ansible prints nothing while a task runs, so nothing
	// it writes to its executor's closed output can stop it early.
	rlyOrphanPlaybook = `---
- name: Change a host well after the play began
  hosts: all
  gather_facts: false
  tasks:
    - name: Begin, wait, then change the host
      ansible.builtin.shell: >-
        echo $$ > {{ marker_dir }}/orphan-pid-{{ inventory_hostname }};
        echo started > {{ marker_dir }}/orphan-start-{{ inventory_hostname }};
        sleep {{ hold_seconds }};
        date +%s > {{ marker_dir }}/orphan-after-{{ inventory_hostname }}
      changed_when: true
`
)

// TestRelayWorkerKilledMidRunStopsItsTool kills a relay worker outright while its play is in the
// middle of a task, the way an out-of-memory kill or a power cut ends one, and watches the host.
//
// The reliability page promises that a replica killed mid-run fails clean: its run is marked
// interrupted, a terminal state ready for an explicit rerun, never silently re-executed. The
// control node does mark it interrupted once the lease expires. But the tool runs in a process
// group of its own and nothing ties its life to its executor's, so the play carries on with no
// executor, no lease, no log, and no record, and changes the host after the run already reads as
// interrupted. A person who reruns the interrupted run, as the page says to, then has two plays
// changing the same hosts at once, and the evidence for the first says it stopped before it did.
//
//nolint:funlen // Test function.
func TestRelayWorkerKilledMidRunStopsItsTool(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	dir := filepath.Join(in.root, "orphan")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the scenario directory: %v", err)
	}
	token := "relay-pool-" + randomHex(t, 16)
	in.addSecret(token)
	pools := filepath.Join(dir, "workers.yaml")
	if err := os.WriteFile(pools, []byte("workers:\n  - name: relay\n    token_sha256: "+
		relay.HashToken(token)+"\n    queues: [relay]\n"), 0o600); err != nil {
		t.Fatalf("write the worker pool file: %v", err)
	}
	s := in.startServer("a", "--worker-pools", pools)

	playbook := filepath.Join(dir, "hold.yml")
	if err := os.WriteFile(playbook, []byte(rlyOrphanPlaybook), 0o600); err != nil {
		t.Fatalf("write the playbook: %v", err)
	}
	inventory := filepath.Join(dir, "inventory.ini")
	if err := os.WriteFile(inventory, []byte("[fleet]\norphanhost ansible_connection=local "+
		"ansible_python_interpreter=\"{{ ansible_playbook_python }}\"\n"), 0o600); err != nil {
		t.Fatalf("write the inventory: %v", err)
	}
	in.must(s, "admin", "POST", "/v1/templates", map[string]any{
		"name": "orphan hold", "playbook": playbook, "inventory": inventory, "queue": "relay",
		"extra_vars": map[string]any{"marker_dir": in.markers, "hold_seconds": rlyHoldSeconds},
	}, 201)

	in.startWorker("relay-a", s.url, token, "relay", "")
	worker := in.servers[len(in.servers)-1]
	rec := in.launched(s, "operator", "orphan hold", nil)
	in.waitMarker("orphan-start", "orphanhost")
	started := time.Now()
	pid, err := strconv.Atoi(strings.TrimSpace(in.waitMarker("orphan-pid", "orphanhost")))
	if err != nil {
		t.Fatalf("read the task's process id: %v", err)
	}
	// Whatever happens below, the task this test started is ended by its process id, so a play
	// that outlived its worker does not outlive the test. The id is used only while it still names
	// this task's shell, whose command line carries the marker path, so a reused id is left alone.
	t.Cleanup(func() { rlyEndTask(pid, "orphan-pid-orphanhost") })

	worker.kill()
	interrupted := in.waitStatus(s, rec.ID, "interrupted")
	recorded := time.Now()
	if recorded.Sub(started) >= rlyHoldSeconds*time.Second {
		t.Fatalf("the run was recorded %s only %s after the task began, too late to tell a "+
			"change made after the record from one made before it", interrupted.Status,
			recorded.Sub(started))
	}

	after := filepath.Join(in.markers, "orphan-after-orphanhost")
	deadline := started.Add(rlyHoldSeconds*time.Second + 20*time.Second)
	for time.Now().Before(deadline) {
		raw, err := os.ReadFile(after)
		if err == nil && strings.HasSuffix(string(raw), "\n") {
			changedAt, perr := strconv.ParseInt(strings.TrimSpace(string(raw)), 10, 64)
			if perr != nil {
				t.Fatalf("read the change time: %v", perr)
			}
			t.Errorf("the run was recorded interrupted at %s, and the play its killed worker "+
				"started changed the host anyway at %s, %s later, with no executor and no record",
				recorded.Format(time.TimeOnly), time.Unix(changedAt, 0).Format(time.TimeOnly),
				time.Unix(changedAt, 0).Sub(recorded).Round(time.Second))
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// rlyEndTask kills the process pid when its command line still contains mark, which identifies
// the task's shell, and leaves any other process alone.
func rlyEndTask(pid int, mark string) {
	out, err := exec.Command("ps", "-o", "args=", "-p", strconv.Itoa(pid)).Output()
	if err != nil || !strings.Contains(string(out), mark) {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}
