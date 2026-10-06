package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// Forge account ids the comment tests link, and one they leave unlinked.
const (
	// forgeApproverOne is linked to the first admin.
	forgeApproverOne int64 = 1001
	// forgeApproverTwo is linked to the second admin.
	forgeApproverTwo int64 = 1002
	// forgeOperator is linked to an operator, who may launch work but not approve it.
	forgeOperator int64 = 1003
	// forgeStranger is linked to nobody.
	forgeStranger int64 = 4242
)

// applyLog records what the saved-plan runner was handed.
type applyLog struct {
	// mu guards everything below.
	mu sync.Mutex
	// plans counts the plans it ran.
	plans int
	// saved holds the plan file each plan saved, in order.
	saved [][]byte
	// applied holds the plan file each apply was handed, in order.
	applied [][]byte
}

// snapshot returns copies of what the runner recorded.
func (l *applyLog) snapshot() (plans int, saved, applied [][]byte) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.plans, append([][]byte(nil), l.saved...), append([][]byte(nil), l.applied...)
}

// savedPlanRunner stands in for terraform with saved plans: a plan prints the plan text the
// checkout holds, saves a plan file naming that text, and reports pending changes so the plan file
// is kept, and an apply records the plan file it was handed and changes nothing else.
func savedPlanRunner(log *applyLog) roundhouse.Runner {
	return roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
		out io.Writer) (roundhouse.Result, error) {
		text, _ := os.ReadFile(filepath.Join(spec.Dir, "infra", "plan.txt"))
		log.mu.Lock()
		defer log.mu.Unlock()
		if spec.DryRun {
			log.plans++
			saved := []byte("saved plan of: " + string(text))
			log.saved = append(log.saved, append([]byte(nil), saved...))
			_, _ = fmt.Fprintf(out, "Terraform will perform the following actions:\n%s", text)
			return roundhouse.Result{ExitCode: 0, Drift: true, PlanFile: saved}, nil
		}
		applied, _ := os.ReadFile(spec.PlanFile)
		log.applied = append(log.applied, applied)
		_, _ = fmt.Fprintf(out, "Apply complete!\n")
		return roundhouse.Result{ExitCode: 0}, nil
	})
}

// commentServer is a review server whose pull request comments can act as linked accounts.
type commentServer struct {
	*reviewServer
	// users holds the SwitchTender accounts.
	users user.Store
	// links holds the forge account links.
	links forgelink.Store
	// applies records the plans and applies that executed.
	applies *applyLog
	// accounts maps each linked forge account id to its SwitchTender account id.
	accounts map[int64]string
}

