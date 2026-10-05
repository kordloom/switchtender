package migration

import (
	"bufio"
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/kordloom/switchtender/internal/relay"
)

// relayPlaybook records digests of the survey answer and of the key file Ansible was handed, the
// key file's path, and how the target was reached, then prints the answer for the log to mask.
const relayPlaybook = `---
- name: Rotate the database password over the relay
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait until the target answers, which a loaded machine can delay
      ansible.builtin.wait_for_connection:
        timeout: 60
    - name: Ask the target how it was reached
      ansible.builtin.command: printenv SSH_CONNECTION
      register: reached
      changed_when: false
      failed_when: false
    - name: Read the key file Ansible was handed
      ansible.builtin.set_fact:
        key_sum: "{{ lookup('ansible.builtin.file', ansible_private_key_file, rstrip=False)
          | hash('sha256') }}"
    - name: Record digests of what the play received
      ansible.builtin.copy:
        content: |
          {{ db_password | hash('sha256') }}
          {{ key_sum }}
          {{ ansible_private_key_file }}
          {{ reached.stdout | default('') }}
        dest: "{{ marker_dir }}/relay-{{ inventory_hostname }}"
        mode: "0600"
    - name: Print the answer, which the log has to mask
      ansible.builtin.debug:
        msg: "rotating to {{ db_password }}"
`

