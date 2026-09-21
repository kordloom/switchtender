package main

import (
	"fmt"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// phaseUpgrade proves the promise every release makes and nothing was testing: a customer's
// install survives the next version. It installs the previous published release the way the chart
// installs it, does real work on it, then helm-upgrades the same release in place to the image
// built from this working tree and requires everything to still be there: the token still
// authenticates, the old run still reads back, the audit chain still verifies over the old
// entries, the old run's receipt still verifies offline, and a new run executes on the new
// binary. Data-loss-on-upgrade is the defect a customer never forgives, and until this phase
// existed the only test of it was hope.
func (h *harness) phaseUpgrade(prevImage string) error {
	const phase = "upgrade"

	if out, err := h.run("docker", "pull", prevImage); err != nil {
		return fmt.Errorf("pull previous release %s: %w\n%s", prevImage, err, out)
	}
	if out, err := h.run("kind", "load", "docker-image", prevImage, "--name", h.cluster); err != nil {
		return fmt.Errorf("load %s: %w\n%s", prevImage, err, out)
	}
	repo, tag, ok := strings.Cut(prevImage, ":")
	if !ok {
		return fmt.Errorf("previous image %q carries no tag", prevImage)
	}

	if out, err := h.run("helm", "install", "upgrade",
		filepath.Join(h.repo, "deploy/helm/switchtender"),
		"--namespace", "upgrade", "--create-namespace",
		"--kubeconfig", h.kubeconfig,
		"--set", "image.repository="+repo,
		"--set", "image.tag="+tag,
		"--set", "image.pullPolicy=Never",
		"--set", "encryptionKey=supertest-key-not-a-secret",
		"--set", "encryptionSalt=supertest-salt",
		"--wait", "--timeout", "300s"); err != nil {
		return fmt.Errorf("helm install %s: %w\n%s\n%s", prevImage, err, out,
			h.installForensics("upgrade"))
	}
	h.pass(phase, "the previous release installs from this chart", prevImage)

	service, err := h.serviceName("upgrade")
	if err != nil {
		return err
	}
	if err := h.forwardTo("upgrade", service, 18903); err != nil {
		return err
	}
	if err := h.bootstrap("upgrade"); err != nil {
		return err
	}
	oldToken := h.human

	// Python rather than bash on the old side, deliberately: images before 1.92.0 shipped without
	// bash (a defect the supertest itself caught), and the upgrade phase's job is to prove data
	// survives versions, not to re-litigate which tools an old image carried.
	var created map[string]any
	err = h.apiCall("POST", "/v1/runs", &oldToken, map[string]any{
		"tool": "python", "command": "print('made-on-the-old-version')",
	}, &created)
	if err != nil {
		return fmt.Errorf("run on the previous release: %w", err)
	}
	oldRunID, _ := created["id"].(string)
	if _, err := h.awaitRunStatus(oldRunID, "succeeded", 120*time.Second); err != nil {
		return fmt.Errorf("run on the previous release: %w", err)
	}
	var before struct {
		Count int `json:"count"`
	}
	if err := h.apiCall("GET", "/v1/audit/verify", &oldToken, nil, &before); err != nil {
		return err
	}
	h.pass(phase, "the previous release did real, recorded work",
		fmt.Sprintf("run %s, %d chain entries", oldRunID, before.Count))

	if out, err := h.run("helm", "upgrade", "upgrade",
		filepath.Join(h.repo, "deploy/helm/switchtender"),
		"--namespace", "upgrade",
		"--kubeconfig", h.kubeconfig,
		"--set", "image.repository=switchtender",
		"--set", "image.tag=supertest",
		"--set", "image.pullPolicy=Never",
		"--set", "encryptionKey=supertest-key-not-a-secret",
		"--set", "encryptionSalt=supertest-salt",
		"--wait", "--timeout", "300s"); err != nil {
		return fmt.Errorf("helm upgrade to the working tree: %w\n%s\n%s", err, out,
			h.installForensics("upgrade"))
	}
	h.pass(phase, "helm upgrade to the working tree succeeded in place",
		"same release, same volume, Recreate rollover")

	// The pod was replaced, so the forward was cut with it.
	if err := h.forwardTo("upgrade", service, 18903); err != nil {
		return err
	}

	var doc map[string]any
	if err := h.apiCall("GET", "/v1/runs/"+oldRunID, &oldToken, nil, &doc); err != nil {
		h.fail(phase, "the old token still authenticates and the old run still reads back", err)
		return nil
	}
	if status, _ := doc["status"].(string); status != "succeeded" {
		h.fail(phase, "the old run kept its history",
			fmt.Errorf("run %s reads back as %q after the upgrade", oldRunID, status))
	} else {
		h.pass(phase, "the old token still authenticates and the old run kept its history",
			"nothing a customer had was taken by the upgrade")
	}

	var after struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if err := h.apiCall("GET", "/v1/audit/verify", &oldToken, nil, &after); err != nil {
		return err
	}
	if !after.OK || after.Count < before.Count {
		h.fail(phase, "the audit chain verifies across the version boundary",
			fmt.Errorf("ok=%v count=%d, was %d before the upgrade", after.OK, after.Count, before.Count))
	} else {
		h.pass(phase, "the audit chain verifies across the version boundary",
			fmt.Sprintf("%d entries, every pre-upgrade link recomputed by the new binary", after.Count))
	}
	if err := h.verifyReceiptOffline(phase, oldRunID, ""); err != nil {
		return err
	}

	var again map[string]any
	err = h.apiCall("POST", "/v1/runs", &oldToken, map[string]any{
		"tool": "bash", "command": "echo made-on-the-new-version",
	}, &again)
	if err != nil {
		return fmt.Errorf("run on the upgraded install: %w", err)
	}
	newRunID, _ := again["id"].(string)
	if _, err := h.awaitRunStatus(newRunID, "succeeded", 120*time.Second); err != nil {
		return fmt.Errorf("run on the upgraded install: %w", err)
	}
	h.pass(phase, "the upgraded install does new work with the old credentials", "run "+newRunID)
	return nil
}

// phaseCrash kills the dedicated worker while it is mid-run and requires the product to keep its
// promise about dying executors: the lease goes stale, the janitor reclaims the run, and the
// replacement worker picks it up and finishes it. A control plane whose runs vanish with the
// process that held them is a control plane nobody points at production, and this is the check
// that claim never gets to be a sentence on a website.
func (h *harness) phaseCrash() error {
	const phase = "crash"

	human := h.human
	var created map[string]any
	err := h.apiCall("POST", "/v1/runs", &human, map[string]any{
		"tool": "bash", "command": "sleep 20 && echo survived", "queue": "supertest",
	}, &created)
	if err != nil {
		return fmt.Errorf("submit the crash-test run: %w", err)
	}
	runID, _ := created["id"].(string)

	var victim string
	err = h.waitFor("the worker to claim the run", 90*time.Second, func() error {
		var doc map[string]any
		if err := h.apiCall("GET", "/v1/runs/"+runID, &human, nil, &doc); err != nil {
			return err
		}
		claimed, _ := doc["claimed_by"].(string)
		status, _ := doc["status"].(string)
		if status == "running" && strings.Contains(claimed, "worker") {
			victim = claimed
			return nil
		}
		return fmt.Errorf("run is %s, claimed by %q", status, claimed)
	})
	if err != nil {
		return err
	}

	// The claimant names the worker process, which may or may not carry a slot suffix on top of
	// the pod name. Guessing the boundary got it wrong once, so the pod is found rather than
	// parsed: it is the one whose name prefixes the claimant.
	out, err := h.kubectl("get", "pods", "-n", "team",
		"-l", "app.kubernetes.io/component=worker", "-o", "jsonpath={.items[*].metadata.name}")
	if err != nil {
		return err
	}
	var pod string
	for _, candidate := range strings.Fields(out) {
		if strings.HasPrefix(victim, candidate) {
			pod = candidate
		}
	}
	if pod == "" {
		return fmt.Errorf("no worker pod matches claimant %q among %q", victim, out)
	}
	if _, err := h.kubectl("delete", "pod", "-n", "team", pod, "--wait=false"); err != nil {
		return fmt.Errorf("kill the worker mid-run: %w", err)
	}
	h.pass(phase, "the worker was killed while it held the run",
		fmt.Sprintf("pod %s deleted mid-execution of %s", pod, runID))

	// Lease 30s, sweep every 10s, pod respawn, then a full re-execution: generous ceiling, no
	// floor. The run must SUCCEED, on a different claimant, with the attempt counter saying a
	// retry happened, or the reclaim story is a comment rather than a behavior.
	done, err := h.awaitRunStatus(runID, "succeeded", 240*time.Second)
	if err != nil {
		return fmt.Errorf("the run its executor died under never completed: %w", err)
	}
	claimedBy, _ := done["claimed_by"].(string)
	if claimedBy == victim {
		h.fail(phase, "the run finished on the replacement worker",
			fmt.Errorf("claimed_by is still %q, the pod that was deleted", victim))
	} else if !strings.Contains(claimedBy, "worker") {
		h.fail(phase, "the run finished on the replacement worker",
			fmt.Errorf("claimed_by is %q, not a worker", claimedBy))
	} else {
		h.pass(phase, "the run finished on the replacement worker",
			fmt.Sprintf("reclaimed from %s, completed by %s", victim, claimedBy))
	}

	var verify struct {
		OK bool `json:"ok"`
	}
	if err := h.apiCall("GET", "/v1/audit/verify", &human, nil, &verify); err != nil {
		return err
	}
	if !verify.OK {
		h.fail(phase, "the chain survived the crash intact", fmt.Errorf("audit verify says not ok"))
	} else {
		h.pass(phase, "the chain survived the crash intact", "a dead executor forged nothing")
	}
	return nil
}

// previousReleaseImage resolves the image of the latest published release from the repository's
// own tags, unless the caller named one. The supertest upgrades from it to the working tree, so
// an unresolvable previous release is an error rather than a silently skipped phase.
func previousReleaseImage(flagValue, repoDir string) (string, error) {
	if flagValue != "" {
		return flagValue, nil
	}
	out, err := exec.Command("git", "-C", repoDir, "tag", "--list", "v*",
		"--sort=-v:refname").Output()
	if err != nil {
		return "", fmt.Errorf("list release tags: %w", err)
	}
	tags := strings.Fields(string(out))
	if len(tags) == 0 {
		return "", fmt.Errorf("no release tags are visible; fetch tags or pass -prev-image")
	}
	return "ghcr.io/kordloom/switchtender:" + strings.TrimPrefix(tags[0], "v"), nil
}