// newCommentServer builds a review server for provider with approvals wired, three SwitchTender
// accounts, two admins and an operator, each linked to a forge account, and the pull request's head
// published on the fake forge. extra adds server options.
func newCommentServer(t *testing.T, provider string, extra ...Option) *commentServer {
	t.Helper()
	ctx := context.Background()
	users := user.NewMemStore()
	links := forgelink.NewMemStore()
	log := &applyLog{}
	opts := append([]Option{WithUsers(users), WithForgeLinks(links)}, extra...)
	rs := newReviewServer(t, reviewSetup{Provider: provider, Approvals: true,
		Runner: savedPlanRunner(log), Options: opts})
	cs := &commentServer{reviewServer: rs, users: users, links: links, applies: log,
		accounts: map[int64]string{}}
	apiURL := forgelink.CanonicalAPIURL(provider, rs.forge.APIURL())
	for i, a := range []struct {
		Name   string
		Role   user.Role
		Forge  int64
		LinkID string
	}{
		{Name: "approver-one", Role: user.RoleAdmin, Forge: forgeApproverOne},
		{Name: "approver-two", Role: user.RoleAdmin, Forge: forgeApproverTwo},
		{Name: "operator-one", Role: user.RoleOperator, Forge: forgeOperator},
	} {
		u, err := user.New(a.Name, "a-long-enough-password", a.Role)
		if err != nil {
			t.Fatalf("user.New() error = %v", err)
		}
		if err := users.Save(ctx, u); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
		if err := links.Create(ctx, &forgelink.Link{ID: fmt.Sprintf("fl_test%d", i), UserID: u.ID,
			Provider: provider, APIURL: apiURL, ForgeUserID: a.Forge,
			CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Create() error = %v", err)
		}
		cs.accounts[a.Forge] = u.ID
	}
	rs.forge.SetHead(7, rs.headSHA)
	return cs
}

// reviewTrigger returns the server's review trigger.
func (cs *commentServer) reviewTrigger(t *testing.T) *trigger.Trigger {
	t.Helper()
	tg, err := cs.srv.triggers.Get(context.Background(), cs.triggerID)
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	return tg
}

// repository returns the repository the server's pull request is in.
func (cs *commentServer) repository() string {
	if cs.provider == trigger.ProviderGitLab {
		return "infra/network"
	}
	return "acme/infra"
}

// commentEvent is the provider's comment event header value.
func (cs *commentServer) commentEvent() string {
	if cs.provider == trigger.ProviderGitLab {
		return "Note Hook"
	}
	return "issue_comment"
}

// comment returns a comment event for the server's provider: comment id on pull request 7 by forge
// account author, carrying body. action is created on GitHub or create on GitLab for a new comment.
func (cs *commentServer) comment(id, author int64, body, action string, bot bool) []byte {
	if cs.provider == trigger.ProviderGitLab {
		if action == "created" {
			action = "create"
		}
		raw, _ := json.Marshal(map[string]any{
			"object_kind": "note", "event_type": "note",
			"user":    map[string]any{"id": author, "username": "forge-person"},
			"project": map[string]any{"path_with_namespace": "infra/network"},
			"object_attributes": map[string]any{"id": id, "note": body,
				"noteable_type": "MergeRequest", "author_id": author, "action": action}, //nolint:misspell // GitLab's own field name.
			"merge_request": map[string]any{"iid": 7, "source_project_id": 5,
				"target_project_id": 5},
		})
		return raw
	}
	kind := "User"
	if bot {
		kind = "Bot"
	}
	raw, _ := json.Marshal(map[string]any{
		"action": action,
		"issue": map[string]any{"number": 7,
			"pull_request": map[string]any{"url": "https://github.example.com/pulls/7"}},
		"comment": map[string]any{"id": id, "body": body,
			"user": map[string]any{"id": author, "login": "forge-person", "type": kind}},
		"repository": map[string]any{"full_name": "acme/infra"},
	})
	return raw
}

// post holds a new comment on the fake forge, the way a person posting it does, and delivers its
// webhook, returning the answer.
func (cs *commentServer) post(t *testing.T, id, author int64, body string) (*httptest.ResponseRecorder,
	map[string]string) {
	t.Helper()
	cs.forge.Hold(7, id, author, body)
	return cs.say(t, id, author, body)
}

// say delivers a new comment's webhook alone, for a comment the forge does not hold, and returns
// the webhook's answer.
func (cs *commentServer) say(t *testing.T, id, author int64, body string) (*httptest.ResponseRecorder,
	map[string]string) {
	t.Helper()
	return cs.deliver(t, cs.comment(id, author, body, "created", false))
}

// deliver sends raw as a comment event and decodes the answer.
func (cs *commentServer) deliver(t *testing.T, raw []byte) (*httptest.ResponseRecorder, map[string]string) {
	t.Helper()
	rec := cs.fire(t, cs.commentEvent(), raw, "")
	out := map[string]string{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec, out
}

// planReported opens the pull request, waits for its plan's final report, and returns the plan.
func (cs *commentServer) planReported(t *testing.T) *run.Run {
	t.Helper()
	rec := cs.fire(t, cs.eventName(), cs.payload(cs.opened(), cs.headSHA, ""), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook status = %d, body %s", rec.Code, rec.Body.String())
	}
	plan := cs.planRun(t, rec)
	cs.waitStatus(t, cs.headSHA, successState)
	cs.srv.reviews.Wait()
	return plan
}

// waitReported waits until the plan runID's final report has landed.
func (cs *commentServer) waitReported(t *testing.T, runID string) {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		rec, err := cs.srv.reviews.Record(context.Background(), runID)
		if err == nil && rec.Done && rec.Phase == review.PhaseSucceeded {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("plan %s was not reported: %+v, %v", runID, rec, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// waitRun waits until run id reaches status.
func (cs *commentServer) waitRun(t *testing.T, id string, status run.Status) *run.Run {
	t.Helper()
	deadline := time.Now().Add(time.Minute)
	for {
		r, err := cs.runs.Get(context.Background(), id)
		if err == nil && r.Status == status {
			return r
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s did not reach %s: %+v, %v", id, status, r, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// replies returns the comments on pull request 7 that are comment command replies: the ones the
// token's own account posted that are not a plan report.
func (cs *commentServer) replies() []string {
	cs.srv.reviews.Wait()
	var out []string
	for _, c := range cs.forge.Comments(7) {
		if c.Author == cs.forge.Login && !strings.HasPrefix(c.Body, "<!-- switchtender-review") {
			out = append(out, c.Body)
		}
	}
	return out
}

// applies returns the runs proposed from the pull request's plans by a comment.
func (cs *commentServer) applyRuns(t *testing.T) []*run.Run {
	t.Helper()
	list, err := cs.runs.ListPage(context.Background(), run.ListFilter{Source: review.ApplySource},
		100, 0)
	if err != nil {
		t.Fatalf("ListPage() error = %v", err)
	}
	return list
}

// decisionEntry returns the chain entry recording the approval of run id.
func (cs *commentServer) decisionEntry(t *testing.T, id string) *audit.Entry {
	t.Helper()
	chain, err := cs.audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if e.Path == "/runs/"+id+"/decision/approved" {
			return e
		}
	}
	t.Fatalf("no approval decision on the chain for %s", id)
	return nil
}

// TestCommentApplyAppliesTheReportedPlan walks the whole path on both forges: a linked admin's
// /switchtender apply approves the plan SwitchTender reported for the head and applies exactly its
// saved plan file, the decision records and commits the comment, and an edit or a deletion of the
// comment afterward changes nothing that was recorded.
func TestCommentApplyAppliesTheReportedPlan(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cs := newCommentServer(t, provider)
			plan := cs.planReported(t)

			body := "/switchtender apply\nlooks right to me"
			rec, out := cs.post(t, 501, forgeApproverOne, body)
			if rec.Code != http.StatusOK || out["result"] != "applied" || out["run"] == "" {
				t.Fatalf("apply comment answered %d %s, want 200 applied", rec.Code, rec.Body.String())
			}
			applied := cs.waitRun(t, out["run"], run.StatusSucceeded)
			if applied.ProposedFrom != plan.ID || applied.DryRun ||
				applied.PinnedCommit != cs.headSHA || applied.Source != review.ApplySource ||
				applied.ActorUserID != cs.accounts[forgeApproverOne] {
				t.Errorf("apply = from %q dry %v pin %q source %q actor %q, want the plan run for real "+
					"at the head by the linked account", applied.ProposedFrom, applied.DryRun,
					applied.PinnedCommit, applied.Source, applied.ActorUserID)
			}
			_, saved, carried := cs.applies.snapshot()
			if len(saved) != 1 || len(carried) != 1 || !bytes.Equal(saved[0], carried[0]) {
				t.Fatalf("plan files saved %q, applied %q, want the apply to carry the plan's own",
					saved, carried)
			}

			records, err := cs.decisions.ForRun(ctx, applied.ID)
			if err != nil || len(records) != 1 {
				t.Fatalf("ForRun() = %v, %v, want one decision", records, err)
			}
			want := &decision.Comment{Forge: provider, APIURL: cs.forge.APIURL(),
				Repository: map[string]string{trigger.ProviderGitHub: "acme/infra",
					trigger.ProviderGitLab: "infra/network"}[provider],
				PullRequest: 7, CommentID: 501, AuthorID: forgeApproverOne,
				BodySHA256: (&review.CommentEvent{Body: body}).BodySHA256(), PlanRunID: plan.ID}
			if diff := cmp.Diff(want, records[0].Comment); diff != "" {
				t.Errorf("decision comment mismatch (-want +got):\n%s", diff)
			}
			if records[0].ActorType != actorTypeComment || records[0].Actor != "approver-one" {
				t.Errorf("decider = %q %q, want the linked account through a comment",
					records[0].Actor, records[0].ActorType)
			}
			entry := cs.decisionEntry(t, applied.ID)
			committed, _, err := outcome.DecisionBodyWith(applied, "approved",
				outcome.ExtrasOf(records[0]))
			if err != nil || !audit.VerifyContentDigest(entry.ContentDigest, entry.Nonce, committed) {
				t.Fatalf("the decision entry does not commit the comment (%v)", err)
			}
			if !strings.Contains(string(committed), `"body_sha256":"`+want.BodySHA256+`"`) {
				t.Errorf("committed body %s lacks the comment fingerprint", committed)
			}

			// The comment is edited to say something else and then deleted. Neither acts, and
			// neither changes the decision or what its entry commits.
			edited := "edited"
			deleted := "deleted"
			if provider == trigger.ProviderGitLab {
				edited, deleted = "update", "update"
			}
			for _, action := range []string{edited, deleted} {
				rec, out = cs.deliver(t, cs.comment(501, forgeApproverOne,
					"/switchtender apply\nnever mind, do not apply", action, false))
				if rec.Code != http.StatusAccepted || out["ignored"] == "" {
					t.Errorf("%s comment answered %d %s, want ignored", action, rec.Code,
						rec.Body.String())
				}
			}
			after, err := cs.decisions.ForRun(ctx, applied.ID)
			if err != nil || len(after) != 1 {
				t.Fatalf("ForRun() after edit = %v, %v", after, err)
			}
			if diff := cmp.Diff(records[0], after[0]); diff != "" {
				t.Errorf("an edit changed the decision record (-want +got):\n%s", diff)
			}
			if got := cs.decisionEntry(t, applied.ID); got.ContentDigest != entry.ContentDigest {
				t.Errorf("an edit changed the decision entry")
			}
			if got := cs.applyRuns(t); len(got) != 1 {
				t.Errorf("applies = %d after the edit, want 1", len(got))
			}
			if r := cs.replies(); len(r) != 1 || !strings.Contains(r[0], "Approved from this comment") {
				t.Errorf("replies = %q, want the one approval reply", r)
			}
		})
	}
}

// TestCommentRefusals proves every refusal acts on nothing: an unlinked commenter and a bot get one
// reply and nothing else, a fork's pull request gets nothing at all, an apply with no reported plan
// or with a head that moved since the plan is refused, and an operator's apply is proposed but not
// approved.
func TestCommentRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider    string
		Author      int64
		Body        string
		Bot         bool
		ForgeBot    bool
		Fork        bool
		Closed      bool
		Plan        bool
		Move        bool
		WantResult  string
		WantReply   string
		WantApplies int
	}{{ // Test 0: An unlinked commenter on GitHub is told how to link.
		Provider: trigger.ProviderGitHub, Author: forgeStranger, Body: "/switchtender apply",
		Plan: true, WantResult: "refused/unlinked",
		WantReply: "https://st.example.com/ui/links",
	}, { // Test 1: An unlinked commenter on GitLab is told how to link.
		Provider: trigger.ProviderGitLab, Author: forgeStranger, Body: "/switchtender plan",
		WantResult: "refused/unlinked", WantReply: "not linked to a SwitchTender account",
	}, { // Test 2: A GitHub bot is refused even when its account is linked.
		Provider: trigger.ProviderGitHub, Author: forgeApproverOne, Body: "/switchtender apply",
		Bot: true, Plan: true, WantResult: "refused/bot", WantReply: "bot accounts",
	}, { // Test 3: A GitLab bot user is refused even when its account is linked.
		Provider: trigger.ProviderGitLab, Author: forgeApproverOne, Body: "/switchtender apply",
		ForgeBot: true, Plan: true, WantResult: "refused/bot", WantReply: "bot accounts",
	}, { // Test 4: A fork's pull request is never acted on and never answered.
		Provider: trigger.ProviderGitHub, Author: forgeApproverOne, Body: "/switchtender apply",
		Fork: true, WantResult: "refused/fork",
	}, { // Test 5: A GitLab fork's merge request is never acted on and never answered.
		Provider: trigger.ProviderGitLab, Author: forgeApproverOne, Body: "/switchtender plan",
		Fork: true, WantResult: "refused/fork",
	}, { // Test 6: With no reported plan nothing is applied.
		Provider: trigger.ProviderGitHub, Author: forgeApproverOne, Body: "/switchtender apply",
		WantResult: "refused/no_plan", WantReply: "reported no plan",
	}, { // Test 7: A head that moved since the reported plan is refused on GitHub.
		Provider: trigger.ProviderGitHub, Author: forgeApproverOne, Body: "/switchtender apply",
		Plan: true, Move: true, WantResult: "refused/head_moved", WantReply: "is now at",
	}, { // Test 8: A head that moved since the reported plan is refused on GitLab.
		Provider: trigger.ProviderGitLab, Author: forgeApproverOne, Body: "/switchtender apply",
		Plan: true, Move: true, WantResult: "refused/head_moved", WantReply: "is now at",
	}, { // Test 9: An operator's apply is proposed and held, since approving is not theirs.
		Provider: trigger.ProviderGitHub, Author: forgeOperator, Body: "/switchtender apply",
		Plan: true, WantResult: "refused/role", WantReply: "cannot approve", WantApplies: 1,
	}, { // Test 10: A closed pull request is not acted on.
		Provider: trigger.ProviderGitLab, Author: forgeApproverOne, Body: "/switchtender apply",
		Plan: true, Closed: true, WantResult: "refused/closed", WantReply: "not open",
	}, { // Test 11: With no reported plan nothing is applied on GitLab either.
		Provider: trigger.ProviderGitLab, Author: forgeApproverOne, Body: "/switchtender apply",
		WantResult: "refused/no_plan", WantReply: "reported no plan",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, test.Provider)
			if test.Plan {
				cs.planReported(t)
			}
			if test.Move {
				cs.forge.SetHead(7, strings.Repeat("e", 40))
			}
			cs.forge.SetFork(7, test.Fork)
			cs.forge.SetClosed(7, test.Closed)
			cs.forge.SetBot(test.Author, test.ForgeBot)
			before, err := cs.runs.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			cs.forge.Hold(7, 77, test.Author, test.Body)
			rec, out := cs.deliver(t, cs.comment(77, test.Author, test.Body, "created", test.Bot))
			if rec.Code != http.StatusAccepted || out["result"] != test.WantResult {
				t.Fatalf("comment answered %d %s, want %s", rec.Code, rec.Body.String(),
					test.WantResult)
			}
			// A redelivery of the same comment is answered and acts on nothing.
			rec, out = cs.deliver(t, cs.comment(77, test.Author, test.Body, "created", test.Bot))
			if rec.Code != http.StatusAccepted || out["ignored"] != "this comment was already handled" {
				t.Errorf("redelivery answered %d %s, want ignored", rec.Code, rec.Body.String())
			}
			replies := cs.replies()
			switch {
			case test.WantReply == "" && len(replies) != 0:
				t.Errorf("replies = %q, want none", replies)
			case test.WantReply != "" && (len(replies) != 1 ||
				!strings.Contains(replies[0], test.WantReply)):
				t.Errorf("replies = %q, want one carrying %q", replies, test.WantReply)
			}
			after, err := cs.runs.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if got := len(after) - len(before); got != test.WantApplies {
				t.Errorf("the comment created %d runs, want %d", got, test.WantApplies)
			}
			for _, r := range cs.applyRuns(t) {
				if r.Status != run.StatusPendingApproval {
					t.Errorf("apply %s is %s, want it left held", r.ID, r.Status)
				}
			}
			if !chainHasPath(t, cs.audits, "/hooks/"+cs.triggerID+"/review/7/comment/77/"+
				cs.commandOf(test.Body)+"/"+test.WantResult) {
				t.Errorf("the refusal is not on the chain")
			}
		})
	}
}

