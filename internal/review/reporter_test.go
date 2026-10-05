package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/review/forgetest"
	"github.com/kordloom/switchtender/internal/roundhouse"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// cloudSecret is a credential value a plan printed. It must never reach a forge.
const cloudSecret = "cloud-secret-value-99"

// failingAudits is an audit store whose appends fail, standing in for an unhealthy trail.
type failingAudits struct {
	audit.Store
}

// Append always fails.
func (failingAudits) Append(context.Context, *audit.Entry) error { return errors.New("disk full") }

// harness wires reporters to a fake forge and in-memory stores. Every reporter it builds shares the
// stores, as replicas sharing one database do.
type harness struct {
	// forge is the fake the reports go to.
	forge *forgetest.Forge
	// trigger is the review trigger reported for.
	trigger *trigger.Trigger
	// triggers holds the trigger.
	triggers trigger.Store
	// templates holds the template the trigger plans.
	templates template.Store
	// creds holds the forge token and the plan's cloud credential.
	creds credential.Store
	// sealer seals the credentials.
	sealer *credential.Sealer
	// redactor masks plan output, nil when the harness withholds it.
	redactor Redactor
	// runs holds the plan runs.
	runs run.Store
	// audits holds the chain.
	audits audit.Store
	// store holds the report records.
	store Store
	// reporter is the default reporter under test.
	reporter *Reporter
}

