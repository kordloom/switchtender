package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/decision"
	"github.com/kordloom/switchtender/internal/dispatch"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/grant"
	"github.com/kordloom/switchtender/internal/outcome"
	"github.com/kordloom/switchtender/internal/review"
	"github.com/kordloom/switchtender/internal/run"
	"github.com/kordloom/switchtender/internal/template"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// actorTypeComment is a person acting through a pull request comment written from the forge account
// linked to their SwitchTender account. It is how the decider authenticated, in the chain's
// vocabulary, and it is never an agent.
const actorTypeComment = "forge_comment"

// lastPlans bounds how many of a pull request's plans an apply looks through for the last one
// reported.
const lastPlans = 50

// Comment command rate limit, per trigger and per forge account.
const (
	// commentRateMax is how many commands one forge account may have acted on through one trigger
	// in commentRateWindow. Past it a command is answered 429 and leaves nothing behind.
	commentRateMax = 20
	// commentRateWindow is the window commentRateMax counts in.
	commentRateWindow = 10 * time.Minute
	// commentRateKeys bounds how many accounts the limiter tracks before it drops lapsed windows.
	commentRateKeys = 10000
)

// commentCommands acts on /switchtender commands written in pull request comments, as the
// SwitchTender account the commenter's forge account is linked to.
type commentCommands struct {
	// links resolves a forge account to the SwitchTender account it is linked to, nil when no
	// account can be linked.
	links forgelink.Store
	// users reads the linked account's role.
	users user.Store
	// approver records the decision an apply comment makes.
	approver Approver
	// authz holds the linked account to the grants it would face in the queue.
	authz *authorizer
	// publicURL is this server's public address, for the links a reply carries.
	publicURL string
	// limiter bounds how many commands one forge account may have acted on through one trigger.
	limiter *commentLimiter
	// attempts bounds how often a refusal's reply is tried for one author, pull request, and
	// reason, since trying needs the comment read back from the forge first.
	attempts *commentLimiter
}

// commentCommands returns what acts on pull request comment commands for this server.
func (s *Server) commentCommands(authz *authorizer) *commentCommands {
	return &commentCommands{links: s.forgeLinks, users: s.users, approver: s.approver, authz: authz,
		publicURL: strings.TrimRight(s.reviewPublicURL, "/"),
		limiter:   &commentLimiter{max: commentRateMax, windows: map[string]*commentWindow{}},
		attempts:  &commentLimiter{max: 1, windows: map[string]*commentWindow{}}}
}

// commentLimiter counts events per key in a fixed window and allows max of them. It is per
// process, so each replica bounds its own share.
type commentLimiter struct {
	// max is how many events a key may have in a window.
	max int
	// mu guards windows.
	mu sync.Mutex
	// windows maps a key to its current window.
	windows map[string]*commentWindow
}

// commentWindow is one account's current count.
type commentWindow struct {
	// start is when the window began.
	start time.Time
	// n is how many commands it counted.
	n int
}

