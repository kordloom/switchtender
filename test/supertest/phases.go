package main

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// manifests carries everything the cluster is built from, so the supertest is one self-contained
// package: no paths into the rest of the repository beyond the chart and the Dockerfile it exists
// to test.
//
//go:embed manifests
var manifests embed.FS

// mustManifest reads an embedded manifest, and panics if it is missing, because a missing embed is
// a build defect rather than a runtime condition.
func mustManifest(name string) string {
	raw, err := manifests.ReadFile("manifests/" + name)
	if err != nil {
		panic(err)
	}
	return string(raw)
}

// fleetHosts are the stable DNS names of the three managed machines, and their pod names, in a
// fixed order so every assertion reads the same fleet.
var fleetHosts = []struct {
	// Pod is the pod name kubectl exec reaches.
	Pod string
	// FQDN is the name the inventory carries and Ansible dials.
	FQDN string
}{
	{Pod: "fleet-0", FQDN: "fleet-0.fleet.fleet.svc.cluster.local"},
	{Pod: "fleet-1", FQDN: "fleet-1.fleet.fleet.svc.cluster.local"},
	{Pod: "fleet-2", FQDN: "fleet-2.fleet.fleet.svc.cluster.local"},
}

// phaseCluster builds the product image from the working tree, builds the fleet image, and brings
// up a fresh Kind cluster carrying both. Building from source is the point: the supertest answers
// for the code as it is now, not for the last release.
func (h *harness) phaseCluster() error {
	const phase = "cluster"

	if out, err := h.run("go", "build", "-o", h.bin, "."); err != nil {
		return fmt.Errorf("build local binary: %w\n%s", err, out)
	}
	h.pass(phase, "the working tree builds", "the same binary later verifies receipts offline")

	if out, err := h.run("docker", "build", "-t", "switchtender:supertest", h.repo); err != nil {
		return fmt.Errorf("build product image: %w\n%s", err, out)
	}
	h.pass(phase, "the product image builds from the repository Dockerfile", "")

	fleetDir := filepath.Join(h.work, "fleet-image")
	if err := os.MkdirAll(fleetDir, 0o755); err != nil {
		return err
	}
	dockerfile := filepath.Join(fleetDir, "Dockerfile")
	if err := os.WriteFile(dockerfile, []byte(mustManifest("fleet.Dockerfile")), 0o644); err != nil {
		return err
	}
	if out, err := h.run("docker", "build", "-t", "supertest-fleet:local", fleetDir); err != nil {
		return fmt.Errorf("build fleet image: %w\n%s", err, out)
	}
	h.pass(phase, "the fleet image builds", "alpine, sshd, python: honestly a server")

	if out, err := h.run("kind", "create", "cluster", "--name", h.cluster,
		"--kubeconfig", h.kubeconfig, "--wait", "120s"); err != nil {
		return fmt.Errorf("create kind cluster: %w\n%s", err, out)
	}
	h.created = true
	h.pass(phase, "a fresh Kind cluster is up", "nothing in it has ever seen this product")

	// PostgreSQL is pulled on the host and loaded like the built images, so the node never dials
	// a registry. A cluster that pulls at pod-start is at the mercy of registry rate limits, and
	// a supertest that fails on somebody else's throttling reports nothing about the product.
	if _, err := h.run("docker", "image", "inspect", postgresImage); err != nil {
		if out, err := h.run("docker", "pull", postgresImage); err != nil {
			return fmt.Errorf("pull %s: %w\n%s", postgresImage, err, out)
		}
	}
	for _, image := range []string{"switchtender:supertest", "supertest-fleet:local", postgresImage} {
		if out, err := h.run("kind", "load", "docker-image", image, "--name", h.cluster); err != nil {
			return fmt.Errorf("load %s: %w\n%s", image, err, out)
		}
	}
	h.pass(phase, "every image is loaded into the cluster",
		"nothing pulls from a registry after this point")
	return nil
}

// postgresImage is the database the Team tier runs against, pinned so every run answers for the
// same dependency.
const postgresImage = "postgres:16-alpine"

