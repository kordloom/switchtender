package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// fakeIDP is an OpenID Connect provider that serves discovery, a JWKS, and a token endpoint, so the
// callback handler can be driven past the code exchange and the ID token verification.
//
// Everything after the handshake guards had no test: the nonce comparison, the missing id_token
// branch, the unreadable claims branch, and the session token the sign-in mints. Those are the
// checks that bind an ID token to this one sign-in and decide what the browser walks away holding,
// and they can only be reached with a provider on the other end of the exchange.
type fakeIDP struct {
	// srv serves the discovery document, the JWKS, and the token endpoint.
	srv *httptest.Server
	// key signs the ID tokens.
	key *rsa.PrivateKey
	// mu guards the fields the test rewrites between requests.
	mu sync.Mutex
	// nonce is written into the next ID token's nonce claim.
	nonce string
	// claims are merged into the next ID token on top of the required ones.
	claims map[string]any
	// omitIDToken drops the id_token from the token response, the shape a plain OAuth2 provider
	// returns when the openid scope was lost.
	omitIDToken bool
	// idTokenOverride replaces the signed ID token with this exact string when set.
	idTokenOverride string
	// lastTokenForm records the form the exchange posted, so PKCE can be checked on the wire.
	lastTokenForm url.Values
}

// newFakeIDP starts a provider and returns it. It is closed when the test ends.
func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate signing key: %v", err)
	}
	idp := &fakeIDP{key: key, claims: map[string]any{}}
	mux := http.NewServeMux()
	idp.srv = httptest.NewServer(mux)
	t.Cleanup(idp.srv.Close)

	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"issuer":                                idp.srv.URL,
			"authorization_endpoint":                idp.srv.URL + "/authorize",
			"token_endpoint":                        idp.srv.URL + "/token",
			"jwks_uri":                              idp.srv.URL + "/jwks",
			"id_token_signing_alg_values_supported": []string{"RS256"},
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{{
			Key: key.Public(), KeyID: "test", Algorithm: string(jose.RS256), Use: "sig",
		}}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			http.Error(w, "bad form", http.StatusBadRequest)
			return
		}
		idp.mu.Lock()
		idp.lastTokenForm = r.PostForm
		omit, override, nonce := idp.omitIDToken, idp.idTokenOverride, idp.nonce
		extra := make(map[string]any, len(idp.claims))
		for k, v := range idp.claims {
			extra[k] = v
		}
		idp.mu.Unlock()

		body := map[string]any{"access_token": "at", "token_type": "bearer"}
		switch {
		case omit:
		case override != "":
			body["id_token"] = override
		default:
			body["id_token"] = idp.signIDToken(t, nonce, extra)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(body)
	})
	return idp
}

// signIDToken returns an RS256 ID token for this provider carrying nonce and the extra claims.
func (i *fakeIDP) signIDToken(t *testing.T, nonce string, extra map[string]any) string {
	t.Helper()
	claims := map[string]any{
		"iss":   i.srv.URL,
		"aud":   "test-client",
		"sub":   "subject-1",
		"exp":   time.Now().Add(time.Hour).Unix(),
		"iat":   time.Now().Unix(),
		"nonce": nonce,
	}
	for k, v := range extra {
		claims[k] = v
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: i.key},
		(&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", "test"),
	)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	sig, err := signer.Sign(payload)
	if err != nil {
		t.Fatalf("sign id token: %v", err)
	}
	raw, err := sig.CompactSerialize()
	if err != nil {
		t.Fatalf("serialize id token: %v", err)
	}
	return raw
}

// set applies f under the provider's lock, for a test rewriting what the next token response says.
func (i *fakeIDP) set(f func(*fakeIDP)) {
	i.mu.Lock()
	defer i.mu.Unlock()
	f(i)
}

// oidcFlow is one sign-in under test: the auth wired to a fake provider plus the stores it writes.
type oidcFlow struct {
	// auth is the handler under test.
	auth *OIDCAuth
	// idp is the provider on the other end of the exchange.
	idp *fakeIDP
	// users holds the accounts the sign-in provisions or adopts.
	users user.Store
	// tokens holds what the sign-in mints.
	tokens auth.Store
}

