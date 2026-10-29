// Package integrationtest holds what the end-to-end suites share: throwaway SSH hosts in Docker
// containers carrying a generated key, and the HTTP helpers that drive a server and wait on a run.
package integrationtest

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/run"
)

// Image is the tag for the throwaway SSH host image built once per machine.
const Image = "switchtender-it-sshhost:1"

// dockerfile builds a minimal SSH server with Python for Ansible modules. The public key arrives
// through the SSH_PUBKEY environment variable at run time.
const dockerfile = `
FROM alpine:3.22
RUN apk add --no-cache openssh python3 && ssh-keygen -A && mkdir -p /root/.ssh && chmod 700 /root/.ssh
CMD ["/bin/sh", "-c", "echo \"$SSH_PUBKEY\" > /root/.ssh/authorized_keys && chmod 600 /root/.ssh/authorized_keys && exec /usr/sbin/sshd -D -e"]
`

// Host is one running SSH container.
type Host struct {
	// Name is the inventory host name.
	Name string
	// Container is the docker container name.
	Container string
	// Port is the host port mapped to the container's SSH port.
	Port string
}

// StandDownOrFail skips on a machine allowed to lack something, and fails where the gate provides
// everything.
//
// The job that runs this suite installs docker, ansible and every advertised engine, and then a
// missing one of them stood the test down and reported a pass. That is the same shape that refused
// v1.97.0's predecessor publication one job over: a gate expected to supply a thing, quietly
// proving less than it claims when the thing is absent.
//
// A developer's machine is still allowed to lack any of it and say so. The variable below is the
// switch the PostgreSQL contract, the drift tests, the loomseal cross-check and the engine contract
// already read for exactly this, so this reads the same one rather than inventing another.
func StandDownOrFail(t *testing.T, format string, args ...any) {
	t.Helper()
	if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
		t.Fatalf("SWITCHTENDER_REQUIRE_FULL_SUITE is set and "+format, args...)
	}
	t.Skipf(format, args...)
}

// RequireStack skips the test unless docker and ansible are usable.
func RequireStack(t *testing.T) {
	t.Helper()
	for _, bin := range []string{"docker", "ansible-playbook", "ansible-inventory", "ssh-keygen"} {
		if _, err := exec.LookPath(bin); err != nil {
			StandDownOrFail(t, "%s is not on PATH", bin)
		}
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		StandDownOrFail(t, "the docker daemon is not running: %v", err)
	}
}

// BuildImage builds the SSH host image once; the docker cache makes repeats instant.
func BuildImage(t *testing.T) {
	t.Helper()
	cmd := exec.Command("docker", "build", "-t", Image, "-")
	cmd.Stdin = strings.NewReader(dockerfile)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("build Image: %v\n%s", err, out)
	}
}

// Keypair generates an ed25519 SSH keypair and returns the private key path and public key text.
func Keypair(t *testing.T) (string, string) {
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

// StartHosts launches n SSH containers carrying the public key and returns them, cleaned up when
// the test ends.
func StartHosts(t *testing.T, n int, pubkey string) []Host {
	t.Helper()
	hosts := make([]Host, 0, n)
	for i := 0; i < n; i++ {
		name := fmt.Sprintf("node%02d", i+1)
		container := fmt.Sprintf("switchtender-it-%s-%d", name, time.Now().UnixNano())
		out, err := exec.Command("docker", "run", "-d", "--rm",
			"--name", container, "-e", "SSH_PUBKEY="+pubkey, "-p", "127.0.0.1:0:22", Image).CombinedOutput()
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
		hosts = append(hosts, Host{Name: name, Container: container, Port: port})
	}
	return hosts
}

// WriteInventory writes an INI inventory targeting the SSH containers and returns its path.
func WriteInventory(t *testing.T, hosts []Host, keyPath string) string {
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

// WritePlaybook writes the playbook text to a temp file and returns its path.
func WritePlaybook(t *testing.T, text string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "play.yml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write playbook: %v", err)
	}
	return path
}

// PostJSON posts a body and decodes the JSON reply into out.
func PostJSON(t *testing.T, url, body string, out any) {
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

// GetJSON fetches a URL and decodes the JSON reply into out.
func GetJSON(t *testing.T, url string, out any) {
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

// WaitTerminal polls a run until it reaches a terminal state.
func WaitTerminal(t *testing.T, base, id string) *run.Run {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for {
		var r run.Run
		GetJSON(t, base+"/v1/runs/"+id, &r)
		if r.Status.Terminal() {
			return &r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stuck in %s", id, r.Status)
		}
		time.Sleep(500 * time.Millisecond)
	}
}
