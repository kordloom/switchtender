package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/schedule"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
)

// WithReviewReporting configures how pull request review triggers report back: the server's public
// address for links to the run, the HTTP client that reaches the forge, nil for the guarded
// default, and how often a plan is checked, zero for the default.
func WithReviewReporting(publicURL string, client *http.Client, interval time.Duration) Option {
	return func(srv *Server) {
		srv.reviewPublicURL = publicURL
		srv.reviewClient = client
		srv.reviewInterval = interval
	}
}

// WithReviewStore keeps pull request review report records in store, the database every replica
// shares, so each report is made once across them and a restart resumes where the last process
// stopped. Without it the records live in this process's memory.
func WithReviewStore(store review.Store) Option {
	return func(srv *Server) {
		srv.reviewStore = store
	}
}

// newReviewReporter builds the reporter review triggers post through, or nil when the server cannot
// run reviews: they need triggers, templates, credentials for the token, and an encryption key.
func (s *Server) newReviewReporter() *review.Reporter {
	if s.triggers == nil || s.templates == nil || s.credentials == nil || s.sealer == nil ||
		!s.sealer.Enabled() {
		return nil
	}
	redactor, _ := s.submitter.(review.Redactor)
	previewer, _ := s.submitter.(review.Previewer)
	var done <-chan struct{}
	if s.shutdown != nil {
		done = s.shutdown.Done()
	}
	if s.reviewStore == nil {
		s.reviewStore = review.NewMemStore()
	}
	return review.NewReporter(review.Config{
		Runs: s.store, Templates: s.templates, Triggers: s.triggers, Store: s.reviewStore,
		Previewer: previewer, Credentials: s.credentials, Sealer: s.sealer, Audits: s.audits,
		Redactor: redactor, HTTPClient: s.reviewClient, PublicURL: s.reviewPublicURL,
		Interval: s.reviewInterval, Done: done, Log: s.log,
	})
}

// ResumeReviews restarts reporting on every review plan still in flight, for a server that
// restarted while plans were running. It is a no-op on a server that cannot run reviews.
func (s *Server) ResumeReviews(ctx context.Context) {
	if s.reviews != nil {
		s.reviews.Resume(ctx, s.triggers)
	}
}

// reviewHookDeps is what the review branch of the webhook handler reads and writes.
type reviewHookDeps struct {
	// templates resolves the trigger's template.
	templates template.Store
	// triggers stamps the fire time.
	triggers trigger.Store
	// submitter launches the plan.
	submitter Submitter
	// store dedupes a redelivered webhook onto the plan it already launched.
	store run.Store
	// sealer opens the trigger's signing secret.
	sealer *credential.Sealer
	// audits records the webhook before anything launches.
	audits audit.Store
	// reviews reports to the pull request, nil when the server cannot.
	reviews *review.Reporter
	// hooks runs the work a delivery starts, answering the forge before it stops waiting.
	hooks *hookFlights
	// comments acts on /switchtender commands in pull request comments, nil when nothing does.
	comments *commentCommands
	// log records failures.
	log *zap.Logger
}

