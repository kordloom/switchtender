package server

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/pgstore"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/sqlitestore"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// approvedApplies returns how many of the runs proposed from a pull request's plans carry an
// approval decision.
func (cs *commentServer) approvedApplies(t *testing.T) int {
	t.Helper()
	n := 0
	for _, r := range cs.applyRuns(t) {
		records, err := cs.decisions.ForRun(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("ForRun() error = %v", err)
		}
		for _, rec := range records {
			if rec.Verdict == "approved" {
				n++
			}
		}
	}
	return n
}

// approvedAppliesBy returns how many of the runs proposed from a pull request's plans carry an
// approval decided from a comment by the forge account author.
func (cs *commentServer) approvedAppliesBy(t *testing.T, author int64) int {
	t.Helper()
	n := 0
	for _, r := range cs.applyRuns(t) {
		records, err := cs.decisions.ForRun(context.Background(), r.ID)
		if err != nil {
			t.Fatalf("ForRun() error = %v", err)
		}
		for _, rec := range records {
			if rec.Verdict == "approved" && rec.Comment != nil && rec.Comment.AuthorID == author {
				n++
			}
		}
	}
	return n
}

// TestRedTeamCommentTheForgeNeverHeldDoesNotApply proves an apply acts only on a comment the forge
// itself confirms. The author's numeric id and the body are read from the webhook body alone, so
// anybody holding the trigger's signing secret, which every admin can rotate and read, can sign a
// comment the forge never held, name another approver's numeric id, and approve as that person.
func TestRedTeamCommentTheForgeNeverHeldDoesNotApply(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider string
		Comment  int64
	}{{ // Test 0: A signed GitHub comment id the forge never issued.
		Provider: trigger.ProviderGitHub, Comment: 9001,
	}, { // Test 1: A signed GitLab note id the forge never issued.
		Provider: trigger.ProviderGitLab, Comment: 9002,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, test.Provider)
			cs.planReported(t)
			_, out := cs.say(t, test.Comment, forgeApproverOne, "/switchtender apply")
			if out["result"] == "applied" {
				t.Errorf("a comment the forge never held was applied as approver one: %v", out)
			}
			if n := cs.approvedApplies(t); n != 0 {
				t.Errorf("approved applies = %d, want 0", n)
			}
		})
	}
}

// TestRedTeamCommentWrittenByAnAppForAPersonIsRefused proves a comment a GitHub App wrote on a
// person's behalf does not approve as that person. GitHub marks such a comment with
// performed_via_github_app while its author stays the person's own account of type User, so an
// agent holding a person's user-to-server token approves with the person's full role.
func TestRedTeamCommentWrittenByAnAppForAPersonIsRefused(t *testing.T) {
	t.Parallel()
	tests := []struct {
		App map[string]any
	}{{ // Test 0: An agent's GitHub App acting for the first approver.
		App: map[string]any{"id": 77, "slug": "coding-agent", "name": "Coding Agent"},
	}, { // Test 1: Any GitHub App at all, named by id alone.
		App: map[string]any{"id": 78},
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, trigger.ProviderGitHub)
			cs.planReported(t)
			var payload map[string]any
			if err := json.Unmarshal(cs.comment(int64(800+testNum), forgeApproverOne,
				"/switchtender apply", "created", false), &payload); err != nil {
				t.Fatalf("Unmarshal() error = %v", err)
			}
			payload["comment"].(map[string]any)["performed_via_github_app"] = test.App
			raw, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			_, out := cs.deliver(t, raw)
			if out["result"] == "applied" {
				t.Errorf("a comment an app wrote for a person approved as that person: %v", out)
			}
			if n := cs.approvedApplies(t); n != 0 {
				t.Errorf("approved applies = %d, want 0", n)
			}
		})
	}
}

