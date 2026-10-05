package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/run"
)

// eventually polls cond until it holds or ten seconds pass.
func eventually(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// runReport reads a run through the API and returns what it carries about its pull request, nil
// when it carries nothing.
func (rs *reviewServer) runReport(t *testing.T, id string) *review.ReportState {
	t.Helper()
	rec := rs.do(t, http.MethodGet, "/v1/runs/"+id, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /v1/runs/%s = %d %s", id, rec.Code, rec.Body.String())
	}
	var body struct {
		// ID is the run id, which proves the run itself is still what answered.
		ID string `json:"id"`
		// PullRequestReport is the field under test.
		PullRequestReport *review.ReportState `json:"pull_request_report"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode run: %v", err)
	}
	if body.ID != id {
		t.Fatalf("GET /v1/runs/%s answered run %q", id, body.ID)
	}
	return body.PullRequestReport
}

// TestReviewHeldPlanSaysItWaitsAndLinksToItsApproval proves a plan held by a rule is reported as
// waiting for approval, with a link to the plan where an approver releases it, and that the one
// line the docs give, exclude_dry_run on the same rule, lets the plan run unattended.
func TestReviewHeldPlanSaysItWaitsAndLinksToItsApproval(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// ExcludeDryRun is the rule's exclude_dry_run setting.
		ExcludeDryRun bool
		// WantHeld is whether the plan waits for approval.
		WantHeld bool
	}{{ // Test 0: A rule holding Terraform runs holds the plan, and the comment links to it.
		ExcludeDryRun: false, WantHeld: true,
	}, { // Test 1: The same rule with exclude_dry_run lets the plan run.
		ExcludeDryRun: true, WantHeld: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rs := newReviewServer(t, reviewSetup{})
			rule := &policy.Policy{ID: "pol_hold", Name: "terraform needs a look",
				Tool: run.ToolTerraform, ExcludeDryRun: test.ExcludeDryRun,
				MaxDestroy: policy.DisabledMaxDestroy}
			if err := rs.policies.Save(context.Background(), rule); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
			planned := rs.planRun(t, rec)
			if !test.WantHeld {
				rs.waitStatus(t, rs.headSHA, successState)
				rs.srv.reviews.Wait()
				return
			}
			eventually(t, "the held comment", func() bool {
				c := rs.forge.Comments(7)
				return len(c) == 1 && strings.Contains(c[0].Body, "Plan waiting for approval")
			})
			body := rs.forge.Comments(7)[0].Body
			link := "[Open the plan in SwitchTender](https://st.example.com/ui/runs/" +
				planned.ID + ")"
			if !strings.Contains(body, link) || !strings.Contains(body, "to approve or reject it") {
				t.Errorf("the held comment does not link to the plan's approval:\n%s", body)
			}
			st := rs.waitStatus(t, rs.headSHA, "pending")
			if !strings.HasPrefix(st.Description, "Plan waiting for approval") ||
				!strings.HasSuffix(st.TargetURL, "/ui/runs/"+planned.ID) {
				t.Errorf("held status = %q linked to %q, want it waiting and linked to the plan",
					st.Description, st.TargetURL)
			}
		})
	}
}

// TestReviewHookKeepsTheNewestPushWhenWebhooksArriveOutOfOrder proves the comment ends on the
// newest push when the forge delivers an older push's webhook after the newer one's. The older
// push's plan is the later run, and it refuses to run because the pull request moved on, and none
// of its reports take the comment from the newer push's plan. Its own commit still gets statuses.
func TestReviewHookKeepsTheNewestPushWhenWebhooksArriveOutOfOrder(t *testing.T) {
	t.Parallel()
	rs := newReviewServer(t, reviewSetup{})
	older := rs.headSHA
	newer := rs.origin.Push(t, map[string]string{
		"infra/plan.txt": "Plan: 0 to add, 0 to change, 0 to destroy.\n",
	})
	rs.forge.SetHead(7, newer)

	rec := rs.fire(t, rs.eventName(), rs.payload(rs.pushed(), newer, ""), "")
	fresh := rs.planRun(t, rec)
	rs.waitStatus(t, newer, successState)
	rs.srv.reviews.Wait()
	settled := rs.forge.Comments(7)
	if len(settled) != 1 {
		t.Fatalf("comments = %+v, want the newer push's one comment", settled)
	}

	rec = rs.fire(t, rs.eventName(), rs.payload(rs.opened(), older, ""), "")
	late := rs.planRun(t, rec)
	rs.waitStatus(t, older, "failure")
	rs.srv.reviews.Wait()

	comments := rs.forge.Comments(7)
	if len(comments) != 1 || comments[0].Edits != settled[0].Edits {
		t.Fatalf("comments = %+v, want the newer push's comment left as it was", comments)
	}
	body := comments[0].Body
	if !strings.Contains(body, fresh.ID) || !strings.Contains(body, newer[:12]) ||
		strings.Contains(body, late.ID) {
		t.Errorf("the comment does not describe the newest push %s:\n%s", newer[:12], body)
	}
	got, err := rs.runs.Get(context.Background(), late.ID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.Status != run.StatusFailed || !strings.Contains(got.Error, newer) {
		t.Errorf("late plan = %s %q, want it refused for a commit the pull request moved past",
			got.Status, got.Error)
	}
}

// TestReviewRunShowsAFailingReport proves a forge failure is surfaced on the plan's run: while the
// forge is down the run's API answer carries the failure and the pending retry, and once it is back
// the run shows the report that landed.
func TestReviewRunShowsAFailingReport(t *testing.T) {
	t.Parallel()
	rs := newReviewServer(t, reviewSetup{})
	rs.forge.SetOutage(http.StatusBadGateway)
	rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
	planned := rs.planRun(t, rec)

	eventually(t, "the failure on the run", func() bool {
		state := rs.runReport(t, planned.ID)
		return state != nil && state.Attempts >= 2
	})
	state := rs.runReport(t, planned.ID)
	if state.PullRequest != 7 || state.Done || state.RetryAt == nil ||
		!strings.Contains(state.LastError, "502") {
		t.Errorf("run shows %+v during the outage, want the pull request, the forge's 502, and a "+
			"pending retry", state)
	}

	rs.forge.SetOutage(0)
	rs.waitStatus(t, rs.headSHA, successState)
	rs.srv.reviews.Wait()
	state = rs.runReport(t, planned.ID)
	if !state.Done || state.Phase != review.PhaseSucceeded || state.StatusState != successState ||
		state.LastError != "" || state.ReportedAt == nil || state.CommentSHA256 == "" {
		t.Errorf("run shows %+v after recovery, want the final report landed", state)
	}

	// A run that is not a review plan carries nothing about a pull request.
	other := &run.Run{ID: "run_plain", Playbook: "site.yml", Status: run.StatusSucceeded,
		Source: "api", CreatedAt: time.Now()}
	if err := rs.runs.Save(context.Background(), other); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if got := rs.runReport(t, "run_plain"); got != nil {
		t.Errorf("a plain run carries a pull request report: %+v", got)
	}
}
