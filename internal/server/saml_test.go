package server

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/crewjam/saml"
	"go.uber.org/zap"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/user"
)

// testIDPMetadata is a minimal identity provider metadata document with a redirect SSO binding.
const testIDPMetadata = `<?xml version="1.0"?>
<EntityDescriptor xmlns="urn:oasis:names:tc:SAML:2.0:metadata" entityID="https://idp.example.com/metadata">
  <IDPSSODescriptor protocolSupportEnumeration="urn:oasis:names:tc:SAML:2.0:protocol">
    <SingleSignOnService Binding="urn:oasis:names:tc:SAML:2.0:bindings:HTTP-Redirect" Location="https://idp.example.com/sso"/>
  </IDPSSODescriptor>
</EntityDescriptor>`

// writeTestKeypair generates a self-signed RSA keypair and writes it as PEM files, returning the
// certificate and key paths.
func writeTestKeypair(t *testing.T) (certFile, keyFile string) {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey() error = %v", err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "switchtender-test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate() error = %v", err)
	}
	dir := t.TempDir()
	certFile = filepath.Join(dir, "sp.crt")
	keyFile = filepath.Join(dir, "sp.key")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	if err := os.WriteFile(certFile, certPEM, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	if err := os.WriteFile(keyFile, keyPEM, 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v", err)
	}
	return certFile, keyFile
}

// newTestSAML builds a SAMLAuth against a fake IdP metadata server.
func newTestSAML(t *testing.T, roleMap map[string]user.Role) *SAMLAuth {
	t.Helper()
	idp := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(testIDPMetadata))
	}))
	t.Cleanup(idp.Close)
	certFile, keyFile := writeTestKeypair(t)
	s, err := NewSAMLAuth(context.Background(), idp.URL, "https://switchtender.example.com",
		certFile, keyFile, "", "groups", user.RoleViewer, roleMap,
		user.NewMemStore(), auth.NewMemStore(), zap.NewNop())
	if err != nil {
		t.Fatalf("NewSAMLAuth() error = %v", err)
	}
	return s
}

// TestNewSAMLAuthValidation covers the required-setting and role checks.
func TestNewSAMLAuthValidation(t *testing.T) {
	t.Parallel()
	certFile, keyFile := writeTestKeypair(t)
	tests := []struct {
		Name        string
		MetadataURL string
		BaseURL     string
		Cert        string
		Key         string
		Role        user.Role
	}{{ // Test 0: A missing metadata URL is refused.
		Name: "no metadata", BaseURL: "https://x", Cert: certFile, Key: keyFile, Role: user.RoleViewer,
	}, { // Test 1: A missing base URL is refused.
		Name: "no base", MetadataURL: "https://idp/md", Cert: certFile, Key: keyFile, Role: user.RoleViewer,
	}, { // Test 2: A missing keypair is refused.
		Name: "no keypair", MetadataURL: "https://idp/md", BaseURL: "https://x", Role: user.RoleViewer,
	}, { // Test 3: An invalid default role is refused.
		Name: "bad role", MetadataURL: "https://idp/md", BaseURL: "https://x",
		Cert: certFile, Key: keyFile, Role: user.Role("boss"),
	}}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			_, err := NewSAMLAuth(context.Background(), test.MetadataURL, test.BaseURL,
				test.Cert, test.Key, "", "groups", test.Role, nil,
				user.NewMemStore(), auth.NewMemStore(), zap.NewNop())
			if err == nil {
				t.Fatal("NewSAMLAuth() error = nil, want error")
			}
		})
	}
}

// TestSAMLMetadata verifies the service provider metadata names the entity id and the ACS endpoint.
func TestSAMLMetadata(t *testing.T) {
	t.Parallel()
	s := newTestSAML(t, nil)
	rec := httptest.NewRecorder()
	s.metadata(rec, httptest.NewRequest(http.MethodGet, "/auth/saml/metadata", nil))
	body := rec.Body.String()
	if rec.Code != http.StatusOK {
		t.Fatalf("metadata status = %d, want 200", rec.Code)
	}
	if !strings.Contains(body, "https://switchtender.example.com/auth/saml/metadata") {
		t.Errorf("metadata missing entity id: %s", body)
	}
	if !strings.Contains(body, "https://switchtender.example.com/auth/saml/acs") {
		t.Errorf("metadata missing ACS endpoint: %s", body)
	}
}

// TestSAMLLoginRedirects verifies login sends the browser to the IdP and sets the state cookie.
func TestSAMLLoginRedirects(t *testing.T) {
	t.Parallel()
	s := newTestSAML(t, nil)
	rec := httptest.NewRecorder()
	s.login(rec, httptest.NewRequest(http.MethodGet, "/auth/saml/login", nil))
	if rec.Code != http.StatusFound {
		t.Fatalf("login status = %d, want 302", rec.Code)
	}
	if loc := rec.Header().Get("Location"); !strings.HasPrefix(loc, "https://idp.example.com/sso") {
		t.Errorf("login redirect = %q, want the IdP SSO URL", loc)
	}
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Name != samlCookie || cookies[0].Value == "" {
		t.Errorf("login cookies = %+v, want one %s cookie", cookies, samlCookie)
	}
}