// TestRedTeamUnlinkedCommentsDoNotSpendTheForgeRateLimit proves a stranger repeating a command does
// not make the operator's token call the forge once per comment. Every comment reads the pull
// request, and on GitLab the author's account too, before the local link lookup refuses it, so
// anybody able to comment on a public repository spends the token's rate limit at will.
func TestRedTeamUnlinkedCommentsDoNotSpendTheForgeRateLimit(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider     string
		Comments     int
		WantMaxCalls int
	}{{ // Test 0: Ten comments from an unlinked GitHub account.
		Provider: trigger.ProviderGitHub, Comments: 10, WantMaxCalls: 2,
	}, { // Test 1: Ten notes from an unlinked GitLab account.
		Provider: trigger.ProviderGitLab, Comments: 10, WantMaxCalls: 2,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			cs := newCommentServer(t, test.Provider)
			cs.srv.reviews.Wait()
			before := cs.forge.Requests()
			for i := range test.Comments {
				_, out := cs.say(t, int64(9100+i), forgeStranger, "/switchtender apply")
				if out["result"] != "refused/unlinked" {
					t.Fatalf("comment %d answered %v, want refused/unlinked", i, out)
				}
			}
			cs.srv.reviews.Wait()
			if got := cs.forge.Requests() - before; got > test.WantMaxCalls {
				t.Errorf("forge calls = %d for %d refused comments, want at most %d", got,
					test.Comments, test.WantMaxCalls)
			}
		})
	}
}

// TestRedTeamPullRequestAuthorCannotApproveTheirOwnChange proves a rule requiring a different
// approver binds the person who wrote the change. The requester of a comment's apply is whoever
// commented first, so once an operator, or an approver the rule then locks out, asks for the apply,
// the pull request's own author approves it from their linked account.
func TestRedTeamPullRequestAuthorCannotApproveTheirOwnChange(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider string
		First    int64
	}{{ // Test 0: An operator asks first on GitHub, and the author approves.
		Provider: trigger.ProviderGitHub, First: forgeOperator,
	}, { // Test 1: Another approver asks first on GitHub and is locked out, and the author approves.
		Provider: trigger.ProviderGitHub, First: forgeApproverTwo,
	}, { // Test 2: An operator asks first on GitLab, and the author approves.
		Provider: trigger.ProviderGitLab, First: forgeOperator,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			cs := newCommentServer(t, test.Provider)
			if err := cs.policies.Save(ctx, &policy.Policy{ID: "pol_sod", Name: "four eyes",
				Tool: run.ToolTerraform, ExcludeDryRun: true, RequireDistinctApprover: true,
				MaxDestroy: policy.DisabledMaxDestroy}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			cs.forge.SetAuthor(7, forgeApproverOne)
			cs.planReported(t)
			if _, out := cs.post(t, 611, test.First, "/switchtender apply"); out["run"] == "" {
				t.Fatalf("first apply answered %v, want a held apply", out)
			}
			_, out := cs.post(t, 612, forgeApproverOne, "/switchtender apply")
			if out["result"] == "applied" {
				t.Errorf("the pull request's author approved their own change: %v", out)
			}
			if n := cs.approvedAppliesBy(t, forgeApproverOne); n != 0 {
				t.Errorf("approved applies by the author = %d, want 0", n)
			}
		})
	}
}

// TestRedTeamCommentPlanNeedsTheGrantsALaunchNeeds proves a plan comment is held to the grants a
// launch of the same template is held to in the queue. The queue authorizes the template's project,
// inventory, credentials, and queue as well as the template, and says use of the template is not
// enough. The comment checks the template alone, so under strict grants an operator holding use of
// the template, and of nothing it runs with, plans the pull request with the template's credential.
func TestRedTeamCommentPlanNeedsTheGrantsALaunchNeeds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider   string
		WantResult string
	}{{ // Test 0: GitHub, use of the template only.
		Provider: trigger.ProviderGitHub, WantResult: "refused/grant",
	}, { // Test 1: GitLab, use of the template only.
		Provider: trigger.ProviderGitLab, WantResult: "refused/grant",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			grants := grant.NewMemStore()
			cs := newCommentServer(t, test.Provider, WithGrants(grants, true))
			if err := grants.Save(context.Background(), &grant.Grant{ID: "grt_tpl_only",
				Subject: cs.accounts[forgeOperator], Object: "tpl_net", Access: grant.AccessUse,
				CreatedAt: time.Now()}); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			authz := &authorizer{grants: grants, strict: true}
			ctx := context.WithValue(context.Background(), actorKey{}, Actor{
				UserID: cs.accounts[forgeOperator], Role: user.RoleOperator, Name: "operator-one"})
			if err := authz.authorizeAll(ctx, grant.AccessUse, "proj_infra",
				"cred_cloud"); !authzRefused(err) {
				t.Fatalf("the queue's launch check on the template's objects = %v, want a refusal",
					err)
			}
			_, out := cs.say(t, 92, forgeOperator, "/switchtender plan")
			if out["result"] != test.WantResult {
				t.Errorf("plan comment answered %v, want %s: the account holds no grant on "+
					"proj_infra or cred_cloud", out, test.WantResult)
			}
		})
	}
}

