package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/user"
)

// linkTestSealer is the server key every link test signs states with, derived once since the
// derivation is deliberately slow.
//
//nolint:gochecknoglobals // Not modified, simplifies testing.
var linkTestSealer = sync.OnceValue(func() *credential.Sealer {
	return credential.NewSealer("link-test-passphrase", "link-test-salt")
})

// Forge test fixtures. The login is a value the forge sends that must never be stored or recorded.
const (
	// testForgeLogin is the login the fake forges answer with.
	testForgeLogin = "octo-dev"
	// testForgeCode is the authorization code the fake forges accept.
	testForgeCode = "code-ok"
	// testForgeToken is the access token the fake forges issue.
	testForgeToken = "forge-access-token-123"
	// testForgeSecret is the OAuth client secret of every fake forge application.
	testForgeSecret = "forge-client-secret"
	// testPublicURL is the public address the handlers send people back to.
	testPublicURL = "https://switchtender.example.com"
)

// fakeForge is an httptest server playing one forge's OAuth token and account endpoints.
type fakeForge struct {
	// srv serves the endpoints.
	srv *httptest.Server
	// app is the OAuth application configured for this forge.
	app forgelink.App
	// accountID is the numeric id the account endpoint answers.
	accountID int64
	// bot makes the account endpoint answer a bot account.
	bot bool
	// exchanges counts token exchanges.
	exchanges atomic.Int32
}

