package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"github.com/kordloom/switchtender/internal/audit"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/forgelink"
	"github.com/kordloom/switchtender/internal/safedial"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// Forge account linking bounds and names.
const (
	// forgeLinkTTL bounds how long a link may take from the redirect to the forge to the callback.
	forgeLinkTTL = 10 * time.Minute
	// forgeLinkCookie names the short-lived cookie that binds a link's state to the browser that
	// started it.
	forgeLinkCookie = "st_forgelink"
	// forgeLinkCookiePath scopes that cookie to the callback, the one place that reads it.
	forgeLinkCookiePath = "/auth/forge"
	// forgeCallbackPath is where a forge sends the browser back after the person authorizes.
	forgeCallbackPath = "/auth/forge/callback"
	// forgeLinksPage is the page that lists a person's links and where every callback lands.
	forgeLinksPage = "/ui/links"
	// forgeRequestTimeout bounds each request the callback makes to the forge.
	forgeRequestTimeout = 30 * time.Second
	// maxForgeUserBody bounds how much of the forge's account answer is read.
	maxForgeUserBody = 1 << 20
)

// forgeView is one forge a person can link an account on, as the API shows it.
type forgeView struct {
	// Provider is github or gitlab.
	Provider string `json:"provider"`
	// APIURL is the forge's REST API base, which a link and a review trigger name it by.
	APIURL string `json:"api_url"`
	// WebURL is the forge's web address.
	WebURL string `json:"web_url"`
	// Host is the forge's host name, for display.
	Host string `json:"host"`
}

// forgeLinksResponse is what GET /v1/me/forge-links answers.
type forgeLinksResponse struct {
	// Forges are the forges this server can link an account on.
	Forges []forgeView `json:"forges"`
	// Links are the signed-in account's links, oldest first.
	Links []*forgelink.Link `json:"links"`
	// CallbackURL is the address each forge's OAuth application must send people back to, empty
	// when the server has no public address and so cannot link anything.
	CallbackURL string `json:"callback_url,omitempty"`
}

// forgeLinkRequest is the body of POST /v1/me/forge-links.
type forgeLinkRequest struct {
	// Provider is github or gitlab.
	Provider string `json:"provider"`
	// APIURL is the forge's REST API base, empty for the provider's public service.
	APIURL string `json:"api_url,omitempty"`
}

// forgeLinkState is what the state parameter carries across the forge's sign-in. It is signed, so
// it is trusted only as far as the signature, and it names the account the link is for, so the
// callback needs no session of its own and any replica can finish what another started.
type forgeLinkState struct {
	// UserID is the SwitchTender account the link is for.
	UserID string `json:"u"`
	// ActorType is how the person who started the link authenticated, recorded on the link's
	// chain entry.
	ActorType string `json:"t,omitempty"`
	// Provider is the forge's provider.
	Provider string `json:"p"`
	// APIURL is the forge's REST API base in canonical form.
	APIURL string `json:"a"`
	// Nonce matches the cookie set on the browser that started the link.
	Nonce string `json:"n"`
	// Expires is the Unix time after which the state is refused.
	Expires int64 `json:"e"`
}

// forgeAccount is the part of the forge's answer about the signed-in account a link reads: the
// numeric id and whether the account is a bot. The login is never read.
type forgeAccount struct {
	// ID is the forge's numeric id of the account.
	ID int64 `json:"id"`
	// Type is the GitHub account type, User or Bot.
	Type string `json:"type"`
	// Bot reports a GitLab bot user, such as a project or group access token's account.
	Bot bool `json:"bot"`
}

// errForgeBot is returned when the account signed in at the forge is a bot, which may never act
// as a person.
var errForgeBot = errors.New("a bot account cannot be linked")