// allow counts one command for key at now and reports whether it is within the limit.
func (l *commentLimiter) allow(key string, now time.Time) bool {
	if l == nil {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(l.windows) >= commentRateKeys {
		for k, w := range l.windows {
			if now.Sub(w.start) >= commentRateWindow {
				delete(l.windows, k)
			}
		}
	}
	w, ok := l.windows[key]
	if !ok || now.Sub(w.start) >= commentRateWindow {
		if !ok && len(l.windows) >= commentRateKeys {
			return false
		}
		w = &commentWindow{start: now}
		l.windows[key] = w
	}
	if w.n >= l.max {
		return false
	}
	w.n++
	return true
}

// commentCall is one comment command being acted on.
type commentCall struct {
	// tg is the review trigger the comment's webhook arrived through.
	tg *trigger.Trigger
	// ev is the comment.
	ev *review.CommentEvent
	// cmd is the command the comment carries.
	cmd review.Command
	// templateName is the name of the trigger's template, for replies.
	templateName string
	// d is the review hook's wiring.
	d reviewHookDeps
	// path is the chain path the command's entries are recorded under.
	path string
	// receipt is the receipt of the entry that recorded the command's arrival.
	receipt string
	// body is the verified webhook body, which a plan's dedupe key is derived from.
	body []byte
	// forge reads the forge with the trigger's token, opened at the first read.
	forge review.Commenter
	// release returns the token's lease once the command is done.
	release func()
	// confirmed is the comment as the forge holds it, set once it has been read back.
	confirmed *review.ForgeComment
}

// commentRecord is the content a comment command's chain entries commit: which comment, by which
// forge account, carrying which body, asking for what, and how it ended.
type commentRecord struct {
	// Forge is github or gitlab.
	Forge string `json:"forge"`
	// Repository is the repository the pull request belongs to.
	Repository string `json:"repository"`
	// PullRequest is the pull request or merge request number.
	PullRequest int `json:"pull_request"`
	// CommentID is the forge's numeric id of the comment.
	CommentID int64 `json:"comment_id"`
	// AuthorID is the forge's numeric id of the comment's author.
	AuthorID int64 `json:"author_id"`
	// BodySHA256 is the hex SHA-256 of the comment body as delivered.
	BodySHA256 string `json:"body_sha256"`
	// Command is plan or apply.
	Command string `json:"command"`
	// Result is how the command ended, empty on the entry recording its arrival.
	Result string `json:"result,omitempty"`
	// RunID is the run the command launched or decided, empty when it touched none.
	RunID string `json:"run_id,omitempty"`
}

// serveReviewComment handles a review trigger's webhook that is not a pull request event: a new
// comment on a pull request carrying a /switchtender command is acted on as the SwitchTender
// account its author's forge account is linked to, under the same rules as the queue. Anything else
// is answered as ignored. The delivery was verified before this is reached.
//
// A comment is acted on once. Its forge id keys a record every process sharing the store sees, so a
// redelivered webhook and a second process receiving the same delivery act on nothing. Each forge
// account's commands are rate limited per trigger before anything is recorded, so a flood of
// comments leaves nothing behind past the limit.
func serveReviewComment(w http.ResponseWriter, r *http.Request, tg *trigger.Trigger, d reviewHookDeps, body []byte) {
	provider := tg.Review.Provider
	ev, err := review.ParseComment(provider, r.Header.Get(review.EventHeader(provider)), body)
	if errors.Is(err, review.ErrNotCommentEvent) {
		respondJSON(w, d.log, http.StatusAccepted, map[string]string{"trigger": tg.ID,
			"ignored": "not a pull request event or a new pull request comment"}, wantsPretty(r))
		return
	}
	if err != nil {
		respondError(w, d.log, http.StatusBadRequest, err.Error())
		return
	}
	cmd, ok := ev.Parse()
	if !ok {
		respondJSON(w, d.log, http.StatusAccepted, map[string]string{"trigger": tg.ID,
			"ignored": "the comment carries no /switchtender command"}, wantsPretty(r))
		return
	}
	if !ev.SameRepository(tg.Review.Repository) {
		respondError(w, d.log, http.StatusUnprocessableEntity,
			"this webhook is for "+ev.Repository+" and the trigger reviews "+tg.Review.Repository)
		return
	}
	t, err := d.templates.Get(r.Context(), tg.TemplateID)
	if err != nil {
		respondError(w, d.log, http.StatusConflict, "trigger template is gone")
		return
	}
	// A command scoped to a template is acted on by that template's trigger alone, and every other
	// trigger watching the repository leaves it be.
	if cmd.Template != "" && !strings.EqualFold(cmd.Template, t.Name) && cmd.Template != t.ID {
		respondJSON(w, d.log, http.StatusAccepted, map[string]string{"trigger": tg.ID,
			"ignored": "the comment names another template"}, wantsPretty(r))
		return
	}
	if d.comments != nil && !d.comments.limiter.allow(tg.ID+"\x00"+strconv.FormatInt(ev.AuthorID, 10),
		time.Now()) {
		respondError(w, d.log, http.StatusTooManyRequests,
			"too many comment commands from this account, so this one was not acted on")
		return
	}
	// A comment already acted on is answered from the store alone. The record that says so is made
	// only once the forge has confirmed the comment, so a forged one leaves none.
	done, err := d.reviews.Commanded(r.Context(), tg, ev)
	if err != nil {
		d.log.Error("server: read a comment command: " + err.Error())
		respondError(w, d.log, http.StatusInternalServerError, "could not read the comment's record")
		return
	}
	if done {
		respondJSON(w, d.log, http.StatusAccepted, map[string]string{"trigger": tg.ID,
			"ignored": "this comment was already handled"}, wantsPretty(r))
		return
	}
	call := &commentCall{tg: tg, ev: ev, cmd: cmd, templateName: t.Name, d: d, body: body,
		path: "/hooks/" + tg.ID + "/review/" + strconv.Itoa(ev.Number) + "/comment/" +
			strconv.FormatInt(ev.CommentID, 10) + "/" + cmd.Verb}
	defer call.close()
	// The command is recorded before anything acts on it, and one that cannot be recorded acts on
	// nothing, the same as every webhook.
	receipt, err := call.record(r.Context(), call.path, "", "")
	if err != nil {
		d.log.Error("server: record a comment command: " + err.Error())
		respondError(w, d.log, http.StatusServiceUnavailable,
			"refused: the comment could not be recorded in the audit trail")
		return
	}
	call.receipt = receipt
	if receipt != "" {
		w.Header().Set(AuditReceiptHeader, receipt)
	}
	if cmd.Verb == review.CommandPlan {
		call.plan(w, r, t)
		return
	}
	call.respond(w, r, call.apply(r.Context()))
}

// client returns the forge client the command reads with, opening the trigger's token at the first
// call.
func (c *commentCall) client(ctx context.Context) (review.Commenter, error) {
	if c.forge != nil {
		return c.forge, nil
	}
	forge, release, err := c.d.reviews.Commenter(ctx, c.tg)
	c.release = release
	if err != nil {
		return nil, err
	}
	c.forge = forge
	return forge, nil
}

// close returns the token's lease, if the command opened it.
func (c *commentCall) close() {
	if c.release != nil {
		c.release()
	}
}

// commentAnswer is how a comment command ended: what the webhook is answered, the reply the pull
// request gets, and what the chain records.
type commentAnswer struct {
	// status is the HTTP status the webhook is answered with.
	status int
	// result names the ending for the chain and the answer, such as applied or refused/unlinked.
	result string
	// reply is the comment posted on the pull request, empty for none.
	reply string
	// runID is the run the command launched or decided, empty when none.
	runID string
	// err is an internal failure to log, nil for every ending a person caused.
	err error
	// quiet marks an ending the forge never confirmed the comment for, which may have no effect at
	// all: no result entry and no reply, only the answer to the webhook.
	quiet bool
	// ignored marks a comment another delivery already acted on.
	ignored bool
}

// respond records how the command ended, posts its reply, and answers the webhook. An ending the
// forge never confirmed the comment for records nothing and posts nothing.
func (c *commentCall) respond(w http.ResponseWriter, r *http.Request, a commentAnswer) {
	if a.ignored {
		respondJSON(w, c.d.log, http.StatusAccepted, map[string]string{"trigger": c.tg.ID,
			"ignored": "this comment was already handled"}, wantsPretty(r))
		return
	}
	if a.err != nil {
		c.d.log.Error("server: comment command: "+a.err.Error(), zap.String("trigger_id", c.tg.ID),
			zap.Int64("comment_id", c.ev.CommentID))
	}
	if a.quiet {
		out := map[string]string{"trigger": c.tg.ID, "pull_request": strconv.Itoa(c.ev.Number),
			"comment": strconv.FormatInt(c.ev.CommentID, 10), "result": a.result}
		respondJSON(w, c.d.log, a.status, out, wantsPretty(r))
		return
	}
	if _, err := c.record(r.Context(), c.path+"/"+a.result, a.result, a.runID); err != nil {
		c.d.log.Error("server: record a comment command's result: " + err.Error())
	}
	// A refusal is something its author can ask for again and again, so its reply goes out once per
	// author, pull request, and reason. Every other ending answers the one comment that caused it.
	if why, refusal := strings.CutPrefix(a.result, "refused/"); refusal {
		c.d.reviews.ReplyRefusal(c.tg, c.ev, why, a.reply)
	} else {
		c.d.reviews.Reply(c.tg, c.ev, a.reply)
	}
	out := map[string]string{"trigger": c.tg.ID, "pull_request": strconv.Itoa(c.ev.Number),
		"comment": strconv.FormatInt(c.ev.CommentID, 10), "result": a.result}
	if a.runID != "" {
		out["run"] = a.runID
	}
	respondJSON(w, c.d.log, a.status, out, wantsPretty(r))
}

// record appends a chain entry at path committing to the comment, the command, and its result.
func (c *commentCall) record(ctx context.Context, path, result, runID string) (string, error) {
	if c.d.audits == nil {
		return "", nil
	}
	body, err := json.Marshal(commentRecord{
		Forge: c.ev.Provider, Repository: c.ev.Repository, PullRequest: c.ev.Number,
		CommentID: c.ev.CommentID, AuthorID: c.ev.AuthorID, BodySHA256: c.ev.BodySHA256(),
		Command: c.ev.Command(), Result: result, RunID: runID,
	})
	if err != nil {
		return "", fmt.Errorf("encode comment command: %w", err)
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return "", fmt.Errorf("digest comment command: %w", err)
	}
	entry := &audit.Entry{
		ID: audit.NewID(), Actor: "webhook:" + c.tg.ID, Method: http.MethodPost, Path: path,
		ContentDigest: digest, Nonce: nonce,
	}
	if err := c.d.audits.Append(ctx, entry); err != nil {
		return "", err
	}
	return audit.Receipt(entry), nil
}

// forgeName is how a reply names the forge.
func (c *commentCall) forgeName() string {
	if c.ev.Provider == trigger.ProviderGitLab {
		return "GitLab"
	}
	return "GitHub"
}

// link returns path on this server's public address, or empty when it has none.
func (c *commentCall) link(path string) string {
	if c.d.comments == nil || c.d.comments.publicURL == "" {
		return ""
	}
	return c.d.comments.publicURL + path
}

// failedReply is what a pull request is told when a command failed on the server's side.
const failedReply = "SwitchTender could not act on this comment because of an error on its side, " +
	"so this command did nothing. Comment again to retry."

// failed is a failure on the server's side, logged with err and answered with status. The pull
// request is told, since the comment did nothing and nothing else will say so.
func failed(why string, status int, err error) commentAnswer {
	return commentAnswer{status: status, result: "failed/" + why, reply: failedReply, err: err}
}

// refused is a refusal a person caused, answered with 202 so the forge does not redeliver it.
func refused(why, reply string) commentAnswer {
	return commentAnswer{status: http.StatusAccepted, result: "refused/" + why, reply: reply}
}

// actor resolves the SwitchTender account the comment would act as, from this server alone, and
// returns it with the request context carrying it, so the grants the queue applies apply here. An
// unlinked author is refused, and told how to link only once the forge confirms the comment.
func (c *commentCall) actor(ctx context.Context) (context.Context, *user.User, *commentAnswer) {
	u, answer := c.linkedUser(ctx)
	if answer != nil {
		a := c.refuseLocal(ctx, *answer)
		return nil, nil, &a
	}
	actor := Actor{UserID: u.ID, Role: u.Role, Name: u.Username, Type: actorTypeComment}
	return context.WithValue(ctx, actorKey{}, actor), u, nil
}

// act confirms the comment with the forge and claims it, the gate every effect a comment has
// passes through. A comment the forge does not confirm gets a quiet answer, with no effect, and one
// another delivery already claimed is answered as ignored.
func (c *commentCall) act(ctx context.Context) *commentAnswer {
	if answer := c.confirm(ctx); answer != nil {
		a := *answer
		a.quiet, a.reply = true, ""
		return &a
	}
	fresh, err := c.d.reviews.MarkCommand(ctx, c.tg, c.ev)
	if err != nil {
		a := failed("record", http.StatusInternalServerError, err)
		return &a
	}
	if !fresh {
		return &commentAnswer{status: http.StatusAccepted, ignored: true}
	}
	return nil
}

// refuseLocal settles a refusal this server found on its own, before reading anything from the
// forge. The answer names it either way, and its reply goes out only once the forge confirms the
// comment and the pull request is not a fork's, and once per author, pull request, and reason. A
// repeat, or an attempt within the window, costs the forge nothing and has no effect.
func (c *commentCall) refuseLocal(ctx context.Context, a commentAnswer) commentAnswer {
	why, refusal := strings.CutPrefix(a.result, "refused/")
	if !refusal || a.reply == "" {
		a.quiet, a.reply = true, ""
		return a
	}
	replied, err := c.d.reviews.RefusalReplied(ctx, c.tg, c.ev, why, a.reply)
	key := c.tg.ID + "\x00" + strconv.Itoa(c.ev.Number) + "\x00" +
		strconv.FormatInt(c.ev.AuthorID, 10) + "\x00" + why
	if err != nil || replied || c.d.comments == nil ||
		!c.d.comments.attempts.allow(key, time.Now()) {
		a.quiet, a.reply = true, ""
		return a
	}
	if answer := c.act(ctx); answer != nil {
		if answer.ignored {
			return *answer
		}
		a.quiet, a.reply = true, ""
		return a
	}
	if _, answer := c.pullRequest(ctx); answer != nil {
		return *answer
	}
	return a
}

// checkPullRequest reads the pull request a confirmed comment is on and refuses a fork's, a closed
// one, a comment GitHub marks as an app's, and a bot's.
func (c *commentCall) checkPullRequest(ctx context.Context) (*review.PullRequest, *commentAnswer) {
	pr, answer := c.pullRequest(ctx)
	if answer != nil {
		return nil, answer
	}
	if !pr.Open {
		a := refused("closed", "This pull request is not open, so this command did nothing.")
		return nil, &a
	}
	if c.ev.ViaApp || c.confirmed.ViaApp {
		a := refused("agent", agentReply)
		return nil, &a
	}
	bot := c.ev.AuthorBot || c.confirmed.AuthorBot
	if !bot && c.ev.Provider == trigger.ProviderGitLab {
		var err error
		if bot, err = c.forge.IsBot(ctx, c.ev.AuthorID); err != nil {
			a := failed("forge", http.StatusBadGateway, err)
			return nil, &a
		}
	}
	if bot {
		a := refused("bot", botReply)
		return nil, &a
	}
	return pr, nil
}

// Replies to refusals that do not depend on the pull request.
const (
	// agentReply answers a comment an app wrote on a person's behalf.
	agentReply = "This comment was written by an app on its author's behalf, and an agent may " +
		"propose changes but never approve them, so this command did nothing. Comment yourself, " +
		"or approve in SwitchTender."
	// botReply answers a bot's comment.
	botReply = "Comments from bot accounts cannot plan or apply, so this command did nothing. A " +
		"person approves from their own linked account or in SwitchTender."
)

// pullRequest reads the pull request the comment is on, refusing a fork's with no reply: a reply
// would let anybody able to open a pull request from a fork make the operator's token write on it.
// Until it is read the pull request may be a fork's, so a failure to read it is not answered on it.
func (c *commentCall) pullRequest(ctx context.Context) (*review.PullRequest, *commentAnswer) {
	forge, err := c.client(ctx)
	if err != nil {
		a := commentAnswer{status: http.StatusBadGateway, result: "failed/forge", err: err}
		return nil, &a
	}
	pr, err := forge.PullRequest(ctx, c.ev.Number)
	if err != nil {
		a := commentAnswer{status: http.StatusBadGateway, result: "failed/forge", err: err}
		return nil, &a
	}
	if pr == nil {
		a := refused("no_pull_request", "")
		return nil, &a
	}
	if pr.Fork {
		a := refused("fork", "")
		return nil, &a
	}
	return pr, nil
}

// linkedUser returns the SwitchTender account the comment's author is linked to, or the refusal an
// unlinked author gets: one reply saying how to link.
func (c *commentCall) linkedUser(ctx context.Context) (*user.User, *commentAnswer) {
	unlinked := refused("unlinked", "Your "+c.forgeName()+" account is not linked to a SwitchTender "+
		"account, so this command did nothing. "+c.linkHint())
	u, err := c.linked(ctx, c.ev.AuthorID)
	if err != nil {
		a := failed("link", http.StatusInternalServerError, err)
		return nil, &a
	}
	if u == nil {
		return nil, &unlinked
	}
	return u, nil
}

// linked returns the SwitchTender account the forge account forgeUserID is linked to, or nil when
// it is linked to none that exists.
func (c *commentCall) linked(ctx context.Context, forgeUserID int64) (*user.User, error) {
	cc := c.d.comments
	if cc == nil || cc.links == nil || cc.users == nil || forgeUserID <= 0 {
		return nil, nil
	}
	l, err := cc.links.Lookup(ctx, c.ev.Provider,
		forgelink.CanonicalAPIURL(c.ev.Provider, c.tg.Review.BaseURL()), forgeUserID)
	if errors.Is(err, forgelink.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	u, err := cc.users.Get(ctx, l.UserID)
	if errors.Is(err, user.ErrNotFound) {
		return nil, nil
	}
	return u, err
}

// confirm reads the comment back from the forge by id and holds it to the webhook: the forge must
// hold it on this pull request, written by the same account, with the same body, by a person rather
// than an app or a bot, and recently. A signed webhook is not enough by itself, because whoever
// holds the trigger's signing secret could sign a comment the forge never held and name another
// approver's id. A comment the forge does not confirm acts on nothing and is not answered.
func (c *commentCall) confirm(ctx context.Context) *commentAnswer {
	forge, err := c.client(ctx)
	if err != nil {
		a := failed("forge", http.StatusBadGateway, err)
		return &a
	}
	fc, err := forge.Comment(ctx, c.ev.Number, c.ev.CommentID)
	if err != nil {
		a := failed("forge", http.StatusBadGateway, err)
		return &a
	}
	if fc == nil || fc.AuthorID != c.ev.AuthorID || fc.Body != c.ev.Body ||
		fc.PullRequest != c.ev.Number {
		a := refused("unconfirmed", "")
		return &a
	}
	if !fc.CreatedAt.IsZero() && time.Since(fc.CreatedAt) > review.CommentMaxAge {
		a := refused("stale", "")
		return &a
	}
	c.confirmed = fc
	return nil
}

// linkHint says where a person links their forge account.
func (c *commentCall) linkHint() string {
	if l := c.link("/ui/links"); l != "" {
		return "Link it at " + l + ", then comment again."
	}
	return "Link it under Linked accounts in SwitchTender, then comment again."
}

// plan launches a plan of the pull request's current head as the linked account, the same plan a
// push to the pull request launches, and reports it to the pull request the same way. The account
// is held to every grant a launch of the template in the queue needs, and the comment is confirmed
// with the forge before anything runs.
func (c *commentCall) plan(w http.ResponseWriter, r *http.Request, t *template.Template) {
	ctx, u, answer := c.actor(r.Context())
	if answer != nil {
		c.respond(w, r, *answer)
		return
	}
	if !roleAllows(u.Role, user.RoleOperator) {
		c.respond(w, r, c.refuseLocal(ctx, refused("role", "Your SwitchTender account cannot "+
			"launch work, so this command did nothing.")))
		return
	}
	objects := append([]string{c.tg.TemplateID}, templateLaunchObjects(t, t.InventoryID,
		t.CredentialIDs)...)
	if err := c.d.comments.authz.authorizeAll(ctx, grant.AccessUse, objects...); err != nil {
		c.respond(w, r, c.refuseLocal(ctx, grantRefusal(err, "Your SwitchTender account may not "+
			"use this template and everything it runs with, so this command did nothing.")))
		return
	}
	if reason := reviewUnsupported(t); reason != "" {
		c.respond(w, r, c.refuseLocal(ctx, refused("unsupported", "This template cannot be "+
			"planned for a pull request: "+reason+".")))
		return
	}
	if answer := c.act(ctx); answer != nil {
		c.respond(w, r, *answer)
		return
	}
	pr, answer := c.checkPullRequest(ctx)
	if answer != nil {
		c.respond(w, r, *answer)
		return
	}
	ev := &review.Event{
		Provider: c.ev.Provider, Action: "comment", Plannable: true, Repository: c.ev.Repository,
		Number: c.ev.Number, HeadSHA: pr.HeadSHA, HeadBranch: pr.HeadBranch, URL: pr.URL,
	}
	// The plan path records its own entries from here, a refusal included, so this one says only
	// that the command was accepted and handed to it.
	if _, err := c.record(r.Context(), c.path+"/accepted", "accepted", ""); err != nil {
		c.d.log.Error("server: record a comment command's result: " + err.Error())
	}
	launchReviewPlan(w, r.WithContext(ctx), c.tg, c.d, ev, t, c.body,
		run.WithActor(u.Username), run.WithActorAccount(u.ID), run.WithAccount(u.Username),
		run.WithActorType(actorTypeComment))
}

// grantRefusal answers a grant check that failed: a refusal for a denied grant, a failure for
// anything else.
func grantRefusal(err error, reply string) commentAnswer {
	if authzRefused(err) {
		return refused("grant", reply)
	}
	return failed("authorize", http.StatusInternalServerError, err)
}

// reportedPlan is one plan this trigger reported to the pull request, with its report record.
type reportedPlan struct {
	// run is the plan run.
	run *run.Run
	// rec is its report record.
	rec *review.Record
}

// apply approves and applies a plan SwitchTender reported for the pull request's current head, as
// the linked account, under the approval rights, separation of duties, and refusals of the queue.
// "/switchtender apply PLAN" names the plan a report showed and applies exactly that one while it
// is still the current plan of the head. A bare "/switchtender apply" is taken only when there is
// no doubt which plan its author read: the head has one reported plan, and it was reported before
// the comment was written. Otherwise the reply names the plan id to use.
//
// The apply is proposed from the plan the way a reconcile is proposed from a drift check: the
// plan's own spec run for real, at the plan's commit, carrying the plan's saved plan file for
// Terraform and OpenTofu, held for approval. Its requester is the pull request's author, read from
// the forge, when their forge account is linked, so separation of duties is judged against the
// person who wrote the change whoever comments first. The comment is confirmed with the forge
// before it decides anything. One apply is ever proposed from a plan, so a repeated comment finds
// it and applies nothing twice.
func (c *commentCall) apply(ctx context.Context) commentAnswer {
	ctx, u, answer := c.actor(ctx)
	if answer != nil {
		return *answer
	}
	if !roleAllows(u.Role, user.RoleOperator) {
		return c.refuseLocal(ctx, refused("role", "Your SwitchTender account cannot launch work, "+
			"so nothing was applied."))
	}
	// Nothing below has an effect until the forge confirms the comment.
	if answer := c.act(ctx); answer != nil {
		return *answer
	}
	pr, answer := c.checkPullRequest(ctx)
	if answer != nil {
		return *answer
	}
	plans, err := c.reportedPlans(ctx)
	if err != nil {
		return failed("plan", http.StatusInternalServerError, err)
	}
	pick, answer := c.choosePlan(plans, pr)
	if answer != nil {
		return *answer
	}
	plan := pick.run
	if err := reexecuteAuthorization(ctx, c.d.comments.authz, plan); err != nil {
		return grantRefusal(err, "Your SwitchTender account may not use what this plan runs or "+
			"the queue it runs on, so nothing was applied.")
	}
	requester := u
	if author, err := c.linked(ctx, pr.AuthorID); err != nil {
		return failed("link", http.StatusInternalServerError, err)
	} else if author != nil {
		requester = author
	}
	proposal, answer := c.propose(ctx, requester, plan)
	if answer != nil {
		return *answer
	}
	// A bare apply names no plan, so it may only mean a plan its author could have read: one
	// reported before the comment was written.
	if c.cmd.Plan == "" && c.confirmed.CreatedAt.Before(pick.rec.ReportedAt) {
		a := refused("name_the_plan", fmt.Sprintf("Plan %s of commit %s was reported after this "+
			"comment was written, so nothing was applied. Read it, then comment "+
			"`/switchtender apply %s` to apply exactly that plan.", review.PlanID(plan.ID),
			short(plan.PinnedCommit), review.PlanID(plan.ID)))
		a.runID = proposal.ID
		return a
	}
	return c.decide(ctx, u, plan, proposal)
}

// reportedPlans returns the plans this trigger reported to the pull request, newest first.
func (c *commentCall) reportedPlans(ctx context.Context) ([]reportedPlan, error) {
	plans, err := c.d.store.ListPage(ctx, run.ListFilter{
		Source: review.Source, SourceID: c.tg.ID, LabelKey: run.LabelPullRequest,
		LabelValue: strconv.Itoa(c.ev.Number),
	}, lastPlans, 0)
	if err != nil {
		return nil, fmt.Errorf("list the pull request's plans: %w", err)
	}
	var out []reportedPlan
	for _, p := range plans {
		rec, err := c.d.reviews.Record(ctx, p.ID)
		if errors.Is(err, review.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read a plan's report: %w", err)
		}
		if rec.Kind != review.KindPlan || rec.ReportedAt.IsZero() || rec.Phase == "" ||
			(rec.Repository != "" && !c.ev.SameRepository(rec.Repository)) {
			continue
		}
		out = append(out, reportedPlan{run: p, rec: rec})
	}
	return out, nil
}

// choosePlan picks the plan the command applies from the reported plans, newest first, or the
// refusal saying why none can be: none reported, an unknown or superseded plan id, a head that
// moved, a plan that did not succeed, or a bare apply with more than one plan of the head to choose
// from.
func (c *commentCall) choosePlan(plans []reportedPlan, pr *review.PullRequest) (*reportedPlan, *commentAnswer) {
	if len(plans) == 0 {
		a := refused("no_plan", "SwitchTender has reported no plan for this pull request, so "+
			"nothing was applied. Comment `/switchtender plan` to plan its current head.")
		return nil, &a
	}
	var head []reportedPlan
	for _, p := range plans {
		if strings.EqualFold(p.run.PinnedCommit, pr.HeadSHA) {
			head = append(head, p)
		}
	}
	pick := &plans[0]
	if c.cmd.Plan != "" {
		pick = nil
		for i := range plans {
			if review.MatchesPlan(c.cmd.Plan, plans[i].run.ID) {
				pick = &plans[i]
				break
			}
		}
		if pick == nil {
			a := refused("unknown_plan", "SwitchTender reported no plan "+c.cmd.Plan+" for this "+
				"pull request, so nothing was applied.")
			return nil, &a
		}
	}
	if !strings.EqualFold(pick.run.PinnedCommit, pr.HeadSHA) {
		a := refused("head_moved", fmt.Sprintf("Plan %s is of commit %s, and this pull request is "+
			"now at %s, so nothing was applied. Wait for the plan of the new head, or comment "+
			"`/switchtender plan`.", review.PlanID(pick.run.ID), short(pick.run.PinnedCommit),
			short(pr.HeadSHA)))
		return nil, &a
	}
	if head[0].run.ID != pick.run.ID {
		a := refused("superseded", fmt.Sprintf("Plan %s was replaced by plan %s of the same "+
			"commit, so nothing was applied. Read it, then comment `/switchtender apply %s`.",
			review.PlanID(pick.run.ID), review.PlanID(head[0].run.ID), review.PlanID(head[0].run.ID)))
		return nil, &a
	}
	if pick.rec.Phase != review.PhaseSucceeded || pick.run.Status != run.StatusSucceeded {
		a := refused("plan_not_succeeded", fmt.Sprintf("Plan %s of this head is %s, so there is "+
			"nothing to apply.", review.PlanID(pick.run.ID), pick.rec.Phase))
		return nil, &a
	}
	if c.cmd.Plan == "" && len(head) > 1 {
		a := refused("name_the_plan", fmt.Sprintf("SwitchTender reported %d plans of commit %s, so "+
			"a bare apply could mean more than one, and nothing was applied. Comment "+
			"`/switchtender apply %s` to apply the latest.", len(head), short(pr.HeadSHA),
			review.PlanID(pick.run.ID)))
		return nil, &a
	}
	return pick, nil
}

// applyKeyFor is the idempotency key of the apply a comment proposes from plan planID, one per plan
// whichever comment or process asks. It carries the server's reserved prefix, which no caller may
// supply.
func applyKeyFor(planID string) string {
	return "st:prapply:" + planID
}

// propose returns the apply proposed from plan, held for approval, creating it when no comment has
// yet, with requester as the account that asked for it.
func (c *commentCall) propose(ctx context.Context, requester *user.User, plan *run.Run) (*run.Run, *commentAnswer) {
	key := applyKeyFor(plan.ID)
	if existing, err := c.d.store.ByIdempotencyKey(ctx, key); err == nil {
		// The key is reserved, so nothing but this path should hold it, and the run it names is
		// reused only when it is the apply this path proposes from this plan, for this pull request
		// and trigger, at the plan's commit.
		if !c.proposedFrom(existing, plan) {
			a := refused("proposal_mismatch", fmt.Sprintf("Run %s holds the key of the apply of "+
				"plan %s and was not proposed from it for this pull request, so nothing was "+
				"applied. An administrator should look at that run.", existing.ID,
				review.PlanID(plan.ID)))
			a.err = fmt.Errorf("run %s holds apply key %s and was not proposed from plan %s",
				existing.ID, key, plan.ID)
			return nil, &a
		}
		return existing, nil
	} else if !errors.Is(err, run.ErrNotFound) {
		a := failed("propose", http.StatusInternalServerError, err)
		return nil, &a
	}
	opts := append(plan.ExecutionOptions(),
		run.WithDryRun(false),
		run.WithRequireApproval(true),
		run.WithProposedFrom(plan.ID),
		run.WithSource(review.ApplySource, plan.ID),
		run.WithActor(requester.Username), run.WithActorAccount(requester.ID),
		run.WithAccount(requester.Username),
		run.WithActorType(actorTypeComment),
		run.WithIdempotencyKey(key),
		run.WithLabels(map[string]string{review.LabelPullRequest: strconv.Itoa(c.ev.Number)}),
	)
	if plan.OrgID != "" {
		opts = append(opts, run.WithOrgID(plan.OrgID))
	}
	// A Terraform or OpenTofu apply carries out the plan file the plan saved, the plan the pull
	// request was shown, with its digest bound into the approval. Planning again when it runs would
	// apply something nobody saw, so a plan that kept none is refused.
	tool := run.NormalizeTool(plan.Tool)
	if tool == run.ToolTerraform || tool == run.ToolOpenTofu {
		sealed, err := c.d.store.DriftPlan(ctx, plan.ID)
		if err != nil {
			a := failed("plan_file", http.StatusInternalServerError, err)
			return nil, &a
		}
		if sealed == "" {
			a := refused("no_plan_file", fmt.Sprintf("Plan %s kept no saved plan to apply: it "+
				"found no changes, or a newer plan of the same configuration replaced it. Comment "+
				"`/switchtender plan` to plan again.", review.PlanID(plan.ID)))
			return nil, &a
		}
		opts = append(opts, run.WithPlanFile(sealed))
	}
	sctx := ctx
	if c.receipt != "" {
		sctx = run.WithAuditReceipt(ctx, c.receipt)
	}
	proposal, err := c.d.submitter.Submit(sctx, plan.Playbook, plan.Inventory, opts...)
	switch {
	case errors.Is(err, dispatch.ErrPolicyDenied), errors.Is(err, dispatch.ErrQueueUnlicensed):
		a := refused("policy", "A rule refuses this apply, so nothing was applied. Its run page "+
			"in SwitchTender names the rule.")
		a.err = err
		return nil, &a
	case err != nil:
		a := failed("propose", http.StatusBadGateway, err)
		return nil, &a
	}
	return proposal, nil
}

// proposedFrom reports whether existing is the apply this path proposes from plan for this pull
// request and trigger: proposed from the plan, with the comment apply's source naming it, labeled
// with this pull request, at the plan's commit, from a plan this trigger made.
func (c *commentCall) proposedFrom(existing, plan *run.Run) bool {
	return existing.ProposedFrom == plan.ID && existing.Source == review.ApplySource &&
		existing.SourceID == plan.ID && plan.SourceID == c.tg.ID &&
		existing.Labels[review.LabelPullRequest] == strconv.Itoa(c.ev.Number) &&
		strings.EqualFold(existing.PinnedCommit, plan.PinnedCommit)
}

// decide approves proposal as u from this comment, recording the comment and the plan it approved
// on the decision.
func (c *commentCall) decide(ctx context.Context, u *user.User, plan, proposal *run.Run) commentAnswer {
	planID := review.PlanID(plan.ID)
	if proposal.Status != run.StatusPendingApproval {
		a := refused("already_applied", fmt.Sprintf("Plan %s was already decided: its apply, run "+
			"%s, is %s. Nothing more was done.", planID, proposal.ID, proposal.Status))
		a.runID = proposal.ID
		return a
	}
	held := fmt.Sprintf("The apply of plan %s is run %s, held for approval.", planID, proposal.ID)
	if l := c.link("/ui/runs/" + proposal.ID); l != "" {
		held = fmt.Sprintf("The apply of plan %s is run %s, held for approval at %s.", planID,
			proposal.ID, l)
	}
	if !roleAllows(u.Role, user.RoleAdmin) {
		a := refused("role", held+" Your SwitchTender account cannot approve, so an approver "+
			"releases it in SwitchTender or from their own linked account.")
		a.runID = proposal.ID
		return a
	}
	if err := c.d.comments.authz.authorizeRun(ctx, grant.AccessUse, proposal); err != nil {
		a := grantRefusal(err, held+" Your SwitchTender account may not approve it.")
		a.runID = proposal.ID
		return a
	}
	reasoned, ok := c.d.comments.approver.(ReasonedApprover)
	if !ok {
		a := refused("unrecordable", held+" This server cannot record a decision made from a "+
			"comment, so approve it in SwitchTender.")
		a.runID = proposal.ID
		return a
	}
	fc := c.confirmed
	sum := sha256.Sum256([]byte(fc.Body))
	_, err := reasoned.DecideRun(ctx, proposal.ID, dispatch.RunDecision{
		Approve: true,
		By:      outcome.Decider{Name: u.Username, Type: actorTypeComment, AccountID: u.ID},
		Comment: &decision.Comment{
			Forge: c.ev.Provider, APIURL: c.tg.Review.BaseURL(), Repository: c.ev.Repository,
			PullRequest: fc.PullRequest, CommentID: fc.ID, AuthorID: fc.AuthorID,
			BodySHA256: hex.EncodeToString(sum[:]), PlanRunID: plan.ID,
		},
	})
	var answer commentAnswer
	switch {
	case err == nil:
		answer = commentAnswer{status: http.StatusOK, result: "applied", reply: fmt.Sprintf(
			"Approved from this comment. Template %s: plan %s of commit %s, applied as run %s.",
			c.templateName, planID, short(plan.PinnedCommit), proposal.ID)}
	case errors.Is(err, dispatch.ErrSelfApproval):
		answer = refused("separation_of_duties", held+" The rule holding it requires a different "+
			"person to approve it than the pull request's author, or the one who asked for it, so "+
			"another approver comments `/switchtender apply "+planID+"` or approves it in "+
			"SwitchTender.")
	case errors.Is(err, dispatch.ErrNotPendingApproval):
		answer = refused("already_applied", fmt.Sprintf("Run %s was decided before this comment "+
			"reached it. Nothing more was done.", proposal.ID))
	case errors.Is(err, dispatch.ErrReasonRequired):
		answer = refused("reason_required", held+" A rule requires a reason to approve it, and a "+
			"comment carries none, so approve it in SwitchTender.")
	case errors.Is(err, dispatch.ErrAgentApproval):
		answer = refused("agent", held+" "+err.Error())
	default:
		answer = commentAnswer{status: http.StatusInternalServerError, result: "failed/decide",
			err: err, reply: held + " The approval could not be recorded, so approve it in " +
				"SwitchTender."}
	}
	answer.runID = proposal.ID
	return answer
}

// short returns the first twelve characters of a commit id.
func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