// TestCommentRefusalRepliesOncePerAuthor proves a refused command is answered once per author,
// pull request, and reason, so repeating it cannot make the operator's token post again and again:
// an unlinked author, a bot, a pull request with no reported plan, a closed one, and a head that
// moved each get one reply however often the author comments, while another author, or a head that
// moved again, is told once more. Every comment's arrival is still recorded on the chain.
func TestCommentRefusalRepliesOncePerAuthor(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider    string
		Author      int64
		Plan        bool
		Move        bool
		ForgeBot    bool
		Closed      bool
		Second      int64
		MoveAgain   bool
		WantResult  string
		WantReplies int
	}{{ // Test 0: An unlinked author is told how to link once, and a second unlinked author once.
		Provider: trigger.ProviderGitHub, Author: forgeStranger, Second: forgeStranger + 1,
		WantResult: "refused/unlinked", WantReplies: 2,
	}, { // Test 1: An unlinked GitLab author is told once.
		Provider: trigger.ProviderGitLab, Author: forgeStranger, WantResult: "refused/unlinked",
		WantReplies: 1,
	}, { // Test 2: A bot is told once.
		Provider: trigger.ProviderGitLab, Author: forgeApproverOne, ForgeBot: true,
		WantResult: "refused/bot", WantReplies: 1,
	}, { // Test 3: A pull request with no reported plan is told once.
		Provider: trigger.ProviderGitHub, Author: forgeApproverOne, WantResult: "refused/no_plan",
		WantReplies: 1,
	}, { // Test 4: A closed pull request is told once.
		Provider: trigger.ProviderGitHub, Author: forgeApproverOne, Plan: true, Closed: true,
		WantResult: "refused/closed", WantReplies: 1,
	}, { // Test 5: A moved head is told once, and told again once it moves again.
		Provider: trigger.ProviderGitLab, Author: forgeApproverOne, Plan: true, Move: true,
		MoveAgain: true, WantResult: "refused/head_moved", WantReplies: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, test.Provider)
			if test.Plan {
				cs.planReported(t)
			}
			if test.Move {
				cs.forge.SetHead(7, strings.Repeat("e", 40))
			}
			cs.forge.SetBot(test.Author, test.ForgeBot)
			cs.forge.SetClosed(7, test.Closed)
			ids := []int64{301, 302, 303}
			for _, id := range ids {
				rec, out := cs.post(t, id, test.Author, "/switchtender apply")
				if rec.Code != http.StatusAccepted || out["result"] != test.WantResult {
					t.Fatalf("comment %d answered %d %s, want %s", id, rec.Code, rec.Body.String(),
						test.WantResult)
				}
			}
			if test.Second != 0 {
				ids = append(ids, 304)
				if _, out := cs.post(t, 304, test.Second, "/switchtender apply"); out["result"] !=
					test.WantResult {
					t.Fatalf("second author's comment answered %v, want %s", out, test.WantResult)
				}
			}
			if test.MoveAgain {
				cs.forge.SetHead(7, strings.Repeat("f", 40))
				ids = append(ids, 305, 306)
				for _, id := range ids[len(ids)-2:] {
					if _, out := cs.post(t, id, test.Author, "/switchtender apply"); out["result"] !=
						test.WantResult {
						t.Fatalf("comment %d answered %v, want %s", id, out, test.WantResult)
					}
				}
			}
			if r := cs.replies(); len(r) != test.WantReplies {
				t.Errorf("replies = %d %q, want %d", len(r), r, test.WantReplies)
			}
			for _, id := range ids {
				if !chainHasPath(t, cs.audits, fmt.Sprintf("/hooks/%s/review/7/comment/%d/apply",
					cs.triggerID, id)) {
					t.Errorf("comment %d's arrival is not on the chain", id)
				}
			}
		})
	}
}

