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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/policy"
	"github.com/kordloom/switchtender/internal/project"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/review/forgetest"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// reviewForgeToken is the token the fake forges accept.
const reviewForgeToken = "ghp_review_token_value_0000"

// reviewCloudSecret is a credential the template runs with, which the fake plan prints.
const reviewCloudSecret = "cloud-secret-value-4242"

// prOriginRepo is a git repository shaped like a forge's: main holds one commit, and the pull
// request's head is published under its pull or merge request ref on a commit no branch holds.
type prOriginRepo struct {
	// dir is the repository path.
	dir string
	// repo is the open repository.
	repo *git.Repository
	// ref is the pull request ref.
	ref plumbing.ReferenceName
	// parent is the commit the next push builds on.
	parent plumbing.Hash
}

// newPROrigin builds the origin with files on main and the pull request ref pointing at a commit
// that replaces them with pr.
func newPROrigin(t *testing.T, ref string, mainFiles, pr map[string]string) (*prOriginRepo, string) {
	t.Helper()
	dir := t.TempDir()
	repo, err := git.PlainInitWithOptions(dir, &git.PlainInitOptions{
		InitOptions: git.InitOptions{DefaultBranch: plumbing.Main},
	})
	if err != nil {
		t.Fatalf("PlainInitWithOptions() error = %v", err)
	}
	o := &prOriginRepo{dir: dir, repo: repo, ref: plumbing.ReferenceName(ref)}
	o.parent = o.commit(t, mainFiles, plumbing.ZeroHash)
	sha := o.Push(t, pr)
	return o, sha
}

// commit writes files as a commit on parent, or as the root commit, without moving any branch, and
// returns it. The first commit lands on main.
func (o *prOriginRepo) commit(t *testing.T, files map[string]string, parent plumbing.Hash) plumbing.Hash {
	t.Helper()
	wt, err := o.repo.Worktree()
	if err != nil {
		t.Fatalf("Worktree() error = %v", err)
	}
	if !parent.IsZero() {
		if err := wt.Checkout(&git.CheckoutOptions{Hash: parent, Force: true}); err != nil {
			t.Fatalf("Checkout() error = %v", err)
		}
	}
	for rel, body := range files {
		full := filepath.Join(o.dir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o750); err != nil {
			t.Fatalf("MkdirAll() error = %v", err)
		}
		if err := os.WriteFile(full, []byte(body), 0o600); err != nil {
			t.Fatalf("WriteFile() error = %v", err)
		}
	}
	if err := wt.AddGlob("."); err != nil {
		t.Fatalf("AddGlob() error = %v", err)
	}
	h, err := wt.Commit("change", &git.CommitOptions{
		Author: &object.Signature{Name: "test", Email: "test@example.invalid", When: time.Now()},
	})
	if err != nil {
		t.Fatalf("Commit() error = %v", err)
	}
	if !parent.IsZero() {
		if err := wt.Checkout(&git.CheckoutOptions{Branch: plumbing.NewBranchReferenceName("main"),
			Force: true}); err != nil {
			t.Fatalf("Checkout(main) error = %v", err)
		}
	}
	return h
}

// Push adds a commit carrying files to the pull request and moves its ref there, as a push to the
// pull request's branch does, and returns the new head.
func (o *prOriginRepo) Push(t *testing.T, files map[string]string) string {
	t.Helper()
	h := o.commit(t, files, o.parent)
	if err := o.repo.Storer.SetReference(plumbing.NewHashReference(o.ref, h)); err != nil {
		t.Fatalf("SetReference() error = %v", err)
	}
	o.parent = h
	return h.String()
}

// specLog records every spec the runner was handed.
type specLog struct {
	// mu guards specs.
	mu sync.Mutex
	// specs holds each spec with the plan file it found in its checkout.
	specs []recordedSpec
}

// recordedSpec is one execution the fake runner saw.
type recordedSpec struct {
	// DryRun is the spec's no-change flag.
	DryRun bool
	// Plan is what infra/plan.txt held in the checkout the run executed in.
	Plan string
}

// all returns a copy of the recorded specs.
func (l *specLog) all() []recordedSpec {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]recordedSpec(nil), l.specs...)
}

// planRunner returns a runner that stands in for terraform: it prints the plan file the checkout
// holds and the credential the template gave it, as a careless provider debug line would.
func planRunner(log *specLog) roundhouse.Runner {
	return roundhouse.RunnerFunc(func(_ context.Context, spec roundhouse.Spec,
		out io.Writer) (roundhouse.Result, error) {
		plan, _ := os.ReadFile(filepath.Join(spec.Dir, "infra", "plan.txt"))
		log.mu.Lock()
		log.specs = append(log.specs, recordedSpec{DryRun: spec.DryRun, Plan: string(plan)})
		log.mu.Unlock()
		_, _ = fmt.Fprintf(out, "Terraform will perform the following actions:\n")
		for _, e := range spec.Env {
			if strings.HasPrefix(e, "CLOUD_SECRET=") {
				_, _ = fmt.Fprintf(out, "provider debug: %s\n", e)
			}
		}
		_, _ = fmt.Fprintf(out, "%s", plan)
		return roundhouse.Result{ExitCode: 0}, nil
	})
}