// phaseFleet stands up the three machines the runs will manage, keyed to a keypair generated for
// this run alone, and proves each one answers before anything is asked of the product.
func (h *harness) phaseFleet() error {
	const phase = "fleet"

	keyPath := filepath.Join(h.work, "fleet_ed25519")
	if out, err := h.run("ssh-keygen", "-t", "ed25519", "-N", "", "-q", "-f", keyPath); err != nil {
		return fmt.Errorf("generate fleet key: %w\n%s", err, out)
	}
	if _, err := h.kubectl("create", "namespace", "fleet"); err != nil {
		return err
	}
	if _, err := h.kubectl("create", "secret", "generic", "fleet-authorized", "-n", "fleet",
		"--from-file=authorized_keys="+keyPath+".pub"); err != nil {
		return err
	}
	if err := h.apply(mustManifest("fleet.yaml")); err != nil {
		return err
	}
	if _, err := h.kubectl("rollout", "status", "statefulset/fleet", "-n", "fleet",
		"--timeout=180s"); err != nil {
		return err
	}
	h.pass(phase, "three SSH machines are ready", "fleet-0 through fleet-2, keys only, no password")
	return nil
}

// phaseCommunity installs the free tier the way the chart installs it, with no license and no
// external database, and proves a real playbook lands on the real fleet. This is the install the
// Kubernetes quickstart produces, and until this phase existed it did not boot at all: the chart
// defaulted to PostgreSQL, whose fresh schema only a Team license may initialize.
func (h *harness) phaseCommunity() error {
	const phase = "community"

	if out, err := h.run("helm", "install", "community",
		filepath.Join(h.repo, "deploy/helm/switchtender"),
		"--namespace", "community", "--create-namespace",
		"--kubeconfig", h.kubeconfig,
		"--set", "image.repository=switchtender",
		"--set", "image.tag=supertest",
		"--set", "image.pullPolicy=Never",
		"--set", "encryptionKey=supertest-key-not-a-secret",
		"--set", "encryptionSalt=supertest-salt",
		"--wait", "--timeout", "300s"); err != nil {
		return fmt.Errorf("helm install community: %w\n%s\n%s", err, out,
			h.installForensics("community"))
	}
	h.pass(phase, "the chart installs with no license and no external database",
		"SQLite on a PersistentVolumeClaim, the Community shape")

	strategy, err := h.kubectl("get", "deploy", "-n", "community",
		"-l", "app.kubernetes.io/component=server",
		"-o", "jsonpath={.items[0].spec.strategy.type}")
	if err != nil {
		return err
	}
	if strings.TrimSpace(strategy) != "Recreate" {
		h.fail(phase, "SQLite mode updates by Recreate",
			fmt.Errorf("strategy is %q; a rolling update would run two writers on one file", strategy))
	} else {
		h.pass(phase, "SQLite mode updates by Recreate",
			"a rolling update would briefly run two writers against one file")
	}

	service, err := h.serviceName("community")
	if err != nil {
		return err
	}
	if err := h.forwardTo("community", service); err != nil {
		return err
	}
	if err := h.bootstrap("community"); err != nil {
		return err
	}
	h.pass(phase, "the first accounts and tokens are minted before anything latches auth",
		"the bootstrap window a real operator uses once")
	nonce := fmt.Sprintf("supertest-%d", time.Now().UnixNano())
	runID, err := h.deployAcrossFleet("community", nonce, nil)
	if err != nil {
		return err
	}
	h.pass(phase, "a real Ansible run succeeded across the fleet", "run "+runID)

	for _, host := range fleetHosts {
		got, err := h.fleetExec(host.Pod, "cat", "/home/ops/supertest-marker")
		if err != nil || got != nonce {
			h.fail(phase, host.Pod+" carries this run's marker",
				fmt.Errorf("read %q, want %q (%v)", got, nonce, err))
			continue
		}
		h.pass(phase, host.Pod+" carries this run's marker, read via kubectl exec",
			"the proof owes nothing to the product's own reporting")
	}

	if err := h.verifyReceiptOffline(phase, runID, ""); err != nil {
		return err
	}
	return nil
}