// commandOf returns the command a test comment body carries.
func (cs *commentServer) commandOf(body string) string {
	return (&review.CommentEvent{Body: body}).Command()
}

// TestCommentPlanPlansTheHeadAsTheLinkedAccount proves /switchtender plan launches the review plan
// of the pull request's current head, as the commenter's linked account, reported like any other.
func TestCommentPlanPlansTheHeadAsTheLinkedAccount(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, provider)
			rec, out := cs.post(t, 88, forgeOperator, "/switchtender plan")
			if rec.Code != http.StatusAccepted || out["run"] == "" {
				t.Fatalf("plan comment answered %d %s, want a plan", rec.Code, rec.Body.String())
			}
			cs.waitStatus(t, cs.headSHA, successState)
			planned, err := cs.runs.Get(context.Background(), out["run"])
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			if !planned.DryRun || planned.PinnedCommit != cs.headSHA || planned.Source != review.Source ||
				planned.ActorUserID != cs.accounts[forgeOperator] || planned.Actor != "operator-one" {
				t.Errorf("plan = dry %v pin %q source %q actor %q %q, want the head planned as the "+
					"linked account", planned.DryRun, planned.PinnedCommit, planned.Source,
					planned.Actor, planned.ActorUserID)
			}
			if plans, _, applied := cs.applies.snapshot(); plans != 1 || len(applied) != 0 {
				t.Errorf("executions = %d plans, %d applies, want one plan", plans, len(applied))
			}
			for _, path := range []string{"/comment/88/plan", "/comment/88/plan/accepted",
				"/planned"} {
				if !chainHasPath(t, cs.audits, "/hooks/"+cs.triggerID+"/review/7"+path) {
					t.Errorf("the chain lacks %s", path)
				}
			}
		})
	}
}

