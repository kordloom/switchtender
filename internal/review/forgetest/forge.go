// Package forgetest provides fake GitHub and GitLab REST APIs for review tests. Each fake serves
// over TLS, checks the token on every request, keeps the comments and commit statuses it was sent,
// and lets an update to a comment somebody else posted fail the way the real forge does. A test can
// also move a pull request's head, as a push does, and take the fake down, wholly or for writes
// alone, to stand in for an outage. Nothing here ever talks to a real forge.
package forgetest

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// botLogin and botID are the token's own account on every fake.
const (
	botLogin = "switchtender-bot"
	botID    = 41
)

// Comment is a comment held by the fake.
type Comment struct {
	// ID identifies the comment.
	ID int64
	// Number is the pull request it was posted on.
	Number int
	// Author is the account that posted it.
	Author string
	// AuthorID is the numeric id of the account that posted it, zero for the token's own.
	AuthorID int64
	// Body is its current text.
	Body string
	// Edits counts how many times it was updated in place.
	Edits int
	// CreatedAt is when it was posted.
	CreatedAt time.Time
}

// Hold records a comment a person posted on pull request number, with the forge's numeric comment
// id and the author's numeric id, so a comment command's webhook names a comment the forge holds.
func (f *Forge) Hold(number int, id, authorID int64, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.comments = append(f.comments, &Comment{ID: id, Number: number,
		Author: "account-" + strconv.FormatInt(authorID, 10), AuthorID: authorID, Body: body,
		CreatedAt: time.Now().UTC()})
}

// SetCommentCreated changes when comment id was written, for a test of how old a comment may be or
// of which plan its author could have read.
func (f *Forge) SetCommentCreated(id int64, at time.Time) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID == id {
			c.CreatedAt = at
		}
	}
}

// SetCommentBody changes the body of comment id, as an edit on the forge does.
func (f *Forge) SetCommentBody(id int64, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID == id {
			c.Body = body
		}
	}
}

// held returns a copy of comment id on pull request number, or nil.
func (f *Forge) held(number int, id int64) *Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID == id && c.Number == number {
			cp := *c
			return &cp
		}
	}
	return nil
}

// commentGitHub answers a GitHub issue comment read by id.
func (f *Forge) commentGitHub(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("b"), 10, 64)
	f.mu.Lock()
	var found *Comment
	for _, c := range f.comments {
		if c.ID == id {
			cp := *c
			found = &cp
		}
	}
	f.mu.Unlock()
	if found == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": found.ID, "body": found.Body, "created_at": found.CreatedAt,
		"issue_url": f.Server.URL + "/api/v3/repos/" + f.Repository + "/issues/" +
			strconv.Itoa(found.Number),
		"user":                     map[string]any{"id": found.AuthorID, "type": "User"},
		"performed_via_github_app": nil,
	})
}

// issuesGitHub routes a GitHub read under issues: a comment by id, or a pull request's comments.
func (f *Forge) issuesGitHub(w http.ResponseWriter, r *http.Request) {
	if r.PathValue("a") == "comments" {
		f.commentGitHub(w, r)
		return
	}
	if r.PathValue("b") != "comments" {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	r.SetPathValue("n", r.PathValue("a"))
	f.listGitHub(w, r)
}

// noteGitLab answers a GitLab merge request note read by id.
func (f *Forge) noteGitLab(w http.ResponseWriter, r *http.Request) {
	number, _ := strconv.Atoi(r.PathValue("n"))
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	c := f.held(number, id)
	if c == nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": c.ID, "body": c.Body, "system": false,
		"created_at": c.CreatedAt, "author": map[string]any{"id": c.AuthorID}})
}

// Status is a commit status the fake was sent.
type Status struct {
	// SHA is the commit it was set on.
	SHA string
	// State is the state as the forge spells it.
	State string
	// Context is the status name.
	Context string
	// Description is the one-line result.
	Description string
	// TargetURL is the link.
	TargetURL string
}

// Forge is a fake forge API.
type Forge struct {
	// Server serves the API.
	Server *httptest.Server
	// Provider is github or gitlab.
	Provider string
	// Token is the token every request must carry.
	Token string
	// Repository is the only repository path the fake answers for.
	Repository string
	// Login is the token's own account name.
	Login string
	// UserID is the token's own numeric account id, GitLab's identity.
	UserID int64
	// mu guards everything below.
	mu sync.Mutex
	// comments holds every comment in creation order.
	comments []*Comment
	// statuses holds every status in arrival order.
	statuses []Status
	// bodies holds every request body the fake received, for leak checks.
	bodies []string
	// unauthorized counts requests that carried the wrong token.
	unauthorized int
	// nextID numbers new comments.
	nextID int64
	// heads maps a pull request number to the commit it proposes now, for the pull request read.
	// A pull request with no head set answers not found.
	heads map[int]string
	// outage is the status every request answers with while the forge is down, zero when it is up.
	outage int
	// writeOutage is the status every write answers with while writes fail and reads still work,
	// zero when writes work.
	writeOutage int
	// requests counts every request that carried the token, answered or failed.
	requests int
	// delay holds every request before it is answered, so two processes reporting at once overlap.
	delay time.Duration
	// forks holds the pull requests whose head lives in a fork.
	forks map[int]bool
	// bots holds the account ids the forge marks as bots.
	bots map[int64]bool
	// closed holds the pull requests that are closed.
	closed map[int]bool
	// authors maps a pull request number to the numeric id of the account that opened it.
	authors map[int]int64
}