// reviewServer is a server wired for pull request review against a fake forge.
type reviewServer struct {
	// handler serves the API.
	handler http.Handler
	// srv is the server, for its reporter.
	srv *Server
	// forge is the fake forge.
	forge *forgetest.Forge
	// runs holds the runs.
	runs run.Store
	// audits holds the chain.
	audits audit.Store
	// policies holds the rules.
	policies policy.Store
	// specs records what executed.
	specs *specLog
	// origin is the repository the project syncs.
	origin *prOriginRepo
	// headSHA is the pull request's first head.
	headSHA string
	// hookPath is the trigger's webhook path.
	hookPath string
	// secret is the trigger's signing secret.
	secret string
	// triggerID is the trigger's id.
	triggerID string
	// provider is github or gitlab.
	provider string
	// decisions holds the decision records, nil unless the setup wired approvals.
	decisions decision.Store
	// disp is the dispatcher executing the runs.
	disp *dispatch.Dispatcher
}

// reviewSetup customizes a review server before the trigger is created.
type reviewSetup struct {
	// Provider is github or gitlab.
	Provider string
	// AllowForks is the trigger's fork setting.
	AllowForks bool
	// Plan is the plan text the pull request's head carries.
	Plan string
	// Runner replaces the fake plan runner.
	Runner roundhouse.Runner
	// PRFiles replaces the files the pull request's head carries.
	PRFiles map[string]string
	// Playbook makes the template an Ansible template running this playbook of the project.
	Playbook string
	// SubmitErr, when set, is what every plan submission answers instead of launching.
	SubmitErr error
	// Audits replaces the audit store.
	Audits audit.Store
	// AnswerWithin bounds how long a delivery waits for its work. Zero waits a minute, so a test
	// reading the answer a finished plan gives is not answered early on a busy machine; a test of
	// the bound itself sets its own.
	AnswerWithin time.Duration
	// Approvals wires the dispatcher as the server's approver, keeping decision records.
	Approvals bool
	// Options adds server options, for a test wiring more than review.
	Options []Option
}

// refusingSubmitter answers every Submit with err and passes everything else to the dispatcher it
// wraps, so the gate's scan and the reporter's previews still run.
type refusingSubmitter struct {
	*dispatch.Dispatcher
	// err is what Submit answers.
	err error
}

// Submit answers err.
func (s refusingSubmitter) Submit(context.Context, string, string, ...run.SubmitOption) (*run.Run,
	error) {
	return nil, s.err
}

// newReviewServer builds the whole path a pull request travels: a git origin with a pull request
// ref, a project over it, a terraform template with a credential, a dispatcher executing with the
// fake runner, and a server whose review trigger reports to a fake forge.
func newReviewServer(t *testing.T, setup reviewSetup) *reviewServer {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	if setup.Provider == "" {
		setup.Provider = trigger.ProviderGitHub
	}
	if setup.Plan == "" {
		setup.Plan = "  + aws_instance.web\nPlan: 1 to add, 0 to change, 0 to destroy.\n"
	}
	repoName, ref := "acme/infra", "refs/pull/7/head"
	forge := forgetest.NewGitHub(t, reviewForgeToken, repoName)
	if setup.Provider == trigger.ProviderGitLab {
		repoName, ref = "infra/network", "refs/merge-requests/7/head"
		forge = forgetest.NewGitLab(t, reviewForgeToken, repoName)
	}
	prFiles := setup.PRFiles
	if prFiles == nil {
		prFiles = map[string]string{"infra/plan.txt": setup.Plan}
	}
	origin, head := newPROrigin(t, ref, map[string]string{"infra/plan.txt": "main\n"}, prFiles)

	sealer := credential.NewSealer("pass", "salt")
	creds := credential.NewMemStore()
	for id, v := range map[string]struct {
		Kind  credential.Kind
		Value string
	}{
		"cred_vcs":   {credential.KindToken, reviewForgeToken},
		"cred_cloud": {credential.KindEnv, "CLOUD_SECRET=" + reviewCloudSecret},
	} {
		sealed, err := sealer.Seal(v.Value)
		if err != nil {
			t.Fatalf("Seal() error = %v", err)
		}
		c := &credential.Credential{ID: id, Name: id, Kind: v.Kind, Secret: sealed}
		if err := creds.Save(ctx, c); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	projects := project.NewMemStore()
	if err := projects.Save(ctx, &project.Project{ID: "proj_infra", Name: "infra", RepoURL: origin.dir,
		Branch: "main"}); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	syncer, err := project.NewSyncer(t.TempDir())
	if err != nil {
		t.Fatalf("NewSyncer() error = %v", err)
	}
	templates := template.NewMemStore()
	tpl := &template.Template{ID: "tpl_net", Name: "network", Tool: run.ToolTerraform,
		Command: "infra", ProjectID: "proj_infra", CredentialIDs: []string{"cred_cloud"}}
	if setup.Playbook != "" {
		tpl.Tool, tpl.Command, tpl.Playbook = run.ToolAnsible, "", setup.Playbook
		tpl.Inventory = "hosts.ini"
	}
	if err := templates.Save(ctx, tpl); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	runs := run.NewMemStore()
	audits := setup.Audits
	if audits == nil {
		audits = audit.NewMemStore()
	}
	policies := policy.NewMemStore()
	specs := &specLog{}
	runner := setup.Runner
	if runner == nil {
		runner = planRunner(specs)
	}
	// The run directories live in a root of this test's own, so no other process on the machine
	// sweeping the shared default root can touch the files of a plan while it runs.
	dopts := []dispatch.Option{dispatch.WithProjects(projects, syncer),
		dispatch.WithCredentials(creds, sealer), dispatch.WithPolicies(policies),
		dispatch.WithAudits(audits), dispatch.WithRunFilesRoot(t.TempDir()),
		dispatch.WithClaimInterval(5 * time.Millisecond)}
	var decisions decision.Store
	if setup.Approvals {
		decisions = decision.NewMemStore()
		dopts = append(dopts, dispatch.WithDecisions(decisions))
	}
	disp := dispatch.New(runs, runner, zap.NewNop(), dopts...)
	var submitter Submitter = disp
	if setup.SubmitErr != nil {
		submitter = refusingSubmitter{Dispatcher: disp, err: setup.SubmitErr}
	}
	within := setup.AnswerWithin
	if within == 0 {
		within = time.Minute
	}
	opts := []Option{
		WithTriggers(trigger.NewMemStore(), sealer), WithTemplates(templates),
		WithCredentials(creds, sealer), WithProjects(projects), WithPolicies(policies), WithAudit(audits),
		WithReviewReporting("https://st.example.com", forge.Client(), 5*time.Millisecond),
		WithShutdown(ctx), WithHookAnswerWithin(within),
	}
	if setup.Approvals {
		opts = append(opts, WithApprover(disp), WithDecisions(decisions))
	}
	srv := New(runs, submitter, zap.NewNop(), append(opts, setup.Options...)...)
	t.Cleanup(func() {
		cancel()
		srv.WaitForHooks(context.Background())
		srv.reviews.Wait()
		disp.Close()
	})
	rs := &reviewServer{
		handler: srv.Handler(), srv: srv, forge: forge, runs: runs, audits: audits, policies: policies,
		specs: specs, origin: origin, headSHA: head, provider: setup.Provider, decisions: decisions,
		disp: disp,
	}
	reqBody := map[string]any{
		"name": "pr plans", "template_id": "tpl_net",
		"review": map[string]any{"provider": setup.Provider, "api_url": forge.APIURL(),
			"repository": repoName, "credential_id": "cred_vcs", "allow_forks": setup.AllowForks},
	}
	rec := rs.do(t, http.MethodPost, "/v1/triggers", reqBody)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create review trigger status = %d, body %s", rec.Code, rec.Body.String())
	}
	var created createTriggerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode trigger: %v", err)
	}
	rs.hookPath, rs.secret = created.WebhookPath, created.SigningSecret
	rs.triggerID = created.Trigger.ID
	return rs
}

