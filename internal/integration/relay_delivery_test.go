//go:build integration

package integration_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/handoff"
	"github.com/kordloom/switchtender/internal/relay"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/server"
	"github.com/kordloom/switchtender/internal/sqlitestore"
)

// TestARelayWorkerReachesARealHostWithADeliveredKey runs a relay worker that holds no database and
// no encryption key against a real SSH host. The SSH key exists only as a sealed credential on the
// control node, and the inventory names no key, so the play can authenticate only with the key the
// control node delivered at claim. The play also writes a digest of a secret survey answer onto the
// host, and the test reads it back over docker to prove the answer arrived intact.
func TestARelayWorkerReachesARealHostWithADeliveredKey(t *testing.T) {
	requireStack(t)
	buildImage(t)
	keyPath, pub := keypair(t)
	hosts := startHosts(t, 1, pub)
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("read the key: %v", err)
	}
	// The key is used only through the delivery, so the copy on disk is removed now.
	if err := os.Remove(keyPath); err != nil {
		t.Fatalf("remove the key: %v", err)
	}
	ctx := context.Background()
	dir := t.TempDir()
	db, err := sqlitestore.Open(filepath.Join(dir, "control.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	sealer := credential.NewSealer("integration-relay-key", "integration-relay-salt")
	sealedKey, err := sealer.Seal(string(keyPEM))
	if err != nil {
		t.Fatalf("seal the key: %v", err)
	}
	if err := db.Credentials().Save(ctx, &credential.Credential{ID: "cred_fleet", Name: "fleet key",
		Kind: credential.KindSSHKey, Secret: sealedKey,
		Settings: map[string]string{"user": "root"}}); err != nil {
		t.Fatalf("save the credential: %v", err)
	}
	control := dispatch.New(db.Runs(), roundhouse.NewAnsibleRunner(), zaptest.NewLogger(t),
		dispatch.WithCredentials(db.Credentials(), sealer), dispatch.WithAudits(db.Audits()))
	t.Cleanup(control.Close)

	poolKey, err := handoff.GenerateKey()
	if err != nil {
		t.Fatalf("generate the pool key: %v", err)
	}
	poolFile := filepath.Join(dir, "workers.yaml")
	if err := os.WriteFile(poolFile, []byte("workers:\n  - name: edge\n    token_sha256: "+
		relay.HashToken("tok-edge")+"\n    queues: [edge]\n    delivery_key: "+
		poolKey.Public().String()+"\n"), 0o600); err != nil {
		t.Fatalf("write the pool file: %v", err)
	}
	pools, err := relay.LoadPools(poolFile)
	if err != nil {
		t.Fatalf("load the pool file: %v", err)
	}
	srv := httptest.NewServer(server.New(db.Runs(), control, nil,
		server.WithCredentials(db.Credentials(), sealer), server.WithAudit(db.Audits()),
		server.WithRelay(db.Runs(), ""), server.WithWorkerPools(pools),
		server.WithSecretOpener(control)).Handler())
	t.Cleanup(srv.Close)

	ring, err := handoff.NewKeyRing(poolKey)
	if err != nil {
		t.Fatalf("build the key ring: %v", err)
	}
	transport := relay.NewHTTPTransport(srv.URL, "tok-edge", srv.Client())
	worker := dispatch.New(relay.NewClient(transport), roundhouse.NewAnsibleRunner(),
		zaptest.NewLogger(t), dispatch.WithOwner("edge-1"), dispatch.WithQueues([]string{"edge"}),
		dispatch.WithNoJanitor(), dispatch.WithPolicies(relay.NewPolicyClient(transport)),
		dispatch.WithSecretDelivery(relay.NewReceiver(transport, ring)),
		dispatch.WithRunFilesRoot(filepath.Join(dir, "worker-runfiles")))
	t.Cleanup(worker.Close)

	inventory := filepath.Join(dir, "edge.ini")
	if err := os.WriteFile(inventory, []byte(fmt.Sprintf("[edge]\n%s ansible_host=127.0.0.1 "+
		"ansible_port=%s ansible_python_interpreter=/usr/bin/python3 ansible_ssh_common_args='-o "+
		"StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o IdentitiesOnly=yes "+
		"-o IdentityAgent=none -o ControlMaster=no -o ControlPath=none'\n", hosts[0].Name,
		hosts[0].Port)), 0o600); err != nil {
		t.Fatalf("write the inventory: %v", err)
	}
	playbook := writePlaybook(t, `
---
- name: Rotate over the relay
  hosts: all
  gather_facts: false
  tasks:
    - name: Wait for ssh readiness
      ansible.builtin.wait_for_connection:
        timeout: 60
    - name: Record a digest of the delivered answer on the host
      ansible.builtin.copy:
        content: "{{ db_password | hash('sha256') }}\n"
        dest: /tmp/relay-answer
`)
	const answer = "integration-relay-answer-71d2"
	sealedAnswer, err := sealer.Seal(answer)
	if err != nil {
		t.Fatalf("seal the answer: %v", err)
	}
	if err := db.Runs().Save(ctx, &run.Run{ID: "run_relay_ssh", Playbook: playbook,
		Inventory: inventory, Status: run.StatusPending, Queue: "edge",
		CredentialIDs: []string{"cred_fleet"}, SealedNames: []string{"db_password"},
		SealedVars: map[string]string{"db_password": sealedAnswer}, CreatedAt: time.Now()}); err != nil {
		t.Fatalf("save the run: %v", err)
	}

	final := waitTerminal(t, srv.URL, "run_relay_ssh")
	if final.Status != run.StatusSucceeded || final.ClaimedBy != "edge-1" {
		logs, _ := db.Runs().Log(ctx, "run_relay_ssh")
		t.Fatalf("relay run = %s on %q (%s), want succeeded on the relay worker\n%s", final.Status,
			final.ClaimedBy, final.Error, logs)
	}
	out, err := exec.Command("docker", "exec", hosts[0].Container, "cat",
		"/tmp/relay-answer").Output()
	if err != nil {
		t.Fatalf("read the host's marker: %v", err)
	}
	sum := sha256.Sum256([]byte(answer))
	if got := strings.TrimSpace(string(out)); got != hex.EncodeToString(sum[:]) {
		t.Errorf("the host holds digest %s, want the digest of the delivered answer", got)
	}
	chain, err := db.Audits().Chain(ctx)
	if err != nil {
		t.Fatalf("read the chain: %v", err)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Fatalf("the chain no longer verifies, broken at %d", at)
	}
	want := relay.DeliveryPath("run_relay_ssh", poolKey.ID(), []string{"cred_fleet"},
		[]string{"db_password"})
	found := false
	for _, e := range chain {
		found = found || (e.Path == want && e.Actor == "pool:edge worker:edge-1")
		if strings.Contains(e.Path+e.Actor, answer) {
			t.Errorf("the chain carries the answer")
		}
	}
	if !found {
		t.Errorf("the chain has no record of the delivery at %s", want)
	}
}