// newFakeForge starts a fake forge for provider answering account id accountID, closed when the
// test ends. A GitHub forge serves its API under /api/v3, the way GitHub Enterprise Server does,
// and a GitLab forge under /api/v4.
func newFakeForge(t *testing.T, provider string, accountID int64, bot bool) *fakeForge {
	t.Helper()
	f := &fakeForge{accountID: accountID, bot: bot}
	mux := http.NewServeMux()
	tokenPath, apiPath := "/login/oauth/access_token", "/api/v3"
	if provider == "gitlab" {
		tokenPath, apiPath = "/oauth/token", "/api/v4"
	}
	mux.HandleFunc("POST "+tokenPath, func(w http.ResponseWriter, r *http.Request) {
		f.exchanges.Add(1)
		if err := r.ParseForm(); err != nil || r.Form.Get("code") != testForgeCode ||
			r.Form.Get("client_secret") != testForgeSecret ||
			r.Form.Get("redirect_uri") != testPublicURL+"/auth/forge/callback" {
			http.Error(w, `{"error":"bad_verification_code"}`, http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"access_token":%q,"token_type":"bearer"}`, testForgeToken)
	})
	mux.HandleFunc("GET "+apiPath+"/user", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer "+testForgeToken {
			http.Error(w, `{"message":"Bad credentials"}`, http.StatusUnauthorized)
			return
		}
		account := map[string]any{"id": f.accountID}
		if provider == "gitlab" {
			account["username"], account["bot"] = testForgeLogin, f.bot
		} else {
			account["login"], account["type"] = testForgeLogin, "User"
			if f.bot {
				account["type"] = "Bot"
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(account)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	f.app = forgelink.App{
		Provider: provider, WebURL: f.srv.URL,
		APIURL:   forgelink.CanonicalAPIURL(provider, f.srv.URL+apiPath),
		ClientID: provider + "-app", ClientSecret: testForgeSecret,
	}
	return f
}

// linkFailingAudits is an audit store whose appends fail.
type linkFailingAudits struct {
	// Store is the store every other call reaches.
	audit.Store
}

// Append refuses every entry.
func (linkFailingAudits) Append(context.Context, *audit.Entry) error {
	return errors.New("audit store unavailable")
}

// linkHarness wires the four forge link handlers to one store, one user store, and one trail.
type linkHarness struct {
	// links holds the links.
	links forgelink.Store
	// users holds the accounts.
	users user.Store
	// audits is the trail the handlers record to.
	audits audit.Store
	// apps are the configured forges.
	apps []forgelink.App
	// sealer is the server key the link states are signed with.
	sealer *credential.Sealer
	// start, callback, list, and remove are the handlers under test.
	start, callback, list, remove http.HandlerFunc
}

// newLinkHarness builds the handlers for apps over fresh stores, with two accounts, usr_a and
// usr_b.
func newLinkHarness(t *testing.T, audits audit.Store, apps ...forgelink.App) *linkHarness {
	t.Helper()
	h := &linkHarness{links: forgelink.NewMemStore(), users: user.NewMemStore(), audits: audits,
		apps: apps, sealer: linkTestSealer()}
	for _, id := range []string{"usr_a", "usr_b"} {
		if err := h.users.Save(context.Background(), &user.User{ID: id, Username: "account-" + id,
			Role: user.RoleViewer, CreatedAt: time.Now()}); err != nil {
			t.Fatalf("Save(%s) error = %v", id, err)
		}
	}
	log := zap.NewNop()
	h.start = forgeLinkStartHandler(h.links, apps, h.sealer, testPublicURL, log)
	h.callback = forgeLinkCallbackHandler(h.links, apps, h.users, audits, h.sealer,
		http.DefaultClient, testPublicURL, log)
	h.list = forgeLinksListHandler(h.links, apps, testPublicURL, log)
	h.remove = forgeLinkDeleteHandler(h.links, audits, log)
	return h
}

// personActor is a signed-in person with account id.
func personActor(id string) Actor {
	return Actor{UserID: id, Role: user.RoleViewer, Name: "account-" + id, Type: actorTypeSession}
}

// asActor returns req carrying actor as the authenticated caller.
func asActor(req *http.Request, actor Actor) *http.Request {
	return req.WithContext(context.WithValue(req.Context(), actorKey{}, actor))
}

// begin starts a link for actor on app and returns the answer, the state the authorize address
// carries, and the browser cookie the answer set.
func (h *linkHarness) begin(t *testing.T, actor Actor, app forgelink.App) (*httptest.ResponseRecorder,
	string, *http.Cookie) {
	t.Helper()
	body := fmt.Sprintf(`{"provider":%q,"api_url":%q}`, app.Provider, app.APIURL)
	rec := httptest.NewRecorder()
	h.start(rec, asActor(httptest.NewRequest(http.MethodPost, "/v1/me/forge-links",
		strings.NewReader(body)), actor))
	if rec.Code != http.StatusOK {
		return rec, "", nil
	}
	var out struct {
		// AuthorizeURL is where the browser goes next.
		AuthorizeURL string `json:"authorize_url"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode the start answer: %v", err)
	}
	u, err := url.Parse(out.AuthorizeURL)
	if err != nil {
		t.Fatalf("parse authorize_url %q: %v", out.AuthorizeURL, err)
	}
	var cookie *http.Cookie
	for _, c := range rec.Result().Cookies() {
		if c.Name == forgeLinkCookie {
			cookie = c
		}
	}
	return rec, u.Query().Get("state"), cookie
}

// finish sends the browser back to the callback with state, code, and cookie, and returns where it
// was redirected.
func (h *linkHarness) finish(t *testing.T, state, code string, cookie *http.Cookie) url.Values {
	t.Helper()
	q := url.Values{"state": {state}, "code": {code}}
	req := httptest.NewRequest(http.MethodGet, "/auth/forge/callback?"+q.Encode(), nil)
	if cookie != nil {
		req.AddCookie(&http.Cookie{Name: cookie.Name, Value: cookie.Value})
	}
	rec := httptest.NewRecorder()
	h.callback(rec, req)
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want %d", rec.Code, http.StatusFound)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil || loc.Path != forgeLinksPage {
		t.Fatalf("callback redirected to %q, want %s", rec.Header().Get("Location"), forgeLinksPage)
	}
	frag, err := url.ParseQuery(loc.Fragment)
	if err != nil {
		t.Fatalf("parse the redirect fragment: %v", err)
	}
	return frag
}

// linkView is what a test compares about a stored link.
type linkView struct {
	// UserID is the SwitchTender account.
	UserID string
	// Provider is the forge's provider.
	Provider string
	// APIURL is the forge's API base.
	APIURL string
	// ForgeUserID is the forge's numeric id.
	ForgeUserID int64
}

// viewsOf reduces links to what a test compares.
func viewsOf(list []*forgelink.Link) []linkView {
	out := []linkView{}
	for _, l := range list {
		out = append(out, linkView{UserID: l.UserID, Provider: l.Provider, APIURL: l.APIURL,
			ForgeUserID: l.ForgeUserID})
	}
	return out
}

// TestForgeLinkLinksTheNumericID links a GitHub Enterprise Server and a GitLab account through each
// forge's OAuth flow, stores the forge's numeric id and nothing else about the account, and records
// the link on the chain without the login or the token.
func TestForgeLinkLinksTheNumericID(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider    string
		AccountID   int64
		WantLinked  string
		WantLinks   int
		WantEntries int
	}{{ // Test 0: A GitHub Enterprise Server account.
		Provider: "github", AccountID: 4242, WantLinked: "github", WantLinks: 1, WantEntries: 1,
	}, { // Test 1: A self-managed GitLab account.
		Provider: "gitlab", AccountID: 77, WantLinked: "gitlab", WantLinks: 1, WantEntries: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			forge := newFakeForge(t, test.Provider, test.AccountID, false)
			trail := audit.NewMemStore()
			h := newLinkHarness(t, trail, forge.app)
			_, state, cookie := h.begin(t, personActor("usr_a"), forge.app)
			if state == "" || cookie == nil {
				t.Fatalf("begin() state %q cookie %v, want both", state, cookie)
			}
			if !cookie.HttpOnly || cookie.Path != forgeLinkCookiePath || !cookie.Secure ||
				cookie.SameSite != http.SameSiteLaxMode {
				t.Errorf("cookie = %+v, want HttpOnly, Secure, SameSite=Lax, path %s", cookie,
					forgeLinkCookiePath)
			}
			frag := h.finish(t, state, testForgeCode, cookie)
			if got := frag.Get("linked"); got != test.WantLinked {
				t.Fatalf("callback linked = %q (error %q), want %q", got, frag.Get("error"),
					test.WantLinked)
			}
			got, err := h.links.ForUser(context.Background(), "usr_a")
			if err != nil {
				t.Fatalf("ForUser() error = %v", err)
			}
			want := []linkView{{UserID: "usr_a", Provider: test.Provider, APIURL: forge.app.APIURL,
				ForgeUserID: test.AccountID}}
			if diff := cmp.Diff(want, viewsOf(got), cmpopts.EquateEmpty()); diff != "" {
				t.Errorf("stored links mismatch (-want +got):\n%s", diff)
			}
			stored, _ := json.Marshal(got)
			entries, err := trail.Chain(context.Background())
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(entries) != test.WantEntries {
				t.Fatalf("chain holds %d entries, want %d", len(entries), test.WantEntries)
			}
			entry, _ := json.Marshal(entries[0])
			for _, leak := range []string{testForgeLogin, testForgeToken, testForgeSecret} {
				if strings.Contains(string(stored), leak) || strings.Contains(string(entry), leak) {
					t.Errorf("%q reached the store or the chain: %s %s", leak, stored, entry)
				}
			}
			wantPath := "/me/forge-links/" + got[0].ID + "/linked"
			if entries[0].Path != wantPath || entries[0].ContentDigest == "" ||
				entries[0].Actor != "account-usr_a" {
				t.Errorf("chain entry = %s, want path %s with a content digest by account-usr_a",
					entry, wantPath)
			}
		})
	}
}

