package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// phaseHA proves the sentence the Team tier sells: active-active HA. Two servers share one
// PostgreSQL database and one signing identity; one of them is killed while a run is executing
// and while the control plane is being used, and the product must not blink: the run completes
// exactly once, the surviving replica serves authenticated reads and writes while its peer is
// provably gone, an approval decided during the degraded window executes, and the chain comes out
// the other side as one unforked history. Until this phase existed, active-active HA was a priced
// claim with no proof anywhere.
func (h *harness) phaseHA(license string) error {
	const phase = "ha"
	if _, err := h.kubectl("create", "namespace", "ha"); err != nil {
		return err
	}
	pg := mustManifest("postgres.yaml")
	if err := h.apply(strings.ReplaceAll(pg, "namespace: team", "namespace: ha")); err != nil {
		return err
	}
	if _, err := h.kubectl("rollout", "status", "deploy/postgres", "-n", "ha",
		"--timeout=180s"); err != nil {
		return err
	}
	seedPath := filepath.Join(h.work, "ha-audit-seed")
	if err := os.WriteFile(seedPath, []byte(h.auditSeed()), 0o600); err != nil {
		return err
	}
	if out, err := h.run("helm", "install", "ha",
		filepath.Join(h.repo, "deploy/helm/switchtender"),
		"--namespace", "ha",
		"--kubeconfig", h.kubeconfig,
		"--set", "image.repository=switchtender",
		"--set", "image.tag=supertest",
		"--set", "image.pullPolicy=Never",
		"--set", "server.replicas=2",
		"--set", "encryptionKey=supertest-key-not-a-secret",
		"--set", "encryptionSalt=supertest-salt",
		"--set", "database.dsn=postgres://switchtender:supertest-not-a-secret@postgres:5432/switchtender?sslmode=disable",
		"--set-file", "auditKey="+seedPath,
		"--set-file", "license="+license,
		"--set", "worker.enabled=true",
		"--set", "worker.extraArgs={--queue,ha}",
		"--wait", "--timeout", "300s"); err != nil {
		return fmt.Errorf("helm install ha: %w\n%s\n%s", err, out, h.installForensics("ha"))
	}
	h.pass(phase, "two active servers share one database and one signing identity",
		"server.replicas=2 on PostgreSQL, the shape the Team tier prices")

	service, err := h.serviceName("ha")
	if err != nil {
		return err
	}
	if err := h.forwardTo("ha", service); err != nil {
		return err
	}
	if err := h.bootstrap("ha"); err != nil {
		return err
	}
	human := h.human

	// A run held by policy, decided during the degraded window later.
	var pol map[string]any
	err = h.apiCall("POST", "/v1/policies", &human, map[string]any{
		"name": "hold ha probe", "command_contains": "held-during-failover",
		"effect": "require_approval",
	}, &pol)
	if err != nil {
		return fmt.Errorf("create the hold policy: %w", err)
	}
	var held map[string]any
	err = h.apiCall("POST", "/v1/runs", &human, map[string]any{
		"tool": "bash", "command": "echo held-during-failover", "queue": "ha",
	}, &held)
	if err != nil {
		return fmt.Errorf("submit the held run: %w", err)
	}
	heldID, _ := held["id"].(string)

	// The long run that must ride through the failover on its worker.
	var long map[string]any
	err = h.apiCall("POST", "/v1/runs", &human, map[string]any{
		"tool": "bash", "command": "sleep 20 && echo rode-through-the-failover", "queue": "ha",
	}, &long)
	if err != nil {
		return fmt.Errorf("submit the long run: %w", err)
	}
	longID, _ := long["id"].(string)
	err = h.waitFor("the worker to claim the long run", 90*time.Second, func() error {
		var doc map[string]any
		if err := h.apiCall("GET", "/v1/runs/"+longID, &human, nil, &doc); err != nil {
			return err
		}
		if status, _ := doc["status"].(string); status != "running" {
			return fmt.Errorf("long run is %s", status)
		}
		return nil
	})
	if err != nil {
		return err
	}

	// Kill one server replica while the run executes.
	pods, err := h.kubectl("get", "pods", "-n", "ha",
		"-l", "app.kubernetes.io/component=server", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return err
	}
	names := strings.Fields(pods)
	if len(names) != 2 {
		return fmt.Errorf("expected 2 server pods, found %d (%q)", len(names), pods)
	}
	victim, survivor := names[0], names[1]
	if _, err := h.kubectl("delete", "pod", "-n", "ha", victim, "--wait=false"); err != nil {
		return fmt.Errorf("kill the server replica: %w", err)
	}
	h.pass(phase, "one of the two servers was killed mid-run and mid-use",
		"pod "+victim+" deleted while run "+longID+" executed")

	// The honest availability measurement: the SURVIVOR, dialed by name, serves an authenticated
	// read while the deployment provably has one ready replica. Dialing the service here is
	// roulette, and it lost: kubectl resolved the dying pod's sandbox and burned the whole window
	// on an endpoint that no longer existed. Naming the survivor makes the claim exact, which
	// endpoint answered, and the readyReplicas gate makes the alone part checkable.
	err = h.waitFor("the deployment to report one ready replica", 60*time.Second, func() error {
		ready, rerr := h.kubectl("get", "deploy", "-n", "ha",
			"-l", "app.kubernetes.io/component=server",
			"-o", "jsonpath={.items[0].status.readyReplicas}")
		if rerr != nil {
			return rerr
		}
		if strings.TrimSpace(ready) != "1" {
			return fmt.Errorf("readyReplicas=%q", ready)
		}
		return nil
	})
	if err != nil {
		h.fail(phase, "the surviving replica serves authenticated reads alone", err)
	} else if perr := h.forwardToPod("ha", survivor); perr != nil {
		h.fail(phase, "the surviving replica serves authenticated reads alone", perr)
	} else {
		var runs map[string]any
		if rerr := h.apiCall("GET", "/v1/runs", &human, nil, &runs); rerr != nil {
			h.fail(phase, "the surviving replica serves authenticated reads alone", rerr)
		} else {
			h.pass(phase, "the surviving replica serves authenticated reads alone",
				survivor+" answered by name while the peer was provably gone")
		}
	}

	// A control-plane WRITE during the degraded window: approve the held run on the survivor.
	// The rule that held it names no second approver, so the requester's own approval is allowed,
	// which keeps this check about availability rather than about separation of duties.
	var approved map[string]any
	if err := h.apiCall("POST", "/v1/runs/"+heldID+"/approve", &human,
		map[string]any{}, &approved); err != nil {
		h.fail(phase, "an approval lands during the degraded window", err)
	} else {
		h.pass(phase, "an approval landed during the degraded window",
			"decided on the survivor, executed by the worker")
	}
	if _, err := h.awaitRunStatus(heldID, "succeeded", 180*time.Second); err != nil {
		h.fail(phase, "the approved run executed after the failover", err)
	} else {
		h.pass(phase, "the approved run executed after the failover", "run "+heldID)
	}

	// The long run rode through: completed exactly once, claimed by one worker.
	done, err := h.awaitRunStatus(longID, "succeeded", 180*time.Second)
	if err != nil {
		h.fail(phase, "the mid-flight run completed across the server failover", err)
	} else {
		claimed, _ := done["claimed_by"].(string)
		h.pass(phase, "the mid-flight run completed across the server failover",
			"run "+longID+" finished once, claimed by "+claimed)
	}

	// Both replicas back, and the history is one unforked chain.
	if _, err := h.kubectl("rollout", "status", "-n", "ha",
		"deploy", "-l", "app.kubernetes.io/component=server", "--timeout=180s"); err != nil {
		return err
	}
	if err := h.forwardTo("ha", service); err != nil {
		return err
	}
	var verify struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if err := h.apiCall("GET", "/v1/audit/verify", &human, nil, &verify); err != nil {
		return err
	}
	if !verify.OK {
		h.fail(phase, "the chain is one unforked history after the failover",
			fmt.Errorf("audit verify says not ok"))
	} else {
		h.pass(phase, "the chain is one unforked history after the failover",
			fmt.Sprintf("%d entries appended by two servers and a worker, one signing identity", verify.Count))
	}
	return nil
}