// newHarness builds a reporter for provider. A false useRedactor withholds output; audits replaces
// the in-memory chain when set.
func newHarness(t *testing.T, provider string, useRedactor bool, audits audit.Store) *harness {
	t.Helper()
	ctx := context.Background()
	fake, cfg := newFake(t, provider)
	sealer := credential.NewSealer("pass", "salt")
	creds := credential.NewMemStore()
	for id, v := range map[string]struct {
		Kind  credential.Kind
		Value string
	}{
		"cred_vcs":   {credential.KindToken, testToken},
		"cred_cloud": {credential.KindEnv, "CLOUD_SECRET=" + cloudSecret},
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
	templates := template.NewMemStore()
	tpl := &template.Template{ID: "tpl_net", Name: "network", Tool: run.ToolTerraform,
		Command: "infra", ProjectID: "proj_1"}
	if err := templates.Save(ctx, tpl); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if audits == nil {
		audits = audit.NewMemStore()
	}
	var redactor Redactor
	if useRedactor {
		idle := roundhouse.RunnerFunc(
			func(context.Context, roundhouse.Spec, io.Writer) (roundhouse.Result, error) {
				return roundhouse.Result{}, nil
			})
		// The dispatcher masks only. It gets a store of its own, so its claim loop can never pick up
		// a plan this test is moving through its phases by hand.
		d := dispatch.New(run.NewMemStore(), idle, zap.NewNop(), dispatch.WithCredentials(creds, sealer))
		t.Cleanup(d.Close)
		redactor = d
	}
	tg := &trigger.Trigger{ID: "trg_rev", Name: "review", TemplateID: "tpl_net", Review: cfg}
	triggers := trigger.NewMemStore()
	if err := triggers.Save(ctx, tg); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	h := &harness{
		forge: fake, trigger: tg, triggers: triggers, templates: templates, creds: creds,
		sealer: sealer, redactor: redactor, runs: run.NewMemStore(), audits: audits,
		store: NewMemStore(),
	}
	h.reporter, _ = h.newReporter(t, nil)
	return h
}

// newReporter builds another reporter on the harness's stores and forge, the way a second process
// sharing the database is one, with mod applied to its configuration. It returns the reporter and
// a stop that ends it as a server shutdown does. The reporter is stopped when the test ends either
// way.
func (h *harness) newReporter(t *testing.T, mod func(*Config)) (*Reporter, func()) {
	t.Helper()
	done := make(chan struct{})
	cfg := Config{
		Runs: h.runs, Templates: h.templates, Triggers: h.triggers, Store: h.store,
		Credentials: h.creds, Sealer: h.sealer, Audits: h.audits, Redactor: h.redactor,
		HTTPClient: h.forge.Client(), PublicURL: "https://st.example.com/",
		Interval: 5 * time.Millisecond, Done: done, Log: zaptest.NewLogger(t),
	}
	if mod != nil {
		mod(&cfg)
	}
	rp := NewReporter(cfg)
	var once sync.Once
	stop := func() {
		once.Do(func() {
			close(done)
			rp.Wait()
		})
	}
	t.Cleanup(stop)
	return rp, stop
}

// saveRun stores a review plan run for sha at created, in status, with log as its output, and
// returns it. Its receipt is a real planned entry on the chain, the one its webhook would have
// written, when the chain accepts one. No outcome is committed.
func (h *harness) saveRun(t *testing.T, id, sha string, created time.Time, status run.Status, log string) *run.Run {
	t.Helper()
	ctx := context.Background()
	planned := &audit.Entry{ID: audit.NewID(), Actor: "webhook:trg_rev", Method: http.MethodPost,
		Path: "/hooks/trg_rev/review/7/planned"}
	receipt := ""
	if err := h.audits.Append(ctx, planned); err == nil {
		receipt = audit.Receipt(planned)
	}
	r := &run.Run{
		ID: id, Tool: run.ToolTerraform, Command: "infra", DryRun: true, Status: run.StatusRunning,
		ProjectID: "proj_1", GitRef: "refs/pull/7/head", PinnedCommit: sha, CommitSHA: sha,
		CredentialIDs: []string{"cred_cloud"}, Source: Source, SourceID: "trg_rev",
		Labels: map[string]string{LabelPullRequest: "7"}, AuditReceipt: receipt, CreatedAt: created,
	}
	if err := h.runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	if log != "" {
		if err := h.runs.AppendLog(ctx, id, []byte(log)); err != nil {
			t.Fatalf("AppendLog() error = %v", err)
		}
	}
	// The log is written while the run executes, as a real one is: a store fences appends to a
	// finished run.
	r.Status = status
	if status.Terminal() {
		ended := time.Now()
		r.EndedAt = &ended
	}
	if err := h.runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	return r
}

// planRun stores a review plan run as saveRun does and, for a finished one, commits its outcome to
// the chain the way the dispatcher does when a run ends.
func (h *harness) planRun(t *testing.T, id, sha string, created time.Time, status run.Status, log string) {
	t.Helper()
	h.saveRun(t, id, sha, created, status, log)
	if status.Terminal() {
		h.commitOutcome(t, id)
	}
}

// finish moves run id to status and commits its outcome, as the dispatcher's finalize does.
func (h *harness) finish(t *testing.T, id string, status run.Status) {
	t.Helper()
	ctx := context.Background()
	r := mustGet(t, h.runs, id)
	ended := time.Now()
	r.Status, r.EndedAt = status, &ended
	if err := h.runs.Save(ctx, r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	h.commitOutcome(t, id)
}

// commitOutcome commits run id's outcome to the chain.
func (h *harness) commitOutcome(t *testing.T, id string) {
	t.Helper()
	r := mustGet(t, h.runs, id)
	err := outcome.Commit(context.Background(), h.audits, h.runs, r, "system:test", nil)
	if err != nil {
		t.Fatalf("outcome.Commit() error = %v", err)
	}
}

// record reads a report record.
func (h *harness) record(t *testing.T, id string) *Record {
	t.Helper()
	rec, err := h.store.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return rec
}

// reportEntries counts the report entries on the chain, in chain order, with the sequence of each.
func (h *harness) reportEntries(t *testing.T) []int64 {
	t.Helper()
	chain, err := h.audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	var seqs []int64
	for _, e := range chain {
		if strings.HasSuffix(e.Path, "/report") {
			seqs = append(seqs, e.Seq)
		}
	}
	return seqs
}

// outcomeSeq returns the sequence of run id's outcome entry, zero when there is none.
func (h *harness) outcomeSeq(t *testing.T, id string) int64 {
	t.Helper()
	chain, err := h.audits.Chain(context.Background())
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	for _, e := range chain {
		if strings.HasPrefix(e.Path, "/runs/"+id+"/outcome/") {
			return e.Seq
		}
	}
	return 0
}

// settle waits, with a deadline, until the store owes no report, then for each of rps to finish
// what it holds, so a reporter that never settles fails the test rather than hanging it.
func (h *harness) settle(t *testing.T, rps ...*Reporter) {
	t.Helper()
	waitFor(t, "every report to settle", func() bool {
		pending, err := h.store.Pending(context.Background(), 0)
		return err == nil && len(pending) == 0
	})
	for _, rp := range rps {
		rp.Wait()
	}
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// lastStatus returns the newest status the forge holds for sha, or the zero status.
func lastStatus(f *forgetest.Forge, sha string) forgetest.Status {
	list := f.Statuses(sha)
	if len(list) == 0 {
		return forgetest.Status{}
	}
	return list[len(list)-1]
}

// states returns the states of every status the forge holds for sha, in arrival order.
func states(f *forgetest.Forge, sha string) string {
	var out []string
	for _, s := range f.Statuses(sha) {
		out = append(out, s.State)
	}
	return strings.Join(out, ",")
}

// planLog is a plan's raw output, carrying a credential value the plan printed.
const planLog = "Initializing...\nCLOUD_SECRET=" + cloudSecret + "\n" +
	"Terraform will perform the following actions:\n  # aws_instance.web will be destroyed\n" +
	"  - token = \"" + cloudSecret + "\"\n" +
	"Plan: 1 to add, 0 to change, 2 to destroy.\n"

// TestReporterKeepsOneCommentPerTemplate proves every push updates the same comment in place, and
// that a plan finishing late cannot overwrite the comment of a newer push, on both forges, when
// the forge does not say which commit is the pull request's head.
func TestReporterKeepsOneCommentPerTemplate(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, provider, true, nil)
			base := time.Now().Add(-time.Hour)
			first := strings.Repeat("a", 40)
			second := strings.Repeat("b", 40)
			stale := strings.Repeat("c", 40)

			h.planRun(t, "run_first", first, base, run.StatusSucceeded, planLog)
			h.reporter.Watch(h.trigger, 7, "run_first")
			waitFor(t, "the first status", func() bool {
				return lastStatus(h.forge, first).State == "success"
			})

			h.planRun(t, "run_second", second, base.Add(2*time.Minute), run.StatusSucceeded, planLog)
			h.reporter.Watch(h.trigger, 7, "run_second")
			waitFor(t, "the second status", func() bool {
				return lastStatus(h.forge, second).State == "success"
			})

			h.planRun(t, "run_stale", stale, base.Add(time.Minute), run.StatusFailed, "")
			h.reporter.Watch(h.trigger, 7, "run_stale")
			waitFor(t, "the stale status", func() bool { return lastStatus(h.forge, stale).State != "" })
			h.settle(t, h.reporter)

			comments := h.forge.Comments(7)
			if len(comments) != 1 {
				t.Fatalf("pull request holds %d comments, want 1 updated in place: %+v",
					len(comments), comments)
			}
			if comments[0].Edits != 1 {
				t.Errorf("comment edited %d times, want 1 (the second push)", comments[0].Edits)
			}
			body := comments[0].Body
			if !strings.Contains(body, "run_second") || strings.Contains(body, "run_stale") {
				t.Errorf("comment does not describe the newest plan:\n%s", comments[0].Body)
			}
		})
	}
}