// TestForgeLinkCallbackRefusals refuses a callback that cannot be tied to the browser and account
// that started it, or to a person at the forge, and links nothing for it.
func TestForgeLinkCallbackRefusals(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Provider      string
		Bot           bool
		Tamper        func(state string, cookie *http.Cookie) (string, *http.Cookie)
		Code          string
		WantError     string
		WantExchanges int32
	}{{ // Test 0: A state whose signature does not verify.
		Provider: "github", Code: testForgeCode,
		Tamper: func(state string, cookie *http.Cookie) (string, *http.Cookie) {
			payload, sig, _ := strings.Cut(state, ".")
			return payload + "." + strings.Repeat("A", len(sig)), cookie
		},
		WantError: "not valid",
	}, { // Test 1: A state re-signed under another account is refused by its signature.
		Provider: "github", Code: testForgeCode,
		Tamper: func(state string, cookie *http.Cookie) (string, *http.Cookie) {
			return forgedState(state, func(st *forgeLinkState) { st.UserID = "usr_b" }), cookie
		},
		WantError: "not valid",
	}, { // Test 2: A link URL opened in a browser that never started it carries no cookie.
		Provider: "github", Code: testForgeCode,
		Tamper: func(state string, _ *http.Cookie) (string, *http.Cookie) {
			return state, nil
		},
		WantError: "another browser",
	}, { // Test 3: A cookie from another link start does not match the state.
		Provider: "gitlab", Code: testForgeCode,
		Tamper: func(state string, _ *http.Cookie) (string, *http.Cookie) {
			return state, &http.Cookie{Name: forgeLinkCookie, Value: "another-nonce"}
		},
		WantError: "another browser",
	}, { // Test 4: A GitHub bot account is refused.
		Provider: "github", Bot: true, Code: testForgeCode, WantError: "bot", WantExchanges: 1,
	}, { // Test 5: A GitLab bot user is refused.
		Provider: "gitlab", Bot: true, Code: testForgeCode, WantError: "bot", WantExchanges: 1,
	}, { // Test 6: A code the forge does not accept links nothing.
		Provider: "gitlab", Code: "code-wrong", WantError: "did not confirm", WantExchanges: 1,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			forge := newFakeForge(t, test.Provider, 9001, test.Bot)
			h := newLinkHarness(t, audit.NewMemStore(), forge.app)
			_, state, cookie := h.begin(t, personActor("usr_a"), forge.app)
			if test.Tamper != nil {
				state, cookie = test.Tamper(state, cookie)
			}
			frag := h.finish(t, state, test.Code, cookie)
			if got := frag.Get("error"); !strings.Contains(got, test.WantError) {
				t.Errorf("callback error = %q, want it to say %q", got, test.WantError)
			}
			if got := forge.exchanges.Load(); got != test.WantExchanges {
				t.Errorf("token exchanges = %d, want %d", got, test.WantExchanges)
			}
			for _, id := range []string{"usr_a", "usr_b"} {
				got, err := h.links.ForUser(context.Background(), id)
				if err != nil || len(got) != 0 {
					t.Errorf("ForUser(%s) = %d links, %v, want none", id, len(got), err)
				}
			}
		})
	}
}