// TestCommentApplyHoldsSeparationOfDuties proves a rule requiring a different approver binds a
// comment as it binds the queue, a rule scoped to people included: the person whose comment asked
// for the apply cannot approve it, a second linked approver's comment does, and a later comment
// applies nothing twice.
func TestCommentApplyHoldsSeparationOfDuties(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider  string
		ActorKind string
	}{{ // Test 0: A rule over every actor, on GitHub.
		Provider: trigger.ProviderGitHub, ActorKind: "",
	}, { // Test 1: A rule over people, which a person commenting from a linked account is.
		Provider: trigger.ProviderGitHub, ActorKind: policy.ActorKindHuman,
	}, { // Test 2: A rule over every actor, on GitLab.
		Provider: trigger.ProviderGitLab, ActorKind: "",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cs := newCommentServer(t, test.Provider)
			if err := cs.policies.Save(ctx, &policy.Policy{ID: "pol_sod", Name: "four eyes",
				Tool: run.ToolTerraform, ExcludeDryRun: true, RequireDistinctApprover: true,
				ActorKind: test.ActorKind, MaxDestroy: policy.DisabledMaxDestroy}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			cs.planReported(t)

			rec, out := cs.post(t, 601, forgeApproverOne, "/switchtender apply")
			if rec.Code != http.StatusAccepted || out["result"] != "refused/separation_of_duties" {
				t.Fatalf("first apply answered %d %s, want separation of duties", rec.Code,
					rec.Body.String())
			}
			held := cs.waitRun(t, out["run"], run.StatusPendingApproval)
			if !held.RequireDistinctApprover || held.ActorUserID != cs.accounts[forgeApproverOne] {
				t.Fatalf("apply = distinct %v requester %q, want the rule's requirement and the "+
					"first commenter as requester", held.RequireDistinctApprover, held.ActorUserID)
			}

			rec, out = cs.post(t, 602, forgeApproverTwo, "/switchtender apply")
			if rec.Code != http.StatusOK || out["result"] != "applied" || out["run"] != held.ID {
				t.Fatalf("second apply answered %d %s, want the held apply approved", rec.Code,
					rec.Body.String())
			}
			cs.waitRun(t, held.ID, run.StatusSucceeded)
			records, err := cs.decisions.ForRun(ctx, held.ID)
			if err != nil || len(records) != 1 || records[0].Comment == nil ||
				records[0].Comment.AuthorID != forgeApproverTwo {
				t.Fatalf("decisions = %+v, %v, want one, from the second approver's comment",
					records, err)
			}

			rec, out = cs.post(t, 603, forgeApproverTwo, "/switchtender apply")
			if rec.Code != http.StatusAccepted || out["result"] != "refused/already_applied" {
				t.Errorf("third apply answered %d %s, want already applied", rec.Code,
					rec.Body.String())
			}
			if got := cs.applyRuns(t); len(got) != 1 {
				t.Errorf("applies = %d, want 1", len(got))
			}
			if _, _, applied := cs.applies.snapshot(); len(applied) != 1 {
				t.Errorf("the plan was applied %d times, want once", len(applied))
			}
		})
	}
}