// TestReporterMasksBeforeAnythingLeaves proves a credential value printed into a plan's log never
// reaches the forge, in the comment, the status, or any other request, and that the token itself is
// never sent as content. The log here is stored unmasked, as an older worker could have left it, so
// only the reporter's own masking stands between it and the pull request.
func TestReporterMasksBeforeAnythingLeaves(t *testing.T) {
	t.Parallel()
	for _, useRedactor := range []bool{true, false} {
		h := newHarness(t, trigger.ProviderGitHub, useRedactor, nil)
		sha := strings.Repeat("d", 40)
		h.planRun(t, "run_mask", sha, time.Now(), run.StatusSucceeded, planLog)
		h.reporter.Watch(h.trigger, 7, "run_mask")
		waitFor(t, "the status", func() bool { return lastStatus(h.forge, sha).State != "" })
		h.settle(t, h.reporter)

		for _, body := range h.forge.Bodies() {
			if strings.Contains(body, cloudSecret) || strings.Contains(body, testToken) {
				t.Errorf("redactor %v: a request carried a secret: %s", useRedactor, body)
			}
		}
		comment := h.forge.Comments(7)[0].Body
		switch {
		case useRedactor && !strings.Contains(comment, "Terraform will perform"):
			t.Errorf("the masked plan was not shown:\n%s", comment)
		case useRedactor && !strings.Contains(comment, "***"):
			t.Errorf("the plan shows no mask where the secret was:\n%s", comment)
		case !useRedactor && !strings.Contains(comment, "Plan output withheld"):
			t.Errorf("with no masker the output was not withheld:\n%s", comment)
		}
		if !strings.Contains(comment, "Destroy count the rules weigh: **2**") {
			t.Errorf("redactor %v: the destroy count is missing:\n%s", useRedactor, comment)
		}
	}
}

// TestReporterRecordsBeforePosting proves each report enters the chain before it is sent, commits
// to the exact comment body, and is not sent at all when it cannot be recorded.
func TestReporterRecordsBeforePosting(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	refused := newHarness(t, trigger.ProviderGitHub, true, failingAudits{audit.NewMemStore()})
	sha := strings.Repeat("e", 40)
	down := refused.saveRun(t, "run_down", sha, time.Now(), run.StatusSucceeded, planLog)
	rep := refused.reporter.planReport(ctx, refused.trigger, down)
	if _, err := refused.reporter.post(ctx, refused.trigger, PlanRecord("run_down", refused.trigger, 7,
		time.Now()), rep); err == nil {
		t.Fatal("post() error = nil with the trail down, want a refusal")
	}
	if n := len(refused.forge.Bodies()); n != 0 {
		t.Errorf("the forge received %d writes although the report was never recorded", n)
	}

	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	ok := h.saveRun(t, "run_ok", sha, time.Now(), run.StatusSucceeded, planLog)
	res, err := h.reporter.post(ctx, h.trigger, PlanRecord("run_ok", h.trigger, 7, time.Now()),
		h.reporter.planReport(ctx, h.trigger, ok))
	if err != nil {
		t.Fatalf("post() error = %v", err)
	}
	chain, err := h.audits.Chain(ctx)
	if err != nil {
		t.Fatalf("Chain() error = %v", err)
	}
	last := chain[len(chain)-1]
	if last.Path != "/hooks/trg_rev/review/7/report" {
		t.Fatalf("chain ends with %s, want the report entry", last.Path)
	}
	if ok, at := audit.Verify(chain); !ok {
		t.Errorf("chain does not verify, broke at %d", at)
	}
	body := h.forge.Comments(7)[0].Body
	sum := sha256.Sum256([]byte(body))
	content, err := json.Marshal(reportRecord{Run: "run_ok", Phase: PhaseSucceeded, Commit: sha,
		Status: StateSuccess, CommentSHA256: hex.EncodeToString(sum[:])})
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if !audit.VerifyContentDigest(last.ContentDigest, last.Nonce, content) {
		t.Error("the report entry does not commit to the comment body that was posted")
	}
	if res.comment != hex.EncodeToString(sum[:]) || res.state != StateSuccess {
		t.Errorf("post() = comment %q state %q, want the body's digest and success", res.comment,
			res.state)
	}
}