// forgedState returns state with its payload changed by edit and its signature left as it was.
func forgedState(state string, edit func(*forgeLinkState)) string {
	payload, sig, _ := strings.Cut(state, ".")
	raw, _ := base64.RawURLEncoding.DecodeString(payload)
	var st forgeLinkState
	_ = json.Unmarshal(raw, &st)
	edit(&st)
	changed, _ := json.Marshal(st)
	return base64.RawURLEncoding.EncodeToString(changed) + "." + sig
}

// TestForgeLinkStateExpires refuses a state past its ten minutes, even with its own cookie.
func TestForgeLinkStateExpires(t *testing.T) {
	t.Parallel()
	forge := newFakeForge(t, "github", 4242, false)
	h := newLinkHarness(t, audit.NewMemStore(), forge.app)
	expired, err := signForgeState(h.sealer, forge.app, forgeLinkState{UserID: "usr_a",
		Provider: "github", APIURL: forge.app.APIURL, Nonce: "nonce-1",
		Expires: time.Now().Add(-time.Second).Unix()})
	if err != nil {
		t.Fatalf("signForgeState() error = %v", err)
	}
	frag := h.finish(t, expired, testForgeCode, &http.Cookie{Name: forgeLinkCookie, Value: "nonce-1"})
	if got := frag.Get("error"); !strings.Contains(got, "expired") {
		t.Errorf("callback error = %q, want an expired state refused", got)
	}
	if got := forge.exchanges.Load(); got != 0 {
		t.Errorf("token exchanges = %d, want none for an expired state", got)
	}
}

