package main

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"
)

// relayNamespace is where the relay phase installs its own SQLite control node and its workers.
const relayNamespace = "relay"

// relayRunFiles is where a relay worker stages a run's files, the memory-backed volume its pod
// mounts.
const relayRunFiles = "/run/switchtender/runfiles/"

// relayHoldSeconds is how long the orphan play's one task waits before it changes its host. It is
// longer than a lease lifetime plus a janitor tick, so the control node has recorded the run
// interrupted well before the change would land.
const relayHoldSeconds = 60

// The Rego modules and policy files the relay phase moves through. Each is written into the
// control node's volume, where the policy file reloads on its own when it or a module changes.
const (
	// relayDenyModule refuses runs labeled rego-freeze outright.
	relayDenyModule = `package switchtender

deny contains "runs labeled rego-freeze are frozen" if {
	input.run.labels.change == "rego-freeze"
}
`
	// relayHoldModule replaces the refusal with a hold, so the same submission runs once a person
	// agrees.
	relayHoldModule = `package switchtender

hold contains "runs labeled rego-freeze run once a person has looked" if {
	input.run.labels.change == "rego-freeze"
}

deny contains "nothing else is frozen" if {
	input.run.labels.change == "frozen-forever"
}
`
	// relayGuardrails loads one Rego policy, so its bundle is the one thing deciding.
	relayGuardrails = "policies: []\nrego:\n  - name: guardrails\n    files: [rego/guardrails.rego]\n"
	// relayAdviceModule warns about a run with no change ticket.
	relayAdviceModule = `package switchtender

warn contains "the run has no change ticket" if {
	input.run.labels.change == "rego-advice"
	not input.run.labels.ticket
}
`
	// relayAdviceNoted loads the advice with its warnings recorded rather than held.
	relayAdviceNoted = "policies: []\nrego:\n  - name: build-advice\n    files: [rego/advice.rego]\n" +
		"    warn: note\n"
	// relayAdviceHeld loads the same advice at the default, so its warnings hold.
	relayAdviceHeld = "policies: []\nrego:\n  - name: build-advice\n    files: [rego/advice.rego]\n"
	// relayNoPolicies leaves nothing to decide, for the relay runs that follow.
	relayNoPolicies = "policies: []\n"
)

// relayKey is what worker key new prints: everything public about a delivery key.
type relayKey struct {
	// KeyID names the key in the audit chain.
	KeyID string `json:"key_id"`
	// DeliveryKey is the public key the pool file registers.
	DeliveryKey string `json:"delivery_key"`
}

// relayEnv is what the relay phase made and later checks against.
type relayEnv struct {
	// pod is the control node's server pod, rediscovered after every restart.
	pod string
	// service is the control node's Service, which the workers dial.
	service string
	// key is the relay pool's delivery key.
	key relayKey
	// secrets are the values no record may hold, by a label safe to print.
	secrets map[string]string
}

// phaseRelay proves the Team features that live in a control node's policy file and in its relay
// workers, on an install of its own: a Rego refusal and its replacement are both on the record, a
// Rego warning noted on a run is in its receipt and held once the setting changes, a relay worker
// runs with sealed secrets and keeps none of them, and a relay worker killed mid-run stops its
// tool. These were scenarios against processes on one machine. Here the control node is the
// chart's, the workers are pods that hold no database and no encryption key, and the hosts are the
// fleet.
func (h *harness) phaseRelay(license string) error {
	const phase = "relay"
	env := &relayEnv{secrets: map[string]string{}}
	if err := h.installRelay(phase, license, env); err != nil {
		return err
	}
	if err := h.checkRegoRefusalAndReplacement(phase, env); err != nil {
		return err
	}
	if err := h.checkRegoWarningNoted(phase, env); err != nil {
		return err
	}
	err := h.writePodFile(relayNamespace, env.pod, "/data/policy.yaml", relayNoPolicies)
	if err != nil {
		return err
	}
	if err := h.startRelayWorkers(env); err != nil {
		return err
	}
	h.pass(phase, "two relay workers connected with no database and no encryption key",
		"relay-a on queue relay holding the pool's delivery key, bare-a on queue bare holding none")
	if err := h.checkRelaySealedSecrets(phase, env); err != nil {
		return err
	}
	// Last, because it kills relay-a.
	return h.checkRelayWorkerKilledMidRun(phase, env)
}