// phaseTeam installs the paid tier against a PostgreSQL it has never seen, then walks the arc the
// product exists for: a destructive change is graded from the playbook's own text, held by a
// policy on that grade, refused to its own requester, released by a second person, executed for
// real, and left behind as evidence that survives the trip out of the building.
func (h *harness) phaseTeam(license string) error {
	const phase = "team"

	if _, err := h.kubectl("create", "namespace", "team"); err != nil {
		return err
	}
	if err := h.apply(mustManifest("postgres.yaml")); err != nil {
		return err
	}
	seedPath := filepath.Join(h.work, "audit-seed")
	if err := os.WriteFile(seedPath, []byte(h.auditSeed()), 0o600); err != nil {
		return err
	}
	if _, err := h.kubectl("rollout", "status", "deploy/postgres", "-n", "team",
		"--timeout=180s"); err != nil {
		return err
	}
	if out, err := h.run("helm", "install", "team",
		filepath.Join(h.repo, "deploy/helm/switchtender"),
		"--namespace", "team",
		"--kubeconfig", h.kubeconfig,
		"--set", "image.repository=switchtender",
		"--set", "image.tag=supertest",
		"--set", "image.pullPolicy=Never",
		"--set", "encryptionKey=supertest-key-not-a-secret",
		"--set", "encryptionSalt=supertest-salt",
		"--set", "database.dsn=postgres://switchtender:supertest-not-a-secret@postgres:5432/switchtender?sslmode=disable",
		// The shared signing identity every process in a PostgreSQL install signs with. The chart
		// refuses to render without one, because the supertest caught an install that looked
		// healthy and answered 404 for every receipt it should have signed. It travels by file
		// rather than on the argv: a failed helm command prints its arguments into a public CI
		// log, and a signing seed in a public log is a forged history waiting to happen.
		"--set-file", "auditKey="+seedPath,
		"--set-file", "license="+license,
		"--set", "worker.enabled=true",
		"--set", "worker.extraArgs={--queue,supertest}",
		"--wait", "--timeout", "300s"); err != nil {
		return fmt.Errorf("helm install team: %w\n%s\n%s", err, out,
			h.installForensics("team"))
	}
	h.pass(phase, "a fresh PostgreSQL schema initialized under the license",
		"the exact operation a Community install is refused")

	service, err := h.serviceName("team")
	if err != nil {
		return err
	}
	if err := h.forwardTo("team", service); err != nil {
		return err
	}
	if err := h.bootstrap("team"); err != nil {
		return err
	}

	var policy map[string]any
	err = h.apiCall("POST", "/v1/policies", &h.human,
		map[string]any{
			"name":                      "irreversible needs a second approver",
			"reversibility":             "irreversible",
			"effect":                    "require_approval",
			"require_distinct_approver": true,
		}, &policy)
	if err != nil {
		return fmt.Errorf("create the grade policy: %w", err)
	}
	h.pass(phase, "a policy holds on the reversibility grade",
		"not on a command string somebody has to keep current")

	nonce := fmt.Sprintf("supertest-%d", time.Now().UnixNano())
	labels := map[string]string{"change": "supertest-arc"}
	deployID, err := h.deployAcrossFleet("team", nonce, labels)
	if err != nil {
		return err
	}
	h.pass(phase, "the routine playbook was not held", "run "+deployID+
		": creating files grades reversible, so the gate stayed open")

	var destroy map[string]any
	err = h.apiCall("POST", "/v1/runs", &h.agent, map[string]any{
		"playbook":       "/data/destroy.yml",
		"inventory_id":   h.mustID("team-inventory"),
		"credential_ids": []string{h.mustID("team-credential")},
		"labels":         labels,
	}, &destroy)
	if err != nil {
		return fmt.Errorf("submit the destructive run: %w", err)
	}
	destroyID, _ := destroy["id"].(string)
	h.ids["team-destroy-run"] = destroyID

	held, err := h.awaitRunStatus(destroyID, "pending_approval", 60*time.Second)
	if err != nil {
		return fmt.Errorf("the destructive run was not held: %w", err)
	}
	class := dig(held, "reversibility", "class")
	if class != "irreversible" {
		h.fail(phase, "the playbook's text graded the run irreversible",
			fmt.Errorf("graded %q", class))
	} else {
		h.pass(phase, "the playbook's text graded the run irreversible",
			"nothing in destroy.yml announces itself as dangerous; the scanner read the file")
	}
	if _, err := h.fleetExec("fleet-0", "test", "-d", "/home/ops/precious"); err != nil {
		h.fail(phase, "the data still exists while the run is held", err)
	} else {
		h.pass(phase, "the data still exists while the run is held", "checked via kubectl exec")
	}

	if detail, err := h.mustRefuse(&h.agent, "POST", "/v1/runs/"+destroyID+"/approve",
		map[string]any{}); err != nil {
		h.fail(phase, "the agent cannot release its own hold", err)
	} else {
		h.pass(phase, "the agent cannot release its own hold", detail)
	}

	if err := h.apiCall("POST", "/v1/runs/"+destroyID+"/approve", &h.human,
		map[string]any{}, nil); err != nil {
		return fmt.Errorf("human approval: %w", err)
	}
	if _, err := h.awaitRunStatus(destroyID, "succeeded", 120*time.Second); err != nil {
		return fmt.Errorf("approved run did not execute: %w", err)
	}
	for _, host := range fleetHosts {
		if _, err := h.fleetExec(host.Pod, "test", "!", "-d", "/home/ops/precious"); err != nil {
			h.fail(phase, host.Pod+" no longer holds the data", err)
			continue
		}
		h.pass(phase, host.Pod+" no longer holds the data",
			"the deletion was real, confirmed via kubectl exec")
	}

	if err := h.verifyReceiptOffline(phase, destroyID, h.auditFingerprint()); err != nil {
		return err
	}
	if err := h.tamperCheck(phase, destroyID); err != nil {
		return err
	}

	var estate struct {
		Total int              `json:"total"`
		Hosts []map[string]any `json:"hosts"`
	}
	if err := h.apiCall("GET", "/v1/estate", &h.human, nil, &estate); err != nil {
		return err
	}
	if estate.Total < len(fleetHosts) {
		h.fail(phase, "the estate remembers every machine the runs touched",
			fmt.Errorf("estate holds %d hosts, want at least %d", estate.Total, len(fleetHosts)))
	} else {
		h.pass(phase, "the estate remembers every machine the runs touched",
			fmt.Sprintf("%d hosts from gathered facts", estate.Total))
	}

	var change map[string]any
	if err := h.apiCall("GET", "/v1/changes/supertest-arc", &h.human, nil, &change); err != nil {
		h.fail(phase, "the two runs read back as one change", err)
	} else if outcome, _ := change["outcome"].(string); outcome != "succeeded" {
		h.fail(phase, "the two runs read back as one change",
			fmt.Errorf("outcome is %q, want succeeded", outcome))
	} else {
		h.pass(phase, "the two runs read back as one change",
			"a build and its destructive follow-up, one arc, outcome derived rather than typed")
	}

	h.phaseRBAC(phase)

	var queued map[string]any
	err = h.apiCall("POST", "/v1/runs", &h.human, map[string]any{
		"tool": "bash", "command": "true", "queue": "supertest",
	}, &queued)
	if err != nil {
		return fmt.Errorf("submit the queued run: %w", err)
	}
	queuedID, _ := queued["id"].(string)
	done, err := h.awaitRunStatus(queuedID, "succeeded", 120*time.Second)
	if err != nil {
		return fmt.Errorf("queued run: %w", err)
	}
	claimedBy, _ := done["claimed_by"].(string)
	if !strings.Contains(claimedBy, "worker") {
		h.fail(phase, "a queue-pinned run was claimed by the dedicated worker",
			fmt.Errorf("claimed_by is %q", claimedBy))
	} else {
		h.pass(phase, "a queue-pinned run was claimed by the dedicated worker",
			"claimed_by "+claimedBy)
	}
	return nil
}