// TestForgeLinkOneAccountOneUser refuses a second SwitchTender account linking a forge account that
// is already linked, and keeps the first link.
func TestForgeLinkOneAccountOneUser(t *testing.T) {
	t.Parallel()
	forge := newFakeForge(t, "github", 4242, false)
	h := newLinkHarness(t, audit.NewMemStore(), forge.app)
	_, state, cookie := h.begin(t, personActor("usr_a"), forge.app)
	if frag := h.finish(t, state, testForgeCode, cookie); frag.Get("linked") != "github" {
		t.Fatalf("first link failed: %q", frag.Get("error"))
	}
	_, state, cookie = h.begin(t, personActor("usr_b"), forge.app)
	frag := h.finish(t, state, testForgeCode, cookie)
	if got := frag.Get("error"); !strings.Contains(got, "already linked") {
		t.Errorf("second link error = %q, want already linked", got)
	}
	a, _ := h.links.ForUser(context.Background(), "usr_a")
	b, _ := h.links.ForUser(context.Background(), "usr_b")
	if len(a) != 1 || len(b) != 0 {
		t.Errorf("links: usr_a %d, usr_b %d, want 1 and 0", len(a), len(b))
	}
}

// TestForgeLinkStartRefusals refuses to start a link for a caller who is not a signed-in person, an
// agent, a forge this server has no application for, and a server with no public address.
func TestForgeLinkStartRefusals(t *testing.T) {
	t.Parallel()
	forge := newFakeForge(t, "github", 4242, false)
	agent := personActor("usr_a")
	agent.Agent, agent.Type = true, actorTypeAgent
	tests := []struct {
		Actor      *Actor
		Body       string
		PublicURL  string
		WantStatus int
	}{{ // Test 0: An agent token is refused even though its account could link.
		Actor: &agent, Body: `{"provider":"github","api_url":"` + forge.app.APIURL + `"}`,
		PublicURL: testPublicURL, WantStatus: http.StatusForbidden,
	}, { // Test 1: A caller with no account is refused.
		Body:      `{"provider":"github","api_url":"` + forge.app.APIURL + `"}`,
		PublicURL: testPublicURL, WantStatus: http.StatusUnauthorized,
	}, { // Test 2: A forge with no configured application is refused.
		Actor: ptrActor(personActor("usr_a")), Body: `{"provider":"gitlab"}`,
		PublicURL: testPublicURL, WantStatus: http.StatusNotFound,
	}, { // Test 3: The public GitHub service is not this GitHub Enterprise Server.
		Actor: ptrActor(personActor("usr_a")), Body: `{"provider":"github"}`,
		PublicURL: testPublicURL, WantStatus: http.StatusNotFound,
	}, { // Test 4: Without a public address the forge could not send anybody back.
		Actor:      ptrActor(personActor("usr_a")),
		Body:       `{"provider":"github","api_url":"` + forge.app.APIURL + `"}`,
		WantStatus: http.StatusConflict,
	}, { // Test 5: A configured forge starts, the control for the refusals above.
		Actor: ptrActor(personActor("usr_a")),
		Body:  `{"provider":"github","api_url":"` + forge.app.APIURL + `/"}`, PublicURL: testPublicURL,
		WantStatus: http.StatusOK,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			start := forgeLinkStartHandler(forgelink.NewMemStore(), []forgelink.App{forge.app},
				linkTestSealer(), test.PublicURL, zap.NewNop())
			req := httptest.NewRequest(http.MethodPost, "/v1/me/forge-links",
				strings.NewReader(test.Body))
			if test.Actor != nil {
				req = asActor(req, *test.Actor)
			}
			rec := httptest.NewRecorder()
			start(rec, req)
			if rec.Code != test.WantStatus {
				t.Errorf("status = %d, want %d: %s", rec.Code, test.WantStatus, rec.Body)
			}
		})
	}
}

// ptrActor returns a pointer to a copy of a.
func ptrActor(a Actor) *Actor {
	return &a
}

