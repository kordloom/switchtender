package review

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kordloom/switchtender/internal/trigger"
)

// Commands a pull request comment can carry. A comment acts only when its first line is
// "/switchtender plan" or "/switchtender apply", optionally naming a plan and a template.
const (
	// CommandPlan plans the pull request's current head, the way a push to it does.
	CommandPlan = "plan"
	// CommandApply approves and applies the plan last reported for the pull request's current head.
	CommandApply = "apply"
)

// commandPrefix starts every command line.
const commandPrefix = "/switchtender"

// Comment record bounds.
const (
	// CommentMaxAge is how old a comment may be, by the forge's own time, and still act. A
	// delivery of an older comment is a redelivery or a replay, and is refused.
	CommentMaxAge = 24 * time.Hour
	// CommentRetention is how long the record of a comment acted on, and of a reply done, is kept.
	// It outlasts CommentMaxAge, so a comment is refused as too old long before the record that
	// stops it acting twice is pruned.
	CommentRetention = 7 * 24 * time.Hour
	// pruneEvery is how often a reporter prunes comment records.
	pruneEvery = time.Hour
)

// ApplySource is the run source of an apply a pull request comment asked for, so it is never taken
// for a review plan and never reported to the pull request as one.
const ApplySource = "review_apply"

// Kinds of record a comment command makes, beside the plan and refusal kinds.
const (
	// KindCommand marks a comment that was acted on, keyed by the forge's comment id, so a
	// redelivered webhook finds it and acts on nothing a second time. It reports nothing.
	KindCommand = "command"
	// KindReply is the one reply a comment command posts as a new comment on the pull request.
	KindReply = "reply"
)

// CommentEvent is one new comment on a pull request or merge request, reduced to what a comment
// command needs. Every field is read from a body whose signature or token was verified before it
// was parsed.
type CommentEvent struct {
	// Provider is github or gitlab.
	Provider string
	// Repository is the repository the pull request belongs to: owner/name on GitHub, the
	// group/project path on GitLab.
	Repository string
	// Number is the pull request number on GitHub, the merge request iid on GitLab.
	Number int
	// CommentID is the forge's numeric id of the comment.
	CommentID int64
	// AuthorID is the forge's numeric id of the comment's author. The login is never read.
	AuthorID int64
	// AuthorBot reports that the forge marks the author as a bot. GitHub says so in the event,
	// GitLab only through its users API, so on GitLab it is read separately.
	AuthorBot bool
	// ViaApp reports that GitHub marks the comment as written by a GitHub App on the author's
	// behalf, which is how an agent holding a person's user-to-server token writes as them.
	ViaApp bool
	// Body is the comment's text as delivered.
	Body string
}

// Command is a parsed comment command.
type Command struct {
	// Verb is CommandPlan or CommandApply.
	Verb string
	// Plan is the plan id an apply names, empty for a bare apply and for a plan command.
	Plan string
	// Template scopes the command to the review trigger of the template with this name or id,
	// empty for every template.
	Template string
}

// planIDPattern matches the short plan id a report shows, and runIDPattern a plan's full run id.
var (
	planIDPattern = regexp.MustCompile(`^[0-9a-f]{8}$`)
	runIDPattern  = regexp.MustCompile(`^run_[0-9a-f]{16}$`)
)

// PlanID returns the short id a report shows for the plan runID, the first eight hex characters
// of its run id, which an apply names to say which plan it approves.
func PlanID(runID string) string {
	id := strings.TrimPrefix(runID, "run_")
	if len(id) > 8 {
		id = id[:8]
	}
	return id
}

// MatchesPlan reports whether id, as an apply comment named it, names the plan runID: its short
// plan id or its full run id.
func MatchesPlan(id, runID string) bool {
	return id != "" && (id == runID || id == PlanID(runID))
}