// do sends a JSON API request.
func (rs *reviewServer) do(t *testing.T, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	rs.handler.ServeHTTP(rec, req)
	return rec
}

// payload returns a pull request event for the server's provider proposing sha. A head repository
// other than the base makes it a fork.
func (rs *reviewServer) payload(action, sha, headRepo string) []byte {
	if rs.provider == trigger.ProviderGitLab {
		source := 5
		if headRepo != "" {
			source = 9
		}
		oldrev := ""
		if action == "update" {
			oldrev = strings.Repeat("0", 40)
		}
		return []byte(fmt.Sprintf(`{"object_kind":"merge_request",`+
			`"project":{"path_with_namespace":"infra/network"},"object_attributes":{"iid":7,`+
			`"action":%q,"oldrev":%q,"source_project_id":%d,"target_project_id":5,`+
			`"source_branch":"feature","url":"https://gitlab.example.com/mr/7","last_commit":{"id":%q}}}`,
			action, oldrev, source, sha))
	}
	if headRepo == "" {
		headRepo = "acme/infra"
	}
	return []byte(fmt.Sprintf(`{"action":%q,"number":7,"pull_request":{`+
		`"html_url":"https://github.example.com/acme/infra/pull/7",`+
		`"head":{"sha":%q,"ref":"feature","repo":{"full_name":%q}},`+
		`"base":{"sha":"x","ref":"main","repo":{"full_name":"acme/infra"}}}}`, action, sha, headRepo))
}

// opened and pushed are the provider's names for a new pull request and a push to one.
func (rs *reviewServer) opened() string {
	if rs.provider == trigger.ProviderGitLab {
		return "open"
	}
	return "opened"
}

// pushed is the provider's action for new commits on a pull request.
func (rs *reviewServer) pushed() string {
	if rs.provider == trigger.ProviderGitLab {
		return "update"
	}
	return "synchronize"
}

// fire delivers body to the trigger, signed or tokened the way the provider does unless auth says
// otherwise: "" signs correctly, "none" sends no credential, anything else is sent as the
// credential itself.
func (rs *reviewServer) fire(t *testing.T, event string, body []byte, auth string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, rs.hookPath, bytes.NewReader(body))
	if rs.provider == trigger.ProviderGitLab {
		req.Header.Set("X-Gitlab-Event", event)
		switch auth {
		case "":
			req.Header.Set("X-Gitlab-Token", rs.secret)
		case "none":
		default:
			req.Header.Set("X-Gitlab-Token", auth)
		}
	} else {
		req.Header.Set("X-GitHub-Event", event)
		switch auth {
		case "":
			req.Header.Set("X-Hub-Signature-256", trigger.SignBody(rs.secret, body))
		case "none":
		default:
			req.Header.Set("X-Hub-Signature-256", auth)
		}
	}
	rec := httptest.NewRecorder()
	rs.handler.ServeHTTP(rec, req)
	return rec
}