// installRelay installs a SQLite control node with the license, adopts its first admin token, makes
// the relay pool's delivery key inside it, writes the pool and policy files onto its volume, and
// restarts it reading both. The fleet credential and inventory are created on it last.
func (h *harness) installRelay(phase, license string, env *relayEnv) error {
	if _, err := h.kubectl("create", "namespace", relayNamespace); err != nil {
		return err
	}
	chart := filepath.Join(h.repo, "deploy/helm/switchtender")
	if out, err := h.run("helm", "install", "relay", chart,
		"--namespace", relayNamespace,
		"--kubeconfig", h.kubeconfig,
		"--set", "image.repository=switchtender",
		"--set", "image.tag=supertest",
		"--set", "image.pullPolicy=Never",
		"--set", "encryptionKey=supertest-key-not-a-secret",
		"--set", "encryptionSalt=supertest-salt",
		"--set-file", "license="+license,
		"--wait", "--timeout", "300s"); err != nil {
		return fmt.Errorf("helm install relay: %w\n%s\n%s", err, out, h.installForensics(relayNamespace))
	}
	service, err := h.serviceName(relayNamespace)
	if err != nil {
		return err
	}
	env.service = service
	if err := h.forwardTo(relayNamespace, service); err != nil {
		return err
	}
	if err := h.bootstrap(relayNamespace); err != nil {
		return err
	}
	if env.pod, err = h.serverPod(relayNamespace); err != nil {
		return err
	}

	// The key is made the way an operator makes it, then carried out to the workers and removed:
	// the private half belongs on the pool's workers and nowhere else.
	out, err := h.runOut("kubectl", "exec", "-n", relayNamespace, env.pod, "--",
		"switchtender", "worker", "key", "new", "--out", "/data/delivery.key")
	if err != nil {
		return fmt.Errorf("make the delivery key: %w", err)
	}
	if env.key, err = parseRelayKey(out); err != nil {
		return err
	}
	keyPEM, err := h.runOut("kubectl", "exec", "-n", relayNamespace, env.pod, "--",
		"cat", "/data/delivery.key")
	if err != nil {
		return fmt.Errorf("read the delivery key: %w", err)
	}
	if _, err := h.kubectl("exec", "-n", relayNamespace, env.pod, "--",
		"rm", "-f", "/data/delivery.key"); err != nil {
		return fmt.Errorf("remove the delivery key from the control node: %w", err)
	}
	for i, line := range pemBodyLines(keyPEM) {
		env.secrets[fmt.Sprintf("delivery key line %d", i+1)] = line
	}
	keyFile := filepath.Join(h.work, "delivery.key")
	if err := os.WriteFile(keyFile, []byte(keyPEM), 0o600); err != nil {
		return err
	}

	relayToken, bareToken := "relay-pool-"+randomHex(16), "bare-pool-"+randomHex(16)
	env.secrets["the relay pool token"] = relayToken
	env.secrets["the bare pool token"] = bareToken
	// The tokens travel by file, never on an argv: a failed kubectl prints its arguments into the
	// report.
	tokenFiles := map[string]string{"relay": relayToken, "bare": bareToken}
	args := []string{"create", "secret", "generic", "relay-pool-tokens", "-n", relayNamespace}
	for pool, token := range tokenFiles {
		path := filepath.Join(h.work, "pool-token-"+pool)
		if err := os.WriteFile(path, []byte(token), 0o600); err != nil {
			return err
		}
		args = append(args, "--from-file="+pool+"="+path)
	}
	if _, err := h.kubectl(args...); err != nil {
		return err
	}
	if _, err := h.kubectl("create", "secret", "generic", "relay-delivery-key", "-n", relayNamespace,
		"--from-file=delivery.key="+keyFile); err != nil {
		return err
	}

	if _, err := h.kubectl("exec", "-n", relayNamespace, env.pod, "--",
		"mkdir", "-p", "/data/rego", "/data/relay"); err != nil {
		return err
	}
	pools := "workers:\n" +
		"  - name: relay\n    token_sha256: " + sha256Hex(relayToken) + "\n    queues: [relay]\n" +
		"    delivery_key: " + env.key.DeliveryKey + "\n" +
		"  - name: bare\n    token_sha256: " + sha256Hex(bareToken) + "\n    queues: [bare]\n"
	for path, content := range map[string]string{
		"/data/workers.yaml":         pools,
		"/data/rego/guardrails.rego": relayDenyModule,
		"/data/policy.yaml":          relayGuardrails,
		"/data/relay/rotate.yml":     mustManifest("relay-rotate.yml"),
		"/data/relay/hold.yml":       mustManifest("relay-hold.yml"),
	} {
		if err := h.writePodFile(relayNamespace, env.pod, path, content); err != nil {
			return err
		}
	}

	// SQLite runs Recreate, so the volume, the database, and the files written above survive the
	// restart, and the tokens bootstrap minted keep working.
	if out, err := h.run("helm", "upgrade", "relay", chart,
		"--namespace", relayNamespace,
		"--kubeconfig", h.kubeconfig,
		"--reuse-values",
		"--set", "server.extraArgs={--worker-pools,/data/workers.yaml,--policy-file,/data/policy.yaml}",
		"--wait", "--timeout", "300s"); err != nil {
		return fmt.Errorf("helm upgrade relay: %w\n%s\n%s", err, out, h.installForensics(relayNamespace))
	}
	if err := h.waitFor("the restarted control node to be the only server pod", 120*time.Second,
		func() error {
			pods, err := h.kubectl("get", "pod", "-n", relayNamespace,
				"-l", "app.kubernetes.io/component=server", "-o", "jsonpath={.items[*].metadata.name}")
			if err != nil {
				return err
			}
			names := strings.Fields(pods)
			if len(names) != 1 || names[0] == env.pod {
				return fmt.Errorf("server pods are %q", pods)
			}
			env.pod = names[0]
			return nil
		}); err != nil {
		return err
	}
	if err := h.forwardTo(relayNamespace, service); err != nil {
		return err
	}
	digest, err := h.regoDigest("guardrails")
	if err != nil {
		return fmt.Errorf("the restarted control node does not read its policy file: %w", err)
	}
	h.pass(phase, "a control node reads its worker pools and its Rego policy from files",
		"--worker-pools and --policy-file on the chart's SQLite server; guardrails bundle sha256:"+
			digest[:12])

	if _, err := h.deployAcrossFleet(relayNamespace, "relay-"+randomHex(4), nil); err != nil {
		return fmt.Errorf("prepare the fleet credential and inventory: %w", err)
	}
	return nil
}