// SetAuthor records the numeric id of the account that opened pull request number.
func (f *Forge) SetAuthor(number int, id int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.authors == nil {
		f.authors = map[int]int64{}
	}
	f.authors[number] = id
}

// author returns the numeric id of the account that opened pull request number, zero when unset.
func (f *Forge) author(number int) int64 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.authors[number]
}

// SetClosed marks pull request number as closed, or open again.
func (f *Forge) SetClosed(number int, closed bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed == nil {
		f.closed = map[int]bool{}
	}
	f.closed[number] = closed
}

// state returns the provider's word for pull request number's state.
func (f *Forge) state(number int) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case f.closed[number]:
		return "closed"
	case f.Provider == "gitlab":
		return "opened"
	default:
		return "open"
	}
}

// SetFork marks pull request number as coming from a fork, or not.
func (f *Forge) SetFork(number int, fork bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.forks == nil {
		f.forks = map[int]bool{}
	}
	f.forks[number] = fork
}

// SetBot marks the account with numeric id as a bot, or not.
func (f *Forge) SetBot(id int64, bot bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.bots == nil {
		f.bots = map[int64]bool{}
	}
	f.bots[id] = bot
}

// fork reports whether pull request number comes from a fork.
func (f *Forge) fork(number int) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.forks[number]
}

// account answers an account read by numeric id with whether the forge marks it as a bot, in the
// field and vocabulary the provider uses.
func (f *Forge) account(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	f.mu.Lock()
	bot := f.bots[id]
	f.mu.Unlock()
	if f.Provider == "gitlab" {
		writeJSON(w, http.StatusOK, map[string]any{"id": id, "bot": bot})
		return
	}
	kind := "User"
	if bot {
		kind = "Bot"
	}
	writeJSON(w, http.StatusOK, map[string]any{"id": id, "type": kind})
}

// NewGitHub starts a fake GitHub Enterprise style API for repo, answering under /api/v3.
func NewGitHub(t *testing.T, token, repo string) *Forge {
	t.Helper()
	f := &Forge{Provider: "github", Token: token, Repository: repo, Login: botLogin, UserID: botID}
	mux := http.NewServeMux()
	base := "/api/v3/repos/" + repo
	mux.HandleFunc("GET /api/v3/user", f.guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"login": f.Login})
	}))
	mux.HandleFunc("GET "+base+"/issues/{a}/{b}", f.guard(f.issuesGitHub))
	mux.HandleFunc("POST "+base+"/issues/{n}/comments", f.guard(f.create))
	mux.HandleFunc("PATCH "+base+"/issues/comments/{id}", f.guard(f.update))
	mux.HandleFunc("POST "+base+"/statuses/{sha}", f.guard(f.status("context")))
	mux.HandleFunc("GET "+base+"/pulls/{n}", f.guard(f.headGitHub))
	mux.HandleFunc("GET /api/v3/user/{id}", f.guard(f.account))
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// NewGitLab starts a fake self-managed GitLab API for the project at path repo, answering under
// /api/v4.
func NewGitLab(t *testing.T, token, repo string) *Forge {
	t.Helper()
	f := &Forge{Provider: "gitlab", Token: token, Repository: repo, Login: botLogin, UserID: botID}
	mux := http.NewServeMux()
	base := "/api/v4/projects/{project}"
	mux.HandleFunc("GET /api/v4/user", f.guard(func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"id": f.UserID, "username": f.Login})
	}))
	mux.HandleFunc("GET "+base+"/merge_requests/{n}/notes", f.guard(f.project(f.listGitLab)))
	mux.HandleFunc("GET "+base+"/merge_requests/{n}/notes/{id}", f.guard(f.project(f.noteGitLab)))
	mux.HandleFunc("POST "+base+"/merge_requests/{n}/notes", f.guard(f.project(f.create)))
	mux.HandleFunc("PUT "+base+"/merge_requests/{n}/notes/{id}", f.guard(f.project(f.update)))
	mux.HandleFunc("POST "+base+"/statuses/{sha}", f.guard(f.project(f.status("name"))))
	mux.HandleFunc("GET "+base+"/merge_requests/{n}", f.guard(f.project(f.headGitLab)))
	mux.HandleFunc("GET /api/v4/users/{id}", f.guard(f.account))
	f.Server = httptest.NewTLSServer(mux)
	t.Cleanup(f.Server.Close)
	return f
}

