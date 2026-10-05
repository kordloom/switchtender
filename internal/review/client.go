package review

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/trigger"
)

// Commit status states, in the vocabulary GitHub uses. A GitLab client maps them onto its own.
const (
	// StatePending means the plan has not finished, or is waiting for approval.
	StatePending = "pending"
	// StateSuccess means the plan finished and nothing about the apply it stands for is refused.
	StateSuccess = "success"
	// StateFailure means the plan failed, or the apply it stands for would be refused.
	StateFailure = "failure"
	// StateError means no plan result exists: the plan was refused, canceled, or never run.
	StateError = "error"
)

// Status is one commit status a review sets on the pull request's head commit.
type Status struct {
	// State is pending, success, failure, or error.
	State string
	// Context names the status, one per template, so each template's plan is its own check.
	Context string
	// Description is the one-line result the forge shows beside the check.
	Description string
	// TargetURL links the check to the run, empty when the server has no public address.
	TargetURL string
}

// Comment is a comment a review previously posted on a pull request.
type Comment struct {
	// ID identifies the comment to the forge.
	ID string
	// Body is the comment's current text.
	Body string
}

// Client reaches one repository on one forge with one token.
type Client interface {
	// FindComment returns the oldest comment on pull request number that the token's own account
	// posted and that carries marker, or nil when there is none.
	FindComment(ctx context.Context, number int, marker string) (*Comment, error)
	// CreateComment posts body as a new comment on pull request number and returns its id.
	CreateComment(ctx context.Context, number int, body string) (string, error)
	// UpdateComment replaces the body of comment id on pull request number.
	UpdateComment(ctx context.Context, number int, id, body string) error
	// SetStatus sets a commit status on sha.
	SetStatus(ctx context.Context, sha string, s Status) error
	// Head returns the commit pull request number proposes now, lowercase, or empty when the forge
	// answers that it has no such pull request for this token, which leaves the caller unable to
	// tell which push is newest.
	Head(ctx context.Context, number int) (string, error)
}

// maxCommentPages bounds how many pages of comments a lookup reads, at a hundred per page. A pull
// request with more than this many comments gets a new review comment rather than an unbounded
// scan on every push.
const maxCommentPages = 20

// maxErrorBody bounds how much of a forge's error response is kept for the error message.
const maxErrorBody = 512

// maxResponseBody bounds how much of a forge's successful response is read.
const maxResponseBody = 8 << 20

// NewClient returns the client for cfg's provider, talking to cfg's API base with token over hc.
func NewClient(cfg *trigger.Review, token string, hc *http.Client) (Client, error) {
	if cfg == nil {
		return nil, fmt.Errorf("%w: no review configuration", trigger.ErrBadReview)
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	if token == "" {
		return nil, ErrNoToken
	}
	if hc == nil {
		hc = http.DefaultClient
	}
	api := &apiClient{base: cfg.BaseURL(), token: token, http: hc, provider: cfg.Provider}
	repo := strings.Trim(cfg.Repository, "/")
	if cfg.Provider == trigger.ProviderGitLab {
		return &gitlabClient{api: api, project: url.PathEscape(repo)}, nil
	}
	owner, name, _ := strings.Cut(repo, "/")
	return &githubClient{api: api, repo: url.PathEscape(owner) + "/" + url.PathEscape(name)}, nil
}

// apiClient sends authenticated JSON requests to a forge's REST API.
type apiClient struct {
	// base is the API root with no trailing slash.
	base string
	// token authenticates every request. It is never logged or put in an error.
	token string
	// http sends the requests.
	http *http.Client
	// provider selects the authentication header.
	provider string
}

// do sends a request with an optional JSON body and decodes a JSON response into out when out is
// not nil. It returns the response headers so a caller can read pagination. A non-2xx answer is an
// ErrForge error carrying the status and the start of the forge's message, never the token.
func (a *apiClient) do(ctx context.Context, method, path string, in, out any) (http.Header, error) {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, a.base+path, body)
	if err != nil {
		return nil, err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "switchtender-review")
	if a.provider == trigger.ProviderGitLab {
		req.Header.Set("PRIVATE-TOKEN", a.token)
	} else {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("Authorization", "Bearer "+a.token)
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	resp, err := a.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: %s %s: %s", ErrForge, method, path, redactToken(err.Error(), a.token))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
		return resp.Header, &forgeError{Method: method, Path: path, Code: resp.StatusCode,
			Message: redactToken(strings.TrimSpace(string(msg)), a.token)}
	}
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseBody))
		return resp.Header, nil
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxResponseBody)).Decode(out); err != nil {
		return resp.Header, fmt.Errorf("%w: %s %s: decode: %v", ErrForge, method, path, err)
	}
	return resp.Header, nil
}