// mustID returns a remembered object id and panics when the name was never stored, which is a
// defect in this package rather than in the product.
func (h *harness) mustID(name string) string {
	id, ok := h.ids[name]
	if !ok {
		panic("no id remembered for " + name)
	}
	return id
}

// deployAcrossFleet writes the playbooks into the server pod, creates the SSH credential and the
// fleet inventory on the current install, and runs the constructive playbook to completion. It
// returns the run id and remembers the created ids under "<namespace>-credential" and
// "<namespace>-inventory".
func (h *harness) deployAcrossFleet(namespace, nonce string,
	labels map[string]string) (string, error) {
	pod, err := h.serverPod(namespace)
	if err != nil {
		return "", err
	}
	for name, content := range map[string]string{
		"deploy.yml":  mustManifest("deploy.yml"),
		"destroy.yml": mustManifest("destroy.yml"),
	} {
		if err := h.writePodFile(namespace, pod, "/data/"+name, content); err != nil {
			return "", err
		}
	}

	key, err := os.ReadFile(filepath.Join(h.work, "fleet_ed25519"))
	if err != nil {
		return "", err
	}

	var cred map[string]any
	err = h.apiCall("POST", "/v1/credentials", &h.human, map[string]any{
		"name": "fleet ssh", "kind": "ssh_key", "secret": string(key),
	}, &cred)
	if err != nil {
		return "", fmt.Errorf("create ssh credential: %w", err)
	}
	credID, _ := cred["id"].(string)
	h.ids[namespace+"-credential"] = credID

	var lines []string
	lines = append(lines, "[fleet]")
	for _, host := range fleetHosts {
		lines = append(lines, host.FQDN)
	}
	lines = append(lines, "", "[fleet:vars]", "ansible_user=ops",
		"ansible_python_interpreter=/usr/bin/python3",
		"ansible_ssh_common_args=-o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null")
	var inv map[string]any
	err = h.apiCall("POST", "/v1/inventories", &h.human, map[string]any{
		"name": "fleet", "content": strings.Join(lines, "\n") + "\n",
	}, &inv)
	if err != nil {
		return "", fmt.Errorf("create inventory: %w", err)
	}
	invID, _ := inv["id"].(string)
	h.ids[namespace+"-inventory"] = invID

	var created map[string]any
	err = h.apiCall("POST", "/v1/runs", &h.human, map[string]any{
		"playbook":       "/data/deploy.yml",
		"inventory_id":   invID,
		"credential_ids": []string{credID},
		"extra_vars":     map[string]any{"marker": nonce},
		"labels":         labels,
	}, &created)
	if err != nil {
		return "", fmt.Errorf("submit deploy run: %w", err)
	}
	runID, _ := created["id"].(string)
	if _, err := h.awaitRunStatus(runID, "succeeded", 180*time.Second); err != nil {
		return runID, err
	}
	return runID, nil
}

