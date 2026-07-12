//go:build integration

// Package integration_test runs Yardmaster end to end against real hosts: SSH servers in Docker
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
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/dcadolph/yardmaster/internal/dispatch"
	"github.com/dcadolph/yardmaster/internal/live"
	"github.com/dcadolph/yardmaster/internal/roundhouse"
	"github.com/dcadolph/yardmaster/internal/run"
	"github.com/dcadolph/yardmaster/internal/server"
	"github.com/dcadolph/yardmaster/internal/sqlitestore"
)

// image is the tag for the throwaway SSH host image built once per machine.
const image = "yardmaster-it-sshhost:1"

// dockerfile builds a minimal SSH server with Python for Ansible modules. The public key arrives
// through the SSH_PUBKEY environment variable at run time.
const dockerfile = `
FROM alpine:3.20
RUN apk add --no-cache openssh python3 && ssh-keygen -A && mkdir -p /root/.ssh && chmod 700 /root/.ssh
CMD ["/bin/sh", "-c", "echo \"$SSH_PUBKEY\" > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys && exec /usr/sbin/sshd -D -e"]
`

// sshHost is one running SSH container.
type sshHost struct {
	// Name is the inventory host name.
	Name string
	// Container is the docker container name.
	Container string
	// Port is the host port mapped to the container's SSH port.
	Port string
}

// requireStack skips the test unless docker and ansible are usable.
func requireStack(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"docker", "ansible-playbook", "ansible-inventory", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH", bin)
		}
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
}

// buildImage builds the SSH host image once; the docker cache makes repeats instant.
func buildImage(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "build", "-t", image, "-")
	cmd.Stdin = strings.NewReader(dockerfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build image: %v\n%s", err, out)
	}
}

// keypair generates an ed25519 SSH keypair and returns the private key path and public key text.
func keypair(t *testing.T) (string, string) {
	t.Helper()
	dir := t.TempDir()
	private := filepath.Join(dir, "id_ed25519")
	if out, err := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", private).CombinedOutput(); err != nil {
		t.Fatalf("ssh-keygen: %v\n%s", err, out)
	}
	pub, err := os.ReadFile(private + ".pub")
	if err != nil {
		t.Fatalf("read public key: %v", err)
	}
	return private, strings.TrimSpace(string(pub))
}

// startHosts launches n SSH containers carrying the public key and returns them, cleaned up when
// the test ends.
func startHosts(t *testing.T, n int, pubkey string) []sshHost {
	t.Helper()
	hosts := make([]sshHost, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("node%02d", i+1)
		container := fmt.Sprintf("yardmaster-it-%s-%d", name, time.Now().UnixNano())
		out, err := exec.Command("docker", "run", "-d", "--rm",
			"--name", container, "-e", "SSH_PUBKEY="+pubkey, "-p", "127.0.0.1:0:22", image).CombinedOutput()
		if err != nil {
			t.Fatalf("start %s: %v\n%s", name, err, out)
		}
		t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", container).Run() })

		portOut, err := exec.Command("docker", "port", container, "22/tcp").Output()
		if err != nil {
			t.Fatalf("port %s: %v", name, err)
		}
		addr := strings.TrimSpace(strings.Split(string(portOut), "\n")[0])
		port := addr[strings.LastIndex(addr, ":")+1:]
		hosts = append(hosts, sshHost{Name: name, Container: container, Port: port})
	}
	return hosts
}

// writeInventory writes an INI inventory targeting the SSH containers and returns its path.
func writeInventory(t *testing.T, hosts []sshHost, keyPath string) string {
	t.Helper()
	var b strings.Builder
	b.WriteString("[fleet]\n")
	for _, h := range hosts {
		fmt.Fprintf(&b, "%s ansible_host=127.0.0.1 ansible_port=%s\n", h.Name, h.Port)
	}
	b.WriteString("\n[fleet:vars]\n")
	b.WriteString("ansible_user=root\n")
	fmt.Fprintf(&b, "ansible_ssh_private_key_file=%s\n", keyPath)
	b.WriteString("ansible_python_interpreter=/usr/bin/python3\n")
	// Multiplexing is off because Docker recycles host ports across tests, and a stale SSH
	// control socket keyed to a reused port would silently target a dead container.
	b.WriteString("ansible_ssh_common_args=-o StrictHostKeyChecking=no" +
		" -o UserKnownHostsFile=/dev/null -o ControlMaster=no -o ControlPath=none\n")

	path := filepath.Join(t.TempDir(), "fleet.ini")
	if err := os.WriteFile(path, []byte(b.String()), 0o600); err != nil {
		t.Fatalf("write inventory: %v", err)
	}
	return path
}

// writePlaybook writes the playbook text to a temp file and returns its path.
func writePlaybook(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "play.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	return path
}

// startServer wires the full serve stack, dispatcher, live hub, SQLite store, and HTTP API, and
// returns its base URL.
func startServer(t *testing.T) string {
	t.Helper()
	db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "yard.db"))
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