// TestForgeLinkUnlink removes the caller's own link and records it, refuses removing another
// account's link, and refuses an agent.
func TestForgeLinkUnlink(t *testing.T) {
	t.Parallel()
	forge := newFakeForge(t, "gitlab", 77, false)
	trail := audit.NewMemStore()
	h := newLinkHarness(t, trail, forge.app)
	_, state, cookie := h.begin(t, personActor("usr_a"), forge.app)
	if frag := h.finish(t, state, testForgeCode, cookie); frag.Get("linked") != "gitlab" {
		t.Fatalf("link failed: %q", frag.Get("error"))
	}
	mine, _ := h.links.ForUser(context.Background(), "usr_a")
	id := mine[0].ID
	agent := personActor("usr_a")
	agent.Agent, agent.Type = true, actorTypeAgent
	remove := func(actor Actor) int {
		req := httptest.NewRequest(http.MethodDelete, "/v1/me/forge-links/"+id, nil)
		req.SetPathValue("id", id)
		rec := httptest.NewRecorder()
		h.remove(rec, asActor(req, actor))
		return rec.Code
	}
	if got := remove(personActor("usr_b")); got != http.StatusNotFound {
		t.Errorf("another account's unlink status = %d, want %d", got, http.StatusNotFound)
	}
	if got := remove(agent); got != http.StatusForbidden {
		t.Errorf("agent unlink status = %d, want %d", got, http.StatusForbidden)
	}
	if left, _ := h.links.ForUser(context.Background(), "usr_a"); len(left) != 1 {
		t.Fatalf("refused unlinks removed the link: %d left", len(left))
	}
	if got := remove(personActor("usr_a")); got != http.StatusOK {
		t.Fatalf("own unlink status = %d, want %d", got, http.StatusOK)
	}
	if left, _ := h.links.ForUser(context.Background(), "usr_a"); len(left) != 0 {
		t.Errorf("own unlink left %d links", len(left))
	}
	entries, _ := trail.Chain(context.Background())
	paths := []string{}
	for _, e := range entries {
		paths = append(paths, e.Path)
	}
	want := []string{"/me/forge-links/" + id + "/linked", "/me/forge-links/" + id + "/unlinked"}
	if diff := cmp.Diff(want, paths); diff != "" {
		t.Errorf("chain paths mismatch (-want +got):\n%s", diff)
	}
}

// TestForgeLinkFailsClosedOnTheChain keeps no link the chain did not record, and puts back a link
// whose unlink the chain did not record.
func TestForgeLinkFailsClosedOnTheChain(t *testing.T) {
	t.Parallel()
	forge := newFakeForge(t, "github", 4242, false)
	h := newLinkHarness(t, linkFailingAudits{Store: audit.NewMemStore()}, forge.app)
	_, state, cookie := h.begin(t, personActor("usr_a"), forge.app)
	frag := h.finish(t, state, testForgeCode, cookie)
	if got := frag.Get("error"); !strings.Contains(got, "audit trail") {
		t.Errorf("callback error = %q, want the unrecorded link refused", got)
	}
	if left, _ := h.links.ForUser(context.Background(), "usr_a"); len(left) != 0 {
		t.Errorf("an unrecorded link was kept: %d links", len(left))
	}
	kept := &forgelink.Link{ID: "fl_kept", UserID: "usr_a", Provider: "github",
		APIURL: forge.app.APIURL, ForgeUserID: 4242, CreatedAt: time.Now()}
	if err := h.links.Create(context.Background(), kept); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/me/forge-links/fl_kept", nil)
	req.SetPathValue("id", "fl_kept")
	rec := httptest.NewRecorder()
	h.remove(rec, asActor(req, personActor("usr_a")))
	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("unrecorded unlink status = %d, want %d", rec.Code, http.StatusServiceUnavailable)
	}
	if left, _ := h.links.ForUser(context.Background(), "usr_a"); len(left) != 1 {
		t.Errorf("an unlink the chain did not record removed the link: %d left", len(left))
	}
}

// TestForgeLinksList answers the caller's own links and the configured forges, and the address the
// forge applications send people back to.
func TestForgeLinksList(t *testing.T) {
	t.Parallel()
	forge := newFakeForge(t, "github", 4242, false)
	h := newLinkHarness(t, audit.NewMemStore(), forge.app)
	if err := h.links.Create(context.Background(), &forgelink.Link{ID: "fl_b", UserID: "usr_b",
		Provider: "github", APIURL: forge.app.APIURL, ForgeUserID: 7,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	rec := httptest.NewRecorder()
	h.list(rec, asActor(httptest.NewRequest(http.MethodGet, "/v1/me/forge-links", nil),
		personActor("usr_a")))
	var got forgeLinksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body)
	}
	host := strings.TrimPrefix(forge.srv.URL, "http://")
	want := forgeLinksResponse{
		Forges: []forgeView{{Provider: "github", APIURL: forge.app.APIURL, WebURL: forge.srv.URL,
			Host: host}},
		CallbackURL: testPublicURL + "/auth/forge/callback",
	}
	if diff := cmp.Diff(want, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("list mismatch (-want +got):\n%s", diff)
	}
}

