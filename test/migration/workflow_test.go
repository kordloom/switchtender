package migration

import (
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
)

// pendingStep is one workflow approval step waiting for a person, as GET /v1/approvals lists it.
type pendingStep struct {
	// ID is the step's own run id.
	ID string `json:"id"`
	// RunID is the workflow's run id.
	RunID string `json:"run_id"`
	// Step is the step's name.
	Step string `json:"step"`
	// RequestedBy names who launched the workflow.
	RequestedBy string `json:"requested_by"`
	// RequestedByType is how they authenticated.
	RequestedByType string `json:"requested_by_type"`
	// OnApprove names what an approval runs next.
	OnApprove []string `json:"on_approve"`
	// OnDeny names what a denial runs next.
	OnDeny []string `json:"on_deny"`
	// StateDigest is the state a decision binds to.
	StateDigest string `json:"state_digest"`
}

// waitPending waits until the workflow waits at an approval step and returns the step, as actor
// sees it.
func (in *install) waitPending(s *server, actor, workflowID string) pendingStep {
	in.t.Helper()
	deadline := time.Now().Add(waitLimit)
	for time.Now().Before(deadline) {
		var page struct {
			// Approvals are the waiting steps.
			Approvals []pendingStep `json:"approvals"`
		}
		in.must(s, actor, "GET", "/v1/approvals", nil, 200).decode(in.t, &page)
		for _, p := range page.Approvals {
			if p.RunID == workflowID {
				return p
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	in.t.Fatalf("workflow %s never waited at an approval step", workflowID)
	return pendingStep{}
}

// noPending fails the scenario when the workflow is still listed as waiting.
func (in *install) noPending(s *server, workflowID string) {
	in.t.Helper()
	var page struct {
		// Approvals are the waiting steps.
		Approvals []pendingStep `json:"approvals"`
	}
	in.must(s, "admin", "GET", "/v1/approvals", nil, 200).decode(in.t, &page)
	for _, p := range page.Approvals {
		if p.RunID == workflowID {
			in.t.Errorf("workflow %s is still listed as waiting at %s after its decision", workflowID,
				p.Step)
		}
	}
}

// stepRuns returns the workflow's step runs by step name.
func (in *install) stepRuns(s *server, workflowID string) map[string][]runRecord {
	in.t.Helper()
	var page struct {
		// Steps are the step runs.
		Steps []map[string]any `json:"steps"`
	}
	in.must(s, "admin", "GET", "/v1/runs/"+workflowID+"/steps", nil, 200).decode(in.t, &page)
	out := map[string][]runRecord{}
	for _, st := range page.Steps {
		name := str(st["step_name"])
		out[name] = append(out[name], runRecord{ID: str(st["id"]), Status: str(st["status"]), Raw: st})
	}
	return out
}

// executions returns how many times a step's play ran on web1, read from the marker it appends to.
func (in *install) executions(step string) int {
	in.t.Helper()
	hosts := in.marked(step)
	if len(hosts) == 0 {
		return 0
	}
	return strings.Count(in.marker(step, "web1"), "\n")
}

// requireSteps fails the scenario unless each step's play ran exactly the given number of times
// and each executed step has exactly one run record.
func (in *install) requireSteps(s *server, workflowID string, want map[string]int) {
	in.t.Helper()
	runs := in.stepRuns(s, workflowID)
	got := map[string]int{}
	for step := range want {
		got[step] = in.executions(step)
		if want[step] > 0 && len(runs[step]) != 1 {
			in.t.Errorf("step %s has %d run records, want exactly one", step, len(runs[step]))
		}
	}
	if diff := cmp.Diff(want, got); diff != "" {
		in.t.Errorf("times each step's play ran (-want +got):\n%s", diff)
	}
}

// TestImportedWorkflowApprovalSurvivesRestartAndResumesOnce is scenario four. AWX exports a
// workflow whose approval node gates shipping and whose failure path pages someone. On two servers
// sharing one PostgreSQL database, the workflow has to pause at the step, keep waiting through a
// restart of the server that launched it, and resume exactly once when a person approves on the
// other server. A denial and a timeout both take the deny path and never ship.
//
//nolint:funlen // Scenario function.
func TestImportedWorkflowApprovalSurvivesRestartAndResumesOnce(t *testing.T) {
	t.Parallel()
	in := newInstall(t, installOptions{Store: onPostgres})
	a := in.startServer("a")
	b := in.startServer("b")

	// Approve: launched on a, a restarts mid-pause, approved on b.
	wf := in.launched(a, "operator", "release", nil)
	step := in.waitPending(b, "operator", wf.ID)
	if step.Step != "approve" || len(step.OnApprove) == 0 || len(step.OnDeny) == 0 {
		t.Fatalf("the waiting step = %+v, want the imported approval node with both paths", step)
	}
	in.requireSteps(b, wf.ID, map[string]int{"build": 1, "ship": 0, "page": 0})
	a.stop()
	a = in.startServer("a")
	if again := in.waitPending(a, "operator", wf.ID); again.ID != step.ID {
		t.Fatalf("after the restart the workflow waits at %s, want the same step %s", again.ID, step.ID)
	}
	if got := in.getRun(a, wf.ID); terminal(got.Status) {
		t.Fatalf("the workflow ended across the restart: %s", describe(got.Raw))
	}
	in.must(b, "approver", "POST", "/v1/runs/"+step.ID+"/approve",
		map[string]any{"state_digest": step.StateDigest}, 200)
	if done := in.waitDone(b, wf.ID); done.Status != "succeeded" {
		t.Fatalf("the approved workflow = %s, want succeeded: %s", done.Status, describe(done.Raw))
	}
	// Both replicas sweep for parked workflows on their janitor tick, so a second resume would land
	// within one tick of the first. Waiting past it is what makes "exactly once" a measurement.
	time.Sleep(12 * time.Second)
	in.requireSteps(a, wf.ID, map[string]int{"build": 1, "ship": 1, "page": 0})
	in.noPending(a, wf.ID)

	// Deny: the deny path runs and nothing ships.
	in.clearMarkers()
	denied := in.launched(b, "operator", "release", nil)
	dstep := in.waitPending(a, "operator", denied.ID)
	in.must(a, "approver", "POST", "/v1/runs/"+dstep.ID+"/reject",
		map[string]any{"state_digest": dstep.StateDigest}, 200)
	in.waitDone(a, denied.ID)
	time.Sleep(12 * time.Second)
	in.requireSteps(b, denied.ID, map[string]int{"build": 1, "ship": 0, "page": 1})
	in.noPending(b, denied.ID)

	// Timeout: nobody answers, and the deny path runs.
	in.clearMarkers()
	late := in.launched(a, "operator", "release with deadline", nil)
	lstep := in.waitPending(b, "operator", late.ID)
	in.waitDone(b, late.ID)
	time.Sleep(12 * time.Second)
	in.requireSteps(a, late.ID, map[string]int{"build": 1, "ship": 0, "page": 1})
	in.noPending(a, late.ID)

	// A step's outcome is rolled into its workflow's, so each workflow's receipt is the one verified,
	// and an approval step's decision is read from the chain it was committed to.
	ev := in.checkEvidence(b, wf.ID, denied.ID, late.ID)
	requireStepDecision(t, ev, step.ID, "approved", "approver-laptop")
	requireStepDecision(t, ev, dstep.ID, "rejected", "approver-laptop")
	requireStepDecision(t, ev, lstep.ID, "timed_out", "")
	for _, c := range []struct {
		// ID is the workflow run.
		ID string
		// Children are the step outcomes its receipt must record, by step name.
		Children map[string]string
	}{
		{wf.ID, map[string]string{"build": "succeeded", "approve": "succeeded", "ship": "succeeded"}},
		{denied.ID, map[string]string{"build": "succeeded", "approve": "rejected", "page": "succeeded"}},
		{late.ID, map[string]string{"build": "succeeded", "approve": "failed", "page": "succeeded"}},
	} {
		rec := ev.Receipts[c.ID]
		requireRecord(t, rec, recordWant{Launcher: "operator-laptop", OnBehalfOf: "operator"})
		requireChildren(t, rec, c.Children)
		if got := str(rec.outcome(t).Spec["limit"]); got != "web1" {
			t.Errorf("the workflow's receipt binds limit %q, want web1", got)
		}
	}
	requireReceiptDecision(t, ev.Receipts[wf.ID], step.ID, "approved", "approver-laptop", "approver")
	requireReceiptDecision(t, ev.Receipts[denied.ID], dstep.ID, "rejected", "approver-laptop",
		"approver")
	requireReceiptDecision(t, ev.Receipts[late.ID], lstep.ID, "timed_out", "system:approval-timeout",
		"operator-laptop")
}

// requireChildren fails the scenario unless a workflow's receipt records exactly these steps as
// having run, each with its status. A step on the path not taken must be absent.
func requireChildren(t *testing.T, rec *receipt, want map[string]string) {
	t.Helper()
	got := map[string]string{}
	list, _ := rec.outcome(t).Outcome["children"].([]any)
	for _, c := range list {
		m, _ := c.(map[string]any)
		got[str(m["name"])] = str(m["status"])
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("steps the workflow's receipt records (-want +got):\n%s", diff)
	}
}

// requireReceiptDecision fails the scenario unless the workflow's receipt discloses the decision on
// its approval step, by the decider, on behalf of the account named.
func requireReceiptDecision(t *testing.T, rec *receipt, stepID, verdict, decider, onBehalfOf string) {
	t.Helper()
	found := rec.find("/steps/" + stepID + "/decision/" + verdict)
	if len(found) != 1 {
		t.Fatalf("the workflow's receipt discloses %d %s decisions on %s, want one: %s", len(found),
			verdict, stepID, describe(rec.Claims))
	}
	if c := found[0]; c.Actor != decider || c.OnBehalfOf != onBehalfOf {
		t.Errorf("the receipt's %s decision is by %s on behalf of %q, want %s on behalf of %q",
			verdict, c.Actor, c.OnBehalfOf, decider, onBehalfOf)
	}
}

// requireStepDecision fails the scenario unless the chain records exactly one decision on the
// approval step, made by decider with the given result. An empty decider is a timeout, which the
// chain must record as the system ending the wait rather than as any person's decision.
func requireStepDecision(t *testing.T, ev *evidence, stepID, result, decider string) {
	t.Helper()
	var decisions []auditEntry
	for _, e := range ev.entries("/steps/" + stepID + "/decision/") {
		if !strings.HasSuffix(e.Path, "/requested") {
			decisions = append(decisions, e)
		}
	}
	if len(decisions) != 1 {
		t.Fatalf("the chain records %d decisions on step %s, want exactly one: %s", len(decisions),
			stepID, describe(decisions))
	}
	d := decisions[0]
	if decider == "" {
		if d.ActorType != "system" || !strings.HasSuffix(d.Path, "/"+result) {
			t.Errorf("a timed out step's decision is %s by %s (%s), want %s by the system", d.Path,
				d.Actor, d.ActorType, result)
		}
		return
	}
	if d.Actor != decider || !strings.HasSuffix(d.Path, "/"+result) {
		t.Errorf("the decision on step %s is %s by %s, want %s by %s", stepID, d.Path, d.Actor,
			result, decider)
	}
}