// TestARelayWorkerRunsWithSealedSecretsAndKeepsNone is the relay scenario. An AWX execution node
// in a segment the control node cannot reach becomes a relay worker, which has no database and no
// encryption key. Its pool registers a delivery key, and a template on its queue needs an SSH key
// and a secret survey answer. The worker receives both, sealed, runs the play with them, over real
// SSH when the host has an SSH server to reach, and afterward neither is in any record, log, API
// answer, the database, or the relay traffic, the key's file is gone, and the chain records which
// pool and worker received which credential. A pool that registered no key keeps today's behavior:
// its run fails with the reason, and nothing is delivered.
//
//nolint:funlen // Test function.
func TestARelayWorkerRunsWithSealedSecretsAndKeepsNone(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	dir := filepath.Join(in.root, "relay")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatalf("create the relay directory: %v", err)
	}

	// The pool's delivery key, made the way an operator makes it.
	deliveryKey := filepath.Join(dir, "delivery.key")
	made := keyInfoFrom(t, in.cli("worker", "key", "new", "--out", deliveryKey))
	for _, line := range pemBodyLines(t, deliveryKey) {
		in.addSecret(line)
	}
	relayToken, bareToken := "relay-pool-"+randomHex(t, 16), "bare-pool-"+randomHex(t, 16)
	in.addSecret(relayToken)
	in.addSecret(bareToken)
	pools := filepath.Join(dir, "workers.yaml")
	if err := os.WriteFile(pools, []byte("workers:\n"+
		"  - name: relay\n    token_sha256: "+relay.HashToken(relayToken)+"\n    queues: [relay]\n"+
		"    delivery_key: "+made.DeliveryKey+"\n"+
		"  - name: bare\n    token_sha256: "+relay.HashToken(bareToken)+"\n    queues: [bare]\n"),
		0o600); err != nil {
		t.Fatalf("write the worker pool file: %v", err)
	}
	s := in.startServer("a", "--worker-pools", pools)

	// The SSH key enters the install once, sealed, and the play reaches its host with it.
	keyPEM, authorized := sshKeypair(t)
	in.addSecret(keyPEM)
	// The first body line of an OpenSSH key is a header every key of its type shares, so it is not
	// a secret of this key, and listing it would flag any other key as this one.
	for i, line := range pemBody(keyPEM) {
		if i > 0 && len(line) >= 16 {
			in.addSecret(line)
		}
	}
	var cred struct {
		// ID is the credential's id.
		ID string `json:"id"`
	}
	in.must(s, "admin", "POST", "/v1/credentials", map[string]any{
		"name": "relay fleet key", "kind": "ssh_key", "secret": keyPEM,
	}, 201).decode(t, &cred)
	inventory, overSSH := relayInventory(t, in, dir, authorized)
	playbook := filepath.Join(dir, "rotate.yml")
	if err := os.WriteFile(playbook, []byte(relayPlaybook), 0o600); err != nil {
		t.Fatalf("write the playbook: %v", err)
	}
	for name, queue := range map[string]string{"relay rotate": "relay", "bare rotate": "bare"} {
		in.must(s, "admin", "POST", "/v1/templates", map[string]any{
			"name": name, "playbook": playbook, "inventory": inventory, "queue": queue,
			"credential_ids": []string{cred.ID},
			"extra_vars":     map[string]any{"marker_dir": in.markers},
			"survey": []map[string]any{{"var": "db_password", "label": "Database password",
				"type": "secret", "required": true}},
		}, 201)
	}

	// The relay traffic passes through a recorder, the way a proxy that logs bodies sees it.
	proxy := in.recordingRelay(s.url)
	in.startWorker("relay-a", proxy, relayToken, "relay", deliveryKey)
	in.startWorker("bare-a", proxy, bareToken, "bare", "")

	answer := "relay-answer-" + randomHex(t, 12)
	in.addSecret(answer)
	rec := in.launched(s, "operator", "relay rotate",
		map[string]any{"answers": map[string]any{"db_password": answer}})
	done := in.waitDone(s, rec.ID)
	if done.Status != "succeeded" || str(done.Raw["claimed_by"]) != "relay-a" {
		t.Fatalf("the relay run = %s on %q, want succeeded on the relay worker: %s", done.Status,
			str(done.Raw["claimed_by"]), describe(done.Raw))
	}
	marks := strings.Split(in.marker("relay", "relayhost"), "\n")
	if len(marks) < 4 {
		t.Fatalf("the play's marker is %q, want four lines", marks)
	}
	answerSum, keySum := sha256.Sum256([]byte(answer)), sha256.Sum256([]byte(keyPEM))
	if marks[0] != hex.EncodeToString(answerSum[:]) {
		t.Errorf("the play received an answer whose digest is %s, want the launch's answer", marks[0])
	}
	if marks[1] != hex.EncodeToString(keySum[:]) {
		t.Errorf("the play was handed a key whose digest is %s, want the credential's key", marks[1])
	}
	// The root may sit under a symbolic link, as /tmp does on macOS, and Ansible reports the path it
	// was handed, so both spellings of the root count.
	root, err := filepath.EvalSymlinks(in.runFiles)
	if err != nil {
		t.Fatalf("resolve the run directory root: %v", err)
	}
	if !strings.HasPrefix(marks[2], in.runFiles+string(os.PathSeparator)) &&
		!strings.HasPrefix(marks[2], root+string(os.PathSeparator)) {
		t.Errorf("the key was written to %s, outside the run directory root %s", marks[2], in.runFiles)
	}
	requireGone(t, "the delivered key file", marks[2])
	if overSSH && marks[3] == "" {
		t.Errorf("the play did not reach its host over SSH, so the delivered key authenticated nothing")
	}
	if dirs := in.runFileDirs(); len(dirs) != 0 {
		t.Errorf("run directories survived the run: %v", dirs)
	}
	logText := string(in.must(s, "admin", "GET", "/v1/runs/"+rec.ID+"/logs", nil, 200).Body)
	if !strings.Contains(logText, "rotating to") {
		t.Fatalf("the log lacks the task printing the answer, so masking was not exercised:\n%s",
			logText)
	}

	bare := in.launched(s, "operator", "bare rotate",
		map[string]any{"answers": map[string]any{"db_password": answer}})
	refused := in.waitDone(s, bare.ID)
	if refused.Status != "failed" ||
		!strings.Contains(str(refused.Raw["error"]),
			`worker pool "bare" has registered no delivery key`) {
		t.Errorf("the run on a pool with no key = %s (%s), want failed naming the pool and the fix",
			refused.Status, str(refused.Raw["error"]))
	}

	ev := in.checkEvidence(s, rec.ID, bare.ID)
	delivered := ev.entries("/relay/delivered/")
	want := relay.DeliveryPath(rec.ID, made.KeyID, []string{cred.ID}, []string{"db_password"})
	if len(delivered) != 1 || delivered[0].Path != want ||
		delivered[0].Actor != "pool:relay worker:relay-a" {
		t.Errorf("delivery records = %v, want exactly %s by pool:relay worker:relay-a", delivered, want)
	}
	requireRecord(t, ev.Receipts[rec.ID], recordWant{
		Launcher: "operator-laptop", OnBehalfOf: "operator", Playbook: playbook,
		Hosts: []string{"relayhost"}, CredentialIDs: []string{cred.ID},
		SealedVars: []string{"db_password"},
	})
}