// TestReporterFollowsAPlanThroughItsPhases proves a plan is reported once per phase, running, held,
// and finished, and that a starting reporter takes in a plan already in flight that no record
// covers.
func TestReporterFollowsAPlanThroughItsPhases(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	h := newHarness(t, trigger.ProviderGitLab, true, nil)
	sha := strings.Repeat("f", 40)
	h.saveRun(t, "run_phases", sha, time.Now(), run.StatusPending, "")
	h.reporter.Resume(ctx, h.triggers)
	waitFor(t, "the running status", func() bool {
		return lastStatus(h.forge, sha).State == "pending"
	})

	var mu sync.Mutex
	step := func(status run.Status, held string) {
		mu.Lock()
		defer mu.Unlock()
		r := mustGet(t, h.runs, "run_phases")
		r.Status, r.HeldByPolicy = status, held
		if err := h.runs.Save(ctx, r); err != nil {
			t.Fatalf("Save() error = %v", err)
		}
	}
	step(run.StatusPendingApproval, "plans need a look")
	waitFor(t, "the held comment", func() bool {
		c := h.forge.Comments(7)
		return len(c) == 1 && strings.Contains(c[0].Body, "plans need a look")
	})
	h.finish(t, "run_phases", run.StatusSucceeded)
	waitFor(t, "the final status", func() bool { return lastStatus(h.forge, sha).State == "success" })
	h.settle(t, h.reporter)

	if got := states(h.forge, sha); got != "pending,pending,success" {
		t.Errorf("statuses = %s, want one per phase: pending, pending, success", got)
	}
	rec := h.record(t, "run_phases")
	if !rec.Done || rec.Phase != PhaseSucceeded || rec.StatusState != StateSuccess ||
		rec.ReportedAt.IsZero() {
		t.Errorf("record = done %v phase %q state %q at %v, want the final report recorded",
			rec.Done, rec.Phase, rec.StatusState, rec.ReportedAt)
	}
	comment := h.forge.Comments(7)[0].Body
	if want := digestOf(comment); rec.CommentSHA256 != want {
		t.Errorf("record comment digest = %q, want the digest of the comment posted %q",
			rec.CommentSHA256, want)
	}
}