// eventName is the provider's pull request event header value.
func (rs *reviewServer) eventName() string {
	if rs.provider == trigger.ProviderGitLab {
		return "Merge Request Hook"
	}
	return "pull_request"
}

// waitStatus waits until the forge holds a status on sha in state.
func (rs *reviewServer) waitStatus(t *testing.T, sha, state string) forgetest.Status {
	t.Helper()
	// A plan runs a real tool in some of these tests, which a busy machine slows by seconds, so the
	// deadline only bounds a status that will never come.
	deadline := time.Now().Add(2 * time.Minute)
	for {
		list := rs.forge.Statuses(sha)
		if len(list) > 0 && list[len(list)-1].State == state {
			return list[len(list)-1]
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for status %s on %s, have %+v", state, sha, list)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// planRun returns the run a review webhook answered with.
func (rs *reviewServer) planRun(t *testing.T, rec *httptest.ResponseRecorder) *run.Run {
	t.Helper()
	var resp map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp["run"] == "" {
		t.Fatalf("webhook answered %d %s, want a run", rec.Code, rec.Body.String())
	}
	r, err := rs.runs.Get(context.Background(), resp["run"])
	if err != nil {
		t.Fatalf("Get(%s) error = %v", resp["run"], err)
	}
	return r
}

// successState is the provider's spelling of a passing status.
const successState = "success"

// TestReviewHookPlansThePullRequestAndUpdatesOneComment walks a pull request through both forges:
// opened, it is planned at its head commit in no-change mode and reported as one comment and a
// status; pushed again, the same comment is updated in place and the new head gets its own status.
func TestReviewHookPlansThePullRequestAndUpdatesOneComment(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			rs := newReviewServer(t, reviewSetup{Provider: provider,
				Plan: "  - aws_instance.old\nPlan: 2 to add, 1 to change, 3 to destroy.\n"})

			rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("webhook status = %d, body %s", rec.Code, rec.Body.String())
			}
			planned := rs.planRun(t, rec)
			st := rs.waitStatus(t, rs.headSHA, successState)

			got, err := rs.runs.Get(context.Background(), planned.ID)
			if err != nil {
				t.Fatalf("Get() error = %v", err)
			}
			wantRef := "refs/pull/7/head"
			if provider == trigger.ProviderGitLab {
				wantRef = "refs/merge-requests/7/head"
			}
			if !got.DryRun || got.GitRef != wantRef || got.PinnedCommit != rs.headSHA ||
				got.CommitSHA != rs.headSHA || got.Source != review.Source ||
				got.Status != run.StatusSucceeded {
				t.Errorf("plan run = dry %v ref %q pin %q commit %q source %q status %s, want a dry run "+
					"of %s at %s", got.DryRun, got.GitRef, got.PinnedCommit, got.CommitSHA, got.Source,
					got.Status, wantRef, rs.headSHA)
			}
			specs := rs.specs.all()
			if len(specs) != 1 || !specs[0].DryRun || !strings.Contains(specs[0].Plan, "3 to destroy") {
				t.Errorf("executions = %+v, want one dry run of the pull request's plan file", specs)
			}
			if diff := cmp.Diff("switchtender/network", st.Context); diff != "" {
				t.Errorf("status context mismatch (-want +got):\n%s", diff)
			}
			if !strings.Contains(st.TargetURL, "/ui/runs/"+planned.ID) {
				t.Errorf("status target = %q, want a link to the run", st.TargetURL)
			}
			comments := rs.forge.Comments(7)
			if len(comments) != 1 {
				t.Fatalf("pull request holds %d comments, want 1", len(comments))
			}
			for _, want := range []string{"Plan succeeded", "| 2 | 1 | 3 | 0 |",
				"Destroy count the rules weigh: **3**", "https://st.example.com/ui/runs/" + planned.ID,
				"Receipt `" + got.AuditReceipt + "`"} {
				if !strings.Contains(comments[0].Body, want) {
					t.Errorf("comment lacks %q:\n%s", want, comments[0].Body)
				}
			}

			second := rs.origin.Push(t, map[string]string{
				"infra/plan.txt": "Plan: 0 to add, 0 to change, 0 to destroy.\n",
			})
			rec = rs.fire(t, rs.eventName(), rs.payload(rs.pushed(), second, ""), "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("second webhook status = %d, body %s", rec.Code, rec.Body.String())
			}
			rs.waitStatus(t, second, successState)
			rs.srv.reviews.Wait()
			comments = rs.forge.Comments(7)
			if len(comments) != 1 || comments[0].Edits < 1 {
				t.Fatalf("comments = %+v, want the one comment updated in place", comments)
			}
			if !strings.Contains(comments[0].Body, second[:12]) {
				t.Errorf("the updated comment does not describe the new head %s:\n%s", second, comments[0].Body)
			}
		})
	}
}