// newOIDCFlow discovers the fake provider and returns a live OIDCAuth wired to memory stores.
func newOIDCFlow(t *testing.T) *oidcFlow {
	t.Helper()
	idp := newFakeIDP(t)
	users, tokens := user.NewMemStore(), auth.NewMemStore()
	o, err := NewOIDCAuth(context.Background(), idp.srv.URL, "test-client", "test-secret",
		"https://app.example.com/auth/oidc/callback", user.RoleOperator, users, tokens, zap.NewNop())
	if err != nil {
		t.Fatalf("NewOIDCAuth() error = %v", err)
	}
	return &oidcFlow{auth: o, idp: idp, users: users, tokens: tokens}
}

// begin runs login and returns the state and nonce the server chose plus a callback request already
// carrying the handshake cookie, which is what a browser comes back from the provider holding.
func (f *oidcFlow) begin(t *testing.T) (state, nonce string, req *http.Request) {
	t.Helper()
	rec := httptest.NewRecorder()
	f.auth.login(rec, httptest.NewRequest(http.MethodGet, "/auth/oidc/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want a redirect to the provider", rec.Code)
	}
	loc, err := url.Parse(rec.Header().Get("Location"))
	if err != nil {
		t.Fatalf("parse the authorization redirect: %v", err)
	}
	q := loc.Query()
	state, nonce = q.Get("state"), q.Get("nonce")
	if state == "" || nonce == "" {
		t.Fatalf("authorization redirect carried state=%q nonce=%q, want both", state, nonce)
	}
	// PKCE is the defense against a stolen authorization code being redeemed by anyone but this
	// browser. The challenge has to be on the wire, and it has to be the hashed form.
	if got := q.Get("code_challenge_method"); got != "S256" {
		t.Errorf("code_challenge_method = %q, want S256", got)
	}
	if q.Get("code_challenge") == "" {
		t.Error("authorization redirect carried no code_challenge, so PKCE is not in force")
	}
	req = httptest.NewRequest(http.MethodGet,
		"/auth/oidc/callback?state="+url.QueryEscape(state)+"&code=test-code", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return state, nonce, req
}

// TestOIDCSignInMintsASessionAndBindsTheNonce drives the whole authorization code flow against a
// real provider and checks both halves of a successful sign-in: the ID token has to be bound to this
// handshake by its nonce, and what the browser leaves with has to be a session token, not a
// long-lived API token that a sign out would have no way to revoke.
func TestOIDCSignInMintsASessionAndBindsTheNonce(t *testing.T) {
	t.Parallel()
	f := newOIDCFlow(t)
	_, nonce, req := f.begin(t)
	f.idp.set(func(i *fakeIDP) {
		i.nonce = nonce
		i.claims = map[string]any{"email": "person@example.com", "email_verified": true}
	})

	rec := httptest.NewRecorder()
	f.auth.callback(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want a redirect", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/ui/#") {
		t.Fatalf("redirect = %q, want the signed-in UI", loc)
	}
	frag, err := url.ParseQuery(strings.TrimPrefix(loc, "/ui/#"))
	if err != nil {
		t.Fatalf("parse the redirect fragment: %v", err)
	}
	if got := frag.Get("user"); got != "person@example.com" {
		t.Errorf("user = %q, want the verified email", got)
	}
	if got := frag.Get("role"); got != string(user.RoleOperator) {
		t.Errorf("role = %q, want the configured default role", got)
	}
	if frag.Get("access_token") == "" {
		t.Fatal("the redirect carried no access token, so the sign-in handed the browser nothing")
	}

	// The exchange has to have carried the PKCE verifier, or the challenge in the redirect was
	// decoration and a stolen code is redeemable by anyone.
	f.idp.mu.Lock()
	verifier := f.idp.lastTokenForm.Get("code_verifier")
	f.idp.mu.Unlock()
	if verifier == "" {
		t.Error("the token exchange sent no code_verifier, so PKCE is not completed")
	}

	tokens, err := f.tokens.List(req.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("minted %d tokens, want exactly 1", len(tokens))
	}
	tok := tokens[0]
	if !tok.IsSession() {
		t.Errorf("minted token kind = %q, want a session so signing out can revoke it", tok.Kind)
	}
	if tok.ExpiresAt == nil {
		t.Error("the session token never expires, so a browser holds it forever")
	} else if tok.Expired(time.Now()) {
		t.Error("the session token is already expired")
	}
	accounts, err := f.users.List(req.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(accounts) != 1 || accounts[0].Source != "oidc" {
		t.Fatalf("accounts = %+v, want one provisioned with source oidc", accounts)
	}
	if tok.UserID != accounts[0].ID {
		t.Errorf("token is bound to %q, want the account it signed in %q", tok.UserID, accounts[0].ID)
	}
}

// TestOIDCCallbackRefusesAReplayedIDToken is the nonce check, executed rather than merely present.
//
// The nonce binds an ID token to the one handshake that asked for it. Without the comparison, an ID
// token captured from any earlier sign-in at the same provider, for any user, is replayable into a
// fresh handshake: the signature verifies, the issuer and audience are right, and the only thing
// that would have said "this is not the token this sign-in asked for" is the nonce. Deleting that
// comparison left the suite green before this test.
func TestOIDCCallbackRefusesAReplayedIDToken(t *testing.T) {
	t.Parallel()
	f := newOIDCFlow(t)
	_, _, req := f.begin(t)
	f.idp.set(func(i *fakeIDP) {
		// A well-formed token from a different handshake at the same provider.
		i.nonce = "a-nonce-from-some-other-sign-in"
		i.claims = map[string]any{"email": "admin@example.com", "email_verified": true}
	})

	rec := httptest.NewRecorder()
	f.auth.callback(rec, req)

	assertOIDCRefusal(t, rec, f, "nonce mismatch")
}

// TestOIDCCallbackRefusesATokenResponseWithoutAnIDToken covers the branch a plain OAuth2 provider
// produces, or one whose openid scope was dropped. There is no identity in that response, so the
// only safe move is to refuse rather than fall through to empty claims.
func TestOIDCCallbackRefusesATokenResponseWithoutAnIDToken(t *testing.T) {
	t.Parallel()
	f := newOIDCFlow(t)
	_, _, req := f.begin(t)
	f.idp.set(func(i *fakeIDP) { i.omitIDToken = true })

	rec := httptest.NewRecorder()
	f.auth.callback(rec, req)

	assertOIDCRefusal(t, rec, f, "no id token")
}

// TestOIDCCallbackRefusesAnUnverifiableIDToken covers a token the verifier rejects. A forged or
// wrongly signed token must not reach the claims decoding, because everything after that point
// treats the claims as the provider's word.
func TestOIDCCallbackRefusesAnUnverifiableIDToken(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name  string
		Token func(t *testing.T, f *oidcFlow, nonce string) string
	}{
		{
			Name: "signed by a key the provider does not publish",
			Token: func(t *testing.T, f *oidcFlow, nonce string) string {
				t.Helper()
				// Claim the real issuer and audience while holding a key the JWKS never lists,
				// which is what a forged token looks like.
				forger := &fakeIDP{srv: f.idp.srv, key: newFakeIDP(t).key}
				return forger.signIDToken(t, nonce, nil)
			},
		},
		{
			Name: "not a JWT at all",
			Token: func(t *testing.T, _ *oidcFlow, _ string) string {
				t.Helper()
				return "not-a-token"
			},
		},
		{
			Name: "issued to a different client",
			Token: func(t *testing.T, f *oidcFlow, nonce string) string {
				t.Helper()
				return f.idp.signIDToken(t, nonce, map[string]any{"aud": "some-other-client"})
			},
		},
		{
			Name: "already expired",
			Token: func(t *testing.T, f *oidcFlow, nonce string) string {
				t.Helper()
				return f.idp.signIDToken(t, nonce,
					map[string]any{"exp": time.Now().Add(-time.Hour).Unix()})
			},
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			f := newOIDCFlow(t)
			_, nonce, req := f.begin(t)
			raw := test.Token(t, f, nonce)
			f.idp.set(func(i *fakeIDP) { i.idTokenOverride = raw })

			rec := httptest.NewRecorder()
			f.auth.callback(rec, req)

			assertOIDCRefusal(t, rec, f, "token invalid")
		})
	}
}