// serveReviewHook handles a webhook for a pull request review trigger: it verifies the delivery,
// reads the pull request event, refuses one from a fork unless the trigger allows forks, and
// launches the trigger's template as a plan of the pull request's head commit. The plan is a dry
// run pinned to that commit and fetched from the ref the base repository publishes it under, so it
// can change nothing and can run nothing but the code the pull request proposes. Its result is
// reported back to the pull request in the background.
//
// Applying is not reachable from here. The plan is a dry run, which the run carries in its spec and
// every executor honors, and nothing in this path or the reporter creates any other kind of run.
func serveReviewHook(w http.ResponseWriter, r *http.Request, tg *trigger.Trigger, d reviewHookDeps) {
	if d.reviews == nil {
		d.log.Error("server: review trigger fired on a server that cannot report reviews: " + tg.ID)
		respondError(w, d.log, http.StatusServiceUnavailable,
			"pull request review needs credentials and an encryption key on this server")
		return
	}
	body, ok := verifyReviewDelivery(w, r, tg, d.sealer, d.log)
	if !ok {
		return
	}
	provider := tg.Review.Provider
	ev, err := review.Parse(provider, r.Header.Get(review.EventHeader(provider)), body)
	if errors.Is(err, review.ErrNotReviewEvent) {
		serveReviewComment(w, r, tg, d, body)
		return
	}
	if err != nil {
		respondError(w, d.log, http.StatusBadRequest, err.Error())
		return
	}
	if !ev.SameRepository(tg.Review.Repository) {
		respondError(w, d.log, http.StatusUnprocessableEntity,
			"this webhook is for "+ev.Repository+" and the trigger reviews "+tg.Review.Repository)
		return
	}
	if !ev.Plannable {
		respondJSON(w, d.log, http.StatusAccepted,
			map[string]string{"trigger": tg.ID, "ignored": "action " + ev.Action + " proposes no new code"},
			wantsPretty(r))
		return
	}
	t, err := d.templates.Get(r.Context(), tg.TemplateID)
	if err != nil {
		respondError(w, d.log, http.StatusConflict, "trigger template is gone")
		return
	}
	if reason := reviewUnsupported(t); reason != "" {
		respondError(w, d.log, http.StatusConflict, reason)
		return
	}
	hookPath := "/hooks/" + tg.ID + "/review/" + strconv.Itoa(ev.Number)

	// A fork's head is code from somebody who cannot push to the repository, and a plan executes it
	// with the template's credentials. Refusing is recorded, because a pull request that was not
	// planned is a decision the trail should show, and the pull request is told why with a commit
	// status alone: a comment would let anybody who can open a pull request from a fork make the
	// operator's token write on the pull request as often as they like.
	if ev.Fork && !tg.Review.AllowForks {
		receipt, ok := recordReviewEntry(w, r, d.audits, d.log, tg, hookPath+"/refused")
		if !ok {
			return
		}
		d.reviews.RefuseFork(tg, ev, receipt)
		respondJSON(w, d.log, http.StatusAccepted,
			map[string]string{"trigger": tg.ID, "refused": "pull request from a fork"}, wantsPretty(r))
		return
	}
	launchReviewPlan(w, r, tg, d, ev, t, body)
}

// launchReviewPlan plans the head commit ev names for the pull request it names, the plan a push to
// the pull request launches and the one a /switchtender plan comment asks for: deduped on the
// verified body, held to the template's unattended survey, recorded before anything acts, and then
// pre-checked and launched apart from the request. extra adds to the plan's submit options, such as
// the account a comment acts as.
func launchReviewPlan(w http.ResponseWriter, r *http.Request, tg *trigger.Trigger, d reviewHookDeps,
	ev *review.Event, t *template.Template, body []byte, extra ...run.SubmitOption) {
	number := strconv.Itoa(ev.Number)
	hookPath := "/hooks/" + tg.ID + "/review/" + number

	// Deduped on the verified body, the same as a push trigger: a redelivery collapses onto the plan
	// the first delivery launched, and a new push is a new body with a new head.
	existing, key, err := resolveHookDedupe(r.Context(), d.store, tg.ID, hookDelivery(r, body),
		time.Now())
	if err != nil {
		d.log.Error("server: resolve review dedupe: " + err.Error())
		respondError(w, d.log, http.StatusInternalServerError, "could not plan the pull request")
		return
	}
	if existing != nil {
		if existing.AuditReceipt != "" {
			w.Header().Set(AuditReceiptHeader, existing.AuditReceipt)
		}
		d.reviews.Watch(tg, ev.Number, existing.ID)
		respondRun(w, r, d.log, http.StatusAccepted, existing)
		return
	}

	// Nobody answers the survey of a pull request's plan, so every question takes its default, and a
	// required one with no usable default refuses the plan: recorded, noted on the trigger, and told
	// to the pull request, rather than planned without an answer the template says it needs.
	survey, err := t.UnattendedOptions()
	if err != nil {
		receipt, ok := recordReviewEntry(w, r, d.audits, d.log, tg, unansweredPath(hookPath, err))
		if !ok {
			return
		}
		reason := t.RefuseUnattended("pull request plan", err).Error()
		noteTriggerError(r.Context(), d.triggers, tg.ID, reason, d.log)
		d.reviews.Refuse(tg, ev, "This plan was refused: "+strings.TrimPrefix(reason, "refused: "),
			receipt)
		respondJSON(w, d.log, http.StatusAccepted, map[string]string{"trigger": tg.ID,
			"refused": "a required survey question has no default"}, wantsPretty(r))
		return
	}
	// The delivery is recorded before anything acts on it. The pre-check below may download the
	// modules the configuration calls with the template's credentials, so the record comes first, and
	// a delivery that cannot be recorded is refused before anything is downloaded.
	accepted, ok := recordReviewEntry(w, r, d.audits, d.log, tg, hookPath+"/accepted")
	if !ok {
		return
	}
	// The pre-check and the plan can take longer than a forge waits for an answer, since the gate
	// may download the configuration's modules first. The work runs apart from this request: the
	// forge is answered when it finishes or when the bound runs out, whichever comes first, and the
	// pull request gets its status and comment either way. A failure after the forge was answered is
	// recorded on the chain, since nobody was told.
	ctx := context.WithoutCancel(r.Context())
	opts := append(append(reviewPlanOptions(t, tg, ev, key), survey...), extra...)
	answer, done := d.hooks.run(tg.ID+"\x00"+key, d.log,
		func() hookAnswer { return planReview(ctx, d, tg, ev, t, opts, hookPath) },
		func(a hookAnswer) { recordUnsent(ctx, d.audits, d.log, tg, hookPath+"/failed", a) })
	if !done {
		hookAnswer{status: http.StatusAccepted, receipt: accepted, body: map[string]string{
			"trigger": tg.ID, "pull_request": number,
			"accepted": "the plan is being prepared, and its status and comment will be posted to " +
				"the pull request",
		}}.write(w, r, d.log)
		return
	}
	answer.write(w, r, d.log)
}