// TestReviewHookVerifiesEveryDelivery proves a review webhook acts on nothing it cannot
// authenticate, on either forge: no signature or token, or a wrong one, launches no plan and
// reaches no forge.
func TestReviewHookVerifiesEveryDelivery(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			rs := newReviewServer(t, reviewSetup{Provider: provider})
			body := rs.payload(rs.opened(), rs.headSHA, "")
			for _, auth := range []string{"none", "sha256=" + strings.Repeat("0", 64), "whs_wrong"} {
				rec := rs.fire(t, rs.eventName(), body, auth)
				if rec.Code != http.StatusUnauthorized {
					t.Errorf("auth %q: status = %d, want 401 (body %s)", auth, rec.Code, rec.Body.String())
				}
			}
			list, err := rs.runs.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(list) != 0 || len(rs.forge.Bodies()) != 0 {
				t.Errorf("an unauthenticated delivery launched %d runs and %d forge requests",
					len(list), len(rs.forge.Bodies()))
			}
		})
	}
}

// TestReviewHookRefusesForksUnlessAllowed proves a pull request from a fork is not planned by
// default, is recorded, and is told why with a commit status alone, and is planned once the
// trigger allows forks.
func TestReviewHookRefusesForksUnlessAllowed(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rs := newReviewServer(t, reviewSetup{})
	rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, "outsider/infra"), "")
	if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), "fork") {
		t.Fatalf("fork webhook = %d %s, want 202 refused", rec.Code, rec.Body.String())
	}
	st := rs.waitStatus(t, rs.headSHA, "error")
	rs.srv.reviews.Wait()
	if st.Description != review.ForkStatus || st.TargetURL != review.ForkDocsURL {
		t.Errorf("status = %q linked to %q, want %q linked to %q", st.Description, st.TargetURL,
			review.ForkStatus, review.ForkDocsURL)
	}
	list, err := rs.runs.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 || len(rs.specs.all()) != 0 {
		t.Errorf("a fork's pull request launched %d runs", len(list))
	}
	if c := rs.forge.Comments(7); len(c) != 0 {
		t.Errorf("comments = %+v, want none: a fork's refusal is a commit status alone", c)
	}
	if !chainHasPath(t, rs.audits, "/hooks/"+rs.triggerID+"/review/7/refused") {
		t.Error("the refusal is not on the chain")
	}

	allowed := newReviewServer(t, reviewSetup{AllowForks: true})
	forked := allowed.payload(allowed.opened(), allowed.headSHA, "outsider/infra")
	rec = allowed.fire(t, allowed.eventName(), forked, "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("allowed fork webhook = %d %s", rec.Code, rec.Body.String())
	}
	allowed.planRun(t, rec)
	allowed.waitStatus(t, allowed.headSHA, successState)
}

// TestReviewHookNeverPostsACredential proves a credential value the plan printed reaches no forge
// request, and that the forge token itself is never sent as content.
func TestReviewHookNeverPostsACredential(t *testing.T) {
	t.Parallel()
	rs := newReviewServer(t, reviewSetup{})
	rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook status = %d, body %s", rec.Code, rec.Body.String())
	}
	rs.waitStatus(t, rs.headSHA, successState)
	rs.srv.reviews.Wait()
	bodies := rs.forge.Bodies()
	if len(bodies) == 0 {
		t.Fatal("nothing reached the forge")
	}
	for _, b := range bodies {
		if strings.Contains(b, reviewCloudSecret) || strings.Contains(b, reviewForgeToken) {
			t.Errorf("a forge request carried a secret: %s", b)
		}
	}
	if c := rs.forge.Comments(7); len(c) != 1 || !strings.Contains(c[0].Body, "provider debug: ***") {
		t.Errorf("the comment does not show the masked line:\n%+v", c)
	}
}

// TestReviewHookEntersTheChain proves the webhook, the plan's outcome, and every report are on the
// chain with the webhook first, that the plan carries the webhook's receipt, and that the chain
// verifies.
func TestReviewHookEntersTheChain(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rs := newReviewServer(t, reviewSetup{})
	rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
	planned := rs.planRun(t, rec)
	rs.waitStatus(t, rs.headSHA, successState)
	rs.srv.reviews.Wait()

	chain, err := rs.audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Fatalf("chain does not verify, broke at %d", at)
	}
	base := "/hooks/" + rs.triggerID + "/review/7"
	var order []string
	for _, e := range chain {
		switch {
		case e.Path == base+"/planned":
			order = append(order, "planned")
			if receipt := audit.Receipt(e); receipt != planned.AuditReceipt ||
				receipt != rec.Header().Get(AuditReceiptHeader) {
				t.Errorf("planned entry receipt %s, run carries %s, response %s", receipt,
					planned.AuditReceipt, rec.Header().Get(AuditReceiptHeader))
			}
		case strings.HasPrefix(e.Path, "/runs/"+planned.ID+"/outcome/"):
			order = append(order, "outcome")
		case e.Path == base+"/report":
			if e.ContentDigest == "" {
				t.Errorf("report entry %d commits to no content", e.Seq)
			}
			order = append(order, "report")
		}
	}
	if !reviewChainOrderHolds(order) {
		t.Errorf("chain order = %s, want the webhook first and the final report after the one "+
			"outcome", strings.Join(order, ","))
	}
}