// checkRegoRefusalAndReplacement is the Rego refusal scenario. The module refuses a labeled run:
// nothing may be created, and the refusal has to carry the bundle that made it, in the answer and
// in the chain. The module is then edited to hold instead, without a restart, and the same
// submission, approved, has to run with evidence naming the new bundle and never the old one.
func (h *harness) checkRegoRefusalAndReplacement(phase string, env *relayEnv) error {
	first, err := h.regoDigest("guardrails")
	if err != nil {
		return err
	}
	before, err := h.runTotal()
	if err != nil {
		return err
	}
	submit := map[string]any{"tool": "bash", "command": "echo frozen",
		"labels": map[string]string{"change": "rego-freeze"}}

	const refused = "a Rego deny refuses the submission and names its bundle"
	err = h.apiCall("POST", "/v1/runs", &h.agent, submit, nil)
	var api *apiError
	switch {
	case err == nil:
		h.fail(phase, refused, fmt.Errorf("the frozen run was accepted"))
	case !errors.As(err, &api) || api.Status != http.StatusForbidden:
		h.fail(phase, refused, fmt.Errorf("want 403: %w", err))
	default:
		var missing []string
		for _, want := range []string{"guardrails", "runs labeled rego-freeze are frozen",
			"rego sha256:" + first[:12]} {
			if !strings.Contains(api.Body, want) {
				missing = append(missing, want)
			}
		}
		if len(missing) > 0 {
			h.fail(phase, refused, fmt.Errorf("the refusal %q does not name %q", api.Body, missing))
		} else {
			h.pass(phase, refused, "403 naming guardrails, the deny text, and rego sha256:"+first[:12])
		}
	}
	const nothing = "a refused submission creates no run"
	if after, err := h.runTotal(); err != nil {
		h.fail(phase, nothing, err)
	} else if after != before {
		h.fail(phase, nothing, fmt.Errorf("runs went from %d to %d", before, after))
	} else {
		h.pass(phase, nothing, fmt.Sprintf("%d runs before and after", after))
	}
	h.checkRefusalRecorded(phase,
		"the chain records the refusal, the policy, and the bundle that decided", "guardrails", first)

	if err := h.writePodFile(relayNamespace, env.pod, "/data/rego/guardrails.rego",
		relayHoldModule); err != nil {
		return err
	}
	var second string
	if err := h.waitFor("the edited module to reload", 60*time.Second, func() error {
		var derr error
		if second, derr = h.regoDigest("guardrails"); derr != nil {
			return derr
		}
		if second == first {
			return fmt.Errorf("the bundle is still sha256:%s", first[:12])
		}
		return nil
	}); err != nil {
		h.fail(phase, "an edited module reloads without a restart", err)
		return nil
	}
	h.pass(phase, "an edited module reloads without a restart",
		"bundle sha256:"+first[:12]+" became sha256:"+second[:12])

	var created map[string]any
	if err := h.apiCall("POST", "/v1/runs", &h.agent, submit, &created); err != nil {
		return fmt.Errorf("submit under the replaced module: %w", err)
	}
	id, _ := created["id"].(string)
	held, err := h.awaitRunStatus(id, "pending_approval", 60*time.Second)
	const holds = "the replacement holds the same submission and names the new bundle"
	if err != nil {
		h.fail(phase, holds, err)
		return nil
	}
	if by := dig(held, "held_by_policy"); !strings.Contains(by, "rego sha256:"+second[:12]) {
		h.fail(phase, holds, fmt.Errorf("held by %q", by))
	} else {
		h.pass(phase, holds, "run "+id+" held by "+by)
	}
	err = h.apiCall("POST", "/v1/runs/"+id+"/approve", &h.human, map[string]any{}, nil)
	if err != nil {
		return fmt.Errorf("approve the held run: %w", err)
	}
	if _, err := h.awaitRunStatus(id, "succeeded", 120*time.Second); err != nil {
		h.fail(phase, "the approved run executed", err)
		return nil
	}
	h.pass(phase, "the approved run executed", "run "+id)

	const evidence = "the receipt names the new bundle and the outcome never names the old one"
	rules, body, err := h.receiptOutcome(id)
	want := "guardrails: decided by Rego package data.switchtender, bundle sha256:" + second
	switch {
	case err != nil:
		h.fail(phase, evidence, err)
	case !anyContains(rules, want):
		h.fail(phase, evidence, fmt.Errorf("the recorded rules %q name no %q", rules, want))
	case strings.Contains(body, first):
		h.fail(phase, evidence, fmt.Errorf("the outcome still names the replaced bundle %s", first))
	default:
		h.pass(phase, evidence, "rules: "+strings.Join(rules, "; "))
	}
	if err := h.verifyReceiptOffline(phase, id, ""); err != nil {
		return err
	}
	// Asked again after the replacement: the record of the refusal is history, and editing the
	// module that made it must leave it exactly as it was.
	h.checkRefusalRecorded(phase, "the refusal's record outlives the module that made it",
		"guardrails", first)
	return nil
}

// checkRefusalRecorded requires the chain to hold exactly one refusal, by the policy engine on
// behalf of the agent's credential that asked, naming the policy and the full digest of the bundle that decided.
func (h *harness) checkRefusalRecorded(phase, claim, policyName, bundle string) {
	entries, err := h.auditEntries()
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	var refusals []auditEntry
	for _, e := range entries {
		if e.Method == "DECISION" && strings.Contains(e.Path, "/decision/refused") {
			refusals = append(refusals, e)
		}
	}
	if len(refusals) != 1 {
		h.fail(phase, claim, fmt.Errorf("the chain holds %d refusals, want the one the policy made",
			len(refusals)))
		return
	}
	r := refusals[0]
	suffix := "/decision/refused/rego/sha256:" + bundle + "/policy/" + policyName
	switch {
	case !strings.HasSuffix(r.Path, suffix):
		h.fail(phase, claim, fmt.Errorf("the refusal is recorded at %s, want a path ending %s",
			r.Path, suffix))
	case r.Actor != "system:policy" || r.ActorType != "system" || r.OnBehalfOf != agentTokenName:
		h.fail(phase, claim, fmt.Errorf("recorded as %s (%s) on behalf of %q, want system:policy "+
			"(system) on behalf of %s", r.Actor, r.ActorType, r.OnBehalfOf, agentTokenName))
	default:
		h.pass(phase, claim, "one DECISION at "+r.Path+" by system:policy on behalf of "+r.OnBehalfOf)
	}
}