// planReview runs a pull request review's pre-check and, when the plan is safe to run, launches
// it, and returns what the forge is told. It runs apart from the request that delivered the event,
// so it writes nothing to that request, and records its own entries on the chain.
func planReview(ctx context.Context, d reviewHookDeps, tg *trigger.Trigger, ev *review.Event,
	t *template.Template, opts []run.SubmitOption, hookPath string) hookAnswer {
	if a, refused := precheckRefusal(ctx, d, tg, ev, reviewProbe(t, opts), hookPath); refused {
		return a
	}
	receipt, err := appendHookEntry(ctx, d.audits, tg, hookPath+"/planned")
	if err != nil {
		d.log.Error("server: record review webhook: " + err.Error())
		return hookAnswer{status: http.StatusServiceUnavailable,
			message: "refused: the webhook could not be recorded in the audit trail"}
	}
	sctx := ctx
	if receipt != "" {
		sctx = run.WithAuditReceipt(ctx, receipt)
	}
	created, err := d.submitter.Submit(sctx, t.Playbook, t.Inventory, opts...)
	// A plan whose composed inventory matched no hosts is skipped, not failed, the way a schedule
	// or a webhook fire is: recorded after the entry for the request, answered with a success the
	// forge has no reason to redeliver, and told to the pull request with a status alone.
	if errors.Is(err, inventory.ErrNoHosts) {
		return reviewSkipAnswer(ctx, d, tg, ev, t, hookPath)
	}
	if errors.Is(err, dispatch.ErrPolicyDenied) {
		d.reviews.Refuse(tg, ev, "A rule refuses this plan: "+err.Error(), receipt)
		return hookAnswer{status: http.StatusForbidden, message: err.Error(), receipt: receipt}
	}
	if errors.Is(err, dispatch.ErrQueueUnlicensed) {
		return hookAnswer{status: http.StatusForbidden, message: err.Error(), receipt: receipt}
	}
	if errors.Is(err, credential.ErrNoSecret) || errors.Is(err, credential.ErrUnreadable) {
		d.log.Warn("server: plan pull request: " + err.Error())
		return hookAnswer{status: http.StatusConflict, message: err.Error(), receipt: receipt}
	}
	if err != nil {
		d.log.Error("server: plan pull request: " + err.Error())
		return hookAnswer{status: http.StatusBadGateway, message: "could not plan the pull request",
			receipt: receipt}
	}
	if err := d.triggers.TouchFired(ctx, tg.ID, time.Now()); err != nil {
		d.log.Error("server: stamp trigger: " + err.Error())
	}
	d.reviews.Watch(tg, ev.Number, created.ID)
	return hookAnswer{status: http.StatusAccepted, receipt: receipt, body: map[string]string{
		"trigger": tg.ID, "run": created.ID, "pull_request": strconv.Itoa(ev.Number)}}
}