// redactToken removes the token from text a transport error might have echoed.
func redactToken(text, token string) string {
	if token == "" {
		return text
	}
	return strings.ReplaceAll(text, token, "***")
}

// forgeError is a non-2xx answer from a forge.
type forgeError struct {
	// Method is the request method.
	Method string
	// Path is the request path, relative to the API base.
	Path string
	// Code is the HTTP status the forge answered with.
	Code int
	// Message is the start of the forge's response body.
	Message string
}

// Error describes the refusal.
func (e *forgeError) Error() string {
	return fmt.Sprintf("%s %s answered %d: %s", e.Method, e.Path, e.Code, e.Message)
}

// Unwrap makes every forge refusal an ErrForge.
func (e *forgeError) Unwrap() error { return ErrForge }

// githubClient is a Client for GitHub and GitHub Enterprise Server.
type githubClient struct {
	// api sends the requests.
	api *apiClient
	// repo is owner/name, each part path escaped.
	repo string
}

// githubComment is the part of a GitHub issue comment a review reads.
type githubComment struct {
	// ID identifies the comment.
	ID int64 `json:"id"`
	// Body is the comment text.
	Body string `json:"body"`
	// User is who posted it.
	User struct {
		// Login is the account name.
		Login string `json:"login"`
	} `json:"user"`
}