// checkRegoWarningNoted is the Rego warning scenario. A run under a policy whose warning is a note
// has to execute with no approval, carry the note, and have a receipt whose rule set says the
// warnings were noted. The file is then edited to the default, without a restart, and the same
// submission has to wait for a person, held by the same warning, with no note.
func (h *harness) checkRegoWarningNoted(phase string, env *relayEnv) error {
	if err := h.writePodFile(relayNamespace, env.pod, "/data/rego/advice.rego",
		relayAdviceModule); err != nil {
		return err
	}
	if err := h.writePodFile(relayNamespace, env.pod, "/data/policy.yaml",
		relayAdviceNoted); err != nil {
		return err
	}
	var bundle string
	if err := h.waitFor("the advice policy to load", 60*time.Second, func() error {
		var derr error
		bundle, derr = h.regoDigest("build-advice")
		return derr
	}); err != nil {
		return err
	}
	note := "build-advice (the run has no change ticket, rego sha256:" + bundle[:12] + ")"
	rule := "build-advice: decided by Rego package data.switchtender, bundle sha256:" + bundle
	// Submitted by the person, never the agent: an agent's run is held by default, which would
	// hold the noted run for a reason that is not the warning under test.
	submit := map[string]any{"tool": "bash", "command": "echo advised",
		"labels": map[string]string{"change": "rego-advice"}}

	var created map[string]any
	if err := h.apiCall("POST", "/v1/runs", &h.human, submit, &created); err != nil {
		return fmt.Errorf("submit under the noted advice: %w", err)
	}
	noted, _ := created["id"].(string)
	const runs = "a noted Rego warning lets the run go ahead and records the note on it"
	done, err := h.awaitRunTerminal(noted, 120*time.Second)
	switch notes := stringsOf(done["policy_notes"]); {
	case err != nil:
		h.fail(phase, runs, err)
	case dig(done, "status") != "succeeded":
		h.fail(phase, runs, fmt.Errorf("the run reached %s, want succeeded with no approval",
			dig(done, "status")))
	case len(notes) != 1 || notes[0] != note:
		h.fail(phase, runs, fmt.Errorf("policy_notes are %q, want [%s]", notes, note))
	default:
		h.pass(phase, runs, "run "+noted+" succeeded unheld, noting "+note)
	}
	const noteReceipt = "the noted run's receipt says its warnings were noted without holding"
	rules, body, err := h.receiptOutcome(noted)
	switch {
	case err != nil:
		h.fail(phase, noteReceipt, err)
	case !slices.Contains(rules, rule+", warnings noted without holding"):
		h.fail(phase, noteReceipt, fmt.Errorf("the recorded rules are %q", rules))
	case !strings.Contains(body, note):
		h.fail(phase, noteReceipt, fmt.Errorf("the outcome does not commit the note"))
	default:
		h.pass(phase, noteReceipt, "rules: "+strings.Join(rules, "; "))
	}

	if err := h.writePodFile(relayNamespace, env.pod, "/data/policy.yaml",
		relayAdviceHeld); err != nil {
		return err
	}
	// The digest covers the modules alone, so the reload cannot be seen there. The submission is
	// retried until the edited file is read, which is when the same warning begins to hold.
	const heldClaim = "at the default the same warning holds the run and notes nothing"
	var heldID string
	var waiting map[string]any
	err = h.waitFor("the default warn setting to hold a run", 90*time.Second, func() error {
		var c map[string]any
		if err := h.apiCall("POST", "/v1/runs", &h.human, submit, &c); err != nil {
			return err
		}
		id, _ := c["id"].(string)
		doc, err := h.awaitRunTerminalOr(id, "pending_approval", 60*time.Second)
		if err != nil {
			return err
		}
		if dig(doc, "status") != "pending_approval" {
			return fmt.Errorf("run %s reached %s, so the edited file was not read yet", id,
				dig(doc, "status"))
		}
		heldID, waiting = id, doc
		return nil
	})
	if err != nil {
		h.fail(phase, heldClaim, err)
		return nil
	}
	if again, err := h.regoDigest("build-advice"); err != nil || again != bundle {
		h.fail(phase, "changing only the warn setting leaves the bundle digest alone",
			fmt.Errorf("the digest is now %q (%v), want %s", again, err, bundle))
	} else {
		h.pass(phase, "changing only the warn setting leaves the bundle digest alone",
			"sha256:"+bundle[:12])
	}
	switch by, notes := dig(waiting, "held_by_policy"), stringsOf(waiting["policy_notes"]); {
	case by != note:
		h.fail(phase, heldClaim, fmt.Errorf("held by %q, want %q", by, note))
	case len(notes) != 0:
		h.fail(phase, heldClaim, fmt.Errorf("a held warning was also noted: %q", notes))
	default:
		h.pass(phase, heldClaim, "run "+heldID+" held by "+by)
	}
	if err := h.apiCall("POST", "/v1/runs/"+heldID+"/approve", &h.human, map[string]any{},
		nil); err != nil {
		return fmt.Errorf("approve the held run: %w", err)
	}
	if _, err := h.awaitRunStatus(heldID, "succeeded", 120*time.Second); err != nil {
		h.fail(phase, "the approved advice run executed", err)
		return nil
	}
	const heldReceipt = "the held run's receipt no longer reads as noted"
	rules, body, err = h.receiptOutcome(heldID)
	switch {
	case err != nil:
		h.fail(phase, heldReceipt, err)
	case !slices.Contains(rules, rule):
		h.fail(phase, heldReceipt, fmt.Errorf("the recorded rules are %q, want %q among them",
			rules, rule))
	case strings.Contains(body, "policy_notes") || strings.Contains(body, "warnings noted"):
		h.fail(phase, heldReceipt, fmt.Errorf("the outcome still reads as noted"))
	default:
		h.pass(phase, heldReceipt, "rules: "+strings.Join(rules, "; "))
	}
	return nil
}

// startRelayWorkers starts the two relay workers as pods that hold no database and no encryption
// key: relay-a in the pool that registered the delivery key, holding its private half, and bare-a
// in the pool that registered none. Each waits until its worker reports it started, which is after
// it dialed the control node.
func (h *harness) startRelayWorkers(env *relayEnv) error {
	rotate := filepath.Join(h.work, "relay-rotate.yml")
	hold := filepath.Join(h.work, "relay-hold.yml")
	for path, name := range map[string]string{rotate: "relay-rotate.yml", hold: "relay-hold.yml"} {
		if err := os.WriteFile(path, []byte(mustManifest(name)), 0o644); err != nil {
			return err
		}
	}
	if _, err := h.kubectl("create", "configmap", "relay-playbooks", "-n", relayNamespace,
		"--from-file=rotate.yml="+rotate, "--from-file=hold.yml="+hold); err != nil {
		return err
	}
	server := "http://" + env.service + ":8080"
	for _, w := range []struct {
		Name, Pool, KeySecret string
	}{
		{Name: "relay-a", Pool: "relay", KeySecret: "relay-delivery-key"},
		{Name: "bare-a", Pool: "bare", KeySecret: "relay-no-delivery-key"},
	} {
		script, err := json.Marshal(relayWorkerScript(server, w.Name, w.Pool, w.Pool == "relay"))
		if err != nil {
			return err
		}
		manifest := strings.NewReplacer(
			"__NAME__", w.Name,
			"__SCRIPT__", string(script),
			"__POOL__", w.Pool,
			"__LICENSE_SECRET__", "relay-switchtender-secret",
			"__KEY_SECRET__", w.KeySecret,
		).Replace(mustManifest("relay-worker.yaml"))
		if err := h.apply(manifest); err != nil {
			return err
		}
	}
	for _, name := range []string{"relay-a", "bare-a"} {
		if err := h.awaitRelayWorker(name); err != nil {
			return err
		}
	}
	return nil
}