// TestOIDCCallbackRefusesAFailedExchange covers the provider answering the code exchange with an
// error, which is what a replayed or already-redeemed code looks like from here.
func TestOIDCCallbackRefusesAFailedExchange(t *testing.T) {
	t.Parallel()
	f := newOIDCFlow(t)
	_, _, req := f.begin(t)
	// Close the provider so the exchange cannot complete at all.
	f.idp.srv.Close()

	rec := httptest.NewRecorder()
	f.auth.callback(rec, req)

	assertOIDCRefusal(t, rec, f, "sign-in failed")
}

// TestOIDCUnverifiedClaimCannotTakeOverAnExistingAccount runs the account takeover guard through the
// real callback rather than through provision alone.
//
// Most providers let a person type any address into their profile. email_verified is the only claim
// that says one was proven. An unverified claim naming an account that already exists must be
// refused, or asserting an administrator's address is enough to become that administrator. The guard
// is unit tested; this checks it is actually wired into the handler, and that a refusal mints
// nothing.
func TestOIDCUnverifiedClaimCannotTakeOverAnExistingAccount(t *testing.T) {
	t.Parallel()
	f := newOIDCFlow(t)
	existing, err := user.New("admin@example.com", "a-long-enough-password", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	existing.Source = "oidc"
	if err := f.users.Save(context.Background(), existing); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	_, nonce, req := f.begin(t)
	f.idp.set(func(i *fakeIDP) {
		i.nonce = nonce
		i.claims = map[string]any{"email": "admin@example.com", "email_verified": false}
	})

	rec := httptest.NewRecorder()
	f.auth.callback(rec, req)

	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want a redirect back to sign-in", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/ui/login") {
		t.Fatalf("redirect = %q, want the sign-in page", loc)
	}
	tokens, err := f.tokens.List(req.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(tokens) != 0 {
		t.Fatalf("a refused sign-in minted %d token(s) for an admin account", len(tokens))
	}
}

// TestOIDCSignInAdoptsAVerifiedExistingAccount is the other side of the takeover guard: a sign-in
// the provider vouched for must still reach the account it names, or verified users are locked out.
func TestOIDCSignInAdoptsAVerifiedExistingAccount(t *testing.T) {
	t.Parallel()
	f := newOIDCFlow(t)
	existing, err := user.New("person@example.com", "a-long-enough-password", user.RoleAdmin)
	if err != nil {
		t.Fatalf("user.New() error = %v", err)
	}
	existing.Source = "oidc"
	if err := f.users.Save(context.Background(), existing); err != nil {
		t.Fatalf("Save() error = %v", err)
	}

	_, nonce, req := f.begin(t)
	f.idp.set(func(i *fakeIDP) {
		i.nonce = nonce
		i.claims = map[string]any{"email": "person@example.com", "email_verified": true}
	})

	rec := httptest.NewRecorder()
	f.auth.callback(rec, req)

	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/ui/#") {
		t.Fatalf("redirect = %q, want the signed-in UI", loc)
	}
	frag, err := url.ParseQuery(strings.TrimPrefix(loc, "/ui/#"))
	if err != nil {
		t.Fatalf("parse the redirect fragment: %v", err)
	}
	// The account already held admin, and the default role must not quietly demote it.
	if got := frag.Get("role"); got != string(user.RoleAdmin) {
		t.Errorf("role = %q, want the existing account's role %q", got, user.RoleAdmin)
	}
	accounts, err := f.users.List(req.Context())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(accounts) != 1 {
		t.Errorf("accounts = %d, want the existing one adopted rather than a second created",
			len(accounts))
	}
}