// Parse reads the command the comment carries and reports whether it carries one. The command is
// the first line that is not blank. A first line indented by a tab or four spaces is a code block
// on both forges, an example rather than a command, so it never acts. The grammar is
// "/switchtender plan [-p TEMPLATE]" and "/switchtender apply [PLAN] [-p TEMPLATE]", where PLAN is
// a plan id a report showed. Anything else on the line makes it no command at all, so a typo is
// never taken for one.
func (c *CommentEvent) Parse() (Command, bool) {
	var first string
	for _, line := range strings.Split(c.Body, "\n") {
		if strings.TrimSpace(line) != "" {
			first = strings.TrimRight(line, "\r")
			break
		}
	}
	if strings.HasPrefix(first, "\t") || strings.HasPrefix(first, "    ") {
		return Command{}, false
	}
	fields := strings.Fields(first)
	if len(fields) < 2 || fields[0] != commandPrefix {
		return Command{}, false
	}
	cmd := Command{Verb: fields[1]}
	if cmd.Verb != CommandPlan && cmd.Verb != CommandApply {
		return Command{}, false
	}
	for i := 2; i < len(fields); i++ {
		switch {
		case fields[i] == "-p" && i+1 < len(fields) && cmd.Template == "":
			cmd.Template = fields[i+1]
			i++
		case cmd.Verb == CommandApply && cmd.Plan == "" &&
			(planIDPattern.MatchString(fields[i]) || runIDPattern.MatchString(fields[i])):
			cmd.Plan = fields[i]
		default:
			return Command{}, false
		}
	}
	return cmd, true
}

// Command returns the verb of the command the comment carries, CommandPlan or CommandApply, or
// empty when it carries none.
func (c *CommentEvent) Command() string {
	cmd, ok := c.Parse()
	if !ok {
		return ""
	}
	return cmd.Verb
}

// BodySHA256 returns the hex SHA-256 of the comment body as delivered, which the decision records
// so an edit or a deletion afterward cannot change what was approved.
func (c *CommentEvent) BodySHA256() string {
	sum := sha256.Sum256([]byte(c.Body))
	return hex.EncodeToString(sum[:])
}

// SameRepository reports whether the comment's repository is repo, ignoring case and surrounding
// slashes.
func (c *CommentEvent) SameRepository(repo string) bool {
	return strings.EqualFold(strings.Trim(c.Repository, "/"), strings.Trim(repo, "/"))
}

// githubCommentPayload is the part of a GitHub issue_comment event a command reads.
type githubCommentPayload struct {
	// Action is created, edited, or deleted.
	Action string `json:"action"`
	// Issue is the issue or pull request commented on.
	Issue struct {
		// Number is the issue or pull request number.
		Number int `json:"number"`
		// PullRequest is present only when the issue is a pull request.
		PullRequest *json.RawMessage `json:"pull_request"`
	} `json:"issue"`
	// Comment is the comment itself.
	Comment struct {
		// ID is the comment's numeric id.
		ID int64 `json:"id"`
		// Body is the comment text.
		Body string `json:"body"`
		// User is who wrote it.
		User struct {
			// ID is the account's numeric id.
			ID int64 `json:"id"`
			// Type is User, Bot, or Organization.
			Type string `json:"type"`
		} `json:"user"`
		// PerformedViaGitHubApp names the GitHub App that wrote the comment on the user's behalf,
		// null for a comment the user wrote themselves.
		PerformedViaGitHubApp json.RawMessage `json:"performed_via_github_app"`
	} `json:"comment"`
	// Repository is the repository the pull request belongs to.
	Repository githubRepo `json:"repository"`
}

// ParseGitHubComment reads a GitHub webhook as a new pull request comment. eventName is the
// X-GitHub-Event header. Anything but a newly created comment on a pull request returns
// ErrNotCommentEvent, so an edit or a deletion never acts.
func ParseGitHubComment(eventName string, body []byte) (*CommentEvent, error) {
	if eventName != "issue_comment" {
		return nil, fmt.Errorf("%w: %q", ErrNotCommentEvent, eventName)
	}
	var p githubCommentPayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPayload, err)
	}
	if p.Action != "created" {
		return nil, fmt.Errorf("%w: action %q", ErrNotCommentEvent, p.Action)
	}
	if p.Issue.PullRequest == nil {
		return nil, fmt.Errorf("%w: a comment on an issue", ErrNotCommentEvent)
	}
	if p.Issue.Number <= 0 || p.Comment.ID <= 0 || p.Comment.User.ID <= 0 ||
		p.Repository.FullName == "" {
		return nil, fmt.Errorf("%w: comment lacks its number, id, author, or repository", ErrBadPayload)
	}
	return &CommentEvent{
		Provider: trigger.ProviderGitHub, Repository: p.Repository.FullName, Number: p.Issue.Number,
		CommentID: p.Comment.ID, AuthorID: p.Comment.User.ID,
		AuthorBot: strings.EqualFold(p.Comment.User.Type, "Bot"),
		ViaApp:    viaApp(p.Comment.PerformedViaGitHubApp), Body: p.Comment.Body,
	}, nil
}

