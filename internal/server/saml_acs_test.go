package server

import (
	"context"
	"encoding/base64"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// samlACSRequest returns a POST to the assertion consumer with the given body, carrying a signed
// request-id cookie unless withCookie is false. That cookie is the record of a sign-in this server
// actually started, which is what an assertion has to answer.
func samlACSRequest(t *testing.T, s *SAMLAuth, withCookie bool, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/auth/saml/acs", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if withCookie {
		rec := httptest.NewRecorder()
		s.setRequestID(rec, "id-from-this-servers-request")
		for _, c := range rec.Result().Cookies() {
			req.AddCookie(c)
		}
	}
	return req
}

// TestSAMLACSRefusesEverythingItCannotVerify drives the assertion consumer itself, which nothing
// executed before this.
//
// An ACS endpoint is a public POST target: anyone can post to it. Everything that keeps it from
// being an open door is a refusal inside this handler, and each one had no test. The unsigned and
// forged cases are the whole attack: if a response that carries no valid signature from the
// registered identity provider reaches the account lookup, then whoever posted it chose the
// username, and a POST is enough to become an administrator.
//
// The refusal is checked by what it leaves behind as well as by what it returns. A handler that
// redirects to the sign-in page but has already provisioned an account or minted a token has not
// refused anything.
func TestSAMLACSRefusesEverythingItCannotVerify(t *testing.T) {
	t.Parallel()
	assertion := `<samlp:Response xmlns:samlp="urn:oasis:names:tc:SAML:2.0:protocol" ` +
		`xmlns:saml="urn:oasis:names:tc:SAML:2.0:assertion" Version="2.0" ` +
		`ID="_forged" IssueInstant="2026-01-01T00:00:00Z" ` +
		`Destination="https://switchtender.example.com/auth/saml/acs" ` +
		`InResponseTo="id-from-this-servers-request">` +
		`<saml:Issuer>https://idp.example.com/metadata</saml:Issuer>` +
		`<saml:Assertion ID="_a" Version="2.0" IssueInstant="2026-01-01T00:00:00Z">` +
		`<saml:Subject><saml:NameID>admin</saml:NameID></saml:Subject>` +
		`</saml:Assertion></samlp:Response>`

	tests := []struct {
		Name     string
		Cookie   bool
		Body     string
		WantText string
	}{
		{
			Name: "no request id cookie, so this server never started this sign-in",
			// An assertion nobody asked for is an unsolicited one, and accepting it is the
			// IdP-initiated replay: an assertion captured once is postable forever.
			Cookie: false, Body: "SAMLResponse=" + url.QueryEscape(
				base64.StdEncoding.EncodeToString([]byte(assertion))),
			WantText: "expired",
		},
		{
			Name:   "an unsigned assertion naming an administrator",
			Cookie: true,
			Body: "SAMLResponse=" + url.QueryEscape(
				base64.StdEncoding.EncodeToString([]byte(assertion))),
			WantText: "sign-in failed",
		},
		{
			Name:     "a SAMLResponse that is not even base64",
			Cookie:   true,
			Body:     "SAMLResponse=%25%25not-base64%25%25",
			WantText: "sign-in failed",
		},
		{
			Name:     "base64 that decodes to something that is not XML",
			Cookie:   true,
			Body:     "SAMLResponse=" + base64.StdEncoding.EncodeToString([]byte("hello")),
			WantText: "sign-in failed",
		},
		{
			Name:     "an empty POST with no SAMLResponse at all",
			Cookie:   true,
			Body:     "",
			WantText: "sign-in failed",
		},
		{
			Name: "a body that is not decodable as a form",
			// ParseForm fails before anything is read out of it, and the handler has to stop there
			// rather than continue with an empty form.
			Cookie: true, Body: "SAMLResponse=%zz", WantText: "unreadable",
		},
	}
	for _, test := range tests {
		t.Run(test.Name, func(t *testing.T) {
			t.Parallel()
			s := newTestSAML(t, nil)
			req := samlACSRequest(t, s, test.Cookie, test.Body)
			rec := httptest.NewRecorder()

			s.acs(rec, req)

			if rec.Code != http.StatusFound {
				t.Fatalf("status = %d, want a redirect back to sign-in", rec.Code)
			}
			loc := rec.Header().Get("Location")
			if !strings.Contains(loc, "/ui/login") {
				t.Errorf("redirect = %q, want the sign-in page", loc)
			}
			// The message rides in a query-encoded fragment, so spaces arrive as plus signs.
			if !strings.Contains(loc, strings.ReplaceAll(test.WantText, " ", "+")) {
				t.Errorf("redirect = %q, want it to name %q", loc, test.WantText)
			}
			ctx := context.Background()
			accounts, err := s.users.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(accounts) != 0 {
				t.Errorf("a refused assertion provisioned %d account(s)", len(accounts))
			}
			tokens, err := s.tokens.List(ctx)
			if err != nil {
				t.Fatalf("List() error = %v", err)
			}
			if len(tokens) != 0 {
				t.Errorf("a refused assertion minted %d token(s)", len(tokens))
			}
		})
	}
}

// TestSAMLACSClearsTheRequestIDOnRefusal checks a refused sign-in does not leave the handshake
// cookie in the browser. A request id that survives a failure is one an attacker can keep answering
// with captured assertions until it is used, which is the replay window the id exists to close.
func TestSAMLACSClearsTheRequestIDOnRefusal(t *testing.T) {
	t.Parallel()
	s := newTestSAML(t, nil)
	req := samlACSRequest(t, s, true, "SAMLResponse=bm90LXhtbA==")
	rec := httptest.NewRecorder()

	s.acs(rec, req)

	var cleared bool
	for _, c := range rec.Result().Cookies() {
		if c.Name == samlCookie && c.MaxAge < 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Errorf("cookies = %v, want the request id cookie expired after a refusal",
			rec.Result().Cookies())
	}
}
