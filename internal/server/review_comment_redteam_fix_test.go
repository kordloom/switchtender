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
	"github.com/kordloom/switchtender/internal/trigger"
)

// TestCommentTheForgeMustConfirm proves a comment acts only as the forge holds it: a comment the
// forge holds under another author, with another body, or written too long ago acts on nothing and
// is not answered, while the same comment held as delivered applies.
func TestCommentTheForgeMustConfirm(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider    string
		HeldAuthor  int64
		HeldBody    string
		HeldAge     time.Duration
		WantResult  string
		WantApplied int
	}{{ // Test 0: The forge holds the comment as delivered, so it applies.
		Provider: trigger.ProviderGitHub, HeldAuthor: forgeApproverOne,
		HeldBody: "/switchtender apply", WantResult: "applied", WantApplied: 1,
	}, { // Test 1: The forge holds it under another author.
		Provider: trigger.ProviderGitHub, HeldAuthor: forgeApproverTwo,
		HeldBody: "/switchtender apply", WantResult: "refused/unconfirmed",
	}, { // Test 2: The forge holds another body.
		Provider: trigger.ProviderGitLab, HeldAuthor: forgeApproverOne,
		HeldBody: "/switchtender plan", WantResult: "refused/unconfirmed",
	}, { // Test 3: The comment is older than a command may act at, a replay.
		Provider: trigger.ProviderGitLab, HeldAuthor: forgeApproverOne,
		HeldBody: "/switchtender apply", HeldAge: 48 * time.Hour, WantResult: "refused/stale",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, test.Provider)
			cs.planReported(t)
			cs.forge.Hold(7, 950, test.HeldAuthor, test.HeldBody)
			if test.HeldAge > 0 {
				cs.forge.SetCommentCreated(950, time.Now().Add(-test.HeldAge))
			}
			_, out := cs.say(t, 950, forgeApproverOne, "/switchtender apply")
			if out["result"] != test.WantResult {
				t.Fatalf("comment answered %v, want %s", out, test.WantResult)
			}
			if n := cs.approvedApplies(t); n != test.WantApplied {
				t.Errorf("approved applies = %d, want %d", n, test.WantApplied)
			}
			if test.WantApplied == 0 {
				if r := cs.replies(); len(r) != 0 {
					t.Errorf("replies = %q, want none for a comment the forge did not confirm", r)
				}
			}
		})
	}
}

// TestCommentWrittenByAnAppIsRefused proves a comment GitHub marks as written by an app on a
// person's behalf never decides, even when the forge holds it as that person's, and its author is
// told once.
func TestCommentWrittenByAnAppIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		App        any
		WantResult string
	}{{ // Test 0: An app wrote it.
		App: map[string]any{"id": 77, "slug": "coding-agent"}, WantResult: "refused/agent",
	}, { // Test 1: The person wrote it.
		App: nil, WantResult: "applied",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, trigger.ProviderGitHub)
			cs.planReported(t)
			cs.forge.Hold(7, 960, forgeApproverOne, "/switchtender apply")
			var payload map[string]any
			if err := json.Unmarshal(cs.comment(960, forgeApproverOne, "/switchtender apply",
				"created", false), &payload); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			payload["comment"].(map[string]any)["performed_via_github_app"] = test.App
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			_, out := cs.deliver(t, raw)
			if out["result"] != test.WantResult {
				t.Fatalf("comment answered %v, want %s", out, test.WantResult)
			}
			if test.WantResult == "refused/agent" {
				if n := cs.approvedApplies(t); n != 0 {
					t.Errorf("approved applies = %d, want 0", n)
				}
				if r := cs.replies(); len(r) != 1 || !strings.Contains(r[0], "app") {
					t.Errorf("replies = %q, want one saying an app wrote it", r)
				}
			}
		})
	}
}

// TestCommentApplyRequesterIsThePullRequestAuthor proves separation of duties is judged against the
// person who wrote the change, read from the forge, whoever comments first: an operator's comment
// proposes the apply in the author's name, the author cannot approve it, and another approver can.
func TestCommentApplyRequesterIsThePullRequestAuthor(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cs := newCommentServer(t, provider)
			if err := cs.policies.Save(ctx, &policy.Policy{ID: "pol_sod", Name: "four eyes",
				Tool: run.ToolTerraform, ExcludeDryRun: true, RequireDistinctApprover: true,
				MaxDestroy: policy.DisabledMaxDestroy}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			cs.forge.SetAuthor(7, forgeApproverOne)
			cs.planReported(t)
			_, out := cs.post(t, 621, forgeOperator, "/switchtender apply")
			if out["result"] != "refused/role" || out["run"] == "" {
				t.Fatalf("operator's apply answered %v, want a held apply", out)
			}
			held := cs.waitRun(t, out["run"], run.StatusPendingApproval)
			if held.ActorUserID != cs.accounts[forgeApproverOne] {
				t.Errorf("requester = %q, want the pull request's author %q", held.ActorUserID,
					cs.accounts[forgeApproverOne])
			}
			if _, out = cs.post(t, 622, forgeApproverOne, "/switchtender apply"); out["result"] !=
				"refused/separation_of_duties" {
				t.Fatalf("author's apply answered %v, want separation of duties", out)
			}
			if _, out = cs.post(t, 623, forgeApproverTwo, "/switchtender apply"); out["result"] !=
				"applied" {
				t.Fatalf("second approver's apply answered %v, want applied", out)
			}
		})
	}
}