// reviewChainOrderHolds reports whether a review's chain entries, in order, keep the order the
// review promises: the webhook first, before anything ran, one outcome, and the final report after
// it, so the pull request is never told a plan finished before the chain records how.
//
// A plan that finishes before the reporter first looks is reported finished alone, so the running
// report is not required. When the plan finishes while its running report is being posted, that
// report's entry can land after the outcome, so only the last report is pinned after the outcome.
func reviewChainOrderHolds(order []string) bool {
	got := strings.Join(order, ",")
	return strings.HasPrefix(got, "planned,") && strings.Count(got, "outcome") == 1 &&
		strings.HasSuffix(got, ",report")
}

// TestReviewChainOrderHolds pins the order check against the interleavings a busy machine
// produces and the one it exists to catch.
func TestReviewChainOrderHolds(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Order    string
		WantHold bool
	}{{ // Test 0: The plan finished before the reporter first looked.
		Order: "planned,outcome,report", WantHold: true,
	}, { // Test 1: Running reported, then the outcome, then the final report.
		Order: "planned,report,outcome,report", WantHold: true,
	}, { // Test 2: The run finished while its running report was being posted.
		Order: "planned,outcome,report,report", WantHold: true,
	}, { // Test 3: The final report was recorded before the outcome.
		Order: "planned,report,outcome", WantHold: false,
	}, { // Test 4: No outcome was recorded at all.
		Order: "planned,report,report", WantHold: false,
	}, { // Test 5: The webhook was not recorded first.
		Order: "outcome,planned,report", WantHold: false,
	}, { // Test 6: Two outcomes for one plan.
		Order: "planned,outcome,outcome,report", WantHold: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := reviewChainOrderHolds(strings.Split(test.Order, ",")); got != test.WantHold {
				t.Errorf("reviewChainOrderHolds(%s) = %t, want %t", test.Order, got, test.WantHold)
			}
		})
	}
}

// TestReviewHookIgnoresWhatIsNotNewCode proves a review trigger plans only pull request events that
// propose code, for its own repository.
func TestReviewHookIgnoresWhatIsNotNewCode(t *testing.T) {
	t.Parallel()
	rs := newReviewServer(t, reviewSetup{})
	tests := []struct {
		Event    string
		Body     []byte
		WantCode int
	}{{ // Test 0: A push event is not a pull request.
		Event: "push", Body: []byte(`{"ref":"refs/heads/main"}`), WantCode: http.StatusAccepted,
	}, { // Test 1: A label proposes no code.
		Event: "pull_request", Body: rs.payload("labeled", rs.headSHA, ""), WantCode: http.StatusAccepted,
	}, { // Test 2: Another repository's pull request is refused.
		Event: "pull_request", Body: bytes.ReplaceAll(rs.payload("opened", rs.headSHA, ""),
			[]byte(`"base":{"sha":"x","ref":"main","repo":{"full_name":"acme/infra"}}`),
			[]byte(`"base":{"sha":"x","ref":"main","repo":{"full_name":"acme/other"}}`)),
		WantCode: http.StatusUnprocessableEntity,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := rs.fire(t, test.Event, test.Body, "")
			if rec.Code != test.WantCode {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, test.WantCode, rec.Body.String())
			}
			list, err := rs.runs.List(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(list) != 0 {
				t.Errorf("an event proposing no code launched %d runs", len(list))
			}
		})
	}
}

// TestReviewCommentShowsTheApplyDecision proves the comment and status carry the decision the apply
// would get after merge, decided by the same rules the gate enforces: a destroy over a plan-content
// limit is held, and a deny rule that leaves plans alone fails the check.
func TestReviewCommentShowsTheApplyDecision(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tests := []struct {
		Policy      *policy.Policy
		WantState   string
		WantComment string
	}{{ // Test 0: A destroy over the limit would be held at the plan gate.
		Policy: &policy.Policy{ID: "pol_g", Name: "destroy guard", Tool: run.ToolTerraform,
			MaxDestroy: 0},
		WantState: successState, WantComment: "`destroy guard (plan destroys 2, limit 0)`",
	}, { // Test 1: A deny rule that excludes dry runs lets the plan run and fails the check.
		Policy: &policy.Policy{ID: "pol_d", Name: "frozen", Tool: run.ToolTerraform,
			Effect: policy.EffectDeny, ExcludeDryRun: true, MaxDestroy: policy.DisabledMaxDestroy},
		WantState: "failure", WantComment: "would be refused",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rs := newReviewServer(t, reviewSetup{Plan: "Plan: 0 to add, 0 to change, 2 to destroy.\n"})
			if err := rs.policies.Save(ctx, test.Policy); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
			rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("webhook status = %d, body %s", rec.Code, rec.Body.String())
			}
			rs.waitStatus(t, rs.headSHA, test.WantState)
			rs.srv.reviews.Wait()
			if c := rs.forge.Comments(7); len(c) != 1 || !strings.Contains(c[0].Body, test.WantComment) {
				t.Errorf("comment lacks %q:\n%+v", test.WantComment, c)
			}
			for _, sp := range rs.specs.all() {
				if !sp.DryRun {
					t.Error("a review executed a run that was not a dry run")
				}
			}
		})
	}
}

// reviewTriggerBody builds a POST /v1/triggers body for template with a valid GitHub review that
// mod may change.
func reviewTriggerBody(template string, mod func(map[string]any)) map[string]any {
	rv := map[string]any{"provider": "github", "repository": "acme/infra", "credential_id": "cred_vcs"}
	if mod != nil {
		mod(rv)
	}
	return map[string]any{"name": "x", "template_id": template, "review": rv}
}