// TestForgeLinksListWithoutLinking proves a server with no linking set up answers that there is
// nothing to link and nothing linked, so the linked accounts page explains how linking is set up
// instead of showing a failure.
func TestForgeLinksListWithoutLinking(t *testing.T) {
	t.Parallel()
	rec := httptest.NewRecorder()
	forgeLinksListHandler(nil, nil, testPublicURL, zap.NewNop())(rec,
		httptest.NewRequest(http.MethodGet, "/v1/me/forge-links", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body %s", rec.Code, rec.Body.String())
	}
	var got forgeLinksResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v: %s", err, rec.Body)
	}
	if diff := cmp.Diff(forgeLinksResponse{}, got, cmpopts.EquateEmpty()); diff != "" {
		t.Errorf("list mismatch (-want +got):\n%s", diff)
	}
}

// TestForgeLinkGate serves the callback without a token, lets every signed-in role manage its own
// links, and leaves the rest of /auth/forge protected.
func TestForgeLinkGate(t *testing.T) {
	t.Parallel()
	g := &authGate{}
	tests := []struct {
		Method        string
		Path          string
		WantProtected bool
		WantRole      user.Role
	}{{ // Test 0: The forge sends the browser back with no token.
		Method: http.MethodGet, Path: "/auth/forge/callback", WantProtected: false,
		WantRole: user.RoleViewer,
	}, { // Test 1: A post to the callback stays protected.
		Method: http.MethodPost, Path: "/auth/forge/callback", WantProtected: true,
		WantRole: user.RoleAdmin,
	}, { // Test 2: Starting a link is open to every signed-in role.
		Method: http.MethodPost, Path: "/v1/me/forge-links", WantProtected: true,
		WantRole: user.RoleViewer,
	}, { // Test 3: Unlinking is open to every signed-in role.
		Method: http.MethodDelete, Path: "/v1/me/forge-links/fl_1", WantProtected: true,
		WantRole: user.RoleViewer,
	}, { // Test 4: Listing is open to every signed-in role.
		Method: http.MethodGet, Path: "/v1/me/forge-links", WantProtected: true,
		WantRole: user.RoleViewer,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			req := httptest.NewRequest(test.Method, test.Path, nil)
			if got := g.protects(req); got != test.WantProtected {
				t.Errorf("protects(%s %s) = %v, want %v", test.Method, test.Path, got,
					test.WantProtected)
			}
			if got := requiredRole(req); got != test.WantRole {
				t.Errorf("requiredRole(%s %s) = %s, want %s", test.Method, test.Path, got,
					test.WantRole)
			}
		})
	}
}