// TestCommentRedeliveryAppliesOnce proves a redelivered apply comment, the same comment id arriving
// again, and two deliveries racing each other, apply once.
func TestCommentRedeliveryAppliesOnce(t *testing.T) {
	t.Parallel()
	cs := newCommentServer(t, trigger.ProviderGitLab)
	cs.planReported(t)
	cs.forge.Hold(7, 701, forgeApproverOne, "/switchtender apply")
	raw := cs.comment(701, forgeApproverOne, "/switchtender apply", "created", false)
	var wg sync.WaitGroup
	results := make([]string, 3)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			rec := cs.fire(t, cs.commentEvent(), raw, "")
			out := map[string]string{}
			_ = json.Unmarshal(rec.Body.Bytes(), &out)
			results[i] = out["result"] + out["ignored"]
		}(i)
	}
	wg.Wait()
	applied := 0
	for _, r := range results {
		if r == "applied" {
			applied++
		} else if r != "this comment was already handled" {
			t.Errorf("a delivery answered %q", r)
		}
	}
	if applied != 1 {
		t.Fatalf("deliveries = %q, want exactly one applied", results)
	}
	list := cs.applyRuns(t)
	if len(list) != 1 {
		t.Fatalf("applies = %d, want 1", len(list))
	}
	cs.waitRun(t, list[0].ID, run.StatusSucceeded)
	if _, _, carried := cs.applies.snapshot(); len(carried) != 1 {
		t.Errorf("the plan was applied %d times, want once", len(carried))
	}
	if r := cs.replies(); len(r) != 1 {
		t.Errorf("replies = %q, want one", r)
	}
}