// awaitRunStatus polls a run until it reaches the wanted status, treating any other terminal
// state as the failure it is.
func (h *harness) awaitRunStatus(id, want string, limit time.Duration) (map[string]any, error) {
	var last map[string]any
	err := h.waitFor(fmt.Sprintf("run %s to reach %s", id, want), limit, func() error {
		var doc map[string]any
		if err := h.apiCall("GET", "/v1/runs/"+id, &h.human, nil, &doc); err != nil {
			return err
		}
		last = doc
		status, _ := doc["status"].(string)
		if status == want {
			return nil
		}
		for _, terminal := range []string{"succeeded", "failed", "canceled", "rejected"} {
			if status == terminal {
				forensics := h.runEventTail(id)
				if strings.TrimSpace(forensics) == "" {
					forensics = h.runLogTail(id)
				}
				return fmt.Errorf("run reached %s instead: %s\n%s",
					status, dig(doc, "error"), forensics)
			}
		}
		return fmt.Errorf("run is %s", status)
	})
	return last, err
}

// runLogTail returns the end of a run's raw output, the forensic well below the event stream: a
// process that died before emitting a single structured event still usually wrote SOMETHING, and
// a report that says "failed" over an empty error and an empty event list made somebody rerun a
// thirteen-minute suite just to learn what a log line already knew.
func (h *harness) runLogTail(id string) string {
	// The endpoint answers plain text, so this reads it raw with the same bearer apiCall carries.
	req, err := http.NewRequest("GET", h.api+"/v1/runs/"+id+"/logs?tail=4096", nil)
	if err != nil {
		return "log unavailable: " + err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+h.human.Token)
	resp, err := h.httpc.Do(req)
	if err != nil {
		return "log unavailable: " + err.Error()
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "log unavailable: " + err.Error()
	}
	if strings.TrimSpace(string(raw)) == "" {
		return "the run produced no log output at all"
	}
	lines := strings.Split(strings.TrimRight(string(raw), "\n"), "\n")
	if len(lines) > 12 {
		lines = lines[len(lines)-12:]
	}
	return "log tail:\n" + strings.Join(lines, "\n")
}