// digestOf returns the hex SHA-256 of text.
func digestOf(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// TestReporterFinalReportWaitsForTheOutcome proves reports land in the order the evidence does: a
// plan that has finished in the store is not reported finished until its outcome entry is on the
// chain, and its final report entry follows the outcome entry.
func TestReporterFinalReportWaitsForTheOutcome(t *testing.T) {
	t.Parallel()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	sha := strings.Repeat("1", 40)
	h.saveRun(t, "run_order", sha, time.Now(), run.StatusRunning, "")
	h.reporter.Watch(h.trigger, 7, "run_order")
	waitFor(t, "the running status", func() bool {
		return lastStatus(h.forge, sha).State == "pending"
	})

	// The run finishes in the store, and its outcome has not reached the chain yet: the window
	// between the dispatcher's terminal write and its outcome commit, held open here.
	r := mustGet(t, h.runs, "run_order")
	ended := time.Now()
	r.Status, r.EndedAt = run.StatusSucceeded, &ended
	if err := h.runs.Save(context.Background(), r); err != nil {
		t.Fatalf("Save() error = %v", err)
	}
	time.Sleep(150 * time.Millisecond)
	if got := states(h.forge, sha); got != "pending" {
		t.Fatalf("statuses = %s before the outcome reached the chain, want the plan running",
			got)
	}

	h.commitOutcome(t, "run_order")
	waitFor(t, "the final status", func() bool { return lastStatus(h.forge, sha).State == "success" })
	h.settle(t, h.reporter)
	reports := h.reportEntries(t)
	if len(reports) != 2 {
		t.Fatalf("chain holds %d report entries, want 2 (running, succeeded)", len(reports))
	}
	if out := h.outcomeSeq(t, "run_order"); out == 0 || reports[1] < out {
		t.Errorf("final report entry %d precedes the outcome entry %d", reports[1], out)
	}
	if strings.Contains(h.forge.Comments(7)[0].Body, "did not reach the audit chain") {
		t.Error("a report that waited for its outcome says the outcome is missing")
	}
}

// TestReporterPostsAfterTheGraceWhenTheOutcomeNeverLands proves a plan whose outcome never reaches
// the chain is still reported once the grace runs out, saying so, rather than left showing a plan
// that never finishes.
func TestReporterPostsAfterTheGraceWhenTheOutcomeNeverLands(t *testing.T) {
	t.Parallel()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	rp, _ := h.newReporter(t, func(c *Config) { c.OutcomeGrace = 100 * time.Millisecond })
	sha := strings.Repeat("2", 40)
	h.saveRun(t, "run_lost", sha, time.Now(), run.StatusSucceeded, "")
	rp.Watch(h.trigger, 7, "run_lost")
	waitFor(t, "the final status", func() bool { return lastStatus(h.forge, sha).State == "success" })
	h.settle(t, rp)
	if !strings.Contains(h.forge.Comments(7)[0].Body, "did not reach the audit chain") {
		t.Errorf("the report does not say the outcome is missing:\n%s", h.forge.Comments(7)[0].Body)
	}
}

// TestReporterReportsAPlanThatFinishedDuringARestart proves a plan that finishes while no server is
// running is reported when one starts, exactly once: one that the stopped server had recorded and
// reported running, and one whose server stopped between launching it and recording it.
func TestReporterReportsAPlanThatFinishedDuringARestart(t *testing.T) {
	t.Parallel()
	tests := []struct {
		// Recorded is whether the first server recorded the plan before it stopped.
		Recorded bool
		// WantStates is the commit statuses the pull request ends with.
		WantStates string
	}{{ // Test 0: The plan was recorded and reported running, then the server stopped.
		Recorded: true, WantStates: "pending,success",
	}, { // Test 1: The server stopped before recording the plan, so nothing was reported.
		Recorded: false, WantStates: "success",
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, trigger.ProviderGitHub, true, nil)
			sha := strings.Repeat("3", 40)
			first, stop := h.newReporter(t, nil)
			h.saveRun(t, "run_restart", sha, time.Now(), run.StatusRunning, "")
			if test.Recorded {
				first.Watch(h.trigger, 7, "run_restart")
				waitFor(t, "the running status", func() bool {
					return lastStatus(h.forge, sha).State == "pending"
				})
			}
			stop()

			// The plan finishes while no server is running, and nothing tells the pull request.
			h.finish(t, "run_restart", run.StatusSucceeded)
			time.Sleep(50 * time.Millisecond)
			if lastStatus(h.forge, sha).State == "success" {
				t.Fatal("the pull request heard of the result with no server running")
			}

			second, _ := h.newReporter(t, nil)
			second.Resume(context.Background(), nil)
			waitFor(t, "the final status after the restart", func() bool {
				return lastStatus(h.forge, sha).State == "success"
			})
			h.settle(t, second)
			if got := states(h.forge, sha); got != test.WantStates {
				t.Errorf("statuses = %s, want %s", got, test.WantStates)
			}
			if n := len(h.reportEntries(t)); n != strings.Count(test.WantStates, ",")+1 {
				t.Errorf("chain holds %d report entries, want one per status", n)
			}
			c := h.forge.Comments(7)
			if len(c) != 1 || !strings.Contains(c[0].Body, "Plan succeeded") {
				t.Errorf("comments = %+v, want the one comment showing the result", c)
			}
		})
	}
}

// TestReporterReplicasReportEachPhaseOnce proves two processes sharing the store report every phase
// of every plan exactly once between them, and never write one pull request's comment at the same
// time: two pull requests, each pushed twice, every plan announced to both processes, against a
// forge slow enough that the two overlap.
func TestReporterReplicasReportEachPhaseOnce(t *testing.T) {
	t.Parallel()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	h.forge.SetDelay(15 * time.Millisecond)
	a, _ := h.newReporter(t, nil)
	b, _ := h.newReporter(t, nil)
	type plan struct {
		// ID is the run id.
		ID string
		// SHA is the commit planned.
		SHA string
		// PR is the pull request number.
		PR int
	}
	base := time.Now().Add(-time.Hour)
	plans := []plan{
		{ID: "run_p7a", SHA: strings.Repeat("4", 40), PR: 7},
		{ID: "run_p7b", SHA: strings.Repeat("5", 40), PR: 7},
		{ID: "run_p8a", SHA: strings.Repeat("6", 40), PR: 8},
		{ID: "run_p8b", SHA: strings.Repeat("7", 40), PR: 8},
	}
	tg := *h.trigger
	for i, p := range plans {
		created := base.Add(time.Duration(i) * time.Minute)
		r := h.saveRun(t, p.ID, p.SHA, created, run.StatusRunning, "")
		if p.PR != 7 {
			r.Labels = map[string]string{LabelPullRequest: "8"}
			if err := h.runs.Save(context.Background(), r); err != nil {
				t.Fatalf("Save() error = %v", err)
			}
		}
		// Both processes hear of every plan, as they do when a webhook is redelivered to the other.
		a.Watch(&tg, p.PR, p.ID)
		b.Watch(&tg, p.PR, p.ID)
	}
	for _, p := range plans {
		waitFor(t, "the running status of "+p.ID, func() bool {
			return lastStatus(h.forge, p.SHA).State == "pending"
		})
	}
	for _, p := range plans {
		h.finish(t, p.ID, run.StatusSucceeded)
	}
	for _, p := range plans {
		waitFor(t, "the final status of "+p.ID, func() bool {
			return lastStatus(h.forge, p.SHA).State == "success"
		})
	}
	h.settle(t, a, b)
	for _, p := range plans {
		if got := states(h.forge, p.SHA); got != "pending,success" {
			t.Errorf("%s: statuses = %s, want each phase reported once: pending,success", p.ID, got)
		}
	}
	for _, pr := range []int{7, 8} {
		if c := h.forge.Comments(pr); len(c) != 1 {
			t.Errorf("pull request %d holds %d comments, want one", pr, len(c))
		}
	}
	if n := len(h.reportEntries(t)); n != 2*len(plans) {
		t.Errorf("chain holds %d report entries, want %d, one per phase per plan", n, 2*len(plans))
	}
}