// TestCommentHonorsGrants proves a comment acts with the grants its linked account holds in the
// queue: under strict grants an operator with no grant on the template cannot plan from a comment,
// and one holding use of the template and of everything a launch of it touches can.
func TestCommentHonorsGrants(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Grant      bool
		WantResult string
	}{{ // Test 0: No grant, so the plan is refused and the pull request told.
		Grant: false, WantResult: "refused/grant",
	}, { // Test 1: Use grants on the template and everything it runs with let the plan go ahead.
		Grant: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			grants := grant.NewMemStore()
			cs := newCommentServer(t, trigger.ProviderGitHub, WithGrants(grants, true))
			if test.Grant {
				// A plan comment needs every grant a launch of the template needs: the template,
				// its project, and the credential it runs with.
				for _, object := range []string{"tpl_net", "proj_infra", "cred_cloud"} {
					if err := grants.Save(context.Background(), &grant.Grant{ID: "grt_" + object,
						Subject: cs.accounts[forgeOperator], Object: object,
						Access: grant.AccessUse, CreatedAt: time.Now()}); err != nil {
						t.Fatalf("Save() error = %v", err)
					}
				}
			}
			rec, out := cs.post(t, 91, forgeOperator, "/switchtender plan")
			if test.WantResult != "" {
				if rec.Code != http.StatusAccepted || out["result"] != test.WantResult {
					t.Fatalf("plan comment answered %d %s, want %s", rec.Code, rec.Body.String(),
						test.WantResult)
				}
				if r := cs.replies(); len(r) != 1 || !strings.Contains(r[0], "may not use") {
					t.Errorf("replies = %q, want one saying the account may not use the template", r)
				}
				return
			}
			if rec.Code != http.StatusAccepted || out["run"] == "" {
				t.Fatalf("plan comment answered %d %s, want a plan", rec.Code, rec.Body.String())
			}
		})
	}
}

// TestCommentCommandParsing proves only a first line of exactly /switchtender plan or apply is a
// command.
func TestCommentCommandParsing(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Body        string
		WantCommand string
	}{{ // Test 0: Apply.
		Body: "/switchtender apply", WantCommand: review.CommandApply,
	}, { // Test 1: Plan with surrounding space and a second line.
		Body: "  /switchtender plan  \nplease", WantCommand: review.CommandPlan,
	}, { // Test 2: A command that is not the first line is not one.
		Body: "lgtm\n/switchtender apply",
	}, { // Test 3: An unknown subcommand is not one.
		Body: "/switchtender destroy",
	}, { // Test 4: Extra words are not one.
		Body: "/switchtender apply now",
	}, { // Test 5: Quoted in a sentence it is not one.
		Body: "run /switchtender apply later",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := (&review.CommentEvent{Body: test.Body}).Command()
			if diff := cmp.Diff(test.WantCommand, got); diff != "" {
				t.Errorf("Command() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}