// redTeamLinkStores returns the stores a deletion test runs against: the in-memory stores, SQLite,
// and PostgreSQL when a database is set.
func redTeamLinkStores(t *testing.T) map[string]func(t *testing.T) (user.Store, forgelink.Store,
	audit.Store) {
	t.Helper()
	out := map[string]func(t *testing.T) (user.Store, forgelink.Store, audit.Store){
		"memory": func(*testing.T) (user.Store, forgelink.Store, audit.Store) {
			return user.NewMemStore(), forgelink.NewMemStore(), audit.NewMemStore()
		},
		"sqlite": func(t *testing.T) (user.Store, forgelink.Store, audit.Store) {
			db, err := sqlitestore.Open(filepath.Join(t.TempDir(), "switchtender.db"))
			if err != nil {
				t.Fatalf("open sqlite: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db.Users(), db.ForgeLinks(), db.Audits()
		},
	}
	if os.Getenv("SWITCHTENDER_TEST_POSTGRES_DSN") != "" {
		out["postgres"] = func(t *testing.T) (user.Store, forgelink.Store, audit.Store) {
			db, err := pgstore.Open(cbkFreshDatabase(t))
			if err != nil {
				t.Fatalf("open postgres: %v", err)
			}
			t.Cleanup(func() { _ = db.Close() })
			return db.Users(), db.ForgeLinks(), db.Audits()
		}
	} else if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
		t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and SWITCHTENDER_TEST_POSTGRES_DSN is not")
	}
	return out
}

// TestRedTeamDeletingAUserRecordsTheUnlink proves the chain shows a forge link ending when its
// account is deleted. The delete removes the links without an unlink entry, so the chain still
// shows a link that no longer exists, against the promise that the chain and the links agree.
func TestRedTeamDeletingAUserRecordsTheUnlink(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Forge int64
	}{{ // Test 0: A linked admin is deleted.
		Forge: 3001,
	}}
	for testNum, test := range tests {
		for name, open := range redTeamLinkStores(t) {
			t.Run(fmt.Sprintf("test %d %s", testNum, name), func(t *testing.T) {
				t.Parallel()
				ctx := context.Background()
				users, links, audits := open(t)
				var ids []string
				for _, n := range []string{"admin-keep", "admin-gone"} {
					u, err := user.New(n, "a-long-enough-password", user.RoleAdmin)
					if err != nil {
						t.Fatalf("user.New() error = %v", err)
					}
					if err := users.Save(ctx, u); err != nil {
						t.Fatalf("Save() error = %v", err)
					}
					ids = append(ids, u.ID)
				}
				link := &forgelink.Link{ID: "fl_rtdel", UserID: ids[1], Provider: "github",
					APIURL: "https://api.github.com", ForgeUserID: test.Forge,
					CreatedAt: time.Now().UTC()}
				if err := links.Create(ctx, link); err != nil {
					t.Fatalf("Create() error = %v", err)
				}
				if err := recordForgeLink(ctx, audits, "linked", link, "admin-gone",
					"user"); err != nil {
					t.Fatalf("recordForgeLink() error = %v", err)
				}
				srv := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(), WithUsers(users),
					WithForgeLinks(links), WithAudit(audits))
				req := httptest.NewRequest(http.MethodDelete, "/v1/users/"+ids[1], nil)
				rec := httptest.NewRecorder()
				srv.Handler().ServeHTTP(rec, req)
				if rec.Code != http.StatusOK {
					t.Fatalf("delete status = %d, body %s", rec.Code, rec.Body.String())
				}
				if _, err := links.Lookup(ctx, "github", "https://api.github.com",
					test.Forge); err == nil {
					t.Fatalf("the deleted account's link still resolves")
				}
				if !chainHasPath(t, audits, "/me/forge-links/fl_rtdel/unlinked") {
					t.Errorf("the chain records the link and never its removal")
				}
			})
		}
	}
}
