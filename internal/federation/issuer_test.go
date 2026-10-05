package federation

import (
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"

	"github.com/kordloom/switchtender/internal/credential"
)

// testSealer is the sealer every test issuer seals its keys with. Deriving one costs an argon2id
// pass, so the tests share it.
var testSealer = credential.NewSealer("federation-test-passphrase", "federation-test-salt")

// testClock is a settable clock for an issuer under test.
type testClock struct {
	// mu guards now.
	mu sync.Mutex
	// now is the current reading.
	now time.Time
}

// Now returns the clock's reading.
func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// Advance moves the clock forward by d.
func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// newTestIssuer returns an issuer at issuerURL over a fresh in-memory key store, on clock when
// given.
func newTestIssuer(t *testing.T, issuerURL string, clock *testClock) (*Issuer, *MemKeyStore) {
	t.Helper()
	keys := NewMemKeyStore()
	var opts []Option
	if clock != nil {
		opts = append(opts, WithClock(clock.Now))
	}
	iss, err := NewIssuer(issuerURL, keys, testSealer, opts...)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	return iss, keys
}

// serveIssuer serves iss's discovery document and key set the way the server does, at the URL the
// returned issuer is built on. The returned issuer shares keys with nothing else.
func serveIssuer(t *testing.T, clock *testClock) (*Issuer, *httptest.Server) {
	t.Helper()
	var iss *Issuer
	mux := http.NewServeMux()
	mux.HandleFunc("GET "+DiscoveryPath, func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(iss.Discovery())
	})
	mux.HandleFunc("GET "+JWKSPath, func(w http.ResponseWriter, r *http.Request) {
		set, err := iss.JWKS(r.Context())
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		_ = json.NewEncoder(w).Encode(set)
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	iss, _ = newTestIssuer(t, srv.URL, clock)
	return iss, srv
}

// sampleSubject is the subject sampleClaims produces.
const sampleSubject = "org:org_acme:project:proj_web:template:tpl_deploy:env:prod:" +
	"run_type:apply:approved:true"

// sampleClaims returns the run identity a test mints for.
func sampleClaims() Claims {
	return Claims{
		Audience: "sts.amazonaws.com", RunID: "run_abc", OrgID: "org_acme", ProjectID: "proj_web",
		TemplateID: "tpl_deploy", Environment: "prod", CredentialID: "cred_aws", Tool: "terraform",
		RunType: RunTypeApply, Source: "template", CommitSHA: "deadbeef", LauncherType: LauncherPerson,
		Actor: "operator-one", ActorType: "session", ActorUserID: "user_1", Approved: true,
		ApprovedBy: "approver-two", ApprovedByType: "session", ApprovalPolicy: "prod gate",
	}
}

// TestIssuerURL pins which URLs a cloud can treat as an issuer.
func TestIssuerURL(t *testing.T) {
	t.Parallel()
	tests := []struct {
		In      string
		WantURL string
		Want    error
	}{{ // Test 0: An https URL is kept.
		In: "https://switchtender.example.com", WantURL: "https://switchtender.example.com",
	}, { // Test 1: A trailing slash is dropped, so iss and the discovery URL agree byte for byte.
		In: "https://switchtender.example.com/", WantURL: "https://switchtender.example.com",
	}, { // Test 2: A path prefix behind a proxy is kept.
		In: "https://example.com/switchtender", WantURL: "https://example.com/switchtender",
	}, { // Test 3: Plain http on a loopback host is allowed for a local trial.
		In: "http://127.0.0.1:8080", WantURL: "http://127.0.0.1:8080",
	}, { // Test 4: Plain http anywhere else is refused.
		In: "http://switchtender.example.com", Want: ErrIssuer,
	}, { // Test 5: A query is refused.
		In: "https://switchtender.example.com/?x=1", Want: ErrIssuer,
	}, { // Test 6: Credentials in the URL are refused.
		In: "https://user:pass@switchtender.example.com", Want: ErrIssuer,
	}, { // Test 7: A relative URL is refused.
		In: "switchtender.example.com", Want: ErrIssuer,
	}, { // Test 8: A fragment is refused.
		In: "https://switchtender.example.com/#frag", Want: ErrIssuer,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := IssuerURL(test.In)
			if !errors.Is(err, test.Want) {
				t.Fatalf("IssuerURL(%q) error = %v, want %v", test.In, err, test.Want)
			}
			if diff := cmp.Diff(test.WantURL, got); diff != "" {
				t.Errorf("IssuerURL(%q) mismatch (-want +got):\n%s", test.In, diff)
			}
		})
	}
}

