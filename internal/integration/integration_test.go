//go:build integration

// Package integration_test runs SwitchTender end to end against real hosts: SSH servers in Docker
// containers, targeted by real ansible-playbook over the SSH transport production uses. The suite
// is gated behind the integration build tag so the default go test stays fast, and it skips
// itself when docker or ansible are unavailable.
//
// SSH containers were chosen over kind on purpose: Ansible's fidelity axis is the SSH transport,
// Python interpreter discovery, and multi host fan out, none of which a Kubernetes control plane
// exercises.
package integration_test

import (
	"context"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/integrationtest"
	"github.com/kordloom/switchtender/internal/live"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// startServer wires the full serve stack, dispatcher, live hub, SQLite store, and HTTP API, and
// returns its base URL.
func startServer(t *testing.T) string {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	hub := live.NewHub()
	disp := dispatch.New(db.Runs(), roundhouse.NewAnsibleRunner(), zaptest.NewLogger(t),
		dispatch.WithPublisher(hub))
	t.Cleanup(disp.Close)

	srv := httptest.NewServer(server.New(db.Runs(), disp, nil,
		server.WithStreamer(hub), server.WithCanceler(disp), server.WithRetrier(disp)).Handler())
	t.Cleanup(srv.Close)
	return srv.URL
}

// waitShardsTerminal polls a parent's shards until the expected count exists and every shard is
// terminal, absorbing the moment between a parent finishing and the last shard save landing.
func waitShardsTerminal(t *testing.T, base, parentID string, want int) []run.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var shards struct {
			Shards []run.Run `json:"shards"`
		}
		integrationtest.GetJSON(t, base+"/v1/runs/"+parentID+"/shards", &shards)
		settled := len(shards.Shards) == want
		for _, s := range shards.Shards {
			if !s.Status.Terminal() {
				settled = false
			}
		}
		if settled {
			return shards.Shards
		}
		if time.Now().After(deadline) {
			t.Fatalf("shards of %s never settled to %d terminal: %+v", parentID, want, shards.Shards)
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// TestRealSSHFleet drives the whole stack against real SSH hosts: a plain run touches every host,
// then a split shards the same fleet, runs in parallel, and merges results, and fleet health
// reflects both.
func TestRealSSHFleet(t *testing.T) {
	integrationtest.RequireStack(t)
	integrationtest.BuildImage(t)
	key, pub := integrationtest.Keypair(t)
	hosts := integrationtest.StartHosts(t, 3, pub)
	inventory := integrationtest.WriteInventory(t, hosts, key)
	playbook := integrationtest.WritePlaybook(t, `
---
- name: Touch every node
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60

    - name: Write a marker
      ansible.builtin.copy:
        content: "switchtender was here\n"
        dest: /tmp/switchtender-marker

    - name: Read the marker back
      ansible.builtin.command: cat /tmp/switchtender-marker
      changed_when: false
`)
	base := startServer(t)

	// A plain run reaches all three hosts over real SSH.
	var created run.Run
	integrationtest.PostJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q}`, playbook, inventory), &created)
	single := integrationtest.WaitTerminal(t, base, created.ID)
	if single.Status != run.StatusSucceeded {
		t.Fatalf("single run status = %q, want succeeded", single.Status)
	}

	var events struct {
		Events []struct {
			Type string `json:"type"`
			Host string `json:"host"`
		} `json:"events"`
	}
	integrationtest.GetJSON(t, base+"/v1/runs/"+created.ID+"/events", &events)
	touched := map[string]bool{}
	for _, e := range events.Events {
		if e.Type == "runner_ok" {
			touched[e.Host] = true
		}
	}
	for _, h := range hosts {
		if !touched[h.Name] {
			t.Errorf("host %s has no successful task events", h.Name)
		}
	}

	// A split shards the same fleet and merges back to one succeeded parent.
	var parent run.Run
	integrationtest.PostJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q,"shards":2}`, playbook, inventory), &parent)
	split := integrationtest.WaitTerminal(t, base, parent.ID)
	if split.Status != run.StatusSucceeded {
		t.Fatalf("split status = %q, want succeeded", split.Status)
	}
	shards := waitShardsTerminal(t, base, parent.ID, 2)
	shardHosts := 0
	for _, s := range shards {
		if s.Status != run.StatusSucceeded {
			t.Errorf("shard %d status = %q, want succeeded", *s.ShardIndex, s.Status)
		}
		shardHosts += len(strings.Split(s.Limit, ","))
	}
	if shardHosts != len(hosts) {
		t.Errorf("shards cover %d hosts, want %d", shardHosts, len(hosts))
	}

	// Fleet health saw every host across both runs. Summaries persist moments after a run turns
	// terminal, so poll briefly instead of asserting one snapshot.
	deadline := time.Now().Add(30 * time.Second)
	for {
		var fleet struct {
			Hosts []run.HostHealth `json:"hosts"`
		}
		integrationtest.GetJSON(t, base+"/v1/fleet?window=5", &fleet)
		settled := len(fleet.Hosts) == len(hosts)
		for _, h := range fleet.Hosts {
			if h.Failures != 0 {
				t.Fatalf("fleet %s shows %d failures, want 0", h.Host, h.Failures)
			}
			if h.Total < 2 {
				settled = false
			}
		}
		if settled {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("fleet never settled to %d hosts with 2 runs each: %+v", len(hosts), fleet.Hosts)
		}
		time.Sleep(300 * time.Millisecond)
	}
}