// reviewPlanOptions builds the submit options for a pull request's plan: the template's own spec,
// forced into its no-change mode with Ansible's diff shown, pinned to the head commit and fetched
// from the ref the base repository publishes it under. The template's notification targets are
// dropped, since a plan of a proposal is not a change any channel was set up to hear about.
func reviewPlanOptions(t *template.Template, tg *trigger.Trigger, ev *review.Event, key string) []run.SubmitOption {
	opts := t.LaunchOptions()
	if t.OrgID != "" {
		opts = append(opts, run.WithOrgID(t.OrgID))
	}
	return append(opts,
		run.WithDryRun(true),
		run.WithDiffMode(true),
		run.WithGitRef(ev.Ref()),
		run.WithPinnedCommit(ev.HeadSHA),
		run.WithIdempotencyKey(key),
		run.WithSource(review.Source, tg.ID),
		run.WithActor("trigger "+tg.Name),
		run.WithActorType("webhook"),
		run.WithLabels(map[string]string{review.LabelPullRequest: strconv.Itoa(ev.Number)}),
		func(r *run.Run) { r.Notifications = nil },
	)
}

// reviewUnsupported returns why template t cannot be planned for a pull request, or empty when it
// can. A plan has to run from a project, since that is where the pull request's commit is fetched,
// and it has to be one run, since a workflow's steps are not planned as a unit.
func reviewUnsupported(t *template.Template) string {
	if t.ProjectID == "" {
		return "pull request review needs a template that runs from a project, so the plan can " +
			"fetch the pull request's commit"
	}
	if len(t.Steps) > 0 {
		return "pull request review plans a single template run, and this template is a workflow"
	}
	return ""
}

// recordReviewEntry appends the chain entry for a review webhook before anything acts on it, and
// fails closed exactly as a push trigger's fire does: a delivery that cannot be recorded is
// refused. It returns the entry's receipt, empty when no trail is kept.
func recordReviewEntry(w http.ResponseWriter, r *http.Request, audits audit.Store, log *zap.Logger,
	tg *trigger.Trigger, path string) (string, bool) {
	receipt, err := appendHookEntry(r.Context(), audits, tg, path)
	if err != nil {
		log.Error("server: record review webhook: " + err.Error())
		respondError(w, log, http.StatusServiceUnavailable,
			"refused: the webhook could not be recorded in the audit trail")
		return "", false
	}
	if receipt != "" {
		w.Header().Set(AuditReceiptHeader, receipt)
	}
	return receipt, true
}

// reviewSkipAnswer records a pull request plan skipped because its template's inventory matched no
// hosts, tells the pull request with a status alone, and returns what the forge is told: a success
// it has no reason to redeliver. A skip the chain will not take is refused instead, so a skip never
// exists without its evidence.
func reviewSkipAnswer(ctx context.Context, d reviewHookDeps, tg *trigger.Trigger, ev *review.Event,
	t *template.Template, hookPath string) hookAnswer {
	skip, err := appendReviewSkip(ctx, d.audits, tg, t, hookPath+"/skipped")
	if err != nil {
		d.log.Error("server: record review skip: " + err.Error())
		return hookAnswer{status: http.StatusServiceUnavailable,
			message: "refused: the skipped plan could not be recorded in the audit trail"}
	}
	d.reviews.SkipNoHosts(tg, ev, skip)
	d.log.Info("server: pull request plan skipped: "+schedule.SkipNoHosts,
		zap.String("trigger_id", tg.ID), zap.String("inventory_id", t.InventoryID))
	return hookAnswer{status: http.StatusOK, receipt: skip, body: map[string]string{
		"trigger": tg.ID, "skipped": schedule.SkipNoHosts, "pull_request": strconv.Itoa(ev.Number)}}
}