// TestCommentApplyBindsToThePlanItsAuthorRead proves an apply approves a plan its author could have
// read: a bare apply is refused, naming the plan id, when the head has more than one reported plan
// or its plan was reported after the comment was written, and an apply naming the id applies
// exactly that plan and records it, while a superseded id is refused.
func TestCommentApplyBindsToThePlanItsAuthorRead(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Replan     bool
		Before     bool
		Name       string
		WantResult string
	}{{ // Test 0: One plan, reported before the comment, so a bare apply is taken.
		WantResult: "applied",
	}, { // Test 1: The comment was written before the plan was reported.
		Before: true, WantResult: "refused/name_the_plan",
	}, { // Test 2: Two plans of the head, so a bare apply could mean either.
		Replan: true, WantResult: "refused/name_the_plan",
	}, { // Test 3: Two plans of the head, and the comment names the latest.
		Replan: true, Name: "latest", WantResult: "applied",
	}, { // Test 4: Two plans of the head, and the comment names the older one.
		Replan: true, Name: "older", WantResult: "refused/superseded",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, trigger.ProviderGitHub)
			first := cs.planReported(t)
			latest := first
			if test.Replan {
				rec, out := cs.post(t, 970, forgeApproverTwo, "/switchtender plan")
				if rec.Code != http.StatusAccepted || out["run"] == "" {
					t.Fatalf("plan comment answered %d %v", rec.Code, out)
				}
				latest = cs.waitRun(t, out["run"], run.StatusSucceeded)
				cs.waitReported(t, latest.ID)
			}
			body := "/switchtender apply"
			switch test.Name {
			case "latest":
				body += " " + review.PlanID(latest.ID)
			case "older":
				body += " " + review.PlanID(first.ID)
			}
			cs.forge.Hold(7, 971, forgeApproverOne, body)
			if test.Before {
				cs.forge.SetCommentCreated(971, time.Now().Add(-time.Hour))
			}
			_, out := cs.say(t, 971, forgeApproverOne, body)
			if out["result"] != test.WantResult {
				t.Fatalf("apply answered %v, want %s", out, test.WantResult)
			}
			if test.WantResult == "refused/name_the_plan" {
				if r := cs.replies(); len(r) != 1 ||
					!strings.Contains(r[0], "/switchtender apply "+review.PlanID(latest.ID)) {
					t.Errorf("replies = %q, want one naming plan %s", r, review.PlanID(latest.ID))
				}
			}
			if test.WantResult != "applied" {
				return
			}
			records, err := cs.decisions.ForRun(context.Background(), out["run"])
			if err != nil || len(records) != 1 || records[0].Comment == nil ||
				records[0].Comment.PlanRunID != latest.ID {
				t.Errorf("decision = %+v, %v, want one recording plan %s", records, err, latest.ID)
			}
		})
	}
}

// TestCommentScopedToATemplate proves "-p" scopes a command to one template's trigger: a comment
// naming another template is left alone, and one naming this template applies, its reply naming
// the template and the plan.
func TestCommentScopedToATemplate(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body        string
		WantIgnored bool
	}{{ // Test 0: Another template.
		Body: "/switchtender apply -p storage", WantIgnored: true,
	}, { // Test 1: This template.
		Body: "/switchtender apply -p network",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, trigger.ProviderGitHub)
			plan := cs.planReported(t)
			_, out := cs.post(t, 980, forgeApproverOne, test.Body)
			if test.WantIgnored {
				if out["ignored"] != "the comment names another template" {
					t.Fatalf("comment answered %v, want ignored", out)
				}
				if n := len(cs.applyRuns(t)); n != 0 {
					t.Errorf("applies = %d, want 0", n)
				}
				return
			}
			if out["result"] != "applied" {
				t.Fatalf("comment answered %v, want applied", out)
			}
			want := "Template network: plan " + review.PlanID(plan.ID)
			if r := cs.replies(); len(r) != 1 || !strings.Contains(r[0], want) {
				t.Errorf("replies = %q, want one carrying %q", r, want)
			}
		})
	}
}

