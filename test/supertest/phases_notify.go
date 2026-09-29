package main

import (
	"fmt"
	"strings"
	"time"
)

// hookSinkPath is where the receiver appends every notification body it is handed.
const hookSinkPath = "/tmp/notifications"

// startHookSink brings up the receiver that the install's run-finished notification is pointed at,
// and returns the URL to configure.
//
// It is created before the install, because the flag naming it is set at start and an operator
// configuring a notifier expects the first finished run to reach it rather than the second.
func (h *harness) startHookSink() (string, error) {
	if _, err := h.kubectl("create", "namespace", "community"); err != nil &&
		!strings.Contains(err.Error(), "AlreadyExists") {
		// A namespace that already exists is the ordinary case on a rerun, and helm creates it
		// otherwise. Anything else is worth stopping for.
		if out, _ := h.kubectl("get", "namespace", "community"); !strings.Contains(out, "community") {
			return "", fmt.Errorf("create the community namespace for the notification sink: %w", err)
		}
	}
	if err := h.apply(mustManifest("hooksink.yaml")); err != nil {
		return "", err
	}
	if _, err := h.kubectl("rollout", "status", "-n", "community", "deploy/hooksink",
		"--timeout=120s"); err != nil {
		return "", fmt.Errorf("the notification sink never became ready: %w", err)
	}
	return "http://hooksink.community.svc.cluster.local:8099/finished", nil
}

// checkTheRunFinishedNotificationArrives proves the install tells somebody when a run ends.
//
// A notifier that silently does not fire is the same shape as a schedule that never comes due: no
// error, nothing in the product to look at, and the operator finds out when they notice they have
// heard nothing for a week. It is the kind of thing a unit test cannot settle, because what is in
// question is whether a deployed process reaches an address outside itself.
//
// What arrived is read out of the receiver with kubectl exec rather than asked of the product, so
// the claim does not rest on the product's own report that it sent something.
func (h *harness) checkTheRunFinishedNotificationArrives(phase, runID string) {
	const claim = "a finished run is announced to the configured notification URL"
	pod, err := h.kubectl("get", "pods", "-n", "community", "-l", "app=hooksink",
		"-o", "jsonpath={.items[0].metadata.name}")
	if err != nil || strings.TrimSpace(pod) == "" {
		h.fail(phase, claim, fmt.Errorf("%w: the notification sink is not running, so nothing "+
			"could have been delivered to it", ErrNothingRead))
		return
	}
	pod = strings.TrimSpace(pod)

	var body string
	err = h.waitFor("the notification to reach the sink", 90*time.Second, func() error {
		out, rerr := h.kubectl("exec", "-n", "community", pod, "--", "cat", hookSinkPath)
		if rerr != nil {
			return fmt.Errorf("the sink has recorded nothing at all yet")
		}
		if !strings.Contains(out, runID) {
			return fmt.Errorf("the sink holds %d bytes and none of it names run %s",
				len(out), runID)
		}
		body = out
		return nil
	})
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	// Named, not merely delivered. A notification that arrives without saying which run finished
	// tells an operator that something happened and leaves them to find out what, which is the
	// message being useless in a different way from not being sent.
	line := ""
	for _, candidate := range strings.Split(strings.TrimSpace(body), "\n") {
		if strings.Contains(candidate, runID) {
			line = candidate
			break
		}
	}
	h.pass(phase, claim, fmt.Sprintf("the sink recorded a notification naming run %s: %s",
		runID, oneLine(clip(line, 160))))
}

// clip shortens a string for the evidence line without cutting a multi-byte character in half.
func clip(s string, limit int) string {
	if limit < 0 {
		limit = 0
	}
	if len(s) <= limit {
		return s
	}
	cut := limit
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "..."
}