// awaitRelayWorker waits until a relay worker pod's worker reports it started, which is after it
// dialed the control node, and gives up at once on a worker that exited, whose log says why.
func (h *harness) awaitRelayWorker(name string) error {
	deadline := time.Now().Add(120 * time.Second)
	var last string
	for time.Now().Before(deadline) {
		logs, err := h.kubectl("logs", "-n", relayNamespace, name)
		switch {
		case err != nil:
			last = oneLine(err.Error())
		case strings.Contains(logs, "the worker process exited"):
			return fmt.Errorf("relay worker %s exited at once:\n%s", name, logs)
		case strings.Contains(logs, "switchtender worker started"):
			return nil
		default:
			last = "not started yet"
		}
		time.Sleep(2 * time.Second)
	}
	return fmt.Errorf("gave up waiting for relay worker %s to start after 2m: %s\n%s", name, last,
		h.installForensics(relayNamespace))
}

// relayWorkerScript is the shell a relay worker pod runs. The worker runs as a child of the pod's
// shell, never as its first process, so killing the worker ends the worker and nothing else: with
// the worker as the first process, its death would tear down the pod's process namespace and take
// the tool with it, and the killed-mid-run check would prove nothing. Its pid is kept for that
// check, and the shell stays up afterward so the pod can still be read.
func relayWorkerScript(server, name, queue string, withKey bool) string {
	var b strings.Builder
	keyFlag := ""
	if withKey {
		b.WriteString("cp /keysrc/delivery.key /run/switchtender/delivery.key && " +
			"chmod 600 /run/switchtender/delivery.key || exit 1\n")
		keyFlag = " --delivery-key /run/switchtender/delivery.key"
	}
	fmt.Fprintf(&b, "switchtender worker --server %s --name %s --queue %s%s &\n",
		server, name, queue, keyFlag)
	b.WriteString("echo $! > /run/switchtender/worker.pid\n" +
		"wait $!\n" +
		"echo \"the worker process exited: $?\"\n" +
		"while true; do sleep 3600; done\n")
	return b.String()
}

// The claims the sealed delivery scenario makes about the relay run and what followed it, named
// once so a run that never happened as asked records each one as not reached.
const (
	claimReceived  = "the play received exactly the launch's answer and the credential's key"
	claimOverSSH   = "the delivered key authenticated a real SSH session"
	claimKeyGone   = "the key's file was staged under the worker's run files and is gone"
	claimMasked    = "the run log prints the task and masks the answer"
	claimBarePool  = "a pool that registered no delivery key fails the run with the reason"
	claimDelivered = "the chain records one delivery, to the relay pool's worker, by name"
)

// relayRunClaims are the claims recorded as not reached when the relay run fails.
var relayRunClaims = []string{claimReceived, claimOverSSH, claimKeyGone, claimMasked, claimBarePool,
	claimDelivered}