// TestCommentCommandsAreRateLimitedPerAuthor proves one forge account's commands through one
// trigger are bounded: past the limit a command is answered 429 and leaves no record behind, while
// another account is unaffected.
func TestCommentCommandsAreRateLimitedPerAuthor(t *testing.T) {
	t.Parallel()
	cs := newCommentServer(t, trigger.ProviderGitHub)
	for i := range commentRateMax {
		if rec, _ := cs.say(t, int64(9500+i), forgeStranger, "/switchtender apply"); rec.Code !=
			http.StatusAccepted {
			t.Fatalf("comment %d answered %d, want 202", i, rec.Code)
		}
	}
	rec, _ := cs.say(t, 9600, forgeStranger, "/switchtender apply")
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("comment past the limit answered %d, want 429", rec.Code)
	}
	if chainHasPath(t, cs.audits, "/hooks/"+cs.triggerID+"/review/7/comment/9600/apply") {
		t.Errorf("a comment past the limit was recorded")
	}
	if rec, _ := cs.say(t, 9601, forgeStranger+1, "/switchtender apply"); rec.Code !=
		http.StatusAccepted {
		t.Errorf("another account's comment answered %d, want 202", rec.Code)
	}
}

// TestForgedCommentHasNoEffect proves a comment the forge never held, signed by somebody holding
// the trigger's webhook secret, has no effect at all: no proposal, no plan, no record in the store,
// no reply, and no chain entry beyond the one recording its arrival, whatever account it names.
func TestForgedCommentHasNoEffect(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider string
		Author   int64
		Body     string
	}{{ // Test 0: A forged apply naming an approver.
		Provider: trigger.ProviderGitHub, Author: forgeApproverOne, Body: "/switchtender apply",
	}, { // Test 1: A forged apply naming an operator, who could only propose.
		Provider: trigger.ProviderGitLab, Author: forgeOperator, Body: "/switchtender apply",
	}, { // Test 2: A forged plan naming an operator.
		Provider: trigger.ProviderGitHub, Author: forgeOperator, Body: "/switchtender plan",
	}, { // Test 3: A forged apply naming an unlinked account, which would be told how to link.
		Provider: trigger.ProviderGitLab, Author: forgeStranger, Body: "/switchtender apply",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cs := newCommentServer(t, test.Provider)
			cs.planReported(t)
			before, err := cs.runs.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			chainBefore, err := cs.audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			cs.say(t, 990, test.Author, test.Body)
			after, err := cs.runs.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(after) != len(before) {
				t.Errorf("a forged comment created %d runs", len(after)-len(before))
			}
			if r := cs.replies(); len(r) != 0 {
				t.Errorf("replies = %q, want none", r)
			}
			done, err := cs.srv.reviews.Commanded(ctx, cs.reviewTrigger(t),
				&review.CommentEvent{Provider: test.Provider, Repository: cs.repository(),
					Number: 7, CommentID: 990})
			if err != nil || done {
				t.Errorf("a forged comment left its record behind: %v, %v", done, err)
			}
			chainAfter, err := cs.audits.Chain(ctx)
			if err != nil {
				t.Fatalf("Chain() error = %v", err)
			}
			var added []string
			for _, e := range chainAfter[len(chainBefore):] {
				added = append(added, e.Path)
			}
			want := fmt.Sprintf("/hooks/%s/review/7/comment/990/%s", cs.triggerID,
				strings.TrimPrefix(test.Body, "/switchtender "))
			if len(added) != 1 || added[0] != want {
				t.Errorf("chain entries added = %q, want only the arrival %s", added, want)
			}
		})
	}
}

// TestCommentRefusesAProposalItDidNotMake proves an apply is reused only when it is the one this
// path proposed from the plan: a run holding the plan's apply key that was proposed from something
// else, or for another pull request, is refused and never approved.
func TestCommentRefusesAProposalItDidNotMake(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Source     string
		Label      string
		WantResult string
	}{{ // Test 0: A run under the key with another source.
		Source: "api", Label: "7", WantResult: "refused/proposal_mismatch",
	}, { // Test 1: A run under the key for another pull request.
		Source: review.ApplySource, Label: "8", WantResult: "refused/proposal_mismatch",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cs := newCommentServer(t, trigger.ProviderGitHub)
			plan := cs.planReported(t)
			planted := &run.Run{ID: run.NewID(), Playbook: plan.Playbook, Inventory: plan.Inventory,
				Status: run.StatusPendingApproval, CreatedAt: time.Now(),
				IdempotencyKey: applyKeyFor(plan.ID), ProposedFrom: plan.ID,
				Source: test.Source, SourceID: plan.ID, PinnedCommit: plan.PinnedCommit,
				Labels: map[string]string{review.LabelPullRequest: test.Label}}
			if err := cs.runs.Save(ctx, planted); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			_, out := cs.post(t, 995, forgeApproverOne, "/switchtender apply")
			if out["result"] != test.WantResult {
				t.Fatalf("apply answered %v, want %s", out, test.WantResult)
			}
			got, err := cs.runs.Get(ctx, planted.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if got.Status != run.StatusPendingApproval {
				t.Errorf("the planted run is %s, want it left held", got.Status)
			}
			if r := cs.replies(); len(r) != 1 || !strings.Contains(r[0], planted.ID) {
				t.Errorf("replies = %q, want one naming %s", r, planted.ID)
			}
		})
	}
}
