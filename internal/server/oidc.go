package server

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"go.uber.org/zap"
	"golang.org/x/oauth2"

	"github.com/dcadolph/switchtender/internal/auth"
	"github.com/dcadolph/switchtender/internal/user"
)

// oidcStateTTL bounds how long a sign-in may take from the redirect to the callback.
const oidcStateTTL = 10 * time.Minute

// oidcCookie names the short-lived signed cookie that carries the sign-in handshake.
const oidcCookie = "st_oidc"

// OIDCAuth signs users in through an OpenID Connect provider with the authorization code flow and
// PKCE. It provisions a local account on first sign-in and mints the same session token the
// password login does, so the rest of the system treats an SSO user like any other.
type OIDCAuth struct {
	// oauth holds the endpoints, client credentials, and scopes for the code flow.
	oauth *oauth2.Config
	// verifier checks an ID token's signature, issuer, audience, and expiry.
	verifier *oidc.IDTokenVerifier
	// users provisions and looks up accounts.
	users user.Store
	// tokens persists the minted session token.
	tokens auth.Store
	// defaultRole is granted to an account created on first sign-in.
	defaultRole user.Role
	// signKey signs the handshake cookie. It is derived from the client secret so every server
	// instance signs alike without shared session state.
	signKey []byte
	// secureCookie marks the handshake cookie secure, set when the redirect URL is https.
	secureCookie bool
	// log records sign-in failures without leaking token contents.
	log *zap.Logger
}

// newOIDCAuth discovers the provider at issuer and builds an OIDCAuth. Discovery does network I/O,
// so this runs at startup and a failure stops the server rather than half-enabling sign-in.
func NewOIDCAuth(ctx context.Context, issuer, clientID, clientSecret, redirectURL string,
	defaultRole user.Role, users user.Store, tokens auth.Store, log *zap.Logger) (*OIDCAuth, error) {
	if clientID == "" || clientSecret == "" || redirectURL == "" {
		return nil, errors.New("oidc: client id, client secret, and redirect url are required")
	}
	if !user.ValidRole(defaultRole) {
		return nil, fmt.Errorf("oidc: invalid default role %q", defaultRole)
	}
	provider, err := oidc.NewProvider(ctx, issuer)
	if err != nil {
		return nil, fmt.Errorf("oidc: discover %q: %w", issuer, err)
	}
	key := sha256.Sum256([]byte("switchtender-oidc\x00" + clientSecret))
	return &OIDCAuth{
		oauth: &oauth2.Config{
			ClientID:     clientID,
			ClientSecret: clientSecret,
			RedirectURL:  redirectURL,
			Endpoint:     provider.Endpoint(),
			Scopes:       []string{oidc.ScopeOpenID, "email", "profile"},
		},
		verifier:     provider.Verifier(&oidc.Config{ClientID: clientID}),
		users:        users,
		tokens:       tokens,
		defaultRole:  defaultRole,
		signKey:      key[:],
		secureCookie: strings.HasPrefix(strings.ToLower(redirectURL), "https"),
		log:          log,
	}, nil
}