// forgeLinksListHandler lists the signed-in account's forge links and the forges it can link an
// account on. A server with no linking set up answers that there are none of either, which is the
// truth and what the linked accounts page explains, rather than an error that reads as a fault.
func forgeLinksListHandler(links forgelink.Store, apps []forgelink.App, publicURL string,
	log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if links == nil {
			respondJSON(w, log, http.StatusOK, forgeLinksResponse{Forges: []forgeView{},
				Links: []*forgelink.Link{}}, wantsPretty(r))
			return
		}
		actor, ok := linkingActor(w, r, log, false)
		if !ok {
			return
		}
		mine, err := links.ForUser(r.Context(), actor.UserID)
		if err != nil {
			log.Error("server: list forge links: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not read your linked accounts")
			return
		}
		resp := forgeLinksResponse{Forges: forgeViews(apps), Links: mine}
		if callback := forgeCallbackURL(publicURL); callback != "" {
			resp.CallbackURL = callback
		}
		respondJSON(w, log, http.StatusOK, resp, wantsPretty(r))
	}
}

// forgeLinkStartHandler starts linking a forge account to the signed-in account: it answers the
// forge address the browser goes to next, carrying a signed state, and sets the cookie that binds
// that state to this browser.
func forgeLinkStartHandler(links forgelink.Store, apps []forgelink.App, sealer *credential.Sealer,
	publicURL string, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if links == nil {
			respondError(w, log, http.StatusNotFound, "forge account linking is not enabled")
			return
		}
		actor, ok := linkingActor(w, r, log, true)
		if !ok {
			return
		}
		var req forgeLinkRequest
		if !decodeStrict(w, log, r.Body, &req) {
			return
		}
		app, found := findForgeApp(apps, req.Provider, req.APIURL)
		if !found {
			respondError(w, log, http.StatusNotFound, "this forge is not set up for linking on this "+
				"server: an administrator adds it with --forge-oauth")
			return
		}
		callback := forgeCallbackURL(publicURL)
		if callback == "" {
			respondError(w, log, http.StatusConflict, "linking needs the server's public address, so "+
				"the forge can send you back: an administrator sets --public-url")
			return
		}
		if sealer == nil || !sealer.Enabled() {
			respondError(w, log, http.StatusConflict, "linking needs the server's encryption key, "+
				"which signs each link request: an administrator sets SWITCHTENDER_ENCRYPTION_KEY")
			return
		}
		nonce, err := randToken()
		if err != nil {
			log.Error("server: forge link nonce: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not start linking")
			return
		}
		state, err := signForgeState(sealer, app, forgeLinkState{
			UserID: actor.UserID, ActorType: actor.Type, Provider: app.Provider, APIURL: app.APIURL,
			Nonce: nonce, Expires: time.Now().Add(forgeLinkTTL).Unix(),
		})
		if err != nil {
			log.Error("server: sign forge link state: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not start linking")
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name: forgeLinkCookie, Value: nonce, Path: forgeLinkCookiePath, HttpOnly: true,
			Secure:   strings.HasPrefix(strings.ToLower(callback), "https://"),
			SameSite: http.SameSiteLaxMode, MaxAge: int(forgeLinkTTL.Seconds()),
		})
		respondJSON(w, log, http.StatusOK, map[string]string{
			"authorize_url": forgeOAuthConfig(app, callback).AuthCodeURL(state,
				forgeAuthParams(app)...),
		}, wantsPretty(r))
	}
}