// TestReporterRetriesThroughAForgeOutage proves a forge that fails is retried with backoff until it
// recovers, that the failure is kept where the run shows it, that a report is recorded on the chain
// once however often its writes are retried, and that the report lands exactly once on recovery.
func TestReporterRetriesThroughAForgeOutage(t *testing.T) {
	t.Parallel()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	sha := strings.Repeat("8", 40)
	h.forge.SetOutage(http.StatusBadGateway)
	h.planRun(t, "run_down", sha, time.Now(), run.StatusSucceeded, planLog)
	h.reporter.Watch(h.trigger, 7, "run_down")

	waitFor(t, "three failed attempts", func() bool {
		return h.record(t, "run_down").Attempts >= 3
	})
	rec := h.record(t, "run_down")
	state := rec.State()
	if rec.Done || !strings.Contains(state.LastError, "502") || state.RetryAt == nil {
		t.Errorf("during the outage the run shows done %v, error %q, retry at %v, want a pending "+
			"retry naming the forge's answer", rec.Done, state.LastError, state.RetryAt)
	}
	if n := len(h.reportEntries(t)); n != 0 || len(h.forge.Statuses(sha)) != 0 {
		t.Errorf("a forge that answered nothing got %d report entries and %d statuses", n,
			len(h.forge.Statuses(sha)))
	}
	d1, d2, capped := h.reporter.backoff(1), h.reporter.backoff(4), h.reporter.backoff(60)
	if d2 <= d1 || capped != maxBackoff {
		t.Errorf("backoff(1) = %v, backoff(4) = %v, backoff(60) = %v, want growth capped at %v",
			d1, d2, h.reporter.backoff(60), maxBackoff)
	}

	// Reads work and writes fail, as a token that lost its write permission does. The report is
	// recorded once, however many times its writes are retried.
	h.forge.SetOutage(0)
	h.forge.SetWriteOutage(http.StatusForbidden)
	before := h.record(t, "run_down").Attempts
	waitFor(t, "two failed writes", func() bool {
		return h.record(t, "run_down").Attempts >= before+2
	})
	if n := len(h.reportEntries(t)); n != 1 {
		t.Errorf("chain holds %d report entries after retried writes, want 1", n)
	}

	h.forge.SetWriteOutage(0)
	waitFor(t, "the status after recovery", func() bool {
		return lastStatus(h.forge, sha).State != ""
	})
	h.settle(t, h.reporter)
	rec = h.record(t, "run_down")
	if !rec.Done || rec.Attempts != 0 || rec.LastError != "" || rec.StatusState != StateSuccess {
		t.Errorf("after recovery: done %v attempts %d error %q state %q, want the report landed "+
			"and the failure cleared", rec.Done, rec.Attempts, rec.LastError, rec.StatusState)
	}
	if got := states(h.forge, sha); got != StateSuccess {
		t.Errorf("statuses = %s, want the one final status", got)
	}
	if c := h.forge.Comments(7); len(c) != 1 || c[0].Edits != 0 {
		t.Errorf("comments = %+v, want one comment written once", c)
	}
	if n := len(h.reportEntries(t)); n != 1 {
		t.Errorf("chain holds %d report entries, want the one recorded before the retries", n)
	}
}

// TestReporterGivesUpOnAForgeThatNeverRecovers proves a forge that fails for longer than MaxWait is
// given up on, with the reason kept where the run shows it, and is not tried again.
func TestReporterGivesUpOnAForgeThatNeverRecovers(t *testing.T) {
	t.Parallel()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	rp, _ := h.newReporter(t, func(c *Config) { c.MaxWait = 60 * time.Millisecond })
	sha := strings.Repeat("9", 40)
	h.forge.SetOutage(http.StatusUnauthorized)
	h.planRun(t, "run_revoked", sha, time.Now(), run.StatusSucceeded, "")
	rp.Watch(h.trigger, 7, "run_revoked")
	waitFor(t, "the reporter to give up", func() bool { return h.record(t, "run_revoked").Done })
	h.settle(t, rp)
	rec := h.record(t, "run_revoked")
	if !strings.HasPrefix(rec.LastError, "gave up after") || rec.State().RetryAt != nil {
		t.Errorf("record error %q, retry %v, want a give-up and no retry", rec.LastError,
			rec.State().RetryAt)
	}
	seen := h.forge.Requests()
	time.Sleep(60 * time.Millisecond)
	if h.forge.Requests() != seen {
		t.Error("the reporter kept calling a forge it gave up on")
	}
}