// checkRelaySealedSecrets is the sealed delivery scenario. A template on the relay queue needs the
// fleet's SSH key and a secret survey answer. The relay worker receives both sealed, runs the play
// across the fleet with them over real SSH, and afterward neither is in any log, API answer,
// export, or the database, the key's file is gone from the worker, and the chain records which pool
// and worker received which credential. A pool that registered no key fails its run with the
// reason.
func (h *harness) checkRelaySealedSecrets(phase string, env *relayEnv) error {
	cred := h.mustID(relayNamespace + "-credential")
	inv := h.mustID(relayNamespace + "-inventory")
	fleetKey, err := os.ReadFile(filepath.Join(h.work, "fleet_ed25519"))
	if err != nil {
		return err
	}
	// The first body line of an OpenSSH key is a header every key of its type shares, so it is not
	// a secret of this key, and listing it would flag any other key as this one.
	for i, line := range pemBodyLines(string(fleetKey)) {
		if i > 0 {
			env.secrets[fmt.Sprintf("fleet key line %d", i+1)] = line
		}
	}
	templates := map[string]string{}
	for name, queue := range map[string]string{"relay rotate": "relay", "bare rotate": "bare"} {
		var t map[string]any
		if err := h.apiCall("POST", "/v1/templates", &h.human, map[string]any{
			"name": name, "playbook": "/data/relay/rotate.yml", "inventory_id": inv,
			"queue": queue, "credential_ids": []string{cred},
			"extra_vars": map[string]any{"marker_dir": "/tmp"},
			"survey": []map[string]any{{"var": "db_password", "label": "Database password",
				"type": "secret", "required": true}},
		}, &t); err != nil {
			return fmt.Errorf("create template %s: %w", name, err)
		}
		id, _ := t["id"].(string)
		if id == "" {
			return fmt.Errorf("%w: template %s", ErrNoID, name)
		}
		templates[name] = id
	}

	answer := "relay-answer-" + randomHex(12)
	env.secrets["the survey answer"] = answer
	launch := map[string]any{"answers": map[string]any{"db_password": answer}}
	relayID, err := h.launchTemplate(templates["relay rotate"], launch)
	if err != nil {
		return err
	}
	const ran = "the relay worker ran the play with the sealed key and answer"
	done, err := h.awaitRunTerminal(relayID, 240*time.Second)
	if err == nil && (dig(done, "status") != "succeeded" || dig(done, "claimed_by") != "relay-a") {
		err = fmt.Errorf("the run reached %s on %q, want succeeded on relay-a: %s\n%s",
			dig(done, "status"), dig(done, "claimed_by"), dig(done, "error"), h.runEventTail(relayID))
	}
	if err != nil {
		h.fail(phase, ran, err)
		// The claims about what the play received are about a run that did not happen as asked,
		// so they are recorded as not reached rather than left out of the report. The leak scan
		// still runs: a failed run is exactly when a secret is most likely to land in a log.
		for _, claim := range relayRunClaims {
			h.fail(phase, claim, fmt.Errorf("not reached: %s failed", ran))
		}
		h.checkRelayKeepsNoSecret(phase, env, relayID)
		return nil
	}
	h.pass(phase, ran, "run "+relayID+" succeeded, claimed by relay-a")

	marker, err := h.fleetExec("fleet-0", "cat", "/tmp/relay-"+fleetHosts[0].FQDN)
	marks := strings.Split(marker, "\n")
	const received = claimReceived
	switch {
	case err != nil:
		h.fail(phase, received, err)
	case len(marks) < 4:
		h.fail(phase, received, fmt.Errorf("the marker holds %d lines, want 4", len(marks)))
	case marks[0] != sha256Hex(answer):
		h.fail(phase, received, fmt.Errorf("the answer's digest is %s, not the launch's", marks[0]))
	case marks[1] != sha256Hex(string(fleetKey)):
		h.fail(phase, received, fmt.Errorf("the key's digest is %s, not the credential's", marks[1]))
	default:
		h.pass(phase, received, "digests on fleet-0 match, read via kubectl exec")
	}
	if len(marks) >= 4 {
		const overSSH = claimOverSSH
		if strings.TrimSpace(marks[3]) == "" {
			h.fail(phase, overSSH, fmt.Errorf("SSH_CONNECTION was empty on fleet-0"))
		} else {
			h.pass(phase, overSSH, "SSH_CONNECTION on fleet-0: "+marks[3])
		}
		const gone = claimKeyGone
		keyPath := strings.TrimSpace(marks[2])
		if !strings.HasPrefix(keyPath, relayRunFiles) {
			h.fail(phase, gone, fmt.Errorf("the key was written to %s, outside %s", keyPath,
				relayRunFiles))
		} else if _, err := h.kubectl("exec", "-n", relayNamespace, "relay-a", "--",
			"test", "!", "-e", keyPath); err != nil {
			h.fail(phase, gone, fmt.Errorf("%s still exists in relay-a", keyPath))
		} else {
			h.pass(phase, gone, keyPath+" no longer exists in relay-a")
		}
	}
	logText, err := h.apiText("/v1/runs/" + relayID + "/logs")
	const masked = claimMasked
	switch {
	case err != nil:
		h.fail(phase, masked, err)
	case !strings.Contains(logText, "rotating to"):
		h.fail(phase, masked, fmt.Errorf("the log lacks the task printing the answer, so masking "+
			"was not exercised"))
	case strings.Contains(logText, answer):
		h.fail(phase, masked, fmt.Errorf("the log carries the answer"))
	default:
		h.pass(phase, masked, "\"rotating to\" is in the log, the answer is not")
	}

	bareID, err := h.launchTemplate(templates["bare rotate"], launch)
	if err != nil {
		return err
	}
	const bare = claimBarePool
	refused, err := h.awaitRunTerminal(bareID, 180*time.Second)
	switch reason := dig(refused, "error"); {
	case err != nil:
		h.fail(phase, bare, err)
	case dig(refused, "status") != "failed" ||
		!strings.Contains(reason, `worker pool "bare" has registered no delivery key`):
		h.fail(phase, bare, fmt.Errorf("the run reached %s (%s)", dig(refused, "status"), reason))
	default:
		h.pass(phase, bare, "run "+bareID+" failed: "+oneLine(reason))
	}

	const recorded = claimDelivered
	want := "/relay/delivered/" + url.PathEscape(relayID) + "/key/" + url.PathEscape(env.key.KeyID) +
		"/credentials/" + url.PathEscape(cred) + "/answers/db_password"
	entries, err := h.auditEntries()
	var delivered []auditEntry
	for _, e := range entries {
		if strings.Contains(e.Path, "/relay/delivered/") {
			delivered = append(delivered, e)
		}
	}
	switch {
	case err != nil:
		h.fail(phase, recorded, err)
	case len(delivered) != 1:
		h.fail(phase, recorded, fmt.Errorf("the chain holds %d deliveries, want 1", len(delivered)))
	case delivered[0].Path != want || delivered[0].Actor != "pool:relay worker:relay-a":
		h.fail(phase, recorded, fmt.Errorf("recorded %s by %s, want %s by pool:relay worker:relay-a",
			delivered[0].Path, delivered[0].Actor, want))
	default:
		h.pass(phase, recorded, want+" by "+delivered[0].Actor)
	}

	h.checkRelayKeepsNoSecret(phase, env, relayID, bareID)
	return nil
}