// forgeLinkDeleteHandler removes one of the signed-in account's forge links. The unlink is recorded
// on the audit chain, and a link whose unlink cannot be recorded is put back and the request
// refused, so the chain never stops showing a link that still acts.
func forgeLinkDeleteHandler(links forgelink.Store, audits audit.Store, log *zap.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if links == nil {
			respondError(w, log, http.StatusNotFound, "forge account linking is not enabled")
			return
		}
		actor, ok := linkingActor(w, r, log, true)
		if !ok {
			return
		}
		mine, err := links.ForUser(r.Context(), actor.UserID)
		if err != nil {
			log.Error("server: unlink forge account: " + err.Error())
			respondError(w, log, http.StatusInternalServerError, "could not unlink the account")
			return
		}
		var target *forgelink.Link
		for _, l := range mine {
			if l.ID == r.PathValue("id") {
				target = l
			}
		}
		if target == nil {
			respondError(w, log, http.StatusNotFound, "no such linked account on your account")
			return
		}
		who, _ := recordedFrom(r.Context())
		name, typ := who.Name, who.Type
		if name == "" {
			name, typ = actor.Name, actor.Type
		}
		// The unlink is recorded before the link is removed, so a link is never gone without the
		// entry that records its end, and one the chain will not take is kept.
		if err := recordForgeLink(r.Context(), audits, "unlinked", target, name, typ); err != nil {
			log.Error("server: record forge unlink: " + err.Error())
			respondError(w, log, http.StatusServiceUnavailable,
				"refused: the unlink could not be recorded in the audit trail, so the link was kept")
			return
		}
		removed, err := links.Delete(r.Context(), actor.UserID, target.ID)
		if err != nil {
			// The chain now says the link ended while it is still here, so it says it holds again.
			log.Error("server: unlink forge account: " + err.Error())
			if rerr := recordForgeLink(context.WithoutCancel(r.Context()), audits, "linked", target,
				name, typ); rerr != nil {
				log.Error("server: record a forge link that was not removed: " + rerr.Error())
			}
			respondError(w, log, http.StatusInternalServerError, "could not unlink the account")
			return
		}
		respondJSON(w, log, http.StatusOK, map[string]any{"unlinked": removed}, wantsPretty(r))
	}
}

// forgeLinkCallbackHandler finishes linking after the forge sends the browser back. It checks the
// signed state and that this browser is the one that started, exchanges the code for a token, reads
// the forge account's numeric id with it, refuses a bot, drops the token, stores the link, and
// records it on the audit chain. A link that cannot be recorded is removed again. Every answer is a
// redirect to the linked accounts page with a message it can show.
func forgeLinkCallbackHandler(links forgelink.Store, apps []forgelink.App, users user.Store,
	audits audit.Store, sealer *credential.Sealer, client *http.Client, publicURL string,
	log *zap.Logger) http.HandlerFunc {
	if client == nil {
		client = safedial.OffHostClient(forgeRequestTimeout)
	}
	return func(w http.ResponseWriter, r *http.Request) {
		cookie, cerr := r.Cookie(forgeLinkCookie)
		http.SetCookie(w, &http.Cookie{Name: forgeLinkCookie, Path: forgeLinkCookiePath, MaxAge: -1})
		if links == nil || users == nil || sealer == nil || !sealer.Enabled() {
			forgeLinkFail(w, r, "forge account linking is not set up on this server")
			return
		}
		q := r.URL.Query()
		app, st, err := openForgeState(sealer, apps, q.Get("state"), time.Now())
		if err != nil {
			forgeLinkFail(w, r, "this link request expired or is not valid, so start again")
			return
		}
		if cerr != nil || subtle.ConstantTimeCompare([]byte(cookie.Value), []byte(st.Nonce)) != 1 {
			forgeLinkFail(w, r, "this link was started in another browser, so it was not finished: "+
				"start it again from this browser")
			return
		}
		if q.Get("error") != "" {
			forgeLinkFail(w, r, "linking was canceled at the forge")
			return
		}
		u, err := users.Get(r.Context(), st.UserID)
		if err != nil {
			forgeLinkFail(w, r, "the account this link was for no longer exists")
			return
		}
		account, err := forgeAccountOf(r.Context(), app, forgeCallbackURL(publicURL), q.Get("code"),
			client)
		if errors.Is(err, errForgeBot) {
			forgeLinkFail(w, r, "this forge account is a bot, and a bot account cannot act as a person")
			return
		}
		if err != nil {
			log.Warn("server: read the forge account for a link: "+err.Error(),
				zap.String("provider", app.Provider), zap.String("api_url", app.APIURL))
			forgeLinkFail(w, r, "the forge did not confirm which account is yours, so nothing was linked")
			return
		}
		link := &forgelink.Link{
			ID: forgelink.NewID(), UserID: u.ID, Provider: app.Provider,
			APIURL:      forgelink.CanonicalAPIURL(app.Provider, app.APIURL),
			ForgeUserID: account.ID, CreatedAt: time.Now().UTC(),
		}
		// The link is recorded before it is stored, so a link never exists, not even for a moment a
		// comment could act in, without the entry that records it.
		if err := recordForgeLink(r.Context(), audits, "linked", link, u.Username,
			st.ActorType); err != nil {
			log.Error("server: record forge link: " + err.Error())
			forgeLinkFail(w, r, "the link could not be recorded in the audit trail, so it was not made")
			return
		}
		err = links.Create(r.Context(), link)
		if err != nil {
			// The chain says the link began, and it was never stored, so the chain says it ended.
			if rerr := recordForgeLink(context.WithoutCancel(r.Context()), audits, "unlinked", link,
				u.Username, st.ActorType); rerr != nil {
				log.Error("server: record a forge link that was not stored: " + rerr.Error())
			}
		}
		if errors.Is(err, forgelink.ErrLinked) {
			forgeLinkFail(w, r, "this forge account is already linked to an account, or yours already "+
				"links an account on this forge: unlink it first")
			return
		}
		if err != nil {
			log.Error("server: store forge link: " + err.Error())
			forgeLinkFail(w, r, "the link could not be saved")
			return
		}
		http.Redirect(w, r, forgeLinksPage+"#"+url.Values{
			"linked": {app.Provider}, "host": {forgeHost(app.WebURL)},
		}.Encode(), http.StatusFound)
	}
}

