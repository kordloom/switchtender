package main

import (
	"fmt"
	"strings"
)

// checkTheInstallCanAnchorItsOwnChain proves a deployed install can fix its chain head somewhere it
// cannot later rewrite, and then says so about itself.
//
// A hash chain proves nothing in it was altered and cannot prove nothing was removed from the end,
// because a prefix of a valid chain is itself a valid chain. An anchor closes that, and it is the
// difference between the evidence level this install reports as chained and the one it reports as
// anchored. It is a headline claim of the paid tiers and of the procurement page, and no deployed
// install had ever been asked to produce one.
//
// It anchors with the https type against a reference nobody can resolve, on purpose. Checking an
// anchor does not fetch anything: it confirms the chain still reaches the coordinate the anchor
// recorded, and fetching is what a relying party does afterwards, outside this product. Anchoring
// against the public timestamp authority the command defaults to would make this suite fail
// whenever somebody else's server is slow, which would say nothing about SwitchTender.
func (h *harness) checkTheInstallCanAnchorItsOwnChain(phase, namespace, dsn string) {
	const claim = "the deployed install anchors its own chain and reports itself anchored"
	before, err := h.auditLevel()
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	if before.Count == 0 {
		h.fail(phase, claim, fmt.Errorf("%w: there is nothing to anchor", ErrEmptyChain))
		return
	}
	pod, err := h.serverPod(namespace)
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	out, err := h.kubectl("exec", "-n", namespace, pod, "--",
		"switchtender", "audit", "anchor", "--db", dsn,
		"--type", "https", "--ref", "https://supertest.invalid/head.txt")
	if err != nil {
		h.fail(phase, claim, fmt.Errorf("anchor the chain in the pod: %w\n%s", err, oneLine(out)))
		return
	}
	// The anchor has to name the link it fixed. One that records no coordinate anchors nothing,
	// and the chain would go on reporting itself anchored over a mark that points at no entry.
	if !strings.Contains(out, `"link"`) || !strings.Contains(out, `"seq"`) {
		h.fail(phase, claim, fmt.Errorf("%w: the anchor names no link or no sequence: %s",
			ErrNothingRead, oneLine(out)))
		return
	}
	after, err := h.auditLevel()
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	switch {
	case after.Anchored <= before.Anchored:
		h.fail(phase, claim, fmt.Errorf("the anchor was written and the install still counts %d "+
			"of them, so it did not reach the chain the API reads", after.Anchored))
	case after.Level <= before.Level:
		h.fail(phase, claim, fmt.Errorf("the install holds %d anchor(s) and still calls its "+
			"evidence %q, so what it tells an auditor did not move", after.Anchored, after.LevelName))
	case !after.OK:
		h.fail(phase, claim, fmt.Errorf("the chain stopped verifying once it was anchored"))
	default:
		h.pass(phase, claim, fmt.Sprintf("%d entries went from %q to %q with %d anchor(s), "+
			"written by the shipped binary against the install's own database",
			after.Count, before.LevelName, after.LevelName, after.Anchored))
	}
}

// auditVerdict is what an install says about its own evidence when asked.
type auditVerdict struct {
	// OK reports whether the chain verifies.
	OK bool `json:"ok"`
	// Count is how many entries the chain holds.
	Count int `json:"count"`
	// Anchored is how many anchors the chain reaches.
	Anchored int `json:"anchored"`
	// Level is the evidence level as a number, which rises when an anchor is added.
	Level int `json:"level"`
	// LevelName is the same level in the words an auditor reads.
	LevelName string `json:"level_name"`
}

// auditLevel asks the install what it currently claims about its own evidence.
func (h *harness) auditLevel() (auditVerdict, error) {
	var v auditVerdict
	if err := h.apiCall("GET", "/v1/audit/verify", &h.human, nil, &v); err != nil {
		return v, fmt.Errorf("read the install's evidence level: %w", err)
	}
	return v, nil
}