// login starts sign-in: it generates state, a nonce, and a PKCE verifier, stashes them in a signed
// short-lived cookie, and redirects the browser to the provider.
func (o *OIDCAuth) login(w http.ResponseWriter, r *http.Request) {
	state, err := randToken()
	if err != nil {
		o.log.Error("oidc: generate state: " + err.Error())
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	nonce, err := randToken()
	if err != nil {
		o.log.Error("oidc: generate nonce: " + err.Error())
		http.Error(w, "sign-in unavailable", http.StatusInternalServerError)
		return
	}
	verifier := oauth2.GenerateVerifier()
	o.setHandshake(w, state, nonce, verifier)
	url := o.oauth.AuthCodeURL(state, oidc.Nonce(nonce), oauth2.S256ChallengeOption(verifier))
	http.Redirect(w, r, url, http.StatusFound)
}

// callback finishes sign-in: it validates the handshake, exchanges the code, verifies the ID token
// and its nonce, provisions or finds the account, mints a session token, and hands it to the
// browser in the redirect fragment where the UI stores it.
func (o *OIDCAuth) callback(w http.ResponseWriter, r *http.Request) {
	hs, err := o.readHandshake(r)
	o.clearHandshake(w)
	if err != nil {
		o.fail(w, r, "sign-in expired, try again")
		return
	}
	if r.URL.Query().Get("state") != hs.state {
		o.fail(w, r, "sign-in state mismatch")
		return
	}
	token, err := o.oauth.Exchange(r.Context(), r.URL.Query().Get("code"),
		oauth2.VerifierOption(hs.verifier))
	if err != nil {
		o.log.Warn("oidc: code exchange failed: " + err.Error())
		o.fail(w, r, "sign-in failed")
		return
	}
	rawID, ok := token.Extra("id_token").(string)
	if !ok {
		o.fail(w, r, "sign-in returned no id token")
		return
	}
	idToken, err := o.verifier.Verify(r.Context(), rawID)
	if err != nil {
		o.log.Warn("oidc: id token verify failed: " + err.Error())
		o.fail(w, r, "sign-in token invalid")
		return
	}
	if idToken.Nonce != hs.nonce {
		o.fail(w, r, "sign-in nonce mismatch")
		return
	}
	var claims struct {
		Email             string `json:"email"`
		PreferredUsername string `json:"preferred_username"`
	}
	if err := idToken.Claims(&claims); err != nil {
		o.fail(w, r, "sign-in claims unreadable")
		return
	}
	username := firstNonEmpty(claims.Email, claims.PreferredUsername, idToken.Subject)
	u, err := o.provision(r.Context(), username)
	if err != nil {
		o.log.Error("oidc: provision account: " + err.Error())
		o.fail(w, r, "could not sign in")
		return
	}
	plain, err := mintSessionToken(r.Context(), o.tokens, u)
	if err != nil {
		o.log.Error("oidc: mint session: " + err.Error())
		o.fail(w, r, "could not sign in")
		return
	}
	frag := url.Values{"access_token": {plain}, "role": {string(u.Role)}, "user": {u.Username}}
	http.Redirect(w, r, "/ui/#"+frag.Encode(), http.StatusFound)
}

// provision returns the account for username or creates one with the default role on first
// sign-in. A new account gets a random password so password login never works for it, keeping
// SSO the only way in.
func (o *OIDCAuth) provision(ctx context.Context, username string) (*user.User, error) {
	u, err := o.users.FindByUsername(ctx, username)
	if err == nil {
		return u, nil
	}
	if !errors.Is(err, user.ErrNotFound) {
		return nil, err
	}
	pw, err := randToken()
	if err != nil {
		return nil, err
	}
	u, err = user.New(username, pw, o.defaultRole)
	if err != nil {
		return nil, err
	}
	if err := o.users.Save(ctx, u); err != nil {
		return nil, err
	}
	o.log.Info("oidc: provisioned account " + username + " as " + string(o.defaultRole))
	return u, nil
}

// fail sends the browser back to the sign-in page with a message it can show.
func (o *OIDCAuth) fail(w http.ResponseWriter, r *http.Request, msg string) {
	http.Redirect(w, r, "/ui/login#"+url.Values{"error": {msg}}.Encode(), http.StatusFound)
}

// handshake is the state carried across the redirect in the signed cookie.
type handshake struct {
	// state is the CSRF value echoed back by the provider.
	state string
	// nonce binds the ID token to this sign-in against replay.
	nonce string
	// verifier is the PKCE code verifier.
	verifier string
}

// setHandshake writes the signed, short-lived handshake cookie.
func (o *OIDCAuth) setHandshake(w http.ResponseWriter, state, nonce, verifier string) {
	exp := strconv.FormatInt(time.Now().Add(oidcStateTTL).Unix(), 10)
	payload := strings.Join([]string{state, nonce, verifier, exp}, "|")
	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + o.sign([]byte(payload))
	http.SetCookie(w, &http.Cookie{
		Name: oidcCookie, Value: value, Path: "/auth/oidc", HttpOnly: true,
		Secure: o.secureCookie, SameSite: http.SameSiteLaxMode, MaxAge: int(oidcStateTTL.Seconds()),
	})
}

// readHandshake verifies the cookie signature and expiry and returns the handshake it carries.
func (o *OIDCAuth) readHandshake(r *http.Request) (*handshake, error) {
	c, err := r.Cookie(oidcCookie)
	if err != nil {
		return nil, err
	}
	encoded, sig, ok := strings.Cut(c.Value, ".")
	if !ok {
		return nil, errors.New("oidc: malformed handshake")
	}
	payload, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil, err
	}
	if !hmac.Equal([]byte(sig), []byte(o.sign(payload))) {
		return nil, errors.New("oidc: bad handshake signature")
	}
	fields := strings.Split(string(payload), "|")
	if len(fields) != 4 {
		return nil, errors.New("oidc: malformed handshake payload")
	}
	exp, err := strconv.ParseInt(fields[3], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return nil, errors.New("oidc: handshake expired")
	}
	return &handshake{state: fields[0], nonce: fields[1], verifier: fields[2]}, nil
}

// clearHandshake expires the handshake cookie once the callback has read it.
func (o *OIDCAuth) clearHandshake(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: oidcCookie, Path: "/auth/oidc", MaxAge: -1})
}

// sign returns the base64 HMAC-SHA256 of payload under the derived key.
func (o *OIDCAuth) sign(payload []byte) string {
	mac := hmac.New(sha256.New, o.signKey)
	mac.Write(payload)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// randToken returns a URL-safe 256-bit random string for state, nonce, and provisioned passwords.
// It returns an error rather than an empty string when the system RNG fails, so a caller never
// proceeds with an empty security value.
func randToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("random token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b[:]), nil
}

// firstNonEmpty returns the first non-empty string, or empty when all are empty.
func firstNonEmpty(vs ...string) string {
	for _, v := range vs {
		if v != "" {
			return v
		}
	}
	return ""
}