// TestRealSSHFailureIsolation proves a real failure lands on the right host and a shard retry
// re-runs only the broken slice.
func TestRealSSHFailureIsolation(t *testing.T) {
	integrationtest.RequireStack(t)
	integrationtest.BuildImage(t)
	key, pub := integrationtest.Keypair(t)
	hosts := integrationtest.StartHosts(t, 3, pub)
	inventory := integrationtest.WriteInventory(t, hosts, key)
	playbook := integrationtest.WritePlaybook(t, `
---
- name: Break one node
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60

    - name: Fail while the break marker exists
      ansible.builtin.command: test ! -f /tmp/switchtender-break
      changed_when: false
`)
	base := startServer(t)

	// Plant the failure on the first host only.
	if out, err := exec.Command("docker", "exec", hosts[0].Container,
		"touch", "/tmp/switchtender-break").CombinedOutput(); err != nil {
		t.Fatalf("plant break marker: %v\n%s", err, out)
	}

	var parent run.Run
	integrationtest.PostJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q,"shards":3}`, playbook, inventory), &parent)
	split := integrationtest.WaitTerminal(t, base, parent.ID)
	if split.Status != run.StatusFailed {
		t.Fatalf("split status = %q, want failed", split.Status)
	}

	shards := waitShardsTerminal(t, base, parent.ID, 3)
	for _, s := range shards {
		t.Logf("post-split shard %s limit=%s status=%s claimed_by=%s error=%q",
			s.ID, s.Limit, s.Status, s.ClaimedBy, s.Error)
	}
	var failedLimit string
	failedCount := 0
	for _, s := range shards {
		if s.Status == run.StatusFailed {
			failedCount++
			failedLimit = s.Limit
		}
	}
	if failedCount != 1 || failedLimit != hosts[0].Name {
		t.Fatalf("failed shards = %d with limit %q, want exactly %s",
			failedCount, failedLimit, hosts[0].Name)
	}

	// Clear the fault and retry: only the broken shard re-runs, and recovers.
	if out, err := exec.Command("docker", "exec", hosts[0].Container,
		"rm", "/tmp/switchtender-break").CombinedOutput(); err != nil {
		t.Fatalf("clear break marker: %v\n%s", err, out)
	}
	preRetry := waitShardsTerminal(t, base, parent.ID, 3)
	for _, s := range preRetry {
		t.Logf("pre-retry shard %s limit=%s status=%s error=%q", s.ID, s.Limit, s.Status, s.Error)
	}
	var retry run.Run
	integrationtest.PostJSON(t, base+"/v1/runs/"+parent.ID+"/retry", "", &retry)
	if retry.RetryOf == nil || *retry.RetryOf != parent.ID {
		t.Fatalf("retry parent missing retry_of link: %+v", retry)
	}
	final := integrationtest.WaitTerminal(t, base, retry.ID)
	if final.Status != run.StatusSucceeded {
		t.Fatalf("retry status = %q, want succeeded", final.Status)
	}
	retryShards := waitShardsTerminal(t, base, retry.ID, 1)
	if retryShards[0].Limit != hosts[0].Name {
		t.Fatalf("retry shards = %+v, want only %s", retryShards, hosts[0].Name)
	}
}

// eeImage is a small public Ansible image pinned to an ansible-core version distinct from the host,
// so a run inside it proves execution is isolated from the host's ansible.
const eeImage = "willhallonline/ansible:2.15-alpine-3.19"

// requireHostBindMounts skips when the docker daemon cannot bind mount a path from this machine.
//
// A containerized run hands the daemon host paths: the project checkout, the playbook's directory,
// the inventory. When the daemon runs in a virtual machine that does not share this filesystem, and
// colima on macOS is the common case, those mounts land as empty directories and the run fails
// looking for a playbook that is right there. That is a property of the developer's docker, not of
// the product, and a gate that can never pass locally is a gate people learn to skip.
//
// It is a skip and not a pass, and it says why, because the alternative is a green run that quietly
// proves nothing about the one thing this test exists to cover.
func requireHostBindMounts(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "probe"), []byte("visible"), 0o600); err != nil {
		t.Fatalf("write bind mount probe: %v", err)
	}
	out, err := exec.Command("docker", "run", "--rm", "-v", dir+":"+dir+":ro",
		"alpine:3.22", "cat", filepath.Join(dir, "probe")).CombinedOutput()
	if err != nil || !strings.Contains(string(out), "visible") {
		integrationtest.StandDownOrFail(t, "this docker cannot bind mount a host path, so container execution "+
			"cannot be exercised here; run it where the daemon shares this filesystem: %v\n%s",
			err, out)
	}
}

// TestContainerExecutionEnvironment runs a playbook inside a pinned container image and confirms the
// ansible-core reported by the play is the image's, not the host's, proving execution isolation.
func TestContainerExecutionEnvironment(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		integrationtest.StandDownOrFail(t, "docker is not on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		integrationtest.StandDownOrFail(t, "the docker daemon is not running: %v", err)
	}
	requireHostBindMounts(t)
	pull, cancelPull := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelPull()
	// A pull that fails is the sharpest of these. Container execution is an advertised feature and
	// a registry having a bad minute made the one test covering it disappear, green.
	if out, err := exec.CommandContext(pull, "docker", "pull", eeImage).CombinedOutput(); err != nil {
		integrationtest.StandDownOrFail(t, "cannot pull %s: %v\n%s", eeImage, err, out)
	}

	dir := t.TempDir()
	playbook := filepath.Join(dir, "play.yml")
	if err := os.WriteFile(playbook, []byte(`---
- hosts: localhost
  connection: local
  gather_facts: false
  tasks:
    - debug:
        msg: "running ansible-core {{ ansible_version.full }}"
`), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	inventory := filepath.Join(dir, "inv.ini")
	if err := os.WriteFile(inventory, []byte("[local]\nlocalhost ansible_connection=local\n"), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	events := filepath.Join(dir, "events.ndjson")
	if err := os.WriteFile(events, nil, 0o600); err != nil {
		t.Fatalf("create events file: %v", err)
	}

	runner := roundhouse.NewSelectiveRunner(true, "docker", "missing", false,
		roundhouse.DefaultContainerLimits())
	var buf strings.Builder
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	res, err := runner.Run(ctx, roundhouse.Spec{
		Playbook: playbook, Inventory: inventory, Dir: dir, Image: eeImage, EventsPath: events,
	}, &buf)
	if err != nil {
		t.Fatalf("Run() error = %v\n%s", err, buf.String())
	}
	if res.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0\n%s", res.ExitCode, buf.String())
	}
	if !strings.Contains(buf.String(), "running ansible-core 2.15.13") {
		t.Errorf("output did not report the image's ansible-core:\n%s", buf.String())
	}
}