// linkingActor returns the signed-in account a forge link request acts for and reports whether the
// handler may go on. Linking needs a person's account, so a caller with none is refused, and an
// agent is refused a change to its account's links: a forge identity lets a comment act as the
// account, which an agent must never hold. It writes the refusal.
func linkingActor(w http.ResponseWriter, r *http.Request, log *zap.Logger, change bool) (Actor, bool) {
	actor, ok := actorFrom(r.Context())
	if !ok || actor.UserID == "" {
		respondError(w, log, http.StatusUnauthorized,
			"linking a forge account needs you to be signed in to your own account")
		return Actor{}, false
	}
	if change && (actor.Agent || actor.Type == actorTypeAgent) {
		respondError(w, log, http.StatusForbidden,
			"an agent cannot link or unlink forge accounts: only a person can")
		return Actor{}, false
	}
	return actor, true
}

// forgeViews describes the configured forges for the API.
func forgeViews(apps []forgelink.App) []forgeView {
	out := make([]forgeView, 0, len(apps))
	for _, a := range apps {
		out = append(out, forgeView{Provider: a.Provider, APIURL: a.APIURL, WebURL: a.WebURL,
			Host: forgeHost(a.WebURL)})
	}
	return out
}

// forgeHost returns the host of a forge's web address, or the address itself when it has none.
func forgeHost(webURL string) string {
	u, err := url.Parse(webURL)
	if err != nil || u.Host == "" {
		return webURL
	}
	return u.Host
}

// findForgeApp returns the configured application for the forge provider serves at apiURL, an empty
// apiURL naming the provider's public service.
func findForgeApp(apps []forgelink.App, provider, apiURL string) (forgelink.App, bool) {
	want := forgelink.CanonicalAPIURL(provider, apiURL)
	for _, a := range apps {
		if a.Provider == provider && forgelink.CanonicalAPIURL(a.Provider, a.APIURL) == want {
			return a, true
		}
	}
	return forgelink.App{}, false
}

// forgeCallbackURL returns the address a forge sends a person back to, empty when the server has no
// public address.
func forgeCallbackURL(publicURL string) string {
	base := strings.TrimRight(strings.TrimSpace(publicURL), "/")
	if base == "" {
		return ""
	}
	return base + forgeCallbackPath
}