// checkRelayKeepsNoSecret reads every place a delivered secret could have landed and requires none
// of them to hold any: the control node's and both workers' logs, the two runs and their logs, the
// audit listing and the exported chain, and the control node's database file.
func (h *harness) checkRelayKeepsNoSecret(phase string, env *relayEnv, runIDs ...string) {
	const claim = "no log, answer, export, or database byte holds a delivered secret"
	places := map[string]string{}
	for _, pod := range []string{env.pod, "relay-a", "bare-a"} {
		logs, err := h.kubectl("logs", "-n", relayNamespace, pod)
		if err != nil {
			h.fail(phase, claim, fmt.Errorf("read the log of %s: %w", pod, err))
			return
		}
		places["the log of "+pod] = logs
	}
	paths := []string{"/v1/audit?limit=1000", "/v1/audit/bundle"}
	for _, id := range runIDs {
		paths = append(paths, "/v1/runs/"+id, "/v1/runs/"+id+"/logs", "/v1/runs/"+id+"/events")
	}
	for _, path := range paths {
		text, err := h.apiText(path)
		if err != nil {
			h.fail(phase, claim, err)
			return
		}
		places[path] = text
	}
	if len(env.secrets) == 0 {
		h.fail(phase, claim, fmt.Errorf("%w: no secret was listed to look for", ErrNothingRead))
		return
	}
	var found []string
	for label, secret := range env.secrets {
		for where, text := range places {
			if strings.Contains(text, secret) {
				found = append(found, label+" in "+where)
			}
		}
		// The needle travels as the script's first argument, never inside the script text, and
		// grep's count is printed whether or not anything matched.
		out, err := h.runOut("kubectl", "exec", "-n", relayNamespace, env.pod, "--", "sh", "-c",
			`cat /data/switchtender.db* | grep -c -F -e "$0" || true`, secret)
		if err != nil {
			h.fail(phase, claim, fmt.Errorf("scan the database for %s: %w", label, err))
			return
		}
		if n, perr := strconv.Atoi(strings.TrimSpace(out)); perr != nil {
			h.fail(phase, claim, fmt.Errorf("scan the database for %s: read %q", label, out))
			return
		} else if n > 0 {
			found = append(found, label+" in the database file")
		}
	}
	if len(found) > 0 {
		h.fail(phase, claim, fmt.Errorf("found %s", strings.Join(found, ", ")))
		return
	}
	h.pass(phase, claim, fmt.Sprintf("%d secrets looked for in %d places and the database file",
		len(env.secrets), len(places)))
}