// postJSON posts a body and decodes the JSON reply into out.
func postJSON(t *testing.T, url, body string, out any) {
	t.Helper()
	res, err := httpClient.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode >= 300 {
		t.Fatalf("POST %s: status %d", url, res.StatusCode)
	}
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// getJSON fetches a URL and decodes the JSON reply into out.
func getJSON(t *testing.T, url string, out any) {
	t.Helper()
	res, err := httpClient.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = res.Body.Close() }()
	if err := json.NewDecoder(res.Body).Decode(out); err != nil {
		t.Fatalf("decode %s: %v", url, err)
	}
}

// httpClient is shared so keep alive connections are reused across polls.
var httpClient = &http.Client{Timeout: 30 * time.Second}

// waitShardsTerminal polls a parent's shards until the expected count exists and every shard is
// terminal, absorbing the moment between a parent finishing and the last shard save landing.
func waitShardsTerminal(t *testing.T, base, parentID string, want int) []run.Run {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		var shards struct {
			Shards []run.Run `json:"shards"`
		}
		getJSON(t, base+"/v1/runs/"+parentID+"/shards", &shards)
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

// waitTerminal polls a run until it reaches a terminal state.
func waitTerminal(t *testing.T, base, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var r run.Run
		getJSON(t, base+"/v1/runs/"+id, &r)
		if r.Status.Terminal() {
			return &r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stuck in %s", id, r.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}

// TestRealSSHFleet drives the whole stack against real SSH hosts: a plain run touches every host,
// then a split shards the same fleet, runs in parallel, and merges results, and fleet health
// reflects both.
func TestRealSSHFleet(t *testing.T) {
	requireStack(t)
	buildImage(t)
	key, pub := keypair(t)
	hosts := startHosts(t, 3, pub)
	inventory := writeInventory(t, hosts, key)
	playbook := writePlaybook(t, `
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
        content: "yardmaster was here\n"
        dest: /tmp/yardmaster-marker

    - name: Read the marker back
      ansible.builtin.command: cat /tmp/yardmaster-marker
      changed_when: false
`)
	base := startServer(t)

	// A plain run reaches all three hosts over real SSH.
	var created run.Run
	postJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q}`, playbook, inventory), &created)
	single := waitTerminal(t, base, created.ID)
	if single.Status != run.StatusSucceeded {
		t.Fatalf("single run status = %q, want succeeded", single.Status)
	}

	var events struct {
		Events []struct {
			Type string `json:"type"`
			Host string `json:"host"`
		} `json:"events"`
	}
	getJSON(t, base+"/v1/runs/"+created.ID+"/events", &events)
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
	postJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q,"shards":2}`, playbook, inventory), &parent)
	split := waitTerminal(t, base, parent.ID)
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
		getJSON(t, base+"/v1/fleet?window=5", &fleet)
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
	requireStack(t)
	buildImage(t)
	key, pub := keypair(t)
	hosts := startHosts(t, 3, pub)
	inventory := writeInventory(t, hosts, key)
	playbook := writePlaybook(t, `
---
- name: Break one node
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60

    - name: Fail while the break marker exists
      ansible.builtin.command: test ! -f /tmp/yardmaster-break
      changed_when: false
`)
	base := startServer(t)

	// Plant the failure on the first host only.
	if out, err := exec.Command("docker", "exec", hosts[0].Container,
		"touch", "/tmp/yardmaster-break").CombinedOutput(); err != nil {
		t.Fatalf("plant break marker: %v\n%s", err, out)
	}

	var parent run.Run
	postJSON(t, base+"/v1/runs",
		fmt.Sprintf(`{"playbook":%q,"inventory":%q,"shards":3}`, playbook, inventory), &parent)
	split := waitTerminal(t, base, parent.ID)
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
		"rm", "/tmp/yardmaster-break").CombinedOutput(); err != nil {
		t.Fatalf("clear break marker: %v\n%s", err, out)
	}
	preRetry := waitShardsTerminal(t, base, parent.ID, 3)
	for _, s := range preRetry {
		t.Logf("pre-retry shard %s limit=%s status=%s error=%q", s.ID, s.Limit, s.Status, s.Error)
	}
	var retry run.Run
	postJSON(t, base+"/v1/runs/"+parent.ID+"/retry", "", &retry)
	if retry.RetryOf == nil || *retry.RetryOf != parent.ID {
		t.Fatalf("retry parent missing retry_of link: %+v", retry)
	}
	final := waitTerminal(t, base, retry.ID)
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

// TestContainerExecutionEnvironment runs a playbook inside a pinned container image and confirms the
// ansible-core reported by the play is the image's, not the host's, proving execution isolation.
func TestContainerExecutionEnvironment(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skip("docker not on PATH")
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		t.Skip("docker daemon not running")
	}
	pull, cancelPull := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancelPull()
	if out, err := exec.CommandContext(pull, "docker", "pull", eeImage).CombinedOutput(); err != nil {
		t.Skipf("cannot pull %s: %v\n%s", eeImage, err, out)
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

	runner := roundhouse.NewSelectiveRunner(true, roundhouse.DefaultContainerLimits())
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