// TestOIDCSignInFallsBackToTheSubjectWithoutAnEmail checks a provider that asserts no email at all,
// which several enterprise issuers do. A sign-in still has to name somebody, and the subject is the
// only claim guaranteed to be there.
func TestOIDCSignInFallsBackToTheSubjectWithoutAnEmail(t *testing.T) {
	t.Parallel()
	f := newOIDCFlow(t)
	_, nonce, req := f.begin(t)
	f.idp.set(func(i *fakeIDP) { i.nonce = nonce })

	rec := httptest.NewRecorder()
	f.auth.callback(rec, req)

	loc := rec.Header().Get("Location")
	if !strings.HasPrefix(loc, "/ui/#") {
		t.Fatalf("redirect = %q, want the signed-in UI", loc)
	}
	frag, err := url.ParseQuery(strings.TrimPrefix(loc, "/ui/#"))
	if err != nil {
		t.Fatalf("parse the redirect fragment: %v", err)
	}
	if got := frag.Get("user"); got != "subject-1" {
		t.Errorf("user = %q, want the subject claim", got)
	}
}

// TestOIDCLoginIssuesAFreshHandshakeEachTime checks two sign-ins do not share a state or a nonce.
// A reused state is a CSRF token that stops being one, and a reused nonce is a replay window.
func TestOIDCLoginIssuesAFreshHandshakeEachTime(t *testing.T) {
	t.Parallel()
	f := newOIDCFlow(t)
	firstState, firstNonce, _ := f.begin(t)
	secondState, secondNonce, _ := f.begin(t)

	if firstState == secondState {
		t.Errorf("both sign-ins used state %q", firstState)
	}
	if firstNonce == secondNonce {
		t.Errorf("both sign-ins used nonce %q", firstNonce)
	}
}