// TestNewIssuerNeedsASealer pins that an issuer refuses to exist with no encryption key, since its
// private key would otherwise be stored in the clear.
func TestNewIssuerNeedsASealer(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Sealer *credential.Sealer
		Want   error
	}{{ // Test 0: No sealer at all.
		Sealer: nil, Want: credential.ErrNoKey,
	}, { // Test 1: A sealer with no key configured.
		Sealer: credential.NewSealer("", ""), Want: credential.ErrNoKey,
	}, { // Test 2: An enabled sealer works.
		Sealer: testSealer, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, err := NewIssuer("https://st.example.com", NewMemKeyStore(), test.Sealer)
			if !errors.Is(err, test.Want) {
				t.Errorf("NewIssuer() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestDiscoveryDocument pins the discovery document's issuer, key set location, algorithm, and that
// its claim list is exactly the claims a token carries. A list that drifted from the token would
// tell a cloud administrator writing a policy about a claim that never arrives.
func TestDiscoveryDocument(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, "https://st.example.com/", nil)
	got := iss.Discovery()
	want := Discovery{
		Issuer: "https://st.example.com", JWKSURI: "https://st.example.com/.well-known/jwks.json",
		ResponseTypesSupported: []string{"id_token"}, SubjectTypesSupported: []string{"public"},
		IDTokenSigningAlgValuesSupported: []string{"RS256"}, ScopesSupported: []string{"openid"},
		ClaimsSupported: ClaimNames(),
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Discovery() mismatch (-want +got):\n%s", diff)
	}

	rt := reflect.TypeOf(Claims{})
	tags := make([]string, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		tags = append(tags, rt.Field(i).Tag.Get("json"))
	}
	if diff := cmp.Diff(tags, ClaimNames()); diff != "" {
		t.Errorf("ClaimNames() does not match the Claims json tags (-tags +names):\n%s", diff)
	}
}

// TestJWKSPublishesOnlyPublicKeys pins that the key set holds the signing key's public half, under
// the RFC 7638 thumbprint as its id, and nothing private.
func TestJWKSPublishesOnlyPublicKeys(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	iss, keys := newTestIssuer(t, "https://st.example.com", nil)
	empty, err := iss.JWKS(ctx)
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	if len(empty.Keys) != 0 {
		t.Fatalf("JWKS() before any key = %d keys, want 0", len(empty.Keys))
	}
	if err := iss.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	set, err := iss.JWKS(ctx)
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	if len(set.Keys) != 1 {
		t.Fatalf("JWKS() = %d keys, want 1", len(set.Keys))
	}
	k := set.Keys[0]
	pub, ok := k.Key.(*rsa.PublicKey)
	if !ok {
		t.Fatalf("published key is %T, want *rsa.PublicKey", k.Key)
	}
	if pub.N.BitLen() != rsaBits {
		t.Errorf("published key is %d bits, want %d", pub.N.BitLen(), rsaBits)
	}
	thumb, err := k.Thumbprint(crypto.SHA256)
	if err != nil {
		t.Fatalf("Thumbprint() error = %v", err)
	}
	if k.KeyID != base64.RawURLEncoding.EncodeToString(thumb) {
		t.Errorf("kid = %q, want the RFC 7638 thumbprint", k.KeyID)
	}
	if k.Algorithm != "RS256" || k.Use != "sig" {
		t.Errorf("alg, use = %q, %q, want RS256, sig", k.Algorithm, k.Use)
	}
	raw, err := json.Marshal(set)
	if err != nil {
		t.Fatalf("marshal key set: %v", err)
	}
	var doc struct {
		Keys []map[string]any `json:"keys"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal key set: %v", err)
	}
	for _, private := range []string{"d", "p", "q", "dp", "dq", "qi"} {
		if _, leaked := doc.Keys[0][private]; leaked {
			t.Errorf("the published key carries the private parameter %q", private)
		}
	}
	stored, err := keys.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if strings.Contains(stored[0].Sealed, "PRIVATE KEY") {
		t.Error("the stored private key is not sealed")
	}
}

// verifyToken checks token against the key set iss publishes and returns its claims, failing the
// test when it does not verify.
func verifyToken(t *testing.T, iss *Issuer, token string) (Claims, string) {
	t.Helper()
	jws, err := jose.ParseSigned(token, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		t.Fatalf("ParseSigned() error = %v", err)
	}
	kid := jws.Signatures[0].Header.KeyID
	set, err := iss.JWKS(context.Background())
	if err != nil {
		t.Fatalf("JWKS() error = %v", err)
	}
	found := set.Key(kid)
	if len(found) != 1 {
		t.Fatalf("kid %q is not in the published key set", kid)
	}
	payload, err := jws.Verify(found[0].Key)
	if err != nil {
		t.Fatalf("Verify() error = %v", err)
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		t.Fatalf("unmarshal claims: %v", err)
	}
	return c, kid
}

// TestMintCarriesTheRunIdentity pins every claim a minted token carries, the registered ones
// stamped by the issuer and the run identity passed through, and that the token verifies against
// the key set.
func TestMintCarriesTheRunIdentity(t *testing.T) {
	t.Parallel()
	clock := &testClock{now: time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)}
	iss, _ := newTestIssuer(t, "https://st.example.com", clock)
	token, expires, err := iss.Mint(context.Background(), sampleClaims(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	got, _ := verifyToken(t, iss, token)
	want := sampleClaims()
	want.Issuer = "https://st.example.com"
	want.Subject = sampleSubject
	want.IssuedAt = clock.Now().Unix()
	want.NotBefore = clock.Now().Unix()
	want.ExpiresAt = clock.Now().Add(10 * time.Minute).Unix()
	if diff := cmp.Diff(want, got, cmpopts.IgnoreFields(Claims{}, "ID")); diff != "" {
		t.Errorf("claims mismatch (-want +got):\n%s", diff)
	}
	if len(got.ID) != 32 {
		t.Errorf("jti = %q, want 32 hex characters", got.ID)
	}
	if !expires.Equal(clock.Now().Add(10 * time.Minute)) {
		t.Errorf("expiry = %v, want %v", expires, clock.Now().Add(10*time.Minute))
	}
	again, _, err := iss.Mint(context.Background(), sampleClaims(), 10*time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	second, _ := verifyToken(t, iss, again)
	if second.ID == got.ID {
		t.Error("two tokens share a jti")
	}
}

// TestMintRefusesWhatCannotVerify pins the refusals: no audience, a lifetime outside the bounds,
// and a subject segment that would shift the others.
func TestMintRefusesWhatCannotVerify(t *testing.T) {
	t.Parallel()
	iss, _ := newTestIssuer(t, "https://st.example.com", nil)
	colon := sampleClaims()
	colon.Environment = "prod:approved:true"
	noAudience := sampleClaims()
	noAudience.Audience = ""
	tests := []struct {
		Claims Claims
		TTL    time.Duration
		Want   error
	}{{ // Test 0: No audience.
		Claims: noAudience, TTL: DefaultTokenTTL, Want: ErrSetting,
	}, { // Test 1: A lifetime under a minute.
		Claims: sampleClaims(), TTL: 30 * time.Second, Want: ErrSetting,
	}, { // Test 2: A lifetime over an hour.
		Claims: sampleClaims(), TTL: 2 * time.Hour, Want: ErrSetting,
	}, { // Test 3: A colon inside a segment.
		Claims: colon, TTL: DefaultTokenTTL, Want: ErrSubject,
	}, { // Test 4: The bounds themselves are allowed.
		Claims: sampleClaims(), TTL: MaxTokenTTL, Want: nil,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			_, _, err := iss.Mint(context.Background(), test.Claims, test.TTL)
			if !errors.Is(err, test.Want) {
				t.Errorf("Mint() error = %v, want %v", err, test.Want)
			}
		})
	}
}

// TestSubject pins the subject format clouds match on.
func TestSubject(t *testing.T) {
	t.Parallel()
	wild := sampleClaims()
	wild.TemplateID = "tpl_*"
	tests := []struct {
		Claims      Claims
		WantSubject string
		Want        error
	}{{ // Test 0: Every segment filled.
		Claims:      sampleClaims(),
		WantSubject: sampleSubject,
	}, { // Test 1: Empty segments read none, and a dry run that nobody approved says so.
		Claims:      Claims{RunType: RunTypeDryRun},
		WantSubject: "org:none:project:none:template:none:env:none:run_type:dry_run:approved:false",
	}, { // Test 2: A wildcard inside a value is refused, so it cannot widen a pattern.
		Claims: wild, Want: ErrSubject,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			got, err := Subject(test.Claims)
			if !errors.Is(err, test.Want) {
				t.Fatalf("Subject() error = %v, want %v", err, test.Want)
			}
			if diff := cmp.Diff(test.WantSubject, got); diff != "" {
				t.Errorf("Subject() mismatch (-want +got):\n%s", diff)
			}
		})
	}
}

// TestTokenVerifiesWithAStandardRelyingParty runs a minted token through go-oidc, an independent
// OpenID Connect verifier that discovers the issuer over HTTP and fetches the key set the way a
// cloud does, and pins audience and expiry enforcement from the relying party's side.
func TestTokenVerifiesWithAStandardRelyingParty(t *testing.T) {
	t.Parallel()
	clock := &testClock{now: time.Now().Truncate(time.Second)}
	iss, _ := serveIssuer(t, clock)
	ctx := context.Background()
	provider, err := oidc.NewProvider(ctx, iss.URL())
	if err != nil {
		t.Fatalf("NewProvider() error = %v", err)
	}
	token, _, err := iss.Mint(ctx, sampleClaims(), 5*time.Minute)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	tests := []struct {
		Audience string
		At       time.Time
		WantOK   bool
	}{{ // Test 0: The right audience inside the lifetime verifies.
		Audience: "sts.amazonaws.com", At: clock.Now().Add(time.Minute), WantOK: true,
	}, { // Test 1: Another relying party's audience is refused.
		Audience: "api://AzureADTokenExchange", At: clock.Now().Add(time.Minute), WantOK: false,
	}, { // Test 2: After expiry the token is refused.
		Audience: "sts.amazonaws.com", At: clock.Now().Add(6 * time.Minute), WantOK: false,
	}}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			verifier := provider.Verifier(&oidc.Config{
				ClientID: test.Audience, Now: func() time.Time { return test.At },
			})
			idt, err := verifier.Verify(ctx, token)
			if (err == nil) != test.WantOK {
				t.Fatalf("Verify() error = %v, want ok %v", err, test.WantOK)
			}
			if err != nil {
				return
			}
			var c Claims
			if err := idt.Claims(&c); err != nil {
				t.Fatalf("Claims() error = %v", err)
			}
			if c.RunID != "run_abc" || !c.Approved || c.ApprovedBy != "approver-two" {
				t.Errorf("claims read by the relying party = %+v", c)
			}
		})
	}
}

// TestEnsureIsIdempotent pins that a key is generated once, so every process starting against one
// database does not add a key of its own on each start.
func TestEnsureIsIdempotent(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	iss, keys := newTestIssuer(t, "https://st.example.com", nil)
	for range 3 {
		if err := iss.Ensure(ctx); err != nil {
			t.Fatalf("Ensure() error = %v", err)
		}
	}
	stored, err := keys.List(ctx)
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(stored) != 1 {
		t.Errorf("keys after three Ensure() calls = %d, want 1", len(stored))
	}
}

// TestASecondProcessSignsWithTheSharedKey pins that two issuers over one store, the serve process
// and a worker, sign with the same key, so a token a worker mints verifies against the set the
// server publishes.
func TestASecondProcessSignsWithTheSharedKey(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	keys := NewMemKeyStore()
	server, err := NewIssuer("https://st.example.com", keys, testSealer)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	worker, err := NewIssuer("https://st.example.com", keys, testSealer)
	if err != nil {
		t.Fatalf("NewIssuer() error = %v", err)
	}
	if err := server.Ensure(ctx); err != nil {
		t.Fatalf("Ensure() error = %v", err)
	}
	token, _, err := worker.Mint(ctx, sampleClaims(), DefaultTokenTTL)
	if err != nil {
		t.Fatalf("Mint() error = %v", err)
	}
	verifyToken(t, server, token)
}