// TestForgeLinkStateNeedsTheServerKey proves a link state verifies only when it was signed with the
// server's own key together with the application's client secret. Somebody holding the client
// secret alone, which the forge's administrators can read, or another server's key, cannot mint a
// state for an account, and a server with no key cannot start a link at all.
func TestForgeLinkStateNeedsTheServerKey(t *testing.T) {
	t.Parallel()
	app := forgelink.App{Provider: "github", WebURL: "https://github.example.com",
		APIURL: "https://github.example.com/api/v3", ClientID: "client-1",
		ClientSecret: "client-secret-1"}
	// The state expires an hour out, well past any wait a loaded test run puts between building it
	// and a parallel subtest reading it. Expiry has its own test.
	st := forgeLinkState{UserID: "usr_a", Provider: app.Provider, APIURL: app.APIURL,
		Nonce: "nonce-1", Expires: time.Now().Add(time.Hour).Unix()}
	payload, err := json.Marshal(st)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	clientOnly := func() string {
		key := sha256.Sum256([]byte("switchtender-forge-link\x00" + app.ClientSecret))
		mac := hmac.New(sha256.New, key[:])
		mac.Write(payload)
		return base64.RawURLEncoding.EncodeToString(payload) + "." +
			base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
	}
	signed := func(sealer *credential.Sealer) string {
		state, err := signForgeState(sealer, app, st)
		if err != nil {
			t.Fatalf("signForgeState() error = %v", err)
		}
		return state
	}
	tests := []struct {
		State    string
		Verifier *credential.Sealer
		WantOK   bool
	}{{ // Test 0: Signed with the client secret alone.
		State: clientOnly(), Verifier: linkTestSealer(),
	}, { // Test 1: Signed by another server's key with the same client secret.
		State:    signed(credential.NewSealer("another-passphrase", "another-salt")),
		Verifier: linkTestSealer(),
	}, { // Test 2: A server with no key verifies nothing.
		State: signed(linkTestSealer()), Verifier: credential.NewSealer("", ""),
	}, { // Test 3: Signed by this server's key and the client secret, the control.
		State: signed(linkTestSealer()), Verifier: linkTestSealer(), WantOK: true,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, got, err := openForgeState(test.Verifier, []forgelink.App{app}, test.State,
				time.Now())
			if ok := err == nil; ok != test.WantOK {
				t.Fatalf("openForgeState() error = %v, want ok %v", err, test.WantOK)
			}
			if test.WantOK && got.UserID != "usr_a" {
				t.Errorf("state names %q, want usr_a", got.UserID)
			}
		})
	}

	start := forgeLinkStartHandler(forgelink.NewMemStore(), []forgelink.App{app},
		credential.NewSealer("", ""), testPublicURL, zap.NewNop())
	req := asActor(httptest.NewRequest(http.MethodPost, "/v1/me/forge-links", strings.NewReader(
		`{"provider":"github","api_url":"`+app.APIURL+`"}`)), personActor("usr_a"))
	rec := httptest.NewRecorder()
	start(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "encryption key") {
		t.Errorf("start without a server key = %d %s, want 409 naming the encryption key",
			rec.Code, rec.Body)
	}
}

// writeCountingLinks is a link store that counts the writes made to it.
type writeCountingLinks struct {
	forgelink.Store
	// creates counts Create calls.
	creates int
	// deletes counts Delete calls.
	deletes int
}

// Create counts the call and stores the link.
func (s *writeCountingLinks) Create(ctx context.Context, l *forgelink.Link) error {
	s.creates++
	return s.Store.Create(ctx, l)
}

// Delete counts the call and removes the link.
func (s *writeCountingLinks) Delete(ctx context.Context, userID, id string) (*forgelink.Link, error) {
	s.deletes++
	return s.Store.Delete(ctx, userID, id)
}

// TestForgeLinkNeverExistsWithoutItsEntry proves a link and its chain entry are written as one: when
// the chain refuses the entry, the link is never stored at all, not stored and then rolled back, and
// an unlink the chain refuses never removes the link.
func TestForgeLinkNeverExistsWithoutItsEntry(t *testing.T) {
	t.Parallel()
	forge := newFakeForge(t, "github", 4343, false)
	h := newLinkHarness(t, linkFailingAudits{Store: audit.NewMemStore()}, forge.app)
	counted := &writeCountingLinks{Store: h.links}
	log := zap.NewNop()
	h.callback = forgeLinkCallbackHandler(counted, h.apps, h.users, h.audits, h.sealer,
		http.DefaultClient, testPublicURL, log)
	h.remove = forgeLinkDeleteHandler(counted, h.audits, log)
	_, state, cookie := h.begin(t, personActor("usr_a"), forge.app)
	h.finish(t, state, testForgeCode, cookie)
	if counted.creates != 0 {
		t.Errorf("link store writes = %d, want none for a link the chain did not record",
			counted.creates)
	}
	if err := h.links.Create(context.Background(), &forgelink.Link{ID: "fl_stays",
		UserID: "usr_a", Provider: "github", APIURL: forge.app.APIURL, ForgeUserID: 4343,
		CreatedAt: time.Now()}); err != nil {
		t.Fatalf("Create() error = %v", err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/v1/me/forge-links/fl_stays", nil)
	req.SetPathValue("id", "fl_stays")
	rec := httptest.NewRecorder()
	h.remove(rec, asActor(req, personActor("usr_a")))
	if rec.Code != http.StatusServiceUnavailable || counted.deletes != 0 {
		t.Errorf("unrecorded unlink = %d with %d removals, want 503 and none", rec.Code,
			counted.deletes)
	}
}