// runEventTail returns the last stretch of a run's own event stream, so a run that fails inside
// the cluster explains itself in the report instead of leaving a bare status. A test that says
// "failed" and nothing else is a test that makes somebody else do the investigating.
func (h *harness) runEventTail(id string) string {
	var resp struct {
		Events []map[string]any `json:"events"`
	}
	if err := h.apiCall("GET", "/v1/runs/"+id+"/events?limit=500", &h.human, nil, &resp); err != nil {
		return "(events unavailable: " + oneLine(err.Error()) + ")"
	}
	var lines []string
	for _, ev := range resp.Events {
		parts := []string{dig(ev, "type"), dig(ev, "task"), dig(ev, "host")}
		for _, field := range []string{"message", "stderr", "stdout"} {
			if v := dig(ev, field); v != "" {
				parts = append(parts, field+"="+v)
			}
		}
		line := strings.TrimSpace(strings.Join(parts, " "))
		if line != "" {
			lines = append(lines, line)
		}
	}
	const keep = 25
	if len(lines) > keep {
		lines = lines[len(lines)-keep:]
	}
	return "  " + strings.Join(lines, "\n  ")
}

// verifyReceiptOffline downloads a run's receipt and verifies it with the locally built binary:
// no cluster, no database, no network, which is the property being sold.
func (h *harness) verifyReceiptOffline(phase, runID, pin string) error {
	path := filepath.Join(h.work, phase+"-"+runID+"-receipt.json")
	var receipt json.RawMessage
	if err := h.apiCall("GET", "/v1/runs/"+runID+"/receipt", &h.human, nil, &receipt); err != nil {
		return fmt.Errorf("download receipt: %w", err)
	}
	if err := os.WriteFile(path, receipt, 0o644); err != nil {
		return err
	}
	// The pin is what upgrades "this receipt is internally consistent" into "this receipt was
	// signed by the identity this run configured". Unpinned, a server that ignored its auditKey
	// and minted an ephemeral identity would still verify green, which is the one silent failure
	// the paid tier's shared-identity story cannot afford.
	args := []string{"verify", path}
	claim := "the run's receipt verifies offline"
	detail := "checked by a local binary that never spoke to the cluster"
	if pin != "" {
		args = append(args, "--pubkey", pin)
		claim = "the run's receipt verifies offline against the configured key"
		detail = "pinned to the fingerprint derived from the seed this run minted"
	}
	if out, err := h.run(h.bin, args...); err != nil {
		h.fail(phase, claim, fmt.Errorf("%w\n%s", err, out))
		return nil
	}
	h.pass(phase, claim, detail)
	return nil
}

// tamperCheck alters one recorded fact in a downloaded receipt and requires verification to
// refuse it. A tamper check that silently changes nothing would report the verifier sound while
// proving nothing, so the edit is asserted before the verdict is.
func (h *harness) tamperCheck(phase, runID string) error {
	original := filepath.Join(h.work, phase+"-"+runID+"-receipt.json")
	raw, err := os.ReadFile(original)
	if err != nil {
		return err
	}
	tampered := strings.Replace(string(raw), `"release-agent"`, `"somebody-else"`, 1)
	if tampered == string(raw) {
		h.fail(phase, "an altered receipt is refused",
			fmt.Errorf("the tamper changed nothing, so this check would have proven nothing"))
		return nil
	}
	path := filepath.Join(h.work, phase+"-"+runID+"-tampered.json")
	if err := os.WriteFile(path, []byte(tampered), 0o644); err != nil {
		return err
	}
	if _, err := h.run(h.bin, "verify", path); err == nil {
		h.fail(phase, "an altered receipt is refused",
			fmt.Errorf("the actor was rewritten and verification still passed"))
		return nil
	}
	h.pass(phase, "an altered receipt is refused",
		"one recorded name rewritten, verification fails")
	return nil
}