// forgeOAuthConfig returns the authorization code flow for app, sending people back to callback.
func forgeOAuthConfig(app forgelink.App, callback string) *oauth2.Config {
	web := strings.TrimRight(app.WebURL, "/")
	cfg := &oauth2.Config{
		ClientID: app.ClientID, ClientSecret: app.ClientSecret, RedirectURL: callback,
		Endpoint: oauth2.Endpoint{
			AuthURL: web + "/login/oauth/authorize", TokenURL: web + "/login/oauth/access_token",
			AuthStyle: oauth2.AuthStyleInParams,
		},
	}
	if app.Provider == trigger.ProviderGitLab {
		cfg.Endpoint.AuthURL, cfg.Endpoint.TokenURL = web+"/oauth/authorize", web+"/oauth/token"
		cfg.Scopes = []string{"read_user"}
	}
	return cfg
}

// forgeAuthParams returns the extra authorization parameters app's forge takes. GitHub is asked not
// to offer creating an account, since only an existing account can be linked.
func forgeAuthParams(app forgelink.App) []oauth2.AuthCodeOption {
	if app.Provider == trigger.ProviderGitHub {
		return []oauth2.AuthCodeOption{oauth2.SetAuthURLParam("allow_signup", "false")}
	}
	return nil
}

// forgeStateMAC signs a link state's payload for app under a key derived from the server's own
// encryption key and the application's client secret together. Holding the client secret alone,
// which the forge's administrators can also read, therefore cannot mint a state for any account,
// and every replica sharing the server's key signs alike without shared state.
func forgeStateMAC(sealer *credential.Sealer, app forgelink.App, payload []byte) ([]byte, error) {
	secret := sha256.Sum256([]byte(app.ClientSecret))
	return sealer.MAC("forge-link-state\x00"+hex.EncodeToString(secret[:]), payload)
}

// signForgeState encodes st and signs it for app with the server's key and the client secret.
func signForgeState(sealer *credential.Sealer, app forgelink.App, st forgeLinkState) (string, error) {
	payload, err := json.Marshal(st)
	if err != nil {
		return "", err
	}
	mac, err := forgeStateMAC(sealer, app, payload)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(payload) + "." +
		base64.RawURLEncoding.EncodeToString(mac), nil
}

// openForgeState verifies a state's signature against the server's key and the application of the
// forge it names, and its expiry at now, and returns both.
func openForgeState(sealer *credential.Sealer, apps []forgelink.App, raw string,
	now time.Time) (forgelink.App, forgeLinkState, error) {
	encoded, sig, ok := strings.Cut(raw, ".")
	if !ok {
		return forgelink.App{}, forgeLinkState{}, errors.New("forge link: malformed state")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return forgelink.App{}, forgeLinkState{}, err
	}
	mac, err := base64.RawURLEncoding.DecodeString(sig)
	if err != nil {
		return forgelink.App{}, forgeLinkState{}, err
	}
	var st forgeLinkState
	if err := strictDecode(bytes.NewReader(payload), &st); err != nil {
		return forgelink.App{}, forgeLinkState{}, err
	}
	app, found := findForgeApp(apps, st.Provider, st.APIURL)
	if !found {
		return forgelink.App{}, forgeLinkState{}, errors.New("forge link: unknown forge")
	}
	want, err := forgeStateMAC(sealer, app, payload)
	if err != nil {
		return forgelink.App{}, forgeLinkState{}, err
	}
	if !hmac.Equal(mac, want) {
		return forgelink.App{}, forgeLinkState{}, errors.New("forge link: bad state signature")
	}
	if st.UserID == "" || st.Nonce == "" || now.Unix() > st.Expires {
		return forgelink.App{}, forgeLinkState{}, errors.New("forge link: state expired")
	}
	return app, st, nil
}