// FindComment pages through the pull request's comments for the oldest one the token's account
// posted that carries marker. When the account cannot be read, which is the case for a GitHub App
// installation token, the oldest comment carrying the marker is taken, and an update that the forge
// refuses falls back to a new comment.
func (g *githubClient) FindComment(ctx context.Context, number int, marker string) (*Comment, error) {
	var me struct {
		// Login is the token's own account.
		Login string `json:"login"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, "/user", nil, &me); err != nil && isUnauthorized(err) {
		return nil, err
	}
	for page := 1; page <= maxCommentPages; page++ {
		var list []githubComment
		path := "/repos/" + g.repo + "/issues/" + strconv.Itoa(number) +
			"/comments?per_page=100&page=" + strconv.Itoa(page)
		if _, err := g.api.do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return nil, err
		}
		for _, c := range list {
			if !strings.Contains(c.Body, marker) {
				continue
			}
			if me.Login != "" && !strings.EqualFold(c.User.Login, me.Login) {
				continue
			}
			return &Comment{ID: strconv.FormatInt(c.ID, 10), Body: c.Body}, nil
		}
		if len(list) < 100 {
			break
		}
	}
	return nil, nil
}

// CreateComment posts a new issue comment on the pull request.
func (g *githubClient) CreateComment(ctx context.Context, number int, body string) (string, error) {
	var out githubComment
	path := "/repos/" + g.repo + "/issues/" + strconv.Itoa(number) + "/comments"
	if _, err := g.api.do(ctx, http.MethodPost, path, commentBody(body), &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.ID, 10), nil
}

// UpdateComment edits an issue comment in place.
func (g *githubClient) UpdateComment(ctx context.Context, _ int, id, body string) error {
	path := "/repos/" + g.repo + "/issues/comments/" + url.PathEscape(id)
	_, err := g.api.do(ctx, http.MethodPatch, path, commentBody(body), nil)
	return err
}

// SetStatus sets a commit status on sha.
func (g *githubClient) SetStatus(ctx context.Context, sha string, s Status) error {
	payload := map[string]string{
		"state": s.State, "context": s.Context, "description": s.Description,
	}
	if s.TargetURL != "" {
		payload["target_url"] = s.TargetURL
	}
	path := "/repos/" + g.repo + "/statuses/" + url.PathEscape(sha)
	_, err := g.api.do(ctx, http.MethodPost, path, payload, nil)
	return err
}

// Head reads the pull request's current head commit.
func (g *githubClient) Head(ctx context.Context, number int) (string, error) {
	var pr struct {
		// Head is the side the code comes from.
		Head struct {
			// SHA is the head commit.
			SHA string `json:"sha"`
		} `json:"head"`
	}
	path := "/repos/" + g.repo + "/pulls/" + strconv.Itoa(number)
	_, err := g.api.do(ctx, http.MethodGet, path, nil, &pr)
	if isNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.ToLower(pr.Head.SHA), nil
}

// gitlabClient is a Client for GitLab, hosted or self-managed.
type gitlabClient struct {
	// api sends the requests.
	api *apiClient
	// project is the group/project path, path escaped as one segment.
	project string
}

// gitlabNote is the part of a GitLab merge request note a review reads.
type gitlabNote struct {
	// ID identifies the note.
	ID int64 `json:"id"`
	// Body is the note text.
	Body string `json:"body"`
	// System reports a note GitLab wrote itself, such as a push notice.
	System bool `json:"system"`
	// Author is who posted it.
	Author struct {
		// ID is the account id.
		ID int64 `json:"id"`
	} `json:"author"`
}

// FindComment pages through the merge request's notes, oldest first, for the first one the token's
// account posted that carries marker.
func (g *gitlabClient) FindComment(ctx context.Context, number int, marker string) (*Comment, error) {
	var me struct {
		// ID is the token's own account.
		ID int64 `json:"id"`
	}
	if _, err := g.api.do(ctx, http.MethodGet, "/user", nil, &me); err != nil && isUnauthorized(err) {
		return nil, err
	}
	for page := 1; page <= maxCommentPages; page++ {
		var list []gitlabNote
		path := "/projects/" + g.project + "/merge_requests/" + strconv.Itoa(number) +
			"/notes?sort=asc&order_by=created_at&per_page=100&page=" + strconv.Itoa(page)
		if _, err := g.api.do(ctx, http.MethodGet, path, nil, &list); err != nil {
			return nil, err
		}
		for _, n := range list {
			if n.System || !strings.Contains(n.Body, marker) {
				continue
			}
			if me.ID != 0 && n.Author.ID != me.ID {
				continue
			}
			return &Comment{ID: strconv.FormatInt(n.ID, 10), Body: n.Body}, nil
		}
		if len(list) < 100 {
			break
		}
	}
	return nil, nil
}

// CreateComment posts a new note on the merge request.
func (g *gitlabClient) CreateComment(ctx context.Context, number int, body string) (string, error) {
	var out gitlabNote
	path := "/projects/" + g.project + "/merge_requests/" + strconv.Itoa(number) + "/notes"
	if _, err := g.api.do(ctx, http.MethodPost, path, commentBody(body), &out); err != nil {
		return "", err
	}
	return strconv.FormatInt(out.ID, 10), nil
}

// UpdateComment edits a merge request note in place.
func (g *gitlabClient) UpdateComment(ctx context.Context, number int, id, body string) error {
	path := "/projects/" + g.project + "/merge_requests/" + strconv.Itoa(number) + "/notes/" +
		url.PathEscape(id)
	_, err := g.api.do(ctx, http.MethodPut, path, commentBody(body), nil)
	return err
}

// gitlabStates maps the review's status vocabulary onto GitLab's, which has no error state.
var gitlabStates = map[string]string{
	StatePending: "pending", StateSuccess: "success", StateFailure: "failed", StateError: "failed",
}

// SetStatus sets a commit status on sha.
func (g *gitlabClient) SetStatus(ctx context.Context, sha string, s Status) error {
	payload := map[string]string{
		"state": gitlabStates[s.State], "name": s.Context, "description": s.Description,
	}
	if s.TargetURL != "" {
		payload["target_url"] = s.TargetURL
	}
	_, err := g.api.do(ctx, http.MethodPost, "/projects/"+g.project+"/statuses/"+url.PathEscape(sha),
		payload, nil)
	return err
}

// Head reads the merge request's current head commit.
func (g *gitlabClient) Head(ctx context.Context, number int) (string, error) {
	var mr struct {
		// SHA is the head commit of the merge request's source branch.
		SHA string `json:"sha"`
	}
	path := "/projects/" + g.project + "/merge_requests/" + strconv.Itoa(number)
	_, err := g.api.do(ctx, http.MethodGet, path, nil, &mr)
	if isNotFound(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return strings.ToLower(mr.SHA), nil
}

// commentBody is the JSON body both forges take for a comment.
func commentBody(body string) map[string]string {
	return map[string]string{"body": body}
}

// isUnauthorized reports whether err is the forge refusing the token outright, as opposed to a
// token that cannot describe its own account.
func isUnauthorized(err error) bool {
	var fe *forgeError
	return errors.As(err, &fe) && fe.Code == http.StatusUnauthorized
}

// isNotFound reports whether err is the forge answering that the object does not exist for this
// token.
func isNotFound(err error) bool {
	var fe *forgeError
	return errors.As(err, &fe) && fe.Code == http.StatusNotFound
}

// isRefused reports whether err is the forge refusing a write to an object the token may not touch
// or cannot see, which for an update means the comment is not ours to edit.
func isRefused(err error) bool {
	var fe *forgeError
	return errors.As(err, &fe) && (fe.Code == http.StatusForbidden || fe.Code == http.StatusNotFound)
}
