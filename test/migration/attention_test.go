package migration

import (
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// attentionItem is the part of an item GET /v1/attention lists that the scenario reads.
type attentionItem struct {
	// Key identifies the item.
	Key string `json:"key"`
	// Kind is run, workflow, split, or schedule.
	Kind string `json:"kind"`
	// Blocker is the item's main blocker.
	Blocker string `json:"blocker"`
	// Main is the main condition.
	Main struct {
		// EligibleWorkers is how many connected workers serve the queue.
		EligibleWorkers *int `json:"eligible_workers"`
		// Queue is the queue a waiting run is on.
		Queue *string `json:"queue"`
		// Approval details an approval.
		Approval *struct {
			// Scope is run or workflow_step.
			Scope string `json:"scope"`
			// WorkflowState says whether the workflow is paused.
			WorkflowState string `json:"workflow_state"`
			// DecisionID is the id approve and reject take.
			DecisionID string `json:"decision_id"`
			// Approvers says who may approve.
			Approvers struct {
				// Agents reports whether an agent may approve.
				Agents bool `json:"agents"`
			} `json:"approvers"`
		} `json:"approval"`
	} `json:"main"`
}

// attentionPage is GET /v1/attention as the scenario reads it.
type attentionPage struct {
	// Counts are the four counts.
	Counts map[string]int `json:"counts"`
	// Items are the listed items.
	Items []attentionItem `json:"items"`
}

// attention reads what needs attention, as actor sees it.
func (in *install) attention(s *server, actor string) attentionPage {
	in.t.Helper()
	var page attentionPage
	in.must(s, actor, "GET", "/v1/attention", nil, 200).decode(in.t, &page)
	return page
}

// waitAttention waits until key is listed with the given main blocker and returns its item.
func (in *install) waitAttention(s *server, actor, key, blocker string) attentionItem {
	in.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		for _, it := range in.attention(s, actor).Items {
			if it.Key == key && it.Blocker == blocker {
				return it
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	in.t.Fatalf("%s was never listed as %s", key, blocker)
	return attentionItem{}
}

// waitNotListed waits until key is no longer listed at all.
func (in *install) waitNotListed(s *server, actor, key string) {
	in.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		listed := false
		for _, it := range in.attention(s, actor).Items {
			listed = listed || it.Key == key
		}
		if !listed {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	in.t.Fatalf("%s is still listed as needing attention after it moved", key)
}

// TestNeedsAttentionNamesWhatStopsImportedWork is the attention scenario. On an imported install a
// real server has to name a workflow waiting at its imported approval node as an approval, with the
// step's own id and the workflow paused, name a run on a queue no worker serves as having none,
// while the default queue the server's own dispatcher serves reads as served, and drop each once it
// moves.
func TestNeedsAttentionNamesWhatStopsImportedWork(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onSQLite})
	a := in.startServer("a")

	wf := in.launched(a, "operator", "release", nil)
	step := in.waitPending(a, "operator", wf.ID)
	// The step is listed as soon as the walk reaches it, and the workflow parks a moment later, once
	// nothing else is left to run. Its approval reads as paused from then on.
	if got := in.waitStatus(a, wf.ID, "pending_approval", "succeeded", "failed",
		"canceled"); got.Status != "pending_approval" {
		t.Fatalf("the workflow reached %q, want it parked at its approval step: %s", got.Status,
			describe(got.Raw))
	}
	item := in.waitAttention(a, "operator", wf.ID, "approval_needed")
	if item.Main.Approval == nil {
		t.Fatalf("the waiting workflow's item carries no approval: %+v", item)
	}
	type approvalView struct {
		// Kind is the item's kind.
		Kind string
		// Scope is the approval's scope.
		Scope string
		// WorkflowState is the workflow's state.
		WorkflowState string
		// DecisionID is the id a decision takes.
		DecisionID string
		// Agents reports whether an agent may approve.
		Agents bool
	}
	got := approvalView{Kind: item.Kind, Scope: item.Main.Approval.Scope,
		WorkflowState: item.Main.Approval.WorkflowState, DecisionID: item.Main.Approval.DecisionID,
		Agents: item.Main.Approval.Approvers.Agents}
	want := approvalView{Kind: "workflow", Scope: "workflow_step", WorkflowState: "paused",
		DecisionID: step.ID}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("the waiting workflow's approval (-want +got):\n%s", diff)
	}

	// A run on a queue nothing serves has no worker, and says so with the queue and a zero count.
	unserved := in.must(a, "admin", "POST", "/v1/runs", map[string]any{
		"tool": "bash", "command": "true", "queue": "unserved",
	}, 202)
	queued := decodeRun(t, unserved.Body)
	lonely := in.waitAttention(a, "operator", queued.ID, "no_worker")
	if lonely.Main.Queue == nil || *lonely.Main.Queue != "unserved" ||
		lonely.Main.EligibleWorkers == nil || *lonely.Main.EligibleWorkers != 0 {
		t.Errorf("the unserved run's item = %+v, want queue unserved with no eligible worker", lonely)
	}
	if n := in.attention(a, "operator").Counts["no_worker"]; n != 1 {
		t.Errorf("no worker count = %d, want 1: the default queue the server serves is not missing", n)
	}

	// Each item leaves the view once it moves.
	in.must(a, "approver", "POST", "/v1/runs/"+step.ID+"/approve",
		map[string]any{"state_digest": step.StateDigest}, 200)
	in.waitNotListed(a, "operator", wf.ID)
	in.must(a, "admin", "POST", "/v1/runs/"+queued.ID+"/cancel", nil, 200, 202)
	in.waitNotListed(a, "operator", queued.ID)
	in.waitDone(a, wf.ID)
}