// forgeAccountOf exchanges code for a token at app's forge, reads the account the token belongs to,
// and returns its numeric id. A bot account is errForgeBot. The token is used for that one read and
// dropped: it is never stored, logged, or returned.
func forgeAccountOf(ctx context.Context, app forgelink.App, callback, code string,
	client *http.Client) (forgeAccount, error) {
	if code == "" {
		return forgeAccount{}, errors.New("no authorization code")
	}
	ctx, cancel := context.WithTimeout(ctx, forgeRequestTimeout)
	defer cancel()
	ctx = context.WithValue(ctx, oauth2.HTTPClient, client)
	token, err := forgeOAuthConfig(app, callback).Exchange(ctx, code)
	if err != nil {
		return forgeAccount{}, fmt.Errorf("exchange the code: %w", redactOAuthError(err))
	}
	defer func() { token.AccessToken, token.RefreshToken = "", "" }()
	return readForgeAccount(ctx, app, client, token.AccessToken)
}

// readForgeAccount reads the account access belongs to from app's forge and returns its numeric
// id. A bot account is errForgeBot. The token is sent in a header and never put in an error.
func readForgeAccount(ctx context.Context, app forgelink.App, client *http.Client,
	access string) (forgeAccount, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		strings.TrimRight(app.APIURL, "/")+"/user", nil)
	if err != nil {
		return forgeAccount{}, err
	}
	req.Header.Set("Authorization", "Bearer "+access)
	req.Header.Set("Accept", "application/json")
	if app.Provider == trigger.ProviderGitHub {
		req.Header.Set("Accept", "application/vnd.github+json")
		req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	}
	req.Header.Set("User-Agent", "switchtender-forge-link")
	resp, err := client.Do(req)
	if err != nil {
		return forgeAccount{}, fmt.Errorf("read the account: %s",
			strings.ReplaceAll(err.Error(), access, "***"))
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return forgeAccount{}, fmt.Errorf("read the account: the forge answered %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxForgeUserBody))
	if err != nil {
		return forgeAccount{}, fmt.Errorf("read the account: %w", err)
	}
	var account forgeAccount
	if err := decodeForeign(raw, &account); err != nil {
		return forgeAccount{}, fmt.Errorf("read the account: %w", err)
	}
	if account.ID <= 0 {
		return forgeAccount{}, errors.New("read the account: the forge named no numeric id")
	}
	if strings.EqualFold(account.Type, "Bot") || account.Bot {
		return forgeAccount{}, errForgeBot
	}
	return account, nil
}

// redactOAuthError returns what an OAuth exchange failure says without the forge's response body,
// which can echo the client secret or the code back.
func redactOAuthError(err error) error {
	var re *oauth2.RetrieveError
	if errors.As(err, &re) {
		code := 0
		if re.Response != nil {
			code = re.Response.StatusCode
		}
		return fmt.Errorf("the forge refused the code with status %d: %s", code, re.ErrorCode)
	}
	return err
}

// recordForgeLink appends the chain entry for a link or an unlink, committing to which forge
// account and which SwitchTender account it ties, and never to a login or a token. It returns nil
// when no trail is kept.
func recordForgeLink(ctx context.Context, audits audit.Store, action string, l *forgelink.Link,
	actor, actorType string) error {
	if audits == nil {
		return nil
	}
	body, err := forgelink.ChangeBody(action, l)
	if err != nil {
		return err
	}
	digest, nonce, err := audit.ContentDigestOf(body)
	if err != nil {
		return fmt.Errorf("digest forge link record: %w", err)
	}
	return audits.Append(ctx, &audit.Entry{
		ID: audit.NewID(), Actor: actor, ActorType: actorType, Method: http.MethodPost,
		Path: forgelink.ChangePath(l.ID, action), ContentDigest: digest, Nonce: nonce,
	})
}

// forgeLinkFail sends the browser back to the linked accounts page with a message it can show.
func forgeLinkFail(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, forgeLinksPage+"#"+url.Values{"error": {msg}}.Encode(), http.StatusFound)
}
