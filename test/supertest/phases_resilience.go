package main

import (
	"fmt"
	"os"
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
	if err := h.forwardTo("upgrade", service); err != nil {
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
	if err := h.forwardTo("upgrade", service); err != nil {
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
	// The rollback phase walks this install back and must find this run again: recorded here so
	// the two phases share one story instead of one re-deriving the other's state.
	h.ids["upgrade-new-run"] = newRunID
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

	// The phase aims itself at the team install rather than inheriting whatever forward the
	// previous phase left behind. Inheriting was a hidden ordering dependency: inserting a phase
	// before this one silently pointed every crash request at the wrong install.
	service, err := h.serviceName("team")
	if err != nil {
		return err
	}
	if err := h.forwardTo("team", service); err != nil {
		return err
	}

	human := h.human
	var created map[string]any
	err = h.apiCall("POST", "/v1/runs", &human, map[string]any{
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

	// Lease 30s, sweep every 10s: the janitor reclaims a run whose executor died. A run that was
	// mid-execution is NOT silently re-run, deliberately, because re-applying a half-applied
	// change could double-act; it is reclaimed to a terminal interrupted state with a stated
	// reason, which is the state a person or a rerun resumes from. Proving it silently completed
	// would be proving the wrong, more dangerous thing.
	reclaimed, err := h.awaitRunStatus(runID, "interrupted", 180*time.Second)
	if err != nil {
		return fmt.Errorf("the run its executor died under was not reclaimed: %w", err)
	}
	// Either honest reason is right, and which one depends on how the worker died. A kubectl
	// delete lets the process shut down gracefully, so it marks its own in-flight run interrupted
	// on the way out ("the server stopped while this run was executing"); a hard kill leaves the
	// janitor to reclaim the stale lease instead ("executor lease expired"). What must never
	// appear is a blank reason or a silent success.
	reason, _ := reclaimed["error"].(string)
	if !strings.Contains(reason, "lease expired") &&
		!strings.Contains(reason, "server stopped while this run was executing") {
		h.fail(phase, "the abandoned run is reclaimed with a stated reason",
			fmt.Errorf("error = %q, want it to name the crash", reason))
	} else {
		h.pass(phase, "the abandoned run is reclaimed to interrupted with a stated reason",
			"a mid-flight change is never silently re-run; "+reason)
	}

	// The chain must survive a crash mid-run: a dead executor forges nothing.
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

	// And the interrupted run is resumable: a rerun replays its spec and finishes on a live
	// worker, so a crash costs a retry, never the work.
	var again map[string]any
	if err := h.apiCall("POST", "/v1/runs/"+runID+"/rerun", &human, map[string]any{}, &again); err != nil {
		return fmt.Errorf("rerun the interrupted run: %w", err)
	}
	rerunID, _ := again["id"].(string)
	done, err := h.awaitRunStatus(rerunID, "succeeded", 180*time.Second)
	if err != nil {
		return fmt.Errorf("the rerun of the interrupted run never completed: %w", err)
	}
	claimedBy, _ := done["claimed_by"].(string)
	if !strings.Contains(claimedBy, "worker") {
		h.fail(phase, "the rerun completed on a live worker",
			fmt.Errorf("claimed_by is %q, not a worker", claimedBy))
	} else {
		h.pass(phase, "a rerun of the interrupted run completed on a live worker",
			"a crash costs a retry, never the work: "+rerunID+" on "+claimedBy)
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

// phaseRollback proves the upgrade is reversible: the same release rolled back to the previous
// binary, on the volume the new binary already wrote, keeps everything and keeps working. This is
// the product's own reversibility standard applied to itself: an upgrade an operator cannot walk
// back is a one-way door sold as a swap. It runs in the upgrade namespace, immediately after the
// upgrade proof, so the rollback crosses a database the newer binary genuinely touched.
func (h *harness) phaseRollback(prevImage string) error {
	const phase = "rollback"
	repo, tag, ok := strings.Cut(prevImage, ":")
	if !ok {
		return fmt.Errorf("previous image %q carries no tag", prevImage)
	}

	if out, err := h.run("helm", "upgrade", "upgrade",
		filepath.Join(h.repo, "deploy/helm/switchtender"),
		"--namespace", "upgrade",
		"--kubeconfig", h.kubeconfig,
		"--set", "image.repository="+repo,
		"--set", "image.tag="+tag,
		"--set", "image.pullPolicy=Never",
		"--set", "encryptionKey=supertest-key-not-a-secret",
		"--set", "encryptionSalt=supertest-salt",
		"--wait", "--timeout", "300s"); err != nil {
		h.fail(phase, "the release rolls back to the previous binary in place",
			fmt.Errorf("helm upgrade back to %s: %w\n%s\n%s", prevImage, err, out,
				h.installForensics("upgrade")))
		return nil
	}
	h.pass(phase, "the release rolled back to the previous binary in place",
		"same release, same volume, on a database the newer binary already wrote")

	service, err := h.serviceName("upgrade")
	if err != nil {
		return err
	}
	if err := h.forwardTo("upgrade", service); err != nil {
		return err
	}

	oldToken := h.human
	var verify struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if err := h.apiCall("GET", "/v1/audit/verify", &oldToken, nil, &verify); err != nil {
		h.fail(phase, "the previous binary reads the state the newer one wrote", err)
		return nil
	}
	if !verify.OK {
		h.fail(phase, "the chain verifies after the rollback",
			fmt.Errorf("audit verify says not ok on the rolled-back binary"))
	} else {
		h.pass(phase, "the chain verifies after the rollback",
			fmt.Sprintf("%d entries, including the ones the newer binary appended", verify.Count))
	}

	// Work done on the newer binary must survive the walk back.
	var doc map[string]any
	if err := h.apiCall("GET", "/v1/runs/"+h.ids["upgrade-new-run"], &oldToken, nil, &doc); err != nil {
		h.fail(phase, "a run recorded by the newer binary reads back on the older one", err)
	} else if status, _ := doc["status"].(string); status != "succeeded" {
		h.fail(phase, "a run recorded by the newer binary keeps its history",
			fmt.Errorf("reads back as %q after the rollback", status))
	} else {
		h.pass(phase, "a run recorded by the newer binary reads back on the older one",
			"an upgrade an operator regrets costs nothing to leave")
	}

	// And the rolled-back install still does new work. Python, not bash: images before 1.92.0
	// shipped without bash, and this phase's job is reversibility, not tool inventory.
	var again map[string]any
	err = h.apiCall("POST", "/v1/runs", &oldToken, map[string]any{
		"tool": "python", "command": "print('made-after-rollback')",
	}, &again)
	if err != nil {
		h.fail(phase, "the rolled-back install executes new work", err)
		return nil
	}
	rbID, _ := again["id"].(string)
	if _, err := h.awaitRunStatus(rbID, "succeeded", 120*time.Second); err != nil {
		h.fail(phase, "the rolled-back install executes new work", err)
	} else {
		h.pass(phase, "the rolled-back install executes new work", "run "+rbID)
	}
	return nil
}

// phaseUpgradeTeam proves the paid shape survives the next version too: PostgreSQL, a license, and
// a dedicated worker, upgraded in place. The Community upgrade proof left the paid tier's upgrade
// as an inference, and an inference is not a proof: the schema is initialized by the old binary
// under license, written by real work, and must be read whole by the new one.
func (h *harness) phaseUpgradeTeam(prevImage, license string) error {
	const phase = "upgrade-team"
	if _, err := h.kubectl("create", "namespace", "upteam"); err != nil {
		return err
	}
	pg := mustManifest("postgres.yaml")
	if err := h.apply(strings.ReplaceAll(pg, "namespace: team", "namespace: upteam")); err != nil {
		return err
	}
	if _, err := h.kubectl("rollout", "status", "deploy/postgres", "-n", "upteam",
		"--timeout=180s"); err != nil {
		return err
	}
	seedPath := filepath.Join(h.work, "upteam-audit-seed")
	if err := os.WriteFile(seedPath, []byte(h.auditSeed()), 0o600); err != nil {
		return err
	}
	repo, tag, ok := strings.Cut(prevImage, ":")
	if !ok {
		return fmt.Errorf("previous image %q carries no tag", prevImage)
	}
	install := func(imageRepo, imageTag string) (string, error) {
		return h.run("helm", "upgrade", "--install", "upteam",
			filepath.Join(h.repo, "deploy/helm/switchtender"),
			"--namespace", "upteam",
			"--kubeconfig", h.kubeconfig,
			"--set", "image.repository="+imageRepo,
			"--set", "image.tag="+imageTag,
			"--set", "image.pullPolicy=Never",
			"--set", "encryptionKey=supertest-key-not-a-secret",
			"--set", "encryptionSalt=supertest-salt",
			"--set", "database.dsn=postgres://switchtender:supertest-not-a-secret@postgres:5432/switchtender?sslmode=disable",
			"--set-file", "auditKey="+seedPath,
			"--set-file", "license="+license,
			"--set", "worker.enabled=true",
			"--set", "worker.extraArgs={--queue,upteam}",
			"--wait", "--timeout", "300s")
	}
	if out, err := install(repo, tag); err != nil {
		return fmt.Errorf("helm install team-shape %s: %w\n%s\n%s", prevImage, err, out,
			h.installForensics("upteam"))
	}
	h.pass(phase, "the previous release initialized a licensed PostgreSQL install", prevImage)

	service, err := h.serviceName("upteam")
	if err != nil {
		return err
	}
	if err := h.forwardTo("upteam", service); err != nil {
		return err
	}
	if err := h.bootstrap("upteam"); err != nil {
		return err
	}
	oldToken := h.human

	var created map[string]any
	err = h.apiCall("POST", "/v1/runs", &oldToken, map[string]any{
		"tool": "python", "command": "print('made-on-old-team')", "queue": "upteam",
	}, &created)
	if err != nil {
		return fmt.Errorf("run on the previous team-shape release: %w", err)
	}
	oldRunID, _ := created["id"].(string)
	if _, err := h.awaitRunStatus(oldRunID, "succeeded", 180*time.Second); err != nil {
		return fmt.Errorf("run on the previous team-shape release: %w", err)
	}
	var before struct {
		Count int `json:"count"`
	}
	if err := h.apiCall("GET", "/v1/audit/verify", &oldToken, nil, &before); err != nil {
		return err
	}
	h.pass(phase, "the previous release did real work through its dedicated worker",
		fmt.Sprintf("run %s, %d chain entries", oldRunID, before.Count))

	if out, err := install("switchtender", "supertest"); err != nil {
		return fmt.Errorf("helm upgrade team-shape to the working tree: %w\n%s\n%s", err, out,
			h.installForensics("upteam"))
	}
	h.pass(phase, "helm upgrade of the licensed PostgreSQL install succeeded in place",
		"server and worker rolled together, schema reused, license carried")

	// Two deployments roll on this upgrade, the server by Recreate, so the service can sit with no
	// ready endpoint longer than a health poll tolerates. Waiting on the rollouts themselves is
	// deterministic; waiting on a port-forward to become lucky is not.
	names, err := h.kubectl("get", "deploy", "-n", "upteam", "-o", "name")
	if err != nil {
		return err
	}
	for _, name := range strings.Fields(names) {
		if strings.Contains(name, "postgres") {
			continue
		}
		if _, err := h.kubectl("rollout", "status", name, "-n", "upteam",
			"--timeout=180s"); err != nil {
			return fmt.Errorf("waiting for %s after the upgrade: %w", name, err)
		}
	}
	if err := h.forwardTo("upteam", service); err != nil {
		return err
	}
	var doc map[string]any
	if err := h.apiCall("GET", "/v1/runs/"+oldRunID, &oldToken, nil, &doc); err != nil {
		h.fail(phase, "the old token and run survive the paid-shape upgrade", err)
		return nil
	}
	if status, _ := doc["status"].(string); status != "succeeded" {
		h.fail(phase, "the old run kept its history across the paid-shape upgrade",
			fmt.Errorf("run %s reads back as %q", oldRunID, status))
	} else {
		h.pass(phase, "the old token and run survive the paid-shape upgrade",
			"nothing a paying customer had was taken by the upgrade")
	}
	var after struct {
		OK    bool `json:"ok"`
		Count int  `json:"count"`
	}
	if err := h.apiCall("GET", "/v1/audit/verify", &oldToken, nil, &after); err != nil {
		return err
	}
	if !after.OK || after.Count < before.Count {
		h.fail(phase, "the audit chain verifies across the paid-shape version boundary",
			fmt.Errorf("ok=%v count=%d, was %d before", after.OK, after.Count, before.Count))
	} else {
		h.pass(phase, "the audit chain verifies across the paid-shape version boundary",
			fmt.Sprintf("%d entries under the shared signing identity", after.Count))
	}

	var again map[string]any
	err = h.apiCall("POST", "/v1/runs", &oldToken, map[string]any{
		"tool": "bash", "command": "echo made-on-new-team", "queue": "upteam",
	}, &again)
	if err != nil {
		return fmt.Errorf("run on the upgraded team-shape install: %w", err)
	}
	newRunID, _ := again["id"].(string)
	if _, err := h.awaitRunStatus(newRunID, "succeeded", 180*time.Second); err != nil {
		h.fail(phase, "the upgraded worker executes new work", err)
	} else {
		h.pass(phase, "the upgraded worker executes new work with the old credentials",
			"run "+newRunID+" through the dedicated worker")
	}
	return nil
}
