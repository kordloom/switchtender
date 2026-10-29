package main

import (
	"fmt"
	"time"
)

// attentionItem is the part of one GET /v1/attention item the checks read.
type attentionItem struct {
	// Key identifies the item, the run id for a run.
	Key string `json:"key"`
	// Blocker is the item's main blocker, such as no_worker.
	Blocker string `json:"blocker"`
	// Main is the main condition.
	Main struct {
		// Queue is the queue a waiting run is on.
		Queue *string `json:"queue"`
		// EligibleWorkers is how many connected workers serve that queue.
		EligibleWorkers *int `json:"eligible_workers"`
	} `json:"main"`
}

// attentionPage is GET /v1/attention as the checks read it.
type attentionPage struct {
	// Counts are the per-blocker counts.
	Counts map[string]int `json:"counts"`
	// Items are the listed items.
	Items []attentionItem `json:"items"`
}

// checkAnUnservedQueueNeedsAttention submits a run to a queue nothing serves and requires the
// attention view to name it as waiting for a worker, with the queue and a zero count, while the
// queue this install's worker serves and the default queue its server serves read as served. It
// runs on the Team install, which has a dedicated worker on one named queue and no other, so the
// one missing worker the view reports is the one this check made missing.
func (h *harness) checkAnUnservedQueueNeedsAttention(phase string) {
	const claim = "a run on a queue no worker serves is named as waiting for one"
	var queued map[string]any
	if err := h.apiCall("POST", "/v1/runs", &h.human, map[string]any{
		"tool": "bash", "command": "true", "queue": "unserved",
	}, &queued); err != nil {
		h.fail(phase, claim, fmt.Errorf("submit the run: %w", err))
		return
	}
	id, _ := queued["id"].(string)
	if id == "" {
		h.fail(phase, claim, fmt.Errorf("%w: the run on the unserved queue", ErrNoID))
		return
	}

	var item attentionItem
	var page attentionPage
	err := h.waitFor("run "+id+" to be listed as waiting for a worker", 60*time.Second, func() error {
		page = attentionPage{}
		if err := h.apiCall("GET", "/v1/attention", &h.human, nil, &page); err != nil {
			return err
		}
		for _, it := range page.Items {
			if it.Key == id && it.Blocker == "no_worker" {
				item = it
				return nil
			}
		}
		return fmt.Errorf("%d items, none for the run as no_worker", len(page.Items))
	})
	switch {
	case err != nil:
		h.fail(phase, claim, err)
	case item.Main.Queue == nil || *item.Main.Queue != "unserved":
		h.fail(phase, claim, fmt.Errorf("the item names queue %v, want unserved", item.Main.Queue))
	case item.Main.EligibleWorkers == nil || *item.Main.EligibleWorkers != 0:
		h.fail(phase, claim, fmt.Errorf("the item counts %v eligible workers, want 0",
			item.Main.EligibleWorkers))
	default:
		h.pass(phase, claim, "run "+id+": blocker no_worker, queue unserved, 0 eligible workers")
	}

	const servedClaim = "the queues this install serves are not reported as missing a worker"
	switch n := page.Counts["no_worker"]; {
	case err != nil:
		h.fail(phase, servedClaim, fmt.Errorf("not asked: the unserved run was never listed: %w", err))
	case n != 1:
		h.fail(phase, servedClaim, fmt.Errorf("no_worker counts %d, want 1: only the unserved "+
			"queue lacks a worker", n))
	default:
		h.pass(phase, servedClaim, "no_worker counts 1: the supertest queue's worker and the "+
			"server's own pool both count as serving")
	}

	const goneClaim = "the item leaves the view once the run moves"
	if err := h.apiCall("POST", "/v1/runs/"+id+"/cancel", &h.human, nil, nil); err != nil {
		h.fail(phase, goneClaim, fmt.Errorf("cancel the run: %w", err))
		return
	}
	err = h.waitFor("run "+id+" to leave the attention view", 60*time.Second, func() error {
		var after attentionPage
		if err := h.apiCall("GET", "/v1/attention", &h.human, nil, &after); err != nil {
			return err
		}
		for _, it := range after.Items {
			if it.Key == id {
				return fmt.Errorf("still listed as %s", it.Blocker)
			}
		}
		return nil
	})
	if err != nil {
		h.fail(phase, goneClaim, err)
		return
	}
	h.pass(phase, goneClaim, "canceled, and no longer listed")
}