// APIURL is the REST API base a review trigger points at.
func (f *Forge) APIURL() string {
	if f.Provider == "gitlab" {
		return f.Server.URL + "/api/v4"
	}
	return f.Server.URL + "/api/v3"
}

// Client returns an HTTP client that trusts the fake's certificate.
func (f *Forge) Client() *http.Client {
	return f.Server.Client()
}

// Seed adds a comment by author directly, standing in for one a person posted.
func (f *Forge) Seed(number int, author, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.nextID++
	f.comments = append(f.comments, &Comment{ID: f.nextID, Number: number, Author: author, Body: body})
}

// Comments returns a copy of the comments on pull request number, oldest first.
func (f *Forge) Comments(number int) []Comment {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Comment
	for _, c := range f.comments {
		if c.Number == number {
			out = append(out, *c)
		}
	}
	return out
}

// Statuses returns a copy of every status set on sha, in arrival order.
func (f *Forge) Statuses(sha string) []Status {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []Status
	for _, s := range f.statuses {
		if s.SHA == sha {
			out = append(out, s)
		}
	}
	return out
}

// Bodies returns every request body the fake received, for checking that nothing secret was sent.
func (f *Forge) Bodies() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.bodies...)
}

// Unauthorized returns how many requests carried the wrong token.
func (f *Forge) Unauthorized() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.unauthorized
}

// SetHead records sha as the commit pull request number proposes now, as a push to it does.
func (f *Forge) SetHead(number int, sha string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.heads == nil {
		f.heads = map[int]string{}
	}
	f.heads[number] = sha
}

// SetOutage makes every request answer code, as a forge that is down does. Zero brings it back.
func (f *Forge) SetOutage(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.outage = code
}

// SetWriteOutage makes every write answer code while reads still work, as a token that lost its
// write permission does. Zero brings writes back.
func (f *Forge) SetWriteOutage(code int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.writeOutage = code
}

// SetDelay holds every request for d before answering it, the latency that lets two processes
// reporting at once overlap the way they do against a real forge.
func (f *Forge) SetDelay(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.delay = d
}

// Requests returns how many requests carrying the token reached the fake, failed ones included.
func (f *Forge) Requests() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.requests
}

// failing returns the status a request with method should be failed with, zero when it should be
// served.
func (f *Forge) failing(method string) int {
	f.mu.Lock()
	delay := f.delay
	f.mu.Unlock()
	time.Sleep(delay)
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests++
	if f.outage != 0 {
		return f.outage
	}
	if f.writeOutage != 0 && method != http.MethodGet {
		return f.writeOutage
	}
	return 0
}

// guard refuses a request without the token, as the forge does, and records the body of one with
// it.
func (f *Forge) guard(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if f.Provider == "gitlab" {
			got = r.Header.Get("PRIVATE-TOKEN")
		}
		if got != f.Token {
			f.mu.Lock()
			f.unauthorized++
			f.mu.Unlock()
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Bad credentials"})
			return
		}
		var payload map[string]string
		if r.Body != nil {
			_ = json.NewDecoder(r.Body).Decode(&payload)
		}
		if payload != nil {
			raw, _ := json.Marshal(payload)
			f.mu.Lock()
			f.bodies = append(f.bodies, string(raw))
			f.mu.Unlock()
		}
		if code := f.failing(r.Method); code != 0 {
			writeJSON(w, code, map[string]string{"message": http.StatusText(code)})
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), payloadKey{}, payload)))
	}
}

// project refuses a GitLab request for any project but the configured one.
func (f *Forge) project(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if r.PathValue("project") != f.Repository {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Project Not Found"})
			return
		}
		next(w, r)
	}
}

// payloadKey is the context key guard stores the decoded request body under.
type payloadKey struct{}

// payload returns the decoded JSON body guard recorded for this request.
func payload(r *http.Request) map[string]string {
	out, _ := r.Context().Value(payloadKey{}).(map[string]string)
	return out
}

// page returns the comments on number for one page of a hundred.
func (f *Forge) page(r *http.Request) []*Comment {
	number, _ := strconv.Atoi(r.PathValue("n"))
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	page = max(page, 1)
	f.mu.Lock()
	defer f.mu.Unlock()
	var all []*Comment
	for _, c := range f.comments {
		if c.Number == number {
			cp := *c
			all = append(all, &cp)
		}
	}
	start := (page - 1) * 100
	if start >= len(all) {
		return nil
	}
	return all[start:min(start+100, len(all))]
}