// TestReporterKeepsTheCommentOnTheNewestPush proves pushes delivered out of order cannot put an
// older push's plan in the comment. The newer push's webhook arrives first and its plan reports,
// and the older push's webhook arrives late, so its plan is the later run. It never takes the
// comment over in any phase, and its own commit still gets its statuses.
func TestReporterKeepsTheCommentOnTheNewestPush(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, provider, true, nil)
			older, newer := strings.Repeat("a1", 20), strings.Repeat("b2", 20)
			h.forge.SetHead(7, newer)
			base := time.Now().Add(-time.Hour)

			h.planRun(t, "run_newer", newer, base, run.StatusSucceeded, planLog)
			h.reporter.Watch(h.trigger, 7, "run_newer")
			waitFor(t, "the newer push's status", func() bool {
				return lastStatus(h.forge, newer).State == "success"
			})

			h.saveRun(t, "run_older", older, base.Add(time.Minute), run.StatusRunning, "")
			h.reporter.Watch(h.trigger, 7, "run_older")
			waitFor(t, "the older push's running status", func() bool {
				return lastStatus(h.forge, older).State == "pending"
			})
			failure := map[string]string{trigger.ProviderGitHub: StateFailure,
				trigger.ProviderGitLab: "failed"}[provider]
			h.finish(t, "run_older", run.StatusFailed)
			waitFor(t, "the older push's final status", func() bool {
				return lastStatus(h.forge, older).State == failure
			})
			h.settle(t, h.reporter)

			comments := h.forge.Comments(7)
			if len(comments) != 1 || comments[0].Edits != 0 {
				t.Fatalf("comments = %+v, want the newer push's comment, never edited", comments)
			}
			body := comments[0].Body
			if !strings.Contains(body, "run_newer") || strings.Contains(body, "run_older") {
				t.Errorf("the comment does not describe the newest push:\n%s", body)
			}
			if got := states(h.forge, older); got != "pending,"+failure {
				t.Errorf("older push statuses = %s, want its own pending and %s", got, failure)
			}
		})
	}
}

// TestReporterRefusesAForkWithAStatusAlone proves a fork's refusal sets the commit status with the
// fixed wording and the docs link, writes no comment, reads no comment, records the report, and is
// made once however often the webhook is delivered.
func TestReporterRefusesAForkWithAStatusAlone(t *testing.T) {
	t.Parallel()
	for _, provider := range []string{trigger.ProviderGitHub, trigger.ProviderGitLab} {
		t.Run(provider, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, provider, true, nil)
			sha := strings.Repeat("c3", 20)
			ev := &Event{Number: 7, HeadSHA: sha, Fork: true}
			h.reporter.RefuseFork(h.trigger, ev, "5:abc")
			h.reporter.RefuseFork(h.trigger, ev, "5:abc")
			waitFor(t, "the fork status", func() bool {
				return lastStatus(h.forge, sha).State != ""
			})
			h.settle(t, h.reporter)

			list := h.forge.Statuses(sha)
			want := map[string]string{trigger.ProviderGitHub: StateError,
				trigger.ProviderGitLab: "failed"}[provider]
			if len(list) != 1 || list[0].State != want || list[0].Description != ForkStatus ||
				list[0].TargetURL != ForkDocsURL {
				t.Errorf("statuses = %+v, want one %s status reading %q linked to %s", list, want,
					ForkStatus, ForkDocsURL)
			}
			if strings.Contains(strings.ToLower(list[0].Description), "disabled") {
				t.Errorf("the fork status says disabled: %q", list[0].Description)
			}
			if c := h.forge.Comments(7); len(c) != 0 {
				t.Errorf("a fork's refusal wrote %d comments, want none", len(c))
			}
			if got := h.forge.Requests(); got != 1 {
				t.Errorf("a fork's refusal made %d forge requests, want the one status write", got)
			}
			if n := len(h.reportEntries(t)); n != 1 {
				t.Errorf("chain holds %d report entries, want the refusal recorded once", n)
			}
		})
	}
}

