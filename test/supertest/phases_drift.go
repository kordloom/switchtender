package main

import (
	"fmt"
	"strings"
	"time"
)

// checkDriftIsSeenAndAttributed proves a deployed install notices that its fleet has moved away
// from what the playbook says, and says which check saw it.
//
// A drift check is a dry run: in check mode a task that reports changed is a task that would
// change. The fleet already carries the marker the constructive run left, so the same playbook
// asked for a different marker would change every host, and an install that reports no drift after
// that is one whose drift view is not reading its runs.
//
// Nothing here is arranged to drift by breaking a host. The playbook is the same one that built the
// fleet and the only difference is the value it would write, which is what real drift looks like:
// the machine is fine and the declaration moved.
func (h *harness) checkDriftIsSeenAndAttributed(phase string) {
	const claim = "a dry run shows the fleet has drifted and the drift view names that check"
	before, err := h.driftedTaskTotal()
	if err != nil {
		h.fail(phase, claim, err)
		return
	}

	var created map[string]any
	if err := h.apiCall("POST", "/v1/runs", &h.human, map[string]any{
		"playbook":       "/data/deploy.yml",
		"inventory_id":   h.mustID("community-inventory"),
		"credential_ids": []string{h.mustID("community-credential")},
		"extra_vars":     map[string]any{"marker": fmt.Sprintf("drifted-%d", time.Now().UnixNano())},
		"dry_run":        true,
	}, &created); err != nil {
		h.fail(phase, claim, fmt.Errorf("submit the drift check: %w", err))
		return
	}
	checkID, _ := created["id"].(string)
	if checkID == "" {
		h.fail(phase, claim, fmt.Errorf("%w: the drift check run", ErrNoID))
		return
	}
	if _, err := h.awaitRunStatus(checkID, "succeeded", 180*time.Second); err != nil {
		h.fail(phase, claim, err)
		return
	}
	// A dry run that changed something is the failure this would hide. Check mode reports what
	// would change and changes nothing, so the marker on the machines has to be the one the
	// constructive run left.
	for _, host := range fleetHosts {
		got, ferr := h.fleetExec(host.Pod, "cat", "/home/ops/supertest-marker")
		if ferr != nil {
			h.fail(phase, claim, fmt.Errorf("read %s after the drift check: %w", host.Pod, ferr))
			return
		}
		if strings.Contains(got, "drifted-") {
			h.fail(phase, claim, fmt.Errorf("%s carries the dry run's marker, so the check wrote "+
				"to the fleet instead of reporting what it would write", host.Pod))
			return
		}
	}

	drifted, err := h.driftedHosts()
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	var silent, misattributed []string
	seen := map[string]bool{}
	for _, d := range drifted {
		seen[d.Host] = true
		if d.DriftedTasks == 0 {
			silent = append(silent, d.Host)
		}
		if d.RunID != checkID {
			misattributed = append(misattributed,
				fmt.Sprintf("%s names %s", d.Host, d.RunID))
		}
	}
	after, err := h.driftedTaskTotal()
	if err != nil {
		h.fail(phase, claim, err)
		return
	}
	switch {
	case len(drifted) == 0:
		h.fail(phase, claim, fmt.Errorf("%w: the drift view names no host after a check that "+
			"would have changed every one of them", ErrNothingRead))
	case after <= before:
		h.fail(phase, claim, fmt.Errorf("the drift view counted %d changed tasks before this "+
			"check and %d after, so the check it just ran changed nothing it reports",
			before, after))
	case len(silent) > 0:
		h.fail(phase, claim, fmt.Errorf("the drift view names %s with no changed task, though "+
			"the playbook would rewrite the marker on every host",
			strings.Join(silent, ", ")))
	case len(misattributed) > 0:
		h.fail(phase, claim, fmt.Errorf("the drift view does not attribute the drift to the check "+
			"that saw it, %s, want %s", strings.Join(misattributed, "; "), checkID))
	default:
		h.pass(phase, claim, fmt.Sprintf("%d host(s) report %d changed task(s) between them, every "+
			"one attributed to check %s, and the fleet still carries the marker the real run left",
			len(drifted), after, checkID))
	}
}

// hostDrift is one host's latest drift reading as the install reports it.
type hostDrift struct {
	// Host is the machine the check looked at.
	Host string `json:"host"`
	// DriftedTasks is how many tasks that check would have changed.
	DriftedTasks int `json:"drifted_tasks"`
	// RunID is the check that observed it.
	RunID string `json:"run_id"`
}

// driftedHosts reads what the install currently says about its fleet's drift.
func (h *harness) driftedHosts() ([]hostDrift, error) {
	var view struct {
		Hosts []hostDrift `json:"hosts"`
	}
	if err := h.apiCall("GET", "/v1/drift", &h.human, nil, &view); err != nil {
		return nil, fmt.Errorf("read the drift view: %w", err)
	}
	return view.Hosts, nil
}

// driftedTaskTotal sums the changed tasks the install currently reports, which is what has to move
// when a check finds drift.
func (h *harness) driftedTaskTotal() (int, error) {
	hosts, err := h.driftedHosts()
	if err != nil {
		return 0, err
	}
	total := 0
	for _, d := range hosts {
		total += d.DriftedTasks
	}
	return total, nil
}