// TestOIDCCallbackIsSafeUnderConcurrentSignIns runs several sign-ins at once through one OIDCAuth,
// which is how it is actually used: one instance serves every browser. Each has its own handshake,
// so each must come back with its own account and its own token, with no crossing.
func TestOIDCCallbackIsSafeUnderConcurrentSignIns(t *testing.T) {
	t.Parallel()
	const signIns = 8

	// One provider cannot hold a different nonce per concurrent exchange, so give each sign-in its
	// own provider and its own auth, sharing the stores that the concurrency actually contends on.
	users, tokens := user.NewMemStore(), auth.NewMemStore()
	var wg sync.WaitGroup
	for i := range signIns {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			flow := newOIDCFlow(t)
			flow.users, flow.tokens = users, tokens
			flow.auth.users, flow.auth.tokens = users, tokens
			_, nonce, req := flow.begin(t)
			flow.idp.set(func(i *fakeIDP) {
				i.nonce = nonce
				i.claims = map[string]any{
					"email":          "person" + string(rune('a'+n)) + "@example.com",
					"email_verified": true,
				}
			})
			rec := httptest.NewRecorder()
			flow.auth.callback(rec, req)
			if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "/ui/#") {
				t.Errorf("sign-in %d redirected to %q, want the signed-in UI", n, loc)
			}
		}(i)
	}
	wg.Wait()

	got, err := tokens.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(got) != signIns {
		t.Errorf("minted %d tokens, want %d, so a concurrent sign-in lost or reused one",
			len(got), signIns)
	}
	seen := make(map[string]bool, len(got))
	for _, tok := range got {
		if seen[tok.UserID] {
			t.Errorf("two tokens are bound to the same account %q", tok.UserID)
		}
		seen[tok.UserID] = true
	}
	accounts, err := users.List(context.Background())
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(accounts) != signIns {
		t.Errorf("provisioned %d accounts, want %d", len(accounts), signIns)
	}
}

// assertOIDCRefusal checks a sign-in ended back at the login page naming want, and that it left no
// account and no token behind. A refusal that still minted something is not a refusal.
func assertOIDCRefusal(t *testing.T, rec *httptest.ResponseRecorder, f *oidcFlow, want string) {
	t.Helper()
	if rec.Code != http.StatusFound {
		t.Fatalf("callback status = %d, want a redirect back to sign-in", rec.Code)
	}
	loc := rec.Header().Get("Location")
	if !strings.Contains(loc, "/ui/login") {
		t.Fatalf("redirect = %q, want the sign-in page", loc)
	}
	if !strings.Contains(loc, strings.ReplaceAll(want, " ", "+")) {
		t.Errorf("redirect = %q, want it to name %q", loc, want)
	}
	ctx := context.Background()
	tokens, err := f.tokens.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(tokens) != 0 {
		t.Errorf("a refused sign-in minted %d token(s)", len(tokens))
	}
	accounts, err := f.users.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(accounts) != 0 {
		t.Errorf("a refused sign-in provisioned %d account(s)", len(accounts))
	}
}