// TestReviewTriggerConfiguration pins what a review trigger may be: it needs the encryption key, a
// token credential, and a template that runs from a project, and it always verifies its
// deliveries.
func TestReviewTriggerConfiguration(t *testing.T) {
	t.Parallel()
	rs := newReviewServer(t, reviewSetup{})
	ctx := context.Background()
	tpls := rs.srv.templates
	local := &template.Template{ID: "tpl_local", Name: "local", Tool: run.ToolBash, Command: "echo"}
	if err := tpls.Save(ctx, local); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	tests := []struct {
		Body     map[string]any
		WantCode int
	}{{ // Test 0: A credential that is not a token is refused.
		Body: reviewTriggerBody("tpl_net", func(r map[string]any) {
			r["credential_id"] = "cred_cloud"
		}),
		WantCode: http.StatusBadRequest,
	}, { // Test 1: A template with no project cannot fetch a pull request.
		Body: reviewTriggerBody("tpl_local", nil), WantCode: http.StatusBadRequest,
	}, { // Test 2: An unknown provider is refused.
		Body:     reviewTriggerBody("tpl_net", func(r map[string]any) { r["provider"] = "bitbucket" }),
		WantCode: http.StatusBadRequest,
	}, { // Test 3: A plain http API base is refused.
		Body: reviewTriggerBody("tpl_net", func(r map[string]any) {
			r["api_url"] = "http://ghe.example.com/api/v3"
		}),
		WantCode: http.StatusBadRequest,
	}, { // Test 4: A valid review is created.
		Body: reviewTriggerBody("tpl_net", nil), WantCode: http.StatusCreated,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			rec := rs.do(t, http.MethodPost, "/v1/triggers", test.Body)
			if rec.Code != test.WantCode {
				t.Errorf("status = %d, want %d (body %s)", rec.Code, test.WantCode, rec.Body.String())
			}
		})
	}

	// A review trigger verifies its deliveries even when the request asked it not to, and the check
	// cannot be turned off afterward.
	body := reviewTriggerBody("tpl_net", nil)
	body["require_signature"] = false
	rec := rs.do(t, http.MethodPost, "/v1/triggers", body)
	var resp createTriggerResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Trigger == nil {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if !resp.Trigger.RequireSignature || resp.SigningSecret == "" {
		t.Errorf("review trigger require_signature = %v secret %q, want enforced with a secret",
			resp.Trigger.RequireSignature, resp.SigningSecret)
	}
	if rec := rs.do(t, http.MethodPut, "/v1/triggers/"+resp.Trigger.ID,
		map[string]any{"name": "x", "require_signature": false}); rec.Code != http.StatusConflict {
		t.Errorf("turning off a review trigger's signature = %d, want 409", rec.Code)
	}

	unkeyed := New(run.NewMemStore(), &fakeSubmitter{}, zap.NewNop(),
		WithTriggers(trigger.NewMemStore(), credential.NewSealer("", "")), WithTemplates(tpls)).Handler()
	raw, err := json.Marshal(reviewTriggerBody("tpl_net", nil))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	rec = httptest.NewRecorder()
	unkeyed.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/v1/triggers", bytes.NewReader(raw)))
	if rec.Code != http.StatusConflict {
		t.Errorf("a review trigger on a server with no encryption key = %d, want 409", rec.Code)
	}
}

// TestReviewPlanNeverApplies proves with real Terraform that a review plan changes nothing. The
// pull request's configuration creates a file when applied. The review plan runs and reports a
// resource to add, and the file does not exist; the same commit run for real creates it, which
// shows the file is exactly what an apply would have left behind.
func TestReviewPlanNeverApplies(t *testing.T) {
	t.Parallel()
	if _, err := exec.LookPath("terraform"); err != nil {
		if os.Getenv("SWITCHTENDER_REQUIRE_FULL_SUITE") == "1" {
			t.Fatal("SWITCHTENDER_REQUIRE_FULL_SUITE is set and terraform is not installed, so the " +
				"plan-only proof cannot run")
		}
		t.Skip("terraform not on PATH")
	}
	marker := filepath.Join(t.TempDir(), "applied")
	mainTF := fmt.Sprintf("resource \"terraform_data\" \"proof\" {\n"+
		"  provisioner \"local-exec\" {\n    command = \"touch %s\"\n  }\n}\n", marker)
	rs := newReviewServer(t, reviewSetup{
		Runner:  roundhouse.NewAnsibleRunner(),
		PRFiles: map[string]string{"infra/main.tf": mainTF},
	})
	rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
	if rec.Code != http.StatusAccepted {
		t.Fatalf("webhook status = %d, body %s", rec.Code, rec.Body.String())
	}
	planned := rs.planRun(t, rec)
	rs.waitStatus(t, rs.headSHA, successState)
	rs.srv.reviews.Wait()
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("the review plan applied the change: %s exists (err %v)", marker, err)
	}
	if c := rs.forge.Comments(7); len(c) != 1 || !strings.Contains(c[0].Body, "| 1 | 0 | 0 | 0 |") {
		t.Errorf("the comment does not report the planned add:\n%+v", c)
	}

	applied, err := rs.srv.submitter.Submit(context.Background(), "", "",
		run.WithTool(run.ToolTerraform), run.WithCommand("infra"), run.WithProject("proj_infra"),
		run.WithGitRef(planned.GitRef), run.WithPinnedCommit(rs.headSHA))
	if err != nil {
		t.Fatalf("Submit() error = %v", err)
	}
	deadline := time.Now().Add(2 * time.Minute)
	for {
		r, err := rs.runs.Get(context.Background(), applied.ID)
		if err == nil && r.Status.Terminal() {
			if r.Status != run.StatusSucceeded {
				t.Fatalf("the control apply ended %s: %s", r.Status, r.Error)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for the control apply")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Errorf("the control apply did not create %s, so the plan proof shows nothing: %v", marker, err)
	}
}

// chainHasPath reports whether the chain holds an entry at path.
func chainHasPath(t *testing.T, audits audit.Store, path string) bool {
	t.Helper()
	chain, err := audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if e.Path == path {
			return true
		}
	}
	return false
}

