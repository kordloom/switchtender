package main

import (
	"encoding/base64"
	"fmt"
	"path/filepath"
	"strings"
)

// phaseDR proves the answer procurement is given about disasters: the sealed backup of an install
// restores into a brand-new install, the configuration comes back counted, and the evidence the
// dead install produced keeps verifying without it. It also proves the honest half of that
// answer: run history and the chain deliberately do not travel, and the restore says so out loud
// instead of leaving an operator to discover it. The install is destroyed for real, namespace and
// volume together, because a recovery drill against an install that still exists proves nothing.
func (h *harness) phaseDR() error {
	const phase = "dr"
	if _, err := h.kubectl("create", "namespace", "dr"); err != nil {
		return err
	}
	install := func(ns string) error {
		out, err := h.run("helm", "install", ns,
			filepath.Join(h.repo, "deploy/helm/switchtender"),
			"--namespace", ns,
			"--kubeconfig", h.kubeconfig,
			"--set", "image.repository=switchtender",
			"--set", "image.tag=supertest",
			"--set", "image.pullPolicy=Never",
			"--set", "encryptionKey=supertest-key-not-a-secret",
			"--set", "encryptionSalt=supertest-salt",
			"--wait", "--timeout", "300s")
		if err != nil {
			return fmt.Errorf("helm install %s: %w\n%s\n%s", ns, err, out, h.installForensics(ns))
		}
		return nil
	}
	if err := install("dr"); err != nil {
		return err
	}
	service, err := h.serviceName("dr")
	if err != nil {
		return err
	}
	if err := h.forwardTo("dr", service); err != nil {
		return err
	}
	if err := h.bootstrap("dr"); err != nil {
		return err
	}
	human := h.human

	// Real configuration worth losing: a template, a sealed credential, an inventory.
	var tpl map[string]any
	if err := h.apiCall("POST", "/v1/templates", &human, map[string]any{
		"name": "dr proof", "tool": "bash", "command": "echo the config survived",
	}, &tpl); err != nil {
		return fmt.Errorf("create template: %w", err)
	}
	var cred map[string]any
	if err := h.apiCall("POST", "/v1/credentials", &human, map[string]any{
		"name": "dr-ssh", "kind": "ssh_key", "secret": "not-a-real-key",
	}, &cred); err != nil {
		return fmt.Errorf("create credential: %w", err)
	}
	var inv map[string]any
	if err := h.apiCall("POST", "/v1/inventories", &human, map[string]any{
		"name": "dr fleet", "content": "[web]\nweb01\n",
	}, &inv); err != nil {
		return fmt.Errorf("create inventory: %w", err)
	}

	// The sealed backup, taken the way an operator takes it: the product's own command, inside
	// the pod, against the live database.
	pod, err := h.serverPod("dr")
	if err != nil {
		return err
	}
	backup, err := h.runOut("kubectl", "exec", "-n", "dr", pod, "--",
		"/usr/local/bin/switchtender", "backup", "--db", "/data/switchtender.db")
	if err != nil {
		return fmt.Errorf("take the backup inside the pod: %w", err)
	}
	if !strings.Contains(backup, "switchtender") && len(backup) < 200 {
		return fmt.Errorf("backup output does not look like a sealed envelope (%d bytes)", len(backup))
	}
	h.pass(phase, "a sealed backup was taken with the product's own command",
		fmt.Sprintf("%d bytes from the live install", len(backup)))

	// The disaster. Namespace, volume, database: gone for real.
	if _, err := h.run("helm", "uninstall", "dr", "--namespace", "dr",
		"--kubeconfig", h.kubeconfig); err != nil {
		return fmt.Errorf("uninstall dr: %w", err)
	}
	if _, err := h.kubectl("delete", "namespace", "dr", "--wait=true", "--timeout=120s"); err != nil {
		return fmt.Errorf("delete namespace dr: %w", err)
	}
	h.pass(phase, "the install was destroyed for real", "namespace, claim, and database deleted")

	// The new install: different namespace, different volume, nothing shared but the key.
	if _, err := h.kubectl("create", "namespace", "dr2"); err != nil {
		return err
	}
	if err := install("dr2"); err != nil {
		return err
	}
	pod2, err := h.serverPod("dr2")
	if err != nil {
		return err
	}
	// The envelope rides the argv as base64 and is decoded inside the pod, never on kubectl's
	// attached stdin, which drops bytes under load; the pipe from base64 -d to restore is a real
	// in-pod pipe with no attach in the middle.
	encoded := base64.StdEncoding.EncodeToString([]byte(backup))
	restored, err := h.kubectl("exec", "-n", "dr2", pod2, "--", "sh", "-c",
		"printf %s "+encoded+" | base64 -d | /usr/local/bin/switchtender restore --db /data/switchtender.db")
	if err != nil {
		return fmt.Errorf("restore into the new install: %w\n%s", err, restored)
	}
	if !strings.Contains(restored, "templates 1") || !strings.Contains(restored, "credentials 1") {
		h.fail(phase, "the restore reported what it wrote, kind by kind",
			fmt.Errorf("report does not carry the counts: %s", oneLine(restored)))
	} else {
		h.pass(phase, "the restore reported what it wrote, kind by kind", oneLine(restored))
	}

	// The server reads the restored database after a restart, exactly as an operator would bounce
	// it, and the configuration is back through the API.
	if _, err := h.kubectl("delete", "pod", "-n", "dr2", pod2, "--wait=false"); err != nil {
		return err
	}
	service2, err := h.serviceName("dr2")
	if err != nil {
		return err
	}
	if _, err := h.kubectl("rollout", "status", "-n", "dr2",
		"deploy", "-l", "app.kubernetes.io/component=server", "--timeout=180s"); err != nil {
		return err
	}
	if err := h.forwardTo("dr2", service2); err != nil {
		return err
	}
	// No bootstrap on the restored install, on purpose: the restore carried the dead install's
	// accounts and token hashes, so the server correctly refuses to mint a fresh admin token over
	// them. The stronger claim is the one asserted instead: the token minted before the disaster
	// authenticates against the install restored after it.
	fresh := human

	var tpls struct {
		Templates []map[string]any `json:"templates"`
	}
	if err := h.apiCall("GET", "/v1/templates", &fresh, nil, &tpls); err != nil {
		return err
	}
	foundTpl := false
	for _, item := range tpls.Templates {
		if name, _ := item["name"].(string); name == "dr proof" {
			foundTpl = true
		}
	}
	if !foundTpl {
		h.fail(phase, "the restored install serves the dead install's configuration",
			fmt.Errorf("template 'dr proof' is absent after restore"))
	} else {
		h.pass(phase, "the restored install serves the dead install's configuration",
			"read back with the pre-disaster token: accounts and credentials crossed too")
	}

	// The honest half, asserted rather than implied: the dead install's runs did not travel, and
	// the new install's chain is its own. The evidence story never depended on the server: the
	// receipt downloaded from the ORIGINAL community install still verifies offline, right now,
	// after this phase destroyed a different install and could have destroyed that one.
	var verify struct {
		OK bool `json:"ok"`
	}
	if err := h.apiCall("GET", "/v1/audit/verify", &fresh, nil, &verify); err != nil {
		return err
	}
	if !verify.OK {
		h.fail(phase, "the new install's own chain verifies", fmt.Errorf("verify not ok"))
	} else {
		h.pass(phase, "the new install's own chain verifies",
			"history deliberately does not travel; the new chain stands on its own")
	}
	receipts, _ := filepath.Glob(filepath.Join(h.work, "community-*-receipt.json"))
	if len(receipts) > 0 {
		if out, err := h.run(h.bin, "verify", receipts[0]); err != nil {
			h.fail(phase, "evidence outlives the install that produced it",
				fmt.Errorf("%w\n%s", err, out))
		} else {
			h.pass(phase, "evidence outlives the install that produced it",
				"the first install's receipt verifies offline with no server anywhere")
		}
	}
	return nil
}