// appendReviewSkip appends the chain entry for a pull request plan skipped because its template's
// inventory matched no hosts, committing to the same record a webhook skip commits to. It returns
// the entry's receipt, empty when no trail is kept.
func appendReviewSkip(ctx context.Context, audits audit.Store, tg *trigger.Trigger,
	t *template.Template, path string) (string, error) {
	if audits == nil {
		return "", nil
	}
	body, err := json.Marshal(hookSkipRecord{
		TriggerID: tg.ID, TemplateID: t.ID, InventoryID: t.InventoryID, Reason: schedule.SkipNoHosts,
	})
	if err != nil {
		return "", fmt.Errorf("encode review skip: %w", err)
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return "", fmt.Errorf("digest review skip: %w", err)
	}
	entry := &audit.Entry{
		ID: audit.NewID(), Actor: "webhook:" + tg.ID, Method: http.MethodPost, Path: path,
		ContentDigest: digest, Nonce: nonce,
	}
	if err := audits.Append(ctx, entry); err != nil {
		return "", err
	}
	return audit.Receipt(entry), nil
}

// appendHookEntry appends a webhook's chain entry at path and returns its receipt, empty when no
// trail is kept. It writes nothing to any request, so work running apart from the request that
// delivered the event can record with it.
func appendHookEntry(ctx context.Context, audits audit.Store, tg *trigger.Trigger,
	path string) (string, error) {
	if audits == nil {
		return "", nil
	}
	entry := &audit.Entry{
		ID: audit.NewID(), Actor: "webhook:" + tg.ID, Method: http.MethodPost, Path: path,
	}
	if err := audits.Append(ctx, entry); err != nil {
		return "", err
	}
	return audit.Receipt(entry), nil
}

// recordUnsent records on the chain, at path, that a webhook's work failed after its sender was
// answered, and logs why, since the answer that would have carried it reached nobody.
func recordUnsent(ctx context.Context, audits audit.Store, log *zap.Logger, tg *trigger.Trigger,
	path string, a hookAnswer) {
	if !a.failed() {
		return
	}
	reason := a.message
	if reason == "" {
		reason = http.StatusText(a.status)
	}
	log.Error("server: webhook work failed after its sender was answered: "+reason,
		zap.String("trigger_id", tg.ID), zap.String("path", path))
	if _, err := appendHookEntry(ctx, audits, tg, path); err != nil {
		log.Error("server: record the failed webhook: " + err.Error())
	}
}

// verifyReviewDelivery reads the body and authenticates it the way the trigger's forge signs: an
// X-Hub-Signature-256 HMAC for GitHub, the X-Gitlab-Token secret for GitLab. A review trigger
// always requires this, whatever its require_signature flag says, because a forged pull request
// event would otherwise have the server fetch and run any ref it named. It writes the error
// response and returns false when the delivery does not verify.
func verifyReviewDelivery(w http.ResponseWriter, r *http.Request, tg *trigger.Trigger, sealer *credential.Sealer,
	log *zap.Logger) ([]byte, bool) {
	if tg.Review.Provider != trigger.ProviderGitLab {
		return verifyHookSignature(w, r, tg, sealer, log)
	}
	if sealer == nil || !sealer.Enabled() || tg.SigningSecret == "" {
		log.Error("server: review trigger has no signing secret available: " + tg.ID)
		respondError(w, log, http.StatusInternalServerError, "signature verification unavailable")
		return nil, false
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxHookBody))
	if err != nil {
		respondError(w, log, http.StatusRequestEntityTooLarge, "webhook body too large")
		return nil, false
	}
	secret, err := sealer.Open(tg.SigningSecret)
	if err != nil {
		log.Error("server: open signing secret: " + err.Error())
		respondError(w, log, http.StatusInternalServerError, "signature verification unavailable")
		return nil, false
	}
	if !trigger.VerifyToken(secret, r.Header.Get("X-Gitlab-Token")) {
		respondError(w, log, http.StatusUnauthorized, "invalid webhook token")
		return nil, false
	}
	return body, true
}