// checkRelayWorkerKilledMidRun kills relay-a outright while its play is in the middle of a task,
// the way an out-of-memory kill ends one, and watches the host. The control node has to mark the
// run interrupted, and the tool the dead worker started must not go on to change the host after
// the record says the run stopped: a person who reruns the interrupted run, as the reliability
// page says to, would otherwise have two plays changing the same hosts at once.
func (h *harness) checkRelayWorkerKilledMidRun(phase string, env *relayEnv) error {
	var inv map[string]any
	if err := h.apiCall("POST", "/v1/inventories", &h.human, map[string]any{
		"name": "orphan", "content": "[fleet]\norphanhost ansible_connection=local " +
			"ansible_python_interpreter=/usr/bin/python3\n",
	}, &inv); err != nil {
		return fmt.Errorf("create the orphan inventory: %w", err)
	}
	invID, _ := inv["id"].(string)
	var t map[string]any
	if err := h.apiCall("POST", "/v1/templates", &h.human, map[string]any{
		"name": "orphan hold", "playbook": "/data/relay/hold.yml", "inventory_id": invID,
		"queue":      "relay",
		"extra_vars": map[string]any{"marker_dir": "/tmp", "hold_seconds": relayHoldSeconds},
	}, &t); err != nil {
		return fmt.Errorf("create the orphan template: %w", err)
	}
	tid, _ := t["id"].(string)
	id, err := h.launchTemplate(tid, nil)
	if err != nil {
		return err
	}
	if err := h.waitFor("the orphan play to begin its task", 120*time.Second, func() error {
		_, err := h.kubectl("exec", "-n", relayNamespace, "relay-a", "--",
			"cat", "/tmp/orphan-start-orphanhost")
		return err
	}); err != nil {
		h.fail(phase, "the killed worker's run is recorded interrupted", err)
		return nil
	}
	started := time.Now()
	if _, err := h.kubectl("exec", "-n", relayNamespace, "relay-a", "--", "sh", "-c",
		`kill -9 "$(cat /run/switchtender/worker.pid)"`); err != nil {
		return fmt.Errorf("kill the relay worker: %w", err)
	}
	h.pass(phase, "the relay worker was killed mid-task",
		"SIGKILL to relay-a's worker process, run "+id)

	const interrupted = "the killed worker's run is recorded interrupted"
	if _, err := h.awaitRunStatus(id, "interrupted", relayHoldSeconds*time.Second); err != nil {
		h.fail(phase, interrupted, err)
		return nil
	}
	recorded := time.Since(started).Round(time.Second)
	h.pass(phase, interrupted, fmt.Sprintf("interrupted %s after the kill, before the task's "+
		"%ds wait ended", recorded, relayHoldSeconds))

	const stopped = "the tool the dead worker started does not change the host afterward"
	deadline := started.Add(relayHoldSeconds*time.Second + 20*time.Second)
	for time.Now().Before(deadline) {
		out, err := h.kubectl("exec", "-n", relayNamespace, "relay-a", "--",
			"cat", "/tmp/orphan-after-orphanhost")
		if err == nil {
			h.fail(phase, stopped, fmt.Errorf("the run was recorded interrupted %s after the kill, "+
				"and the play changed the host anyway at %s", recorded, strings.TrimSpace(out)))
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	h.pass(phase, stopped, fmt.Sprintf("no change on the host %ds after the task began",
		relayHoldSeconds+20))
	return nil
}

// launchTemplate launches a template as the human and returns the new run's id.
func (h *harness) launchTemplate(id string, body map[string]any) (string, error) {
	if body == nil {
		body = map[string]any{}
	}
	var created map[string]any
	if err := h.apiCall("POST", "/v1/templates/"+id+"/launch", &h.human, body, &created); err != nil {
		return "", fmt.Errorf("launch template %s: %w", id, err)
	}
	runID, _ := created["id"].(string)
	if runID == "" {
		return "", fmt.Errorf("%w: the launch of template %s", ErrNoID, id)
	}
	return runID, nil
}

// awaitRunTerminalOr polls a run until it settles or reaches the extra status, whichever is first.
func (h *harness) awaitRunTerminalOr(id, extra string, limit time.Duration) (map[string]any, error) {
	var last map[string]any
	err := h.waitFor(fmt.Sprintf("run %s to settle or reach %s", id, extra), limit, func() error {
		var doc map[string]any
		if err := h.apiCall("GET", "/v1/runs/"+id, &h.human, nil, &doc); err != nil {
			return err
		}
		last = doc
		switch status := dig(doc, "status"); status {
		case extra, "succeeded", "failed", "canceled", "rejected", "interrupted":
			return nil
		default:
			return fmt.Errorf("run is %s", status)
		}
	})
	return last, err
}

// regoDigest returns the bundle digest the install reports for the named Rego policy.
func (h *harness) regoDigest(name string) (string, error) {
	var page struct {
		Policies []struct {
			Name string `json:"name"`
			Rego *struct {
				SHA256 string `json:"sha256"`
			} `json:"rego"`
		} `json:"policies"`
	}
	if err := h.apiCall("GET", "/v1/policies", &h.human, nil, &page); err != nil {
		return "", err
	}
	for _, p := range page.Policies {
		if p.Name == name && p.Rego != nil && len(p.Rego.SHA256) == 64 {
			return p.Rego.SHA256, nil
		}
	}
	return "", fmt.Errorf("no Rego policy %q is in force", name)
}

// runTotal returns how many top-level runs the install holds.
func (h *harness) runTotal() (int, error) {
	var page struct {
		Summary struct {
			Total int `json:"total"`
		} `json:"summary"`
	}
	if err := h.apiCall("GET", "/v1/runs?limit=1", &h.human, nil, &page); err != nil {
		return 0, err
	}
	return page.Summary.Total, nil
}

// auditEntry is one entry of the audit listing, as the checks read it.
type auditEntry struct {
	// Actor is who the entry names.
	Actor string `json:"actor"`
	// ActorType is user, agent, or system.
	ActorType string `json:"actor_type"`
	// OnBehalfOf is who a system decision was made for.
	OnBehalfOf string `json:"on_behalf_of"`
	// Method is the entry's verb, such as DECISION.
	Method string `json:"method"`
	// Path is what the entry is about.
	Path string `json:"path"`
}

// auditEntries returns the install's audit listing.
func (h *harness) auditEntries() ([]auditEntry, error) {
	var listing struct {
		Entries []auditEntry `json:"entries"`
	}
	if err := h.apiCall("GET", "/v1/audit?limit=1000", &h.human, nil, &listing); err != nil {
		return nil, err
	}
	if len(listing.Entries) == 0 {
		return nil, fmt.Errorf("%w: the audit listing is empty", ErrNothingRead)
	}
	return listing.Entries, nil
}

// receiptOutcome waits for a run's receipt and returns the rules its outcome entry records and the
// outcome body itself.
func (h *harness) receiptOutcome(id string) ([]string, string, error) {
	var raw string
	err := h.waitFor("the receipt of run "+id, 60*time.Second, func() error {
		var err error
		raw, err = h.apiText("/v1/runs/" + id + "/receipt")
		return err
	})
	if err != nil {
		return nil, "", err
	}
	return outcomeRules(raw, id)
}

// outcomeRules reads the outcome entry out of a receipt and returns the rules it records and the
// outcome body.
func outcomeRules(receipt, runID string) ([]string, string, error) {
	var doc struct {
		Claims []struct {
			Payload struct {
				Method      string `json:"method"`
				Path        string `json:"path"`
				OutcomeBody string `json:"outcome_body"`
			} `json:"payload"`
		} `json:"claims"`
	}
	if err := json.Unmarshal([]byte(receipt), &doc); err != nil {
		return nil, "", fmt.Errorf("decode the receipt of %s: %w", runID, err)
	}
	for _, c := range doc.Claims {
		p := c.Payload
		if p.Method != "RUN" || !strings.HasPrefix(p.Path, "/runs/"+runID+"/outcome/") {
			continue
		}
		var outcome struct {
			PolicySet struct {
				Rules []string `json:"rules"`
			} `json:"policy_set"`
		}
		if err := json.Unmarshal([]byte(p.OutcomeBody), &outcome); err != nil {
			return nil, "", fmt.Errorf("decode the outcome of %s: %w", runID, err)
		}
		return outcome.PolicySet.Rules, p.OutcomeBody, nil
	}
	return nil, "", fmt.Errorf("%w: the receipt of %s discloses no outcome entry", ErrNothingRead,
		runID)
}

// apiText sends one GET as the human and returns the body as written, for answers that are not
// JSON or are read for what they contain rather than decoded.
func (h *harness) apiText(path string) (string, error) {
	req, err := http.NewRequest("GET", h.api+path, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+h.human.Token)
	resp, err := h.httpc.Do(req)
	if err != nil {
		return "", fmt.Errorf("GET %s: %w", path, err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return "", fmt.Errorf("read GET %s: %w", path, err)
	}
	if resp.StatusCode != http.StatusOK {
		return "", &apiError{Method: "GET", Path: path, Status: resp.StatusCode,
			Body: strings.TrimSpace(string(raw))}
	}
	return string(raw), nil
}

// parseRelayKey reads the JSON line worker key new printed.
func parseRelayKey(out string) (relayKey, error) {
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		var k relayKey
		if json.Unmarshal([]byte(strings.TrimSpace(sc.Text())), &k) == nil && k.DeliveryKey != "" &&
			k.KeyID != "" {
			return k, nil
		}
	}
	return relayKey{}, fmt.Errorf("%w: worker key new printed no key", ErrNothingRead)
}

// pemBodyLines returns the base64 body lines of PEM text long enough to be a secret on their own.
func pemBodyLines(text string) []string {
	var out []string
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if len(line) >= 16 && !strings.HasPrefix(line, "-----") && !strings.HasPrefix(line, "#") {
			out = append(out, line)
		}
	}
	return out
}

// stringsOf reads a decoded JSON array of strings, and nil for anything else.
func stringsOf(v any) []string {
	items, _ := v.([]any)
	var out []string
	for _, item := range items {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

// anyContains reports whether any of items contains want.
func anyContains(items []string, want string) bool {
	for _, item := range items {
		if strings.Contains(item, want) {
			return true
		}
	}
	return false
}

// sha256Hex returns the hex SHA-256 of s.
func sha256Hex(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// randomHex returns n random bytes as hex.
func randomHex(n int) string {
	raw := make([]byte, n)
	if _, err := rand.Read(raw); err != nil {
		panic(err)
	}
	return hex.EncodeToString(raw)
}