// TestReviewHookRefusesAPlaybookThatForcesRealTasks proves a pull request whose playbook sets
// check_mode to false is not planned, since its plan would change hosts from an unmerged branch:
// no run is created, the refusal is recorded, the pull request is told which task forces it, and
// its status is an error. The same template with a playbook that forces nothing is planned.
func TestReviewHookRefusesAPlaybookThatForcesRealTasks(t *testing.T) {
	t.Parallel()
	const forcing = "- hosts: all\n  tasks:\n    - name: Restart the service\n" +
		"      ansible.builtin.command: systemctl restart web\n      check_mode: false\n"
	const clean = "- hosts: all\n  tasks:\n    - name: Show uptime\n" +
		"      ansible.builtin.command: uptime\n"
	tests := []struct {
		// Playbook is the playbook the pull request's head carries.
		Playbook string
		// WantRefused is whether the plan is refused.
		WantRefused bool
	}{{ // Test 0: A playbook forcing a task under check mode is refused.
		Playbook: forcing, WantRefused: true,
	}, { // Test 1: A playbook that forces nothing is planned.
		Playbook: clean,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			ctx := context.Background()
			rs := newReviewServer(t, reviewSetup{Playbook: "site.yml",
				PRFiles: map[string]string{"site.yml": test.Playbook, "hosts.ini": "web1\n"}})
			rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
			if rec.Code != http.StatusAccepted {
				t.Fatalf("webhook = %d %s, want 202", rec.Code, rec.Body.String())
			}
			if !test.WantRefused {
				if r := rs.planRun(t, rec); !r.DryRun {
					t.Error("the plan is not a dry run")
				}
				return
			}
			st := rs.waitStatus(t, rs.headSHA, "error")
			rs.srv.reviews.Wait()
			if !strings.HasPrefix(st.Description, "Not planned") {
				t.Errorf("status description = %q, want the refusal", st.Description)
			}
			list, err := rs.runs.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(list) != 0 {
				t.Errorf("a refused plan created %d runs", len(list))
			}
			c := rs.forge.Comments(7)
			if len(c) != 1 || !strings.Contains(c[0].Body, "check mode") ||
				!strings.Contains(c[0].Body, "Restart the service") {
				t.Errorf("comments = %+v, want one naming the forcing task", c)
			}
			if !chainHasPath(t, rs.audits, "/hooks/"+rs.triggerID+"/review/7/refused") {
				t.Error("the refusal is not on the chain")
			}
		})
	}
}

// TestReviewPlanMatchingNoHostsIsSkipped proves a pull request whose plan's composed inventory
// matches no hosts is skipped the way a schedule or a webhook fire is: answered 200 as skipped, the
// skip recorded on the chain after the request's entry, no run, and a commit status reading
// "Not planned: the inventory matched no hosts" with no comment.
func TestReviewPlanMatchingNoHostsIsSkipped(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	rs := newReviewServer(t, reviewSetup{SubmitErr: fmt.Errorf("%w: patch window matched no hosts",
		inventory.ErrNoHosts)})
	rec := rs.fire(t, rs.eventName(), rs.payload(rs.opened(), rs.headSHA, ""), "")
	if rec.Code != http.StatusOK {
		t.Fatalf("webhook = %d %s, want 200", rec.Code, rec.Body.String())
	}
	var body map[string]string
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rec.Body.String(), err)
	}
	if diff := cmp.Diff(map[string]string{"trigger": rs.triggerID, "skipped": "no hosts matched",
		"pull_request": "7"}, body); diff != "" {
		t.Errorf("body mismatch (-want +got):\n%s", diff)
	}
	st := rs.waitStatus(t, rs.headSHA, "error")
	rs.srv.reviews.Wait()
	if st.Description != review.NoHostsStatus || st.TargetURL != review.NoHostsDocsURL {
		t.Errorf("status = %q linking %q, want %q linking %q", st.Description, st.TargetURL,
			review.NoHostsStatus, review.NoHostsDocsURL)
	}
	if c := rs.forge.Comments(7); len(c) != 0 {
		t.Errorf("comments = %+v, want none", c)
	}
	list, err := rs.runs.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(list) != 0 {
		t.Errorf("a skipped plan created %d runs", len(list))
	}
	for _, path := range []string{"planned", "skipped"} {
		if !chainHasPath(t, rs.audits, "/hooks/"+rs.triggerID+"/review/7/"+path) {
			t.Errorf("the chain has no %s entry", path)
		}
	}
}