// listGitHub answers a GitHub issue comment listing.
func (f *Forge) listGitHub(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, c := range f.page(r) {
		user := map[string]any{"login": c.Author}
		out = append(out, map[string]any{"id": c.ID, "body": c.Body, "user": user})
	}
	writeJSON(w, http.StatusOK, out)
}

// listGitLab answers a GitLab merge request note listing, with author ids derived from the login.
func (f *Forge) listGitLab(w http.ResponseWriter, r *http.Request) {
	out := []map[string]any{}
	for _, c := range f.page(r) {
		id := int64(7)
		if c.Author == f.Login {
			id = f.UserID
		}
		out = append(out, map[string]any{"id": c.ID, "body": c.Body, "system": false,
			"author": map[string]any{"id": id, "username": c.Author}})
	}
	writeJSON(w, http.StatusOK, out)
}

// create posts a new comment as the token's account.
func (f *Forge) create(w http.ResponseWriter, r *http.Request) {
	number, err := strconv.Atoi(r.PathValue("n"))
	body := payload(r)["body"]
	if err != nil || body == "" {
		writeJSON(w, http.StatusUnprocessableEntity, map[string]string{"message": "body is missing"})
		return
	}
	f.mu.Lock()
	f.nextID++
	c := &Comment{ID: f.nextID, Number: number, Author: f.Login, Body: body}
	f.comments = append(f.comments, c)
	f.mu.Unlock()
	writeJSON(w, http.StatusCreated, map[string]any{"id": c.ID, "body": c.Body})
}

// update edits a comment in place, refusing one the token's account did not post.
func (f *Forge) update(w http.ResponseWriter, r *http.Request) {
	id, _ := strconv.ParseInt(r.PathValue("id"), 10, 64)
	body := payload(r)["body"]
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.comments {
		if c.ID != id {
			continue
		}
		if c.Author != f.Login {
			writeJSON(w, http.StatusForbidden, map[string]string{"message": "Forbidden"})
			return
		}
		c.Body = body
		c.Edits++
		writeJSON(w, http.StatusOK, map[string]any{"id": c.ID, "body": c.Body})
		return
	}
	writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
}

// head returns the commit pull request n proposes now, or false when none was set.
func (f *Forge) head(r *http.Request) (int, string, bool) {
	number, _ := strconv.Atoi(r.PathValue("n"))
	f.mu.Lock()
	defer f.mu.Unlock()
	sha, ok := f.heads[number]
	return number, sha, ok
}

// headGitHub answers a GitHub pull request read with its head commit.
func (f *Forge) headGitHub(w http.ResponseWriter, r *http.Request) {
	number, sha, ok := f.head(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not Found"})
		return
	}
	headRepo := f.Repository
	if f.fork(number) {
		headRepo = "someone/fork-of-" + strings.ReplaceAll(f.Repository, "/", "-")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"number": number, "state": f.state(number), "user": map[string]any{"id": f.author(number)},
		"html_url": f.Server.URL + "/" + f.Repository + "/pull/" + strconv.Itoa(number),
		"head": map[string]any{"sha": sha, "ref": "feature",
			"repo": map[string]any{"full_name": headRepo}},
		"base": map[string]any{"sha": "base", "ref": "main",
			"repo": map[string]any{"full_name": f.Repository}},
	})
}

// headGitLab answers a GitLab merge request read with its head commit.
func (f *Forge) headGitLab(w http.ResponseWriter, r *http.Request) {
	number, sha, ok := f.head(r)
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "404 Not found"})
		return
	}
	source := 5
	if f.fork(number) {
		source = 9
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"iid": number, "sha": sha, "state": f.state(number), "source_branch": "feature",
		"source_project_id": source, "target_project_id": 5,
		"author":  map[string]any{"id": f.author(number)},
		"web_url": f.Server.URL + "/" + f.Repository + "/-/merge_requests/" + strconv.Itoa(number),
	})
}

// status records a commit status, reading its name from the field the provider uses.
func (f *Forge) status(nameField string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		p := payload(r)
		s := Status{
			SHA: r.PathValue("sha"), State: p["state"], Context: p[nameField],
			Description: p["description"], TargetURL: p["target_url"],
		}
		if s.State == "" || s.Context == "" {
			writeJSON(w, http.StatusUnprocessableEntity,
				map[string]string{"message": "state and name required"})
			return
		}
		f.mu.Lock()
		f.statuses = append(f.statuses, s)
		f.mu.Unlock()
		writeJSON(w, http.StatusCreated, map[string]any{"state": s.State})
	}
}

// writeJSON answers with v encoded as JSON.
func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