// keyInfo is what worker key new prints.
type keyInfo struct {
	// KeyID names the key.
	KeyID string `json:"key_id"`
	// DeliveryKey is the public key for the pool file.
	DeliveryKey string `json:"delivery_key"`
}

// keyInfoFrom reads the JSON line worker key new printed among its output.
func keyInfoFrom(t *testing.T, out string) keyInfo {
	t.Helper()
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var k keyInfo
		if json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &k) == nil && k.DeliveryKey != "" {
			return k
		}
	}
	t.Fatalf("worker key new printed no key:\n%s", out)
	return keyInfo{}
}

// pemBody returns the base64 body lines of PEM text, in order.
func pemBody(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		if line != "" && !strings.HasPrefix(line, "-----") && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// pemBodyLines returns the base64 lines of a PEM file, each a secret on its own.
func pemBodyLines(t *testing.T, path string) []string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var out []string
	for _, line := range strings.Split(string(raw), "\n") {
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// sshKeypair returns a new ed25519 private key in OpenSSH form and its authorized_keys line.
func sshKeypair(t *testing.T) (string, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the SSH key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(priv, "")
	if err != nil {
		t.Fatalf("marshal the SSH key: %v", err)
	}
	sshPub, err := ssh.NewPublicKey(pub)
	if err != nil {
		t.Fatalf("encode the SSH public key: %v", err)
	}
	return string(pem.EncodeToMemory(block)), string(ssh.MarshalAuthorizedKey(sshPub))
}

// relayInventory writes the inventory the relay play targets and reports whether it reaches its
// host over SSH. When the machine has an SSH server, one is started on loopback, as this account,
// accepting only the credential's key, so the play authenticates with the key it was delivered and
// nothing else. Without one, the host is reached locally and the play still reads the key file
// Ansible was handed.
func relayInventory(t *testing.T, in *install, dir, authorized string) (string, bool) {
	t.Helper()
	path := filepath.Join(dir, "inventory.ini")
	line := "relayhost ansible_connection=local"
	port, ok := localSSHD(t, dir, authorized)
	if ok {
		me, err := user.Current()
		if err != nil {
			t.Fatalf("read the current account: %v", err)
		}
		line = fmt.Sprintf("relayhost ansible_connection=ssh ansible_host=127.0.0.1 ansible_port=%s "+
			"ansible_timeout=60 ansible_user=%s ansible_remote_tmp=%s "+
			"ansible_ssh_common_args='-o StrictHostKeyChecking=no "+
			"-o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes -o IdentityAgent=none "+
			"-o ControlMaster=no -o ControlPath=none'", port, me.Username,
			filepath.Join(in.root, "remote-tmp"))
	}
	text := "[fleet]\n" + line + " ansible_python_interpreter=\"{{ ansible_playbook_python }}\"\n"
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatalf("write the inventory: %v", err)
	}
	return path, ok
}

// localSSHD starts an SSH server as this account on a loopback port, accepting only the key in
// authorized, and returns the port. It reports false, starting nothing, on a machine without one or
// where one cannot run unprivileged, which leaves the scenario reaching its host locally.
func localSSHD(t *testing.T, dir, authorized string) (string, bool) {
	t.Helper()
	bin, err := exec.LookPath("sshd")
	if err != nil {
		bin = "/usr/sbin/sshd"
		if _, err := os.Stat(bin); err != nil {
			return "", false
		}
	}
	_, hostKey, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("generate the host key: %v", err)
	}
	block, err := ssh.MarshalPrivateKey(hostKey, "")
	if err != nil {
		t.Fatalf("marshal the host key: %v", err)
	}
	hostKeyPath := filepath.Join(dir, "ssh_host_ed25519_key")
	authPath := filepath.Join(dir, "authorized_keys")
	if err := os.WriteFile(hostKeyPath, pem.EncodeToMemory(block), 0o600); err != nil {
		t.Fatalf("write the host key: %v", err)
	}
	if err := os.WriteFile(authPath, []byte(authorized), 0o600); err != nil {
		t.Fatalf("write authorized_keys: %v", err)
	}
	port := freePort(t)
	config := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(config, []byte(strings.Join([]string{
		"Port " + port, "ListenAddress 127.0.0.1", "HostKey " + hostKeyPath,
		"PidFile " + filepath.Join(dir, "sshd.pid"), "AuthorizedKeysFile " + authPath,
		"StrictModes no", "PasswordAuthentication no", "KbdInteractiveAuthentication no",
		"PubkeyAuthentication yes", "UsePAM no", "LogLevel VERBOSE", "",
	}, "\n")), 0o600); err != nil {
		t.Fatalf("write sshd_config: %v", err)
	}
	logFile, err := os.Create(filepath.Join(dir, "sshd.log"))
	if err != nil {
		t.Fatalf("create the sshd log: %v", err)
	}
	c := exec.Command(bin, "-D", "-e", "-f", config)
	c.Stdout, c.Stderr = logFile, logFile
	if err := c.Start(); err != nil {
		_ = logFile.Close()
		return "", false
	}
	t.Cleanup(func() {
		_ = c.Process.Signal(syscall.SIGTERM)
		_ = c.Wait()
		_ = logFile.Close()
	})
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", "127.0.0.1:"+port, time.Second)
		if err == nil {
			banner := make([]byte, 4)
			_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
			_, rerr := io.ReadFull(conn, banner)
			_ = conn.Close()
			if rerr == nil && string(banner) == "SSH-" {
				return port, true
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	log, _ := os.ReadFile(filepath.Join(dir, "sshd.log"))
	t.Logf("no unprivileged SSH server could start here, so the play reaches its host locally:\n%s",
		log)
	return "", false
}

// recordingRelay starts a reverse proxy to the control node that records every relay request and
// response body for the leak scan, and returns its URL. Bodies are what a proxy that logs traffic
// keeps, and they are where a delivered secret would show if it ever crossed in the clear.
func (in *install) recordingRelay(target string) string {
	in.t.Helper()
	u, err := url.Parse(target)
	if err != nil {
		in.t.Fatalf("parse the server URL: %v", err)
	}
	proxy := httputil.NewSingleHostReverseProxy(u)
	proxy.Transport = recordingTransport{in: in, base: http.DefaultTransport}
	ts := httptest.NewServer(proxy)
	in.t.Cleanup(ts.Close)
	return ts.URL
}

// recordingTransport records each relay exchange's bodies before passing them on.
type recordingTransport struct {
	// in is the install whose leak scan receives the bodies.
	in *install
	// base carries the request.
	base http.RoundTripper
}

// RoundTrip records the request and response bodies of one relay call.
func (r recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	what := "relay traffic " + req.Method + " " + req.URL.Path
	if req.Body != nil {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		r.in.recordAnswer(what+" request", body)
		req.Body = io.NopCloser(bytes.NewReader(body))
	}
	resp, err := r.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	r.in.recordAnswer(what+" response", body)
	resp.Body = io.NopCloser(bytes.NewReader(body))
	return resp, nil
}

// startWorker starts a relay worker child process serving queue through relayURL with the pool
// token, and with deliveryKey when one is given. It holds no database and none of the install's
// encryption keys, which is what a relay worker in another segment holds. Its output is kept with
// the servers' so the leak scan reads it too.
func (in *install) startWorker(name, relayURL, token, queue, deliveryKey string) {
	in.t.Helper()
	args := []string{"worker", "--server", relayURL, "--queue", queue, "--name", name}
	if deliveryKey != "" {
		args = append(args, "--delivery-key", deliveryKey)
	}
	var env []string
	for _, kv := range in.env {
		if strings.HasPrefix(kv, "SWITCHTENDER_ENCRYPTION_") {
			continue
		}
		env = append(env, kv)
	}
	env = append(env, "SWITCHTENDER_WORKER_TOKEN="+token)
	self, err := os.Executable()
	if err != nil {
		in.t.Fatalf("locate the test binary: %v", err)
	}
	logPath := filepath.Join(in.root, "logs", "worker-"+name+".log")
	logFile, err := os.Create(logPath)
	if err != nil {
		in.t.Fatalf("create the worker log: %v", err)
	}
	c := exec.Command(self, args...)
	c.Env, c.Dir = env, in.root
	c.Stdout, c.Stderr = logFile, logFile
	if err := c.Start(); err != nil {
		in.t.Fatalf("start worker %s: %v", name, err)
	}
	w := &server{name: "worker " + name, cmd: c, logPath: logPath, done: make(chan struct{})}
	go func() {
		w.exitErr = c.Wait()
		_ = logFile.Close()
		close(w.done)
	}()
	in.servers = append(in.servers, w)
	in.t.Cleanup(w.stop)
	select {
	case <-w.done:
		log, _ := os.ReadFile(logPath)
		in.t.Fatalf("worker %s exited at once: %v\n%s", name, w.exitErr, log)
	case <-time.After(500 * time.Millisecond):
	}
}
