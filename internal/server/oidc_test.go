package server

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// oidcTest builds an OIDCAuth with only the signing key set, enough to exercise the handshake
// cookie without discovering a real provider.
func oidcTest() *OIDCAuth {
	return &OIDCAuth{signKey: []byte("test-signing-key")}
}

// cookieReq returns a request carrying the handshake cookie that a recorder was given.
func cookieReq(t *testing.T, rec *httptest.ResponseRecorder) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
	for _, c := range rec.Result().Cookies() {
		req.AddCookie(c)
	}
	return req
}

// TestOIDCHandshakeRoundTrip checks that a written handshake reads back with the same values.
func TestOIDCHandshakeRoundTrip(t *testing.T) {
	t.Parallel()
	o := oidcTest()
	rec := httptest.NewRecorder()
	o.setHandshake(rec, "state-1", "nonce-1", "verifier-1")

	hs, err := o.readHandshake(cookieReq(t, rec))
	if err != nil {
		t.Fatalf("readHandshake() error = %v", err)
	}
	if hs.state != "state-1" || hs.nonce != "nonce-1" || hs.verifier != "verifier-1" {
		t.Errorf("handshake = %+v, want state-1/nonce-1/verifier-1", hs)
	}
}

// TestOIDCHandshakeTamper checks that changing the payload invalidates the signature.
func TestOIDCHandshakeTamper(t *testing.T) {
	t.Parallel()
	o := oidcTest()
	rec := httptest.NewRecorder()
	o.setHandshake(rec, "state-1", "nonce-1", "verifier-1")
	cookie := rec.Result().Cookies()[0]

	encoded, sig, _ := strings.Cut(cookie.Value, ".")
	payload, _ := base64.RawURLEncoding.DecodeString(encoded)
	payload[0] ^= 0xff
	cookie.Value = base64.RawURLEncoding.EncodeToString(payload) + "." + sig

	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
	req.AddCookie(cookie)
	if _, err := o.readHandshake(req); err == nil {
		t.Error("readHandshake() accepted a tampered payload")
	}
}

// TestOIDCHandshakeExpired checks that a past expiry is rejected even with a valid signature.
func TestOIDCHandshakeExpired(t *testing.T) {
	t.Parallel()
	o := oidcTest()
	past := strconv.FormatInt(time.Now().Add(-time.Minute).Unix(), 10)
	payload := strings.Join([]string{"s", "n", "v", past}, "|")
	value := base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + o.sign([]byte(payload))

	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
	req.AddCookie(&http.Cookie{Name: oidcCookie, Value: value})
	if _, err := o.readHandshake(req); err == nil {
		t.Error("readHandshake() accepted an expired handshake")
	}
}

// TestOIDCHandshakeMissing checks that no cookie is an error, not a panic.
func TestOIDCHandshakeMissing(t *testing.T) {
	t.Parallel()
	req := httptest.NewRequest(http.MethodGet, "/auth/oidc/callback", nil)
	if _, err := oidcTest().readHandshake(req); err == nil {
		t.Error("readHandshake() with no cookie should error")
	}
}

// TestOIDCBrandMatchesRegistrableHost proves the sign-in button brand is chosen by the issuer's host,
// not a substring of the whole issuer, so a known provider is labeled and a vanity domain that merely
// contains a provider's name is not mislabeled.
func TestOIDCBrandMatchesRegistrableHost(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Issuer string
		Want   string
	}{
		{Issuer: "https://accounts.google.com", Want: "google"},
		{Issuer: "https://login.microsoftonline.com/tenant/v2.0", Want: "microsoft"},
		{Issuer: "https://sts.windows.net/abc/", Want: "microsoft"},
		{Issuer: "https://token.actions.example.gitlab.com", Want: "gitlab"},
		{Issuer: "https://dev-12345.okta.com", Want: "okta"},
		{Issuer: "https://github.com", Want: "github"},
		// A vanity host that only contains a provider's name must not borrow its brand.
		{Issuer: "https://auth.mycompany-google.com", Want: ""},
		{Issuer: "https://notgoogle.example.com", Want: ""},
		{Issuer: "https://keycloak.internal/realms/x", Want: ""},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d", testNum), func(t *testing.T) {
			t.Parallel()
			if got := oidcBrand(test.Issuer); got != test.Want {
				t.Errorf("oidcBrand(%q) = %q, want %q", test.Issuer, got, test.Want)
			}
		})
	}
}