// TestSAMLRequestIDRoundTrip covers the signed state cookie: a clean roundtrip, a tampered value,
// and a missing cookie.
func TestSAMLRequestIDRoundTrip(t *testing.T) {
	t.Parallel()
	s := newTestSAML(t, nil)

	// Test 0: A clean roundtrip returns the id.
	rec := httptest.NewRecorder()
	s.setRequestID(rec, "id_12345")
	req := httptest.NewRequest(http.MethodPost, "/auth/saml/acs", nil)
	req.AddCookie(rec.Result().Cookies()[0])
	id, err := s.readRequestID(req)
	if err != nil || id != "id_12345" {
		t.Fatalf("readRequestID() = %q, %v, want id_12345", id, err)
	}

	// Test 1: A tampered cookie is refused.
	c := rec.Result().Cookies()[0]
	c.Value = "x" + c.Value
	req = httptest.NewRequest(http.MethodPost, "/auth/saml/acs", nil)
	req.AddCookie(c)
	if _, err := s.readRequestID(req); err == nil {
		t.Error("readRequestID(tampered) error = nil, want error")
	}

	// Test 2: A missing cookie is refused.
	req = httptest.NewRequest(http.MethodPost, "/auth/saml/acs", nil)
	if _, err := s.readRequestID(req); err == nil {
		t.Error("readRequestID(missing) error = nil, want error")
	}
}

// TestSAMLAttributeHandling covers username selection and group extraction from an assertion.
func TestSAMLAttributeHandling(t *testing.T) {
	t.Parallel()
	assertion := &saml.Assertion{
		Subject: &saml.Subject{NameID: &saml.NameID{Value: "carol@example.com"}},
		AttributeStatements: []saml.AttributeStatement{{
			Attributes: []saml.Attribute{{
				Name: "urn:oid:2.5.4.42", FriendlyName: "displayName",
				Values: []saml.AttributeValue{{Value: "Carol"}},
			}, {
				Name:   "groups",
				Values: []saml.AttributeValue{{Value: "platform-Admins"}, {Value: "dev"}},
			}},
		}},
	}

	s := newTestSAML(t, map[string]user.Role{"platform-admins": user.RoleAdmin})

	// Test 0: The default username is the subject NameID.
	if got := s.username(assertion); got != "carol@example.com" {
		t.Errorf("username() = %q, want carol@example.com", got)
	}

	// Test 1: A configured attribute overrides the NameID, matched by friendly name.
	s.usernameAttr = "displayName"
	if got := s.username(assertion); got != "Carol" {
		t.Errorf("username(attr) = %q, want Carol", got)
	}

	// Test 2: Groups map to a role case-insensitively.
	role := roleForGroups(s.groups(assertion), s.roleMap, s.defaultRole)
	if role != user.RoleAdmin {
		t.Errorf("role = %q, want admin", role)
	}

	// Test 3: A missing groups attribute falls back to the default role.
	s.groupsAttr = "memberships"
	role = roleForGroups(s.groups(assertion), s.roleMap, s.defaultRole)
	if role != user.RoleViewer {
		t.Errorf("fallback role = %q, want viewer", role)
	}
}

// TestProtectsSAMLPaths verifies the token gate exempts the SAML handshake and still protects
// everything else under /auth/saml.
func TestProtectsSAMLPaths(t *testing.T) {
	t.Parallel()
	g := &authGate{}
	tests := []struct {
		Method string
		Path   string
		Want   bool
	}{{ // Test 0: The login redirect runs before the user has a token.
		Method: http.MethodGet, Path: "/auth/saml/login", Want: false,
	}, { // Test 1: The metadata read is public for IdP registration.
		Method: http.MethodGet, Path: "/auth/saml/metadata", Want: false,
	}, { // Test 2: The IdP posts the assertion without a token.
		Method: http.MethodPost, Path: "/auth/saml/acs", Want: false,
	}, { // Test 3: Any other SAML post stays protected.
		Method: http.MethodPost, Path: "/auth/saml/login", Want: true,
	}, { // Test 4: An API path is unaffected.
		Method: http.MethodGet, Path: "/v1/runs", Want: true,
	}}
	for _, test := range tests {
		req := httptest.NewRequest(test.Method, test.Path, nil)
		if got := g.protects(req); got != test.Want {
			t.Errorf("protects(%s %s) = %v, want %v", test.Method, test.Path, got, test.Want)
		}
	}
}

// TestSAMLCookieSurvivesTheCrossSiteAssertion covers the reason SAML sign-in could never complete. The
// identity provider returns its assertion as a form POST from its own origin to /auth/saml/acs, which
// is a cross-site POST. A SameSite=Lax cookie is not sent on one, so the request-id cookie written at
// the start of the handshake never arrived, readRequestID failed, and every assertion was refused as
// a sign-in this browser did not start. Nothing in the product's own tests noticed, because a test
// adds the cookie to the request by hand and a browser is the only thing that applies the rule.
//
// The cookie has to ride the cross-site POST, which means SameSite=None, which browsers accept only on
// a Secure cookie. On an https base URL that is exactly what it gets. On a plain http base URL, where
// Secure would make the cookie unusable, it stays Lax and the server says so at startup, because a
// SAML deployment on http cannot work anyway.
func TestSAMLCookieSurvivesTheCrossSiteAssertion(t *testing.T) {
	t.Parallel()
	s := newTestSAML(t, nil)
	rec := httptest.NewRecorder()
	s.login(rec, httptest.NewRequest(http.MethodGet, "/auth/saml/login", nil))
	cookies := rec.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("login cookies = %d, want 1", len(cookies))
	}
	c := cookies[0]
	if c.SameSite != http.SameSiteNoneMode {
		t.Errorf("request-id cookie SameSite = %v, want None: the identity provider posts its "+
			"assertion cross-site, and a Lax cookie is not sent on that request, so sign-in can "+
			"never complete", c.SameSite)
	}
	if !c.Secure {
		t.Error("request-id cookie is not Secure, which every browser requires before it will " +
			"accept SameSite=None")
	}
	if !c.HttpOnly {
		t.Error("request-id cookie is readable by script")
	}
}