// TestCommentFor pins what a report does to the pull request's comment for every combination of
// head, existing comment, and plan time that decides it.
func TestCommentFor(t *testing.T) {
	t.Parallel()
	older, newer := strings.Repeat("a", 40), strings.Repeat("b", 40)
	at := time.Unix(1700000000, 0)
	comment := func(runID string, when time.Time, commit string) *Comment {
		return &Comment{ID: "1", Body: marker(Report{TemplateID: "tpl", RunID: runID, At: when,
			CommitSHA: commit}) + "\nbody"}
	}
	tests := []struct {
		// Report is the report being posted.
		Report Report
		// Head is the commit the pull request proposes now, empty when unknown.
		Head string
		// Existing is the comment the pull request holds, nil for none.
		Existing *Comment
		// WantAction is what the report does to it.
		WantAction commentAction
	}{{ // Test 0: No comment yet, so one is created.
		Report: Report{RunID: "run_a", At: at, CommitSHA: newer}, Head: newer,
		WantAction: commentCreate,
	}, { // Test 1: The comment is this plan's own, kept current after the pull request moved on.
		Report: Report{RunID: "run_a", At: at, CommitSHA: older}, Head: newer,
		Existing: comment("run_a", at, older), WantAction: commentUpdate,
	}, { // Test 2: A plan of a superseded push never takes over another plan's comment.
		Report: Report{RunID: "run_old", At: at.Add(time.Hour), CommitSHA: older}, Head: newer,
		Existing: comment("run_new", at, newer), WantAction: commentNone,
	}, { // Test 3: The head's plan replaces an older push's comment even as the earlier run.
		Report: Report{RunID: "run_new", At: at, CommitSHA: newer}, Head: newer,
		Existing: comment("run_old", at.Add(time.Hour), older), WantAction: commentUpdate,
	}, { // Test 4: The head's plan leaves a later plan of the same head alone.
		Report: Report{RunID: "run_1", At: at, CommitSHA: newer}, Head: newer,
		Existing: comment("run_2", at.Add(time.Minute), newer), WantAction: commentNone,
	}, { // Test 5: The head's plan replaces an earlier plan of the same head.
		Report: Report{RunID: "run_2", At: at.Add(time.Minute), CommitSHA: newer}, Head: newer,
		Existing: comment("run_1", at, newer), WantAction: commentUpdate,
	}, { // Test 6: With no head known, a later plan's comment is kept.
		Report:   Report{RunID: "run_1", At: at, CommitSHA: older},
		Existing: comment("run_2", at.Add(time.Minute), newer), WantAction: commentNone,
	}, { // Test 7: With no head known, an earlier plan's comment is replaced.
		Report:   Report{RunID: "run_2", At: at.Add(time.Minute), CommitSHA: newer},
		Existing: comment("run_1", at, older), WantAction: commentUpdate,
	}, { // Test 8: A comment with no readable marker is replaced by the head's plan.
		Report: Report{RunID: "run_1", At: at, CommitSHA: newer}, Head: newer,
		Existing:   &Comment{ID: "1", Body: "<!-- switchtender-review template=tpl garbled"},
		WantAction: commentUpdate,
	}, { // Test 9: A comment written before markers carried a commit gives way to the head's plan.
		Report: Report{RunID: "run_1", At: at, CommitSHA: newer}, Head: newer,
		Existing: &Comment{ID: "1",
			Body: "<!-- switchtender-review template=tpl run=run_0 at=1800000000000000000 -->"},
		WantAction: commentUpdate,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got := commentFor(test.Report, test.Head, test.Existing)
			if diff := cmp.Diff(test.WantAction, got); diff != "" {
				t.Errorf("commentFor() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// mustGet reads a run.
func mustGet(t *testing.T, runs run.Store, id string) *run.Run {
	t.Helper()
	r, err := runs.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("Get(%s) error = %v", id, err)
	}
	return r
}

// blockingClock is a run store whose clock holds every caller until the test lets it answer, so a
// test can stop the reporter's loop in the middle of a sweep.
type blockingClock struct {
	run.Store
	// held is closed by the test to let the clock answer.
	held chan struct{}
	// entered is closed the first time the clock is asked.
	entered chan struct{}
	// once closes entered once.
	once sync.Once
}

// Now waits until the clock is released, then answers with this process's clock.
func (b *blockingClock) Now(context.Context) (time.Time, error) {
	b.once.Do(func() { close(b.entered) })
	<-b.held
	return time.Now(), nil
}

// TestReporterWaitOutlastsTheLoopAfterShutdown proves that once Done has closed, Wait returns only
// after the loop has stopped, even with nothing left to report, so no sweep in progress outlives a
// shutdown and nothing the reporter started can run after the server or the test has finished.
func TestReporterWaitOutlastsTheLoopAfterShutdown(t *testing.T) {
	t.Parallel()
	h := newHarness(t, trigger.ProviderGitHub, true, nil)
	clock := &blockingClock{Store: h.runs, held: make(chan struct{}), entered: make(chan struct{})}
	done := make(chan struct{})
	rp := NewReporter(Config{
		Runs: clock, Templates: h.templates, Triggers: h.triggers, Store: h.store,
		Credentials: h.creds, Sealer: h.sealer, Interval: time.Hour, Done: done,
		Log: zaptest.NewLogger(t),
	})
	rp.start()
	<-clock.entered
	close(done)
	returned := make(chan struct{})
	go func() {
		rp.Wait()
		close(returned)
	}()
	select {
	case <-returned:
		t.Fatal("Wait returned while the loop was still inside a sweep")
	case <-time.After(100 * time.Millisecond):
	}
	close(clock.held)
	select {
	case <-returned:
	case <-time.After(10 * time.Second):
		t.Fatal("Wait never returned once the loop could finish its sweep")
	}
	select {
	case <-rp.loopDone:
	default:
		t.Error("Wait returned before the loop stopped")
	}
}