// viaApp reports whether a GitHub performed_via_github_app value names an app: present and not
// null.
func viaApp(raw json.RawMessage) bool {
	v := strings.TrimSpace(string(raw))
	return v != "" && v != "null"
}

// gitlabNotePayload is the part of a GitLab note event a command reads.
type gitlabNotePayload struct {
	// ObjectKind is note for a note event.
	ObjectKind string `json:"object_kind"`
	// User is who wrote the note.
	User struct {
		// ID is the account's numeric id.
		ID int64 `json:"id"`
	} `json:"user"`
	// Project is the project the hook is configured on.
	Project struct {
		// PathWithNamespace is group/project.
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
	// ObjectAttributes is the note itself.
	ObjectAttributes struct {
		// ID is the note's numeric id.
		ID int64 `json:"id"`
		// Note is the note text.
		Note string `json:"note"`
		// NoteableType is MergeRequest for a note on a merge request.
		NoteableType string `json:"noteable_type"` //nolint:misspell // GitLab's own field name.
		// AuthorID is the author's numeric id.
		AuthorID int64 `json:"author_id"`
		// System reports a note GitLab wrote itself.
		System bool `json:"system"`
		// Action is create or update on releases that send it, absent on older ones, which send
		// a note event only when a note is created.
		Action string `json:"action"`
	} `json:"object_attributes"`
	// MergeRequest is the merge request the note is on.
	MergeRequest *struct {
		// IID is the merge request number within its project.
		IID int `json:"iid"`
	} `json:"merge_request"`
}

// ParseGitLabComment reads a GitLab webhook as a new merge request note. eventName is the
// X-Gitlab-Event header. Anything but a newly created note a person wrote on a merge request
// returns ErrNotCommentEvent, so an edit never acts.
func ParseGitLabComment(eventName string, body []byte) (*CommentEvent, error) {
	if eventName != "Note Hook" {
		return nil, fmt.Errorf("%w: %q", ErrNotCommentEvent, eventName)
	}
	var p gitlabNotePayload
	if err := json.Unmarshal(body, &p); err != nil {
		return nil, fmt.Errorf("%w: %v", ErrBadPayload, err)
	}
	attrs := p.ObjectAttributes
	if p.ObjectKind != "note" || attrs.NoteableType != "MergeRequest" || p.MergeRequest == nil {
		return nil, fmt.Errorf("%w: not a merge request note", ErrNotCommentEvent)
	}
	if attrs.System || (attrs.Action != "" && attrs.Action != "create") {
		return nil, fmt.Errorf("%w: action %q", ErrNotCommentEvent, attrs.Action)
	}
	author := attrs.AuthorID
	if author == 0 {
		author = p.User.ID
	}
	if p.MergeRequest.IID <= 0 || attrs.ID <= 0 || author <= 0 || p.Project.PathWithNamespace == "" {
		return nil, fmt.Errorf("%w: note lacks its merge request, id, author, or project", ErrBadPayload)
	}
	return &CommentEvent{
		Provider: trigger.ProviderGitLab, Repository: p.Project.PathWithNamespace,
		Number: p.MergeRequest.IID, CommentID: attrs.ID, AuthorID: author, Body: attrs.Note,
	}, nil
}

// ParseComment reads a webhook for provider as a new pull request comment, using the event header
// that provider sends.
func ParseComment(provider, eventName string, body []byte) (*CommentEvent, error) {
	if provider == trigger.ProviderGitLab {
		return ParseGitLabComment(eventName, body)
	}
	return ParseGitHubComment(eventName, body)
}

// commentKey derives the id part a comment's records share from the trigger the comment arrived
// through and everything that names the comment: the forge, its API base, the repository, and the
// comment id. Each review trigger watching a repository acts on its own template's plan, so each
// handles the comment once.
func commentKey(tg *trigger.Trigger, ev *CommentEvent) string {
	sum := sha256.Sum256([]byte(tg.ID + "\x00" + ev.Provider + "\x00" + tg.Review.BaseURL() + "\x00" +
		strings.ToLower(strings.Trim(ev.Repository, "/")) + "\x00" +
		strconv.FormatInt(ev.CommentID, 10)))
	return hex.EncodeToString(sum[:12])
}

// CommandRecord returns the record that marks comment ev as acted on. It is born done, since it
// reports nothing, and its id is derived from the comment, so a second delivery of the same comment
// finds it.
func CommandRecord(tg *trigger.Trigger, ev *CommentEvent, at time.Time) *Record {
	rec := &Record{
		ID: "cmd_" + commentKey(tg, ev), Kind: KindCommand, TriggerID: tg.ID,
		PullRequest: ev.Number, Reason: ev.Command(), Done: true, CreatedAt: at,
	}
	rec.setDestination(tg.Review)
	rec.Repository = ev.Repository
	return rec
}

// ReplyRecord returns the record of the one reply comment ev gets, carrying text.
func ReplyRecord(tg *trigger.Trigger, ev *CommentEvent, text string, at time.Time) *Record {
	rec := &Record{
		ID: "rpl_" + commentKey(tg, ev), Kind: KindReply, TriggerID: tg.ID,
		PullRequest: ev.Number, Reason: text, CreatedAt: at,
	}
	rec.setDestination(tg.Review)
	rec.Repository = ev.Repository
	return rec
}

// RefusalReplyRecord returns the record of the reply a refused comment command gets. Its id is
// derived from the trigger, the pull request, the author's numeric id, the reason, and the reply's
// text rather than from the comment, so an author who repeats a refused command on the same pull
// request finds the record the first refusal made and is not answered again. A comment can then
// never make the operator's token post the same refusal over and over, nor spend the forge's rate
// limit doing it.
func RefusalReplyRecord(tg *trigger.Trigger, ev *CommentEvent, reason, text string, at time.Time) *Record {
	sum := sha256.Sum256([]byte(tg.ID + "\x00" + ev.Provider + "\x00" + tg.Review.BaseURL() + "\x00" +
		strings.ToLower(strings.Trim(ev.Repository, "/")) + "\x00" + strconv.Itoa(ev.Number) + "\x00" +
		strconv.FormatInt(ev.AuthorID, 10) + "\x00" + reason + "\x00" + text))
	rec := &Record{
		ID: "rpa_" + hex.EncodeToString(sum[:12]), Kind: KindReply, TriggerID: tg.ID,
		PullRequest: ev.Number, Reason: text, CreatedAt: at,
	}
	rec.setDestination(tg.Review)
	rec.Repository = ev.Repository
	return rec
}

// PullRequest is what a comment command reads about a pull request at the moment it acts, from the
// forge rather than from the comment's event, which carries none of it on GitHub.
type PullRequest struct {
	// HeadSHA is the commit the pull request proposes now, lowercase.
	HeadSHA string
	// HeadBranch is the branch the pull request comes from, for display.
	HeadBranch string
	// Fork reports that the head lives in another repository, or in one that was deleted.
	Fork bool
	// Open reports that the pull request is open.
	Open bool
	// URL is the pull request's web address.
	URL string
	// AuthorID is the forge's numeric id of the account that opened the pull request, zero when
	// the forge did not say.
	AuthorID int64
}

// ForgeComment is a comment as the forge itself holds it, read back by id, so what a command acts
// on is what the forge says was written rather than what a webhook body claims.
type ForgeComment struct {
	// ID is the comment's numeric id.
	ID int64
	// PullRequest is the pull request or merge request the comment is on.
	PullRequest int
	// AuthorID is the forge's numeric id of the comment's author.
	AuthorID int64
	// AuthorBot reports that the forge marks the author as a bot.
	AuthorBot bool
	// ViaApp reports that GitHub marks the comment as written by a GitHub App for its author.
	ViaApp bool
	// Body is the comment's current text.
	Body string
	// CreatedAt is when the comment was written, zero when the forge did not say.
	CreatedAt time.Time
}

// Commenter is the forge access a comment command needs beyond what a report uses.
type Commenter interface {
	// PullRequest reads pull request number, or returns nil when the forge has no such pull
	// request for this token.
	PullRequest(ctx context.Context, number int) (*PullRequest, error)
	// IsBot reports whether the forge account with numeric id userID is a bot.
	IsBot(ctx context.Context, userID int64) (bool, error)
	// Comment reads comment id on pull request number back from the forge, or returns nil when
	// the forge holds no such comment on that pull request.
	Comment(ctx context.Context, number int, id int64) (*ForgeComment, error)
}

// PullRequest reads a GitHub pull request's head, its fork status, and its state.
func (g *githubClient) PullRequest(ctx context.Context, number int) (*PullRequest, error) {
	var pr struct {
		// State is open or closed.
		State string `json:"state"`
		// HTMLURL is the pull request's web address.
		HTMLURL string `json:"html_url"`
		// User is the account that opened the pull request.
		User struct {
			// ID is the account's numeric id.
			ID int64 `json:"id"`
		} `json:"user"`
		// Head is the side the code comes from.
		Head githubRef `json:"head"`
		// Base is the side the code is proposed into.
		Base githubRef `json:"base"`
	}
	path := "/repos/" + g.repo + "/pulls/" + strconv.Itoa(number)
	_, err := g.api.do(ctx, http.MethodGet, path, nil, &pr)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sha := strings.ToLower(pr.Head.SHA)
	if !shaPattern.MatchString(sha) {
		return nil, fmt.Errorf("%w: pull request %d head sha %q", ErrForge, number, pr.Head.SHA)
	}
	fork := pr.Head.Repo == nil || pr.Base.Repo == nil ||
		!strings.EqualFold(pr.Head.Repo.FullName, pr.Base.Repo.FullName)
	return &PullRequest{HeadSHA: sha, HeadBranch: pr.Head.Ref, Fork: fork, Open: pr.State == "open",
		URL: pr.HTMLURL, AuthorID: pr.User.ID}, nil
}

// Comment reads a GitHub issue comment back by id and checks it is on pull request number, read
// from the issue address the forge answers with.
func (g *githubClient) Comment(ctx context.Context, number int, id int64) (*ForgeComment, error) {
	var c struct {
		// ID is the comment's numeric id.
		ID int64 `json:"id"`
		// Body is the comment text.
		Body string `json:"body"`
		// IssueURL is the API address of the issue or pull request the comment is on.
		IssueURL string `json:"issue_url"`
		// CreatedAt is when the comment was written.
		CreatedAt time.Time `json:"created_at"`
		// User is who wrote it.
		User struct {
			// ID is the account's numeric id.
			ID int64 `json:"id"`
			// Type is User, Bot, or Organization.
			Type string `json:"type"`
		} `json:"user"`
		// PerformedViaGitHubApp names the app that wrote it for the user, null for none.
		PerformedViaGitHubApp json.RawMessage `json:"performed_via_github_app"`
	}
	path := "/repos/" + g.repo + "/issues/comments/" + strconv.FormatInt(id, 10)
	_, err := g.api.do(ctx, http.MethodGet, path, nil, &c)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	on, _ := strconv.Atoi(c.IssueURL[strings.LastIndex(c.IssueURL, "/")+1:])
	if c.ID != id || on != number {
		return nil, nil
	}
	return &ForgeComment{ID: c.ID, PullRequest: on, AuthorID: c.User.ID,
		AuthorBot: strings.EqualFold(c.User.Type, "Bot"), ViaApp: viaApp(c.PerformedViaGitHubApp),
		Body: c.Body, CreatedAt: c.CreatedAt}, nil
}

// IsBot reads a GitHub account's type. A comment event already carries it, so a command reads the
// event's, and this serves a caller that holds only the id.
func (g *githubClient) IsBot(ctx context.Context, userID int64) (bool, error) {
	var u struct {
		// Type is User, Bot, or Organization.
		Type string `json:"type"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, "/user/"+strconv.FormatInt(userID, 10), nil,
		&u); err != nil {
		return false, err
	}
	return strings.EqualFold(u.Type, "Bot"), nil
}

// PullRequest reads a GitLab merge request's head, its fork status, and its state.
func (g *gitlabClient) PullRequest(ctx context.Context, number int) (*PullRequest, error) {
	var mr struct {
		// SHA is the head commit of the merge request's source branch.
		SHA string `json:"sha"`
		// State is opened, closed, locked, or merged.
		State string `json:"state"`
		// SourceBranch is the branch the code comes from.
		SourceBranch string `json:"source_branch"`
		// SourceProjectID is where the code comes from.
		SourceProjectID int64 `json:"source_project_id"`
		// TargetProjectID is where the code is proposed into.
		TargetProjectID int64 `json:"target_project_id"`
		// WebURL is the merge request's web address.
		WebURL string `json:"web_url"`
		// Author is the account that opened the merge request.
		Author struct {
			// ID is the account's numeric id.
			ID int64 `json:"id"`
		} `json:"author"`
	}
	path := "/projects/" + g.project + "/merge_requests/" + strconv.Itoa(number)
	_, err := g.api.do(ctx, http.MethodGet, path, nil, &mr)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	sha := strings.ToLower(mr.SHA)
	if !shaPattern.MatchString(sha) {
		return nil, fmt.Errorf("%w: merge request %d head sha %q", ErrForge, number, mr.SHA)
	}
	return &PullRequest{HeadSHA: sha, HeadBranch: mr.SourceBranch,
		Fork: mr.SourceProjectID != mr.TargetProjectID, Open: mr.State == "opened",
		URL: mr.WebURL, AuthorID: mr.Author.ID}, nil
}

// Comment reads a GitLab merge request note back by id. The note is read under the merge request,
// so a note on any other merge request or issue is not found.
func (g *gitlabClient) Comment(ctx context.Context, number int, id int64) (*ForgeComment, error) {
	var n struct {
		// ID is the note's numeric id.
		ID int64 `json:"id"`
		// Body is the note text.
		Body string `json:"body"`
		// System reports a note GitLab wrote itself.
		System bool `json:"system"`
		// CreatedAt is when the note was written.
		CreatedAt time.Time `json:"created_at"`
		// Author is who wrote it.
		Author struct {
			// ID is the account's numeric id.
			ID int64 `json:"id"`
		} `json:"author"`
	}
	path := "/projects/" + g.project + "/merge_requests/" + strconv.Itoa(number) + "/notes/" +
		strconv.FormatInt(id, 10)
	_, err := g.api.do(ctx, http.MethodGet, path, nil, &n)
	if isNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if n.ID != id || n.System {
		return nil, nil
	}
	return &ForgeComment{ID: n.ID, PullRequest: number, AuthorID: n.Author.ID, Body: n.Body,
		CreatedAt: n.CreatedAt}, nil
}

// IsBot reads whether a GitLab account is a bot user, such as a project or group access token's
// account or a service account.
func (g *gitlabClient) IsBot(ctx context.Context, userID int64) (bool, error) {
	var u struct {
		// Bot reports a bot user.
		Bot bool `json:"bot"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, "/users/"+strconv.FormatInt(userID, 10), nil,
		&u); err != nil {
		return false, err
	}
	return u.Bot, nil
}

// replyRecord is the content a reply's chain entry commits: which reply it is and the SHA-256 of
// the exact body posted.
type replyRecord struct {
	// Reply is the reply record's id, derived from the comment it answers.
	Reply string `json:"reply"`
	// Phase is replied.
	Phase string `json:"phase"`
	// CommentSHA256 is the hex SHA-256 of the reply body.
	CommentSHA256 string `json:"comment_sha256"`
}

// postReply records a comment command's reply on the chain and posts it as a new comment, once. A
// reply retried after its chain entry landed is not recorded again, and one whose claim lapsed is
// not posted, the same as a report.
func (rp *Reporter) postReply(ctx context.Context, client Client, tg *trigger.Trigger, rec *Record) (posted, error) {
	body := rec.Reason
	sum := digest([]byte(body))
	raw, err := json.Marshal(replyRecord{Reply: rec.ID, Phase: PhaseReplied, CommentSHA256: sum})
	if err != nil {
		return posted{}, err
	}
	res := posted{}
	if recorded := digest(raw); recorded != rec.RecordedSHA256 {
		if err := rp.record(ctx, tg, rec.PullRequest, raw); err != nil {
			return posted{}, err
		}
		res.recorded = recorded
	}
	if err := rp.holding(ctx, rec); err != nil {
		return res, err
	}
	if _, err := client.CreateComment(ctx, rec.PullRequest, body); err != nil {
		return res, fmt.Errorf("write the reply: %w", err)
	}
	res.comment = sum
	return res, nil
}

// MarkCommand records that comment ev is being acted on and reports whether this call made the
// record. A false means the comment was handled before, by this process or another sharing the
// store, so nothing is acted on a second time: a redelivered webhook is answered and ignored.
func (rp *Reporter) MarkCommand(ctx context.Context, tg *trigger.Trigger, ev *CommentEvent) (bool, error) {
	return rp.cfg.Store.Create(ctx, CommandRecord(tg, ev, rp.clock(ctx)))
}

// Commanded reports whether comment ev was already acted on through tg, read from the store alone.
func (rp *Reporter) Commanded(ctx context.Context, tg *trigger.Trigger, ev *CommentEvent) (bool, error) {
	_, err := rp.cfg.Store.Get(ctx, CommandRecord(tg, ev, time.Time{}).ID)
	if errors.Is(err, ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Reply posts text as a new comment on the pull request ev was written on, in the background, once
// across every process sharing the store, however often the comment's webhook is delivered.
func (rp *Reporter) Reply(tg *trigger.Trigger, ev *CommentEvent, text string) {
	if tg == nil || tg.Review == nil || ev == nil || text == "" {
		return
	}
	rp.track(func(now time.Time) *Record { return ReplyRecord(tg, ev, text, now) })
}

// ReplyRefusal posts text as the reply to a comment command refused for reason, at most once per
// trigger, pull request, author, reason, and text, however many comments the author writes. It is
// the claim-once record Reply uses, keyed on the author rather than the comment.
func (rp *Reporter) ReplyRefusal(tg *trigger.Trigger, ev *CommentEvent, reason, text string) {
	if tg == nil || tg.Review == nil || ev == nil || text == "" {
		return
	}
	rp.track(func(now time.Time) *Record { return RefusalReplyRecord(tg, ev, reason, text, now) })
}

// RefusalReplied reports whether the reply to a command refused for reason, carrying text, was
// already owed or posted for this author and pull request, so a caller can skip the forge reads a
// reply would need.
func (rp *Reporter) RefusalReplied(ctx context.Context, tg *trigger.Trigger, ev *CommentEvent, reason,
	text string) (bool, error) {
	_, err := rp.cfg.Store.Get(ctx, RefusalReplyRecord(tg, ev, reason, text, time.Time{}).ID)
	if errors.Is(err, ErrRecordNotFound) {
		return false, nil
	}
	return err == nil, err
}

// Record returns the report record with id, a plan's being keyed by its run id, or
// ErrRecordNotFound.
func (rp *Reporter) Record(ctx context.Context, id string) (*Record, error) {
	return rp.cfg.Store.Get(ctx, id)
}

// Commenter opens tg's forge token and returns what a comment command reads the forge through,
// with a release the caller runs once its reads are done.
func (rp *Reporter) Commenter(ctx context.Context, tg *trigger.Trigger) (Commenter, func(), error) {
	if tg == nil || tg.Review == nil {
		return nil, func() {}, fmt.Errorf("%w: no review configuration", trigger.ErrBadReview)
	}
	cfg := *tg.Review
	client, lease, err := rp.client(ctx, &cfg)
	release := func() { revoke(lease) }
	if err != nil {
		return nil, release, err
	}
	c, ok := client.(Commenter)
	if !ok {
		return nil, release, fmt.Errorf("%w: this forge client cannot read pull requests", ErrForge)
	}
	return c, release, nil
}