// dig walks nested maps by key and renders what it finds, for assertions on decoded JSON.
func dig(doc map[string]any, path ...string) string {
	var cur any = doc
	for _, key := range path {
		m, ok := cur.(map[string]any)
		if !ok {
			return ""
		}
		cur = m[key]
	}
	if cur == nil {
		return ""
	}
	return fmt.Sprintf("%v", cur)
}

// oneLine collapses an error message to its first line, for the report.
func oneLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

// phaseRBAC proves the authorization model on the deployed install by attempting, as each kind of
// principal, the things it must be refused. Every check here is an action a compromised or
// overreaching credential would try, sent over the same HTTP surface a real one would use; a
// refusal that only exists in a unit test protects nobody's cluster.
func (h *harness) phaseRBAC(phase string) {
	// The agent token is capped below identity and access management no matter what account it is
	// bound to. An agent that can mint accounts or tokens can mint its own approver.
	if detail, err := h.mustRefuse(&h.agent, "POST", "/v1/users", map[string]any{
		"username": "sneaky", "password": "x", "role": "admin",
	}); err != nil {
		h.fail(phase, "an agent token cannot create accounts", err)
	} else {
		h.pass(phase, "an agent token cannot create accounts", detail)
	}
	if detail, err := h.mustRefuse(&h.agent, "POST", "/v1/tokens", map[string]any{
		"name": "sneaky", "username": "casey",
	}); err != nil {
		h.fail(phase, "an agent token cannot mint tokens", err)
	} else {
		h.pass(phase, "an agent token cannot mint tokens", detail)
	}
	if detail, err := h.mustRefuse(&h.agent, "POST", "/v1/credentials", map[string]any{
		"name": "sneaky", "kind": "ssh_key", "secret": "not-a-real-key",
	}); err != nil {
		h.fail(phase, "an agent token cannot write secrets", err)
	} else {
		h.pass(phase, "an agent token cannot write secrets", detail)
	}

	// A viewer reads and does nothing else. The account and its token are created here as the
	// admin, which is itself an assertion that the admin can.
	if err := h.apiCall("POST", "/v1/users", &h.human, map[string]any{
		"username": "auditor", "password": "supertest-not-a-secret", "role": "viewer",
	}, nil); err != nil {
		h.fail(phase, "the admin can create a viewer account", err)
		return
	}
	var minted struct {
		Token string `json:"token"`
	}
	if err := h.apiCall("POST", "/v1/tokens", &h.human, map[string]any{
		"name": "supertest-viewer", "username": "auditor",
	}, &minted); err != nil {
		h.fail(phase, "the admin can mint the viewer a token", err)
		return
	}
	viewer := actor{Name: "auditor", Type: "user", Token: minted.Token}
	h.pass(phase, "the admin created a viewer with a token of its own", "")

	var listed map[string]any
	if err := h.apiCall("GET", "/v1/runs", &viewer, nil, &listed); err != nil {
		h.fail(phase, "the viewer can read the run history", err)
	} else {
		h.pass(phase, "the viewer can read the run history", "")
	}
	if detail, err := h.mustRefuse(&viewer, "POST", "/v1/runs", map[string]any{
		"tool": "bash", "command": "id",
	}); err != nil {
		h.fail(phase, "the viewer cannot launch a run", err)
	} else {
		h.pass(phase, "the viewer cannot launch a run", detail)
	}
	if detail, err := h.mustRefuse(&viewer, "POST",
		"/v1/runs/"+h.mustID("team-destroy-run")+"/approve", map[string]any{}); err != nil {
		h.fail(phase, "the viewer cannot approve a held run", err)
	} else {
		h.pass(phase, "the viewer cannot approve a held run", detail)
	}
}
